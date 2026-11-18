//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 通过真正Apply入口覆盖claim→effects→commit/rollback，而非直接测试effects helper。
func TestUsageBillingV0222ApplyTransactionAndIdempotencyContract(t *testing.T) {
	for _, scenario := range []string{
		"fresh_commit", "duplicate_no_effect", "duplicate_conflict", "archive_no_effect", "archive_conflict",
		"claim_error_rollback", "archive_error_rollback", "effect_error_rollback", "commit_error",
	} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := &usageBillingRepository{db: db}
			cmd := &service.UsageBillingCommand{
				RequestID: " v0222-billing-request ", RequestFingerprint: "v0222-fingerprint", APIKeyID: 7, UserID: 42, BalanceCost: 2.5,
			}
			sentinel := errors.New("synthetic billing transaction failure")
			mock.ExpectBegin()
			claim := mock.ExpectQuery(`(?s)INSERT INTO usage_billing_dedup .*ON CONFLICT .*DO NOTHING.*RETURNING id`).
				WithArgs("v0222-billing-request", int64(7), "v0222-fingerprint")
			duplicate := scenario == "duplicate_no_effect" || scenario == "duplicate_conflict"
			switch {
			case scenario == "claim_error_rollback":
				claim.WillReturnError(sentinel)
			case duplicate:
				claim.WillReturnError(sql.ErrNoRows)
				fingerprint := "v0222-fingerprint"
				if scenario == "duplicate_conflict" {
					fingerprint = "different-fingerprint"
				}
				mock.ExpectQuery(`(?s)SELECT request_fingerprint\s+FROM usage_billing_dedup\s+WHERE request_id = \$1 AND api_key_id = \$2`).
					WithArgs("v0222-billing-request", int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow(fingerprint))
			default:
				claim.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(99)))
				archived := mock.ExpectQuery(`(?s)SELECT request_fingerprint\s+FROM usage_billing_dedup_archive\s+WHERE request_id = \$1 AND api_key_id = \$2`).
					WithArgs("v0222-billing-request", int64(7))
				switch scenario {
				case "archive_no_effect":
					archived.WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow("v0222-fingerprint"))
				case "archive_conflict":
					archived.WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow("different-fingerprint"))
				case "archive_error_rollback":
					archived.WillReturnError(sentinel)
				default:
					archived.WillReturnError(sql.ErrNoRows)
				}
			}
			effects := scenario == "fresh_commit" || scenario == "effect_error_rollback" || scenario == "commit_error"
			if effects {
				deduct := mock.ExpectQuery(conditionalBalanceDeductSQL).WithArgs(2.5, int64(42))
				if scenario == "effect_error_rollback" {
					deduct.WillReturnError(sentinel)
				} else {
					deduct.WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(7.5))
				}
			}
			if scenario == "fresh_commit" {
				mock.ExpectCommit()
			} else if scenario == "commit_error" {
				mock.ExpectCommit().WillReturnError(sentinel)
			} else {
				mock.ExpectRollback()
			}

			result, err := repo.Apply(context.Background(), cmd)
			switch scenario {
			case "fresh_commit":
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.Applied)
				require.NotNil(t, result.NewBalance)
				require.Equal(t, 7.5, *result.NewBalance)
			case "duplicate_no_effect", "archive_no_effect":
				require.NoError(t, err)
				require.Equal(t, &service.UsageBillingApplyResult{Applied: false}, result)
			case "duplicate_conflict", "archive_conflict":
				require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
				require.Nil(t, result)
			default:
				require.ErrorIs(t, err, sentinel)
				require.Nil(t, result)
			}
			// 未声明任何effect SQL的duplicate/archive场景如产生扣费会直接失败。
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
