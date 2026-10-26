package repository

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestCompositeRouteConditions(t *testing.T) {
	for _, tt := range []struct {
		name      string
		filter    string
		aliased   []string
		unaliased []string
	}{
		{
			"routed", usagestats.CompositeRouteFilterRouted,
			[]string{"ul.composite_route_id IS NOT NULL"},
			[]string{"composite_route_id IS NOT NULL"},
		},
		{
			"compaction", usagestats.CompositeRouteFilterCompaction,
			[]string{"ul.composite_route_id IS NOT NULL", "ul.composite_route_kind = 'compaction'"},
			[]string{"composite_route_id IS NOT NULL", "composite_route_kind = 'compaction'"},
		},
		{
			"fallback", usagestats.CompositeRouteFilterFallback,
			[]string{"ul.composite_route_id IS NOT NULL", "ul.composite_fallback_chain IS NOT NULL"},
			[]string{"composite_route_id IS NOT NULL", "composite_fallback_chain IS NOT NULL"},
		},
		{
			"none", usagestats.CompositeRouteFilterNone,
			[]string{"ul.composite_route_id IS NULL"},
			[]string{"composite_route_id IS NULL"},
		},
		{"empty", "", nil, nil},
		{"unknown", "bogus", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.aliased, compositeRouteConditions(tt.filter, "ul"))
			require.Equal(t, tt.unaliased, compositeRouteConditions(tt.filter, ""))
			// The unaliased appender must keep emitting the same conditions.
			require.Equal(t, tt.unaliased, appendCompositeRouteWhereCondition(nil, tt.filter))
		})
	}
}

func TestAppendCompositeRouteQueryFilter(t *testing.T) {
	require.Equal(t, "SELECT 1", appendCompositeRouteQueryFilter("SELECT 1", "", "ul"))
	require.Equal(t, "SELECT 1", appendCompositeRouteQueryFilter("SELECT 1", "bogus", ""))
	require.Equal(t,
		"SELECT 1 AND ul.composite_route_id IS NOT NULL AND ul.composite_route_kind = 'compaction'",
		appendCompositeRouteQueryFilter("SELECT 1", usagestats.CompositeRouteFilterCompaction, "ul"),
	)
}

func TestShouldUsePreaggregatedTrendRejectsCompositeRouteFilter(t *testing.T) {
	require.True(t, shouldUsePreaggregatedTrend("day", 0, 0, 0, 0, "", nil, nil, nil, "", nil, nil, ""))
	require.True(t, shouldUsePreaggregatedTrend("hour", 0, 0, 0, 0, "", nil, nil, nil, "", nil, nil, ""))
	for _, filter := range []string{
		usagestats.CompositeRouteFilterRouted,
		usagestats.CompositeRouteFilterCompaction,
		usagestats.CompositeRouteFilterFallback,
		usagestats.CompositeRouteFilterNone,
	} {
		require.False(t, shouldUsePreaggregatedTrend("day", 0, 0, 0, 0, "", nil, nil, nil, "", nil, nil, filter), filter)
	}
}

func TestUsageLogRepositoryDashboardQueriesApplyCompositeRoute(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	for _, tt := range []struct {
		filter       string
		where        string
		aliasedWhere string
	}{
		{
			usagestats.CompositeRouteFilterRouted,
			"composite_route_id IS NOT NULL",
			"ul.composite_route_id IS NOT NULL",
		},
		{
			usagestats.CompositeRouteFilterCompaction,
			"composite_route_id IS NOT NULL AND composite_route_kind = 'compaction'",
			"ul.composite_route_id IS NOT NULL AND ul.composite_route_kind = 'compaction'",
		},
		{
			usagestats.CompositeRouteFilterFallback,
			"composite_route_id IS NOT NULL AND composite_fallback_chain IS NOT NULL",
			"ul.composite_route_id IS NOT NULL AND ul.composite_fallback_chain IS NOT NULL",
		},
		{
			usagestats.CompositeRouteFilterNone,
			"composite_route_id IS NULL",
			"ul.composite_route_id IS NULL",
		},
	} {
		filters := usagestats.UsageLogFilters{CompositeRoute: tt.filter}

		t.Run(tt.filter+"/trend bypasses preaggregate", func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			// Only one query is expected: the usage_dashboard_daily aggregate must not be consulted.
			mock.ExpectQuery("(?s)FROM usage_logs.*"+regexp.QuoteMeta(" AND "+tt.where+" GROUP BY date")).
				WithArgs(start, end).
				WillReturnRows(sqlmock.NewRows([]string{"date", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost"}))

			_, err := repo.GetUsageTrendWithUsageFilters(context.Background(), start, end, "day", filters)
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})

		t.Run(tt.filter+"/model", func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			mock.ExpectQuery("(?s)FROM usage_logs.*"+regexp.QuoteMeta(" AND "+tt.where+" GROUP BY ")).
				WithArgs(start, end).
				WillReturnRows(sqlmock.NewRows([]string{
					"model", "requests", "input_tokens", "output_tokens", "cache_creation_tokens",
					"cache_read_tokens", "total_tokens", "cost", "actual_cost", "account_cost",
				}))

			_, err := repo.GetModelStatsWithUsageFiltersBySource(context.Background(), start, end, filters, usagestats.ModelSourceRequested)
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})

		t.Run(tt.filter+"/group", func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			mock.ExpectQuery("(?s)FROM usage_logs ul.*"+regexp.QuoteMeta(" AND "+tt.aliasedWhere+" GROUP BY ul.group_id")).
				WithArgs(start, end).
				WillReturnRows(sqlmock.NewRows([]string{"group_id", "group_name", "requests", "total_tokens", "cost", "actual_cost", "account_cost"}))

			_, err := repo.GetGroupStatsWithUsageFilters(context.Background(), start, end, filters)
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUsageLogRepositoryDashboardQueriesWithoutCompositeRouteHaveNoCompositeCondition(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	noCompositeMatcher := sqlmock.QueryMatcherFunc(func(_, actual string) error {
		if strings.Contains(actual, "composite_") {
			return fmt.Errorf("unexpected composite condition in query: %s", actual)
		}
		return nil
	})
	newMock := func(t *testing.T) (*usageLogRepository, sqlmock.Sqlmock) {
		t.Helper()
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(noCompositeMatcher))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		return &usageLogRepository{sql: db}, mock
	}

	t.Run("trend", func(t *testing.T) {
		repo, mock := newMock(t)
		// A model filter forces the raw usage_logs query instead of the preaggregate.
		mock.ExpectQuery("FROM usage_logs").
			WithArgs(start, end, "gpt-5").
			WillReturnRows(sqlmock.NewRows([]string{"date", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost"}))
		_, err := repo.GetUsageTrendWithUsageFilters(context.Background(), start, end, "day", usagestats.UsageLogFilters{Model: "gpt-5"})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("model", func(t *testing.T) {
		repo, mock := newMock(t)
		mock.ExpectQuery("FROM usage_logs").
			WithArgs(start, end).
			WillReturnRows(sqlmock.NewRows([]string{
				"model", "requests", "input_tokens", "output_tokens", "cache_creation_tokens",
				"cache_read_tokens", "total_tokens", "cost", "actual_cost", "account_cost",
			}))
		_, err := repo.GetModelStatsWithUsageFiltersBySource(context.Background(), start, end, usagestats.UsageLogFilters{}, usagestats.ModelSourceRequested)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("group", func(t *testing.T) {
		repo, mock := newMock(t)
		mock.ExpectQuery("FROM usage_logs ul").
			WithArgs(start, end).
			WillReturnRows(sqlmock.NewRows([]string{"group_id", "group_name", "requests", "total_tokens", "cost", "actual_cost", "account_cost"}))
		_, err := repo.GetGroupStatsWithUsageFilters(context.Background(), start, end, usagestats.UsageLogFilters{})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
