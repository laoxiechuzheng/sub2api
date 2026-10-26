package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type dashboardUsageFiltersProbe struct {
	service.UsageLogRepository
	trend, models, groups []usagestats.UsageLogFilters
}

func (r *dashboardUsageFiltersProbe) GetUsageTrendWithUsageFilters(_ context.Context, _, _ time.Time, _ string, filters usagestats.UsageLogFilters) ([]usagestats.TrendDataPoint, error) {
	r.trend = append(r.trend, filters)
	return []usagestats.TrendDataPoint{{Requests: 1}}, nil
}

func (r *dashboardUsageFiltersProbe) GetModelStatsWithUsageFiltersBySource(_ context.Context, _, _ time.Time, filters usagestats.UsageLogFilters, _ string) ([]usagestats.ModelStat, error) {
	r.models = append(r.models, filters)
	return []usagestats.ModelStat{{Model: filters.Model, Requests: 1}}, nil
}

func (r *dashboardUsageFiltersProbe) GetGroupStatsWithUsageFilters(_ context.Context, _, _ time.Time, filters usagestats.UsageLogFilters) ([]usagestats.GroupStat, error) {
	r.groups = append(r.groups, filters)
	return []usagestats.GroupStat{{Requests: 1}}, nil
}

func TestDashboardUsageFiltersPreserveModelBillingModeAndCacheIsolation(t *testing.T) {
	for _, endpoint := range []string{"trend", "models", "groups", "snapshot-v2"} {
		t.Run(endpoint, func(t *testing.T) {
			resetDashboardReadCachesForTest()
			t.Cleanup(resetDashboardReadCachesForTest)
			gin.SetMode(gin.TestMode)
			repo := &dashboardUsageFiltersProbe{}
			h := NewDashboardHandler(service.NewDashboardService(repo, nil, nil, nil), nil)
			router := gin.New()
			router.GET("/trend", h.GetUsageTrend)
			router.GET("/models", h.GetModelStats)
			router.GET("/groups", h.GetGroupStats)
			router.GET("/snapshot-v2", h.GetSnapshotV2)
			for _, model := range []string{"public-a", "public-b"} {
				for _, mode := range []string{"token", "image"} {
					path := fmt.Sprintf("/%s?start_date=2026-10-01&end_date=2026-10-02&model=%s&billing_mode=%s&composite_route=fallback&include_stats=false&include_group_stats=true", endpoint, model, mode)
					for _, cache := range []string{"miss", "hit"} {
						rec := httptest.NewRecorder()
						router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
						require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
						require.Equal(t, cache, rec.Header().Get("X-Snapshot-Cache"), path)
					}
					for _, captured := range [][]usagestats.UsageLogFilters{repo.trend, repo.models, repo.groups} {
						if len(captured) == 0 {
							continue
						}
						got := captured[len(captured)-1]
						require.Equal(t, model, got.Model)
						require.Equal(t, usagestats.ModelSourceRequested, got.ModelFilterSource)
						require.Equal(t, mode, got.BillingMode)
						require.Equal(t, usagestats.CompositeRouteFilterFallback, got.CompositeRoute)
					}
				}
			}
			for name, captures := range map[string][]usagestats.UsageLogFilters{"trend": repo.trend, "models": repo.models, "groups": repo.groups} {
				if endpoint == name || endpoint == "snapshot-v2" {
					require.Len(t, captures, 4, name)
				}
			}
		})
	}
}

func TestDashboardBreakdownPreservesUsageBillingModeAndAudit(t *testing.T) {
	repo := &userBreakdownRepoCapture{}
	router := newUserBreakdownRouter(repo)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/dashboard/user-breakdown?model=upstream-a&model_source=upstream&requested_model=public-a&billing_mode=image&upstream_model_mismatch=false&composite_route=none", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "image", repo.capturedDim.BillingMode)
	require.NotNil(t, repo.capturedDim.UpstreamModelMismatch)
	require.False(t, *repo.capturedDim.UpstreamModelMismatch)
	require.Equal(t, "upstream-a", repo.capturedDim.Model)
	require.Equal(t, "public-a", repo.capturedDim.RequestedModel)
	require.Equal(t, usagestats.ModelSourceUpstream, repo.capturedDim.ModelType)
	require.Equal(t, usagestats.CompositeRouteFilterNone, repo.capturedDim.CompositeRoute)
}

func TestUsageListStatsModelBillingModeCompositeCacheIsolation(t *testing.T) {
	usageStatsCache = newSnapshotCache(30 * time.Second)
	t.Cleanup(func() { usageStatsCache = newSnapshotCache(30 * time.Second) })
	repo := &adminUsageRepoCapture{}
	router := newAdminUsageRequestTypeTestRouter(repo)
	for _, model := range []string{"public-a", "public-b"} {
		for _, mode := range []string{"token", "image"} {
			query := fmt.Sprintf("?model=%s&billing_mode=%s&composite_route=fallback&start_date=2026-10-01&end_date=2026-10-02", model, mode)
			list := httptest.NewRecorder()
			router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/admin/usage"+query, nil))
			require.Equal(t, http.StatusOK, list.Code, list.Body.String())
			for _, cache := range []string{"miss", "hit"} {
				stats := httptest.NewRecorder()
				router.ServeHTTP(stats, httptest.NewRequest(http.MethodGet, "/admin/usage/stats"+query, nil))
				require.Equal(t, http.StatusOK, stats.Code, stats.Body.String())
				require.Equal(t, cache, stats.Header().Get("X-Usage-Stats-Cache"))
			}
			for _, filters := range []usagestats.UsageLogFilters{repo.listFilters, repo.statsFilters} {
				require.Equal(t, model, filters.Model)
				require.Equal(t, mode, filters.BillingMode)
				require.Equal(t, usagestats.ModelSourceRequested, filters.ModelFilterSource)
				require.Equal(t, usagestats.CompositeRouteFilterFallback, filters.CompositeRoute)
			}
		}
	}
}
