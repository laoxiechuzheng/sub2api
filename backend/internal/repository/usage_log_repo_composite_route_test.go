package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPrepareUsageLogInsertPersistsCompositeRouteAttribution(t *testing.T) {
	routeID := int64(42)
	kind := "compaction"
	chain := "gemini-3.8-flash-high (429) → backup"
	prepared := prepareUsageLogInsert(&service.UsageLog{
		RequestID: "client:route", Model: "gpt-6.1-sol", CreatedAt: time.Now(),
		CompositeRouteID: &routeID, CompositeRouteKind: &kind, CompositeFallbackChain: &chain,
	})

	require.Len(t, prepared.args, len(usageLogInsertArgTypes))
	n := len(prepared.args)
	require.Equal(t, []string{"bigint", "text", "text", "timestamptz"}, usageLogInsertArgTypes[n-4:])
	require.Equal(t, sql.NullInt64{Int64: 42, Valid: true}, prepared.args[n-4])
	require.Equal(t, sql.NullString{String: kind, Valid: true}, prepared.args[n-3])
	require.Equal(t, sql.NullString{String: chain, Valid: true}, prepared.args[n-2])

	plain := prepareUsageLogInsert(&service.UsageLog{RequestID: "client:plain", Model: "gpt-5", CreatedAt: time.Now()})
	require.Equal(t, sql.NullInt64{}, plain.args[n-4])
	require.Equal(t, sql.NullString{}, plain.args[n-3])
	require.Equal(t, sql.NullString{}, plain.args[n-2])
}

func TestUsageLogInsertStatementsListCompositeRouteColumns(t *testing.T) {
	prepared := prepareUsageLogInsert(&service.UsageLog{RequestID: "client:route", Model: "gpt-5", CreatedAt: time.Now()})
	batch, _ := buildUsageLogBatchInsertQuery([]string{"k"}, map[string]usageLogInsertPrepared{"k": prepared})
	bestEffort, _ := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
	for _, query := range []string{batch, bestEffort} {
		// input CTE, INSERT column list and SELECT list each carry the three columns.
		require.Equal(t, 3, countSubstring(query, "composite_route_id,"))
		require.Equal(t, 3, countSubstring(query, "composite_fallback_chain,"))
	}
}

func countSubstring(s, sub string) int {
	count := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			count++
		}
	}
	return count
}

func TestUsageLogRepositoryListWithFiltersCompositeRoute(t *testing.T) {
	for _, tt := range []struct {
		filter string
		where  string
	}{
		{usagestats.CompositeRouteFilterRouted, "composite_route_id IS NOT NULL"},
		{usagestats.CompositeRouteFilterCompaction, "composite_route_id IS NOT NULL AND composite_route_kind = 'compaction'"},
		{usagestats.CompositeRouteFilterFallback, "composite_route_id IS NOT NULL AND composite_fallback_chain IS NOT NULL"},
		{usagestats.CompositeRouteFilterNone, "composite_route_id IS NULL"},
	} {
		t.Run(tt.filter, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			userID := int64(7)
			filters := usagestats.UsageLogFilters{UserID: userID, CompositeRoute: tt.filter, ExactTotal: true}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM usage_logs WHERE user_id = $1 AND " + tt.where)).
				WithArgs(userID).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta("FROM usage_logs WHERE user_id = $1 AND "+tt.where+" ORDER BY id DESC LIMIT $2 OFFSET $3")).
				WithArgs(userID, 20, 0).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			_, _, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20}, filters)
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestParseCompositeRouteFilter(t *testing.T) {
	for _, value := range []string{"", "routed", "COMPACTION", " fallback ", "none"} {
		_, ok := usagestats.ParseCompositeRouteFilter(value)
		require.True(t, ok, value)
	}
	_, ok := usagestats.ParseCompositeRouteFilter("routed' OR 1=1")
	require.False(t, ok)
}

func TestCompositeRouteFiltersAlwaysUseExactPaginationTotal(t *testing.T) {
	for _, filter := range []string{"routed", "compaction", "fallback", "none"} {
		t.Run(filter, func(t *testing.T) {
			require.False(t, shouldUseFastUsageLogTotal(usagestats.UsageLogFilters{CompositeRoute: filter}))
			for _, pageNumber := range []int{1, 2, 3, 5} {
				db, mock := newSQLMock(t)
				repo := &usageLogRepository{sql: db}
				mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM usage_logs WHERE ").
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(100)))
				mock.ExpectQuery("FROM usage_logs WHERE .* ORDER BY id DESC LIMIT \\$1 OFFSET \\$2").
					WithArgs(20, (pageNumber-1)*20).
					WillReturnRows(sqlmock.NewRows([]string{"id"}))
				_, page, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: pageNumber, PageSize: 20}, usagestats.UsageLogFilters{CompositeRoute: filter})
				require.NoError(t, err)
				require.EqualValues(t, 100, page.Total)
				require.Equal(t, 5, page.Pages)
				require.NoError(t, mock.ExpectationsWereMet())
			}
		})
	}
	require.True(t, shouldUseFastUsageLogTotal(usagestats.UsageLogFilters{}))
}
