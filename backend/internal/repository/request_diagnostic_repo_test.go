package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestDiagnosticRepositorySaveCanonicalBindingAndCapacity(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewRequestDiagnosticRepository(db)
	now := time.Now().UTC()
	record := &service.RequestDiagnosticRecord{APIKeyID: 7, UsageRequestID: "client:exact", UsageCreatedAt: now, CapturedAt: now, ExpiresAt: now.Add(time.Hour), PayloadGzip: []byte{1, 2}, PayloadBytes: 123}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_try_advisory_xact_lock(242, 1)`)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectExec(`(?s)INSERT INTO usage_request_diagnostics.*SELECT \$1::bigint, \$2::varchar, \$3::timestamptz, \$4::timestamptz, \$5::timestamptz, \$6::bytea, \$7::integer.*WHERE EXISTS.*api_key_id = \$1 AND request_id = \$2.*created_at = \$3::timestamptz.*ON CONFLICT \(api_key_id, usage_request_id\) DO NOTHING`).WithArgs(record.APIKeyID, record.UsageRequestID, record.UsageCreatedAt, record.CapturedAt, record.ExpiresAt, record.PayloadGzip, record.PayloadBytes).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)WITH budget AS.*octet_length\(payload_gzip\).*SET payload_gzip = ''::bytea, payload_bytes = 0.*total > \$1`).WithArgs(int64(256 << 20)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, repo.Save(context.Background(), record, 256<<20))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRequestDiagnosticRepositoryStorageLockNeverWaits(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	mock.ExpectQuery(`pg_try_advisory_xact_lock`).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	mock.ExpectRollback()
	err = NewRequestDiagnosticRepository(db).Save(context.Background(), &service.RequestDiagnosticRecord{}, 1)
	require.ErrorContains(t, err, "storage busy")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRequestDiagnosticRepositoryGetUsesKeyAndHidesExpiredPayload(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	query := `(?s)SELECT.*CASE WHEN expires_at > now\(\) THEN payload_gzip ELSE ''::bytea END.*api_key_id = \$1 AND usage_request_id = \$2`
	columns := []string{"api_key_id", "usage_request_id", "usage_created_at", "captured_at", "expires_at", "payload_gzip", "payload_bytes"}
	mock.ExpectQuery(query).WithArgs(int64(9), "client:old").WillReturnRows(sqlmock.NewRows(columns).AddRow(9, "client:old", now, now, now.Add(-time.Hour), []byte{}, 0))
	record, err := NewRequestDiagnosticRepository(db).Get(context.Background(), 9, "client:old")
	require.NoError(t, err)
	require.Empty(t, record.PayloadGzip)
	require.Zero(t, record.PayloadBytes)
	mock.ExpectQuery(query).WithArgs(int64(9), "client:missing").WillReturnError(sql.ErrNoRows)
	record, err = NewRequestDiagnosticRepository(db).Get(context.Background(), 9, "client:missing")
	require.NoError(t, err)
	require.Nil(t, record)
	mock.ExpectQuery(query).WithArgs(int64(9), "client:db-error").WillReturnError(errors.New("connection failed"))
	_, err = NewRequestDiagnosticRepository(db).Get(context.Background(), 9, "client:db-error")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRequestDiagnosticRepositoryCleanupClearsBoundedExpiredAndOrphanRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectExec(`(?s)WITH expired AS.*expires_at <= now\(\).*LIMIT 500.*SET payload_gzip = ''::bytea, payload_bytes = 0`).WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectExec(`(?s)WITH stale AS.*interval '7 days'.*NOT EXISTS.*u.api_key_id = d.api_key_id AND u.request_id = d.usage_request_id.*u.created_at = d.usage_created_at.*LIMIT 500.*DELETE FROM usage_request_diagnostics`).WillReturnResult(sqlmock.NewResult(0, 2))
	require.NoError(t, NewRequestDiagnosticRepository(db).Cleanup(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
}
