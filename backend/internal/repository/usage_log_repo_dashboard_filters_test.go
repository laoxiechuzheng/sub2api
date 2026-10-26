//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestUsageDashboardModelSourceKeepsRequestedFilter(t *testing.T) {
	for _, source := range []string{usagestats.ModelSourceRequested, usagestats.ModelSourceUpstream, usagestats.ModelSourceMapping} {
		t.Run(source, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			end := start.Add(24 * time.Hour)
			mock.ExpectQuery(regexp.QuoteMeta("AND "+resolveModelDimensionExpression(usagestats.ModelSourceRequested)+" = $3")).
				WithArgs(start, end, "public-a", "image").
				WillReturnRows(sqlmock.NewRows([]string{"model", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost", "account_cost"}))
			_, err := repo.GetModelStatsWithUsageFiltersBySource(context.Background(), start, end, usagestats.UsageLogFilters{
				Model: "public-a", ModelFilterSource: usagestats.ModelSourceRequested, BillingMode: "image", CompositeRoute: "fallback",
			}, source)
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUsageDashboardBreakdownCombinesRequestedModelBillingModeAndAudit(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	matched := false
	want := "AND " + resolveModelDimensionExpressionWithAlias(usagestats.ModelSourceRequested, "ul") + " = $4"
	mock.ExpectQuery(regexp.QuoteMeta(want)+".*"+regexp.QuoteMeta("ul.upstream_model_mismatch IS FALSE")+".*"+regexp.QuoteMeta("ul.composite_route_id IS NOT NULL")).
		WithArgs(start, end, "upstream-a", "public-a", "image").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "requests", "input_tokens", "output_tokens", "cache_tokens", "total_tokens", "cost", "actual_cost", "account_cost"}))
	_, err := repo.GetUserBreakdownStats(context.Background(), start, end, usagestats.UserBreakdownDimension{
		Model: "upstream-a", ModelType: usagestats.ModelSourceUpstream, RequestedModel: "public-a", BillingMode: "image", UpstreamModelMismatch: &matched, CompositeRoute: "fallback",
	}, 50)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
