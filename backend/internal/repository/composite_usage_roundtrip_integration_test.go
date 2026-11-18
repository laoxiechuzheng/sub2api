//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type compositeUsageFixture struct {
	repo                                          *usageLogRepository
	client                                        *dbent.Client
	userID, apiKeyID, accountID, groupID, routeID int64
	createdAt                                     time.Time
}

func newCompositeUsageFixture(t *testing.T, batched bool) compositeUsageFixture {
	t.Helper()
	ctx := context.Background()
	var client *dbent.Client
	var repo *usageLogRepository
	if batched {
		client = testEntClient(t)
		repo = newUsageLogRepositoryWithSQL(client, integrationDB)
	} else {
		tx := testEntTx(t)
		client = tx.Client()
		repo = newUsageLogRepositoryWithSQL(client, tx)
	}
	name := "composite-usage-" + uuid.NewString()
	user := mustCreateUser(t, client, &service.User{Email: name + "@example.com"})
	group := mustCreateGroup(t, client, &service.Group{Name: name, Platform: service.PlatformComposite, RateMultiplier: 1})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-" + name, Name: name})
	account := mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI})
	route := &service.CompositeModelRoute{GroupID: group.ID, PublicModel: "public-a", MatchType: service.CompositeRouteMatchExact,
		TargetPlatform: service.PlatformOpenAI, UpstreamModel: "upstream-a", Endpoint: service.CompositeRouteEndpointResponses, Enabled: true}
	require.NoError(t, NewCompositeModelRouteRepository(client).Create(ctx, route))
	f := compositeUsageFixture{repo: repo, client: client, userID: user.ID, apiKeyID: key.ID, accountID: account.ID, groupID: group.ID, routeID: route.ID,
		createdAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	if batched {
		t.Cleanup(func() {
			for _, row := range []struct {
				query string
				id    int64
			}{
				{"DELETE FROM usage_logs WHERE api_key_id = $1", f.apiKeyID},
				{"DELETE FROM api_keys WHERE id = $1", f.apiKeyID},
				{"DELETE FROM composite_model_routes WHERE id = $1", f.routeID},
				{"DELETE FROM accounts WHERE id = $1", f.accountID},
				{"DELETE FROM groups WHERE id = $1", f.groupID},
				{"DELETE FROM users WHERE id = $1", f.userID},
			} {
				_, err := integrationDB.ExecContext(context.Background(), row.query, row.id)
				require.NoError(t, err)
			}
		})
	}
	return f
}

func (f compositeUsageFixture) log(requestID, model, mode string) *service.UsageLog {
	kind := service.CompositeRouteRequestKindCompaction
	chain := "primary (429) -> backup"
	upstream := "upstream-a"
	matched := false
	return &service.UsageLog{UserID: f.userID, APIKeyID: f.apiKeyID, AccountID: f.accountID, GroupID: &f.groupID,
		RequestID: requestID, Model: upstream, RequestedModel: model, UpstreamModel: &upstream, UpstreamModelMismatch: &matched,
		InputTokens: 11, OutputTokens: 3, TotalCost: 1, ActualCost: 1, RateMultiplier: 1,
		BillingMode: &mode, Stream: true, RequestType: service.RequestTypeStream,
		CompositeRouteID: &f.routeID, CompositeRouteKind: &kind, CompositeFallbackChain: &chain, CreatedAt: f.createdAt}
}

func TestCompositeRouteFallbackConfigJSONBRoundTrip(t *testing.T) {
	f := newCompositeUsageFixture(t, false)
	ctx := context.Background()
	repo := NewCompositeModelRouteRepository(f.client)
	routes, err := repo.ListByGroup(ctx, f.groupID, true)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	route := routes[0]
	// Create a distinct rule with nonempty JSONB rather than testing only its default.
	route.PublicModel = "fallback-jsonb"
	route.FallbackModels = []service.CompositeRouteFallback{{UpstreamModel: "backup-a"}, {TargetPlatform: service.PlatformDeepseek, UpstreamModel: "backup-b"}}
	require.NoError(t, repo.Create(ctx, &route))
	read := func(want []service.CompositeRouteFallback) {
		t.Helper()
		got, err := repo.ListByGroup(ctx, f.groupID, true)
		require.NoError(t, err)
		for _, candidate := range got {
			if candidate.ID == route.ID {
				require.Equal(t, want, candidate.FallbackModels)
				return
			}
		}
		t.Fatal("created fallback rule not returned")
	}
	read(route.FallbackModels)
	var raw string
	require.NoError(t, scanSingleRow(ctx, f.repo.sql, "SELECT fallback_models::text FROM composite_model_routes WHERE id = $1", []any{route.ID}, &raw))
	require.JSONEq(t, `[{"upstream_model":"backup-a"},{"target_platform":"deepseek","upstream_model":"backup-b"}]`, raw)
	route.FallbackModels = []service.CompositeRouteFallback{{TargetPlatform: service.PlatformOpenAI, UpstreamModel: "replacement"}, {UpstreamModel: "backup-a"}}
	require.NoError(t, repo.Update(ctx, &route))
	read(route.FallbackModels)
	route.FallbackModels = nil
	require.NoError(t, repo.Update(ctx, &route))
	read([]service.CompositeRouteFallback{})
	require.NoError(t, scanSingleRow(ctx, f.repo.sql, "SELECT fallback_models::text FROM composite_model_routes WHERE id = $1", []any{route.ID}, &raw))
	require.JSONEq(t, "[]", raw)
}

func TestCompositeUsageAttributionInsertReadRoundTrip(t *testing.T) {
	for _, path := range []string{"single", "batch", "best_effort_batch", "no_result"} {
		t.Run(path, func(t *testing.T) {
			f := newCompositeUsageFixture(t, path != "single")
			ctx := context.Background()
			const count = 8
			logs := make([]*service.UsageLog, count)
			errs := make([]error, count)
			inserted := make([]bool, count)
			var wg sync.WaitGroup
			for i := range logs {
				logs[i] = f.log(uuid.NewString(), "public-a", "image")
				logs[i].CacheCreation5mTokens, logs[i].CacheCreation1hTokens = 17+i, 19+i
				logs[i].CacheCreation30mTokens, logs[i].CacheCreation12hTokens = 73+i, 89+i
				logs[i].CacheCreationTokens = 198 + 4*i
				logs[i].TotalCost, logs[i].ActualCost = 12.5+float64(i), 3.25+float64(i)/10
				accountCost, accountRate := 7.75+float64(i)/10, 1.5
				logs[i].AccountStatsCost, logs[i].AccountRateMultiplier = &accountCost, &accountRate
				upstreamID, session := fmt.Sprintf("upstream-%d", i), fmt.Sprintf("session-%d", i)
				logs[i].UpstreamRequestID, logs[i].SessionID = &upstreamID, &session
				logs[i].NativeCompactionV2 = true
				if path == "single" {
					inserted[i], errs[i] = f.repo.Create(ctx, logs[i])
					continue
				}
				if path == "no_result" {
					errs[i] = execUsageLogInsertNoResult(ctx, f.repo.sql, prepareUsageLogInsert(logs[i]))
					continue
				}
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if path == "best_effort_batch" {
						errs[i] = f.repo.CreateBestEffort(ctx, logs[i])
						return
					}
					inserted[i], errs[i] = f.repo.Create(ctx, logs[i])
				}(i)
			}
			wg.Wait()
			if path == "batch" {
				require.NotNil(t, f.repo.createBatchCh)
			}
			if path == "best_effort_batch" {
				require.NotNil(t, f.repo.bestEffortBatchCh)
			}
			for i, log := range logs {
				require.NoError(t, errs[i], "path=%s index=%d", path, i)
				if path != "best_effort_batch" && path != "no_result" {
					require.True(t, inserted[i])
					require.Positive(t, log.ID)
				}
				rows, page, err := f.repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, usagestats.UsageLogFilters{APIKeyID: f.apiKeyID, RequestID: log.RequestID})
				require.NoError(t, err)
				require.EqualValues(t, 1, page.Total)
				require.Len(t, rows, 1)
				got, err := f.repo.GetByID(ctx, rows[0].ID)
				require.NoError(t, err)
				for _, row := range []*service.UsageLog{&rows[0], got} {
					require.Equal(t, log.CompositeRouteID, row.CompositeRouteID)
					require.Equal(t, log.CompositeRouteKind, row.CompositeRouteKind)
					require.Equal(t, log.CompositeFallbackChain, row.CompositeFallbackChain)
					require.Equal(t, "public-a", row.RequestedModel)
					require.Equal(t, 17+i, row.CacheCreation5mTokens)
					require.Equal(t, 19+i, row.CacheCreation1hTokens)
					require.Equal(t, 73+i, row.CacheCreation30mTokens)
					require.Equal(t, 89+i, row.CacheCreation12hTokens)
					require.Equal(t, 198+4*i, row.CacheCreationTokens)
					require.InDelta(t, 12.5+float64(i), row.TotalCost, 1e-8)
					require.InDelta(t, 3.25+float64(i)/10, row.ActualCost, 1e-8)
					require.NotNil(t, row.AccountStatsCost)
					require.InDelta(t, 7.75+float64(i)/10, *row.AccountStatsCost, 1e-8)
					require.NotNil(t, row.AccountRateMultiplier)
					require.Equal(t, 1.5, *row.AccountRateMultiplier)
					require.NotNil(t, row.UpstreamRequestID)
					require.Equal(t, fmt.Sprintf("upstream-%d", i), *row.UpstreamRequestID)
					require.NotNil(t, row.SessionID)
					require.Equal(t, fmt.Sprintf("session-%d", i), *row.SessionID)
					require.True(t, row.NativeCompactionV2)
					require.True(t, row.CreatedAt.Equal(f.createdAt))
				}
			}
		})
	}
}

func TestCompositeUsageCombinedFiltersAndExactPagination(t *testing.T) {
	f := newCompositeUsageFixture(t, false)
	ctx := context.Background()
	combinations := []struct {
		model, mode string
		count       int
	}{
		{"public-a", "image", 100}, {"public-a", "token", 7}, {"public-b", "image", 9}, {"public-b", "token", 11},
	}
	for _, combination := range combinations {
		for i := 0; i < combination.count; i++ {
			log := f.log(fmt.Sprintf("%s-%s-%d-%s", combination.model, combination.mode, i, uuid.NewString()), combination.model, combination.mode)
			log.TotalCost, log.ActualCost = 12.5, 3.25
			accountCost, accountRate := 7.75, 1.5
			log.AccountStatsCost, log.AccountRateMultiplier = &accountCost, &accountRate
			_, err := f.repo.Create(ctx, log)
			require.NoError(t, err)
		}
	}
	start := f.createdAt.Add(-time.Hour)
	end := f.createdAt.Add(time.Hour)
	matched, native := false, false
	requestType := int16(service.RequestTypeStream)
	billingType := int8(0)
	for _, combination := range combinations {
		t.Run(combination.model+"/"+combination.mode, func(t *testing.T) {
			filters := usagestats.UsageLogFilters{GroupID: f.groupID, Model: combination.model, ModelFilterSource: usagestats.ModelSourceRequested,
				BillingMode: combination.mode, CompositeRoute: usagestats.CompositeRouteFilterFallback,
				StartTime: &start, EndTime: &end, RequestType: &requestType, BillingType: &billingType,
				UpstreamModelMismatch: &matched, NativeCompactionV2: &native}
			// No user/key/account selector and ExactTotal remains false, matching the admin UI.
			rows, page, err := f.repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, filters)
			require.NoError(t, err)
			require.EqualValues(t, combination.count, page.Total)
			require.Len(t, rows, min(20, combination.count))
			stats, err := f.repo.GetStatsWithFilters(ctx, filters)
			require.NoError(t, err)
			require.EqualValues(t, combination.count, stats.TotalRequests)
			n := float64(combination.count)
			require.InDelta(t, 12.5*n, stats.TotalCost, 1e-8)
			require.InDelta(t, 3.25*n, stats.TotalActualCost, 1e-8)
			require.NotNil(t, stats.TotalAccountCost)
			require.InDelta(t, 11.625*n, *stats.TotalAccountCost, 1e-8)
			for _, endpoints := range [][]usagestats.EndpointStat{stats.Endpoints, stats.UpstreamEndpoints, stats.EndpointPaths} {
				require.Len(t, endpoints, 1)
				require.InDelta(t, 12.5*n, endpoints[0].Cost, 1e-8)
				require.InDelta(t, 3.25*n, endpoints[0].ActualCost, 1e-8)
				require.InDelta(t, 11.625*n, endpoints[0].AccountCost, 1e-8)
			}
			trend, err := f.repo.GetUsageTrendWithUsageFilters(ctx, start, end, "day", filters)
			require.NoError(t, err)
			require.Len(t, trend, 1)
			require.EqualValues(t, combination.count, trend[0].Requests)
			require.InDelta(t, 12.5*n, trend[0].Cost, 1e-8)
			require.InDelta(t, 3.25*n, trend[0].ActualCost, 1e-8)
			for _, source := range []string{usagestats.ModelSourceRequested, usagestats.ModelSourceUpstream, usagestats.ModelSourceMapping} {
				models, err := f.repo.GetModelStatsWithUsageFiltersBySource(ctx, start, end, filters, source)
				require.NoError(t, err)
				require.Len(t, models, 1)
				require.EqualValues(t, combination.count, models[0].Requests, source)
				require.InDelta(t, 12.5*n, models[0].Cost, 1e-8, source)
				require.InDelta(t, 3.25*n, models[0].ActualCost, 1e-8, source)
				require.InDelta(t, 11.625*n, models[0].AccountCost, 1e-8, source)
			}
			groups, err := f.repo.GetGroupStatsWithUsageFilters(ctx, start, end, filters)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Equal(t, f.groupID, groups[0].GroupID)
			require.EqualValues(t, combination.count, groups[0].Requests)
			require.InDelta(t, 12.5*n, groups[0].Cost, 1e-8)
			require.InDelta(t, 3.25*n, groups[0].ActualCost, 1e-8)
			require.InDelta(t, 11.625*n, groups[0].AccountCost, 1e-8)
			breakdown, err := f.repo.GetUserBreakdownStats(ctx, start, end, usagestats.UserBreakdownDimension{
				GroupID: f.groupID, Model: "upstream-a", ModelType: usagestats.ModelSourceUpstream, RequestedModel: combination.model,
				BillingMode: combination.mode, CompositeRoute: usagestats.CompositeRouteFilterFallback,
				RequestType: &requestType, BillingType: &billingType, UpstreamModelMismatch: &matched, NativeCompactionV2: &native,
			}, 50)
			require.NoError(t, err)
			require.Len(t, breakdown, 1)
			require.EqualValues(t, combination.count, breakdown[0].Requests)
			require.InDelta(t, 12.5*n, breakdown[0].Cost, 1e-8)
			require.InDelta(t, 3.25*n, breakdown[0].ActualCost, 1e-8)
			require.InDelta(t, 11.625*n, breakdown[0].AccountCost, 1e-8)
			if combination.count == 100 {
				seen := make(map[int64]bool)
				for pageNumber := 1; pageNumber <= 5; pageNumber++ {
					pageRows, paginationResult, err := f.repo.ListWithFilters(ctx, pagination.PaginationParams{Page: pageNumber, PageSize: 20}, filters)
					require.NoError(t, err)
					require.EqualValues(t, 100, paginationResult.Total)
					require.Equal(t, 5, paginationResult.Pages)
					require.Len(t, pageRows, 20)
					for _, row := range pageRows {
						require.False(t, seen[row.ID])
						seen[row.ID] = true
					}
				}
				require.Len(t, seen, 100)
				for _, compositeFilter := range []string{usagestats.CompositeRouteFilterRouted, usagestats.CompositeRouteFilterCompaction} {
					filters.CompositeRoute = compositeFilter
					_, paginationResult, err := f.repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 3, PageSize: 20}, filters)
					require.NoError(t, err)
					require.EqualValues(t, 100, paginationResult.Total)
				}
			}
		})
	}
}

func TestCompositeUsageLegacyInsertCompatibilityAndUnknownAttribution(t *testing.T) {
	f := newCompositeUsageFixture(t, false)
	ctx := context.Background()
	var legacyID int64
	legacyRequest := uuid.NewString()
	require.NoError(t, scanSingleRow(ctx, f.repo.sql, `INSERT INTO usage_logs
		(user_id, api_key_id, account_id, group_id, request_id, model, input_tokens, output_tokens, total_cost, actual_cost, created_at)
		VALUES ($1, $2, $3, $4, $5, 'legacy-model', 11, 3, 1, 1, $6) RETURNING id`,
		[]any{f.userID, f.apiKeyID, f.accountID, f.groupID, legacyRequest, f.createdAt}, &legacyID))
	legacy, err := f.repo.GetByID(ctx, legacyID)
	require.NoError(t, err)
	legacyRows, legacyPage, err := f.repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, usagestats.UsageLogFilters{APIKeyID: f.apiKeyID, RequestID: legacyRequest})
	require.NoError(t, err)
	require.EqualValues(t, 1, legacyPage.Total)
	require.Len(t, legacyRows, 1)
	for _, row := range []*service.UsageLog{legacy, &legacyRows[0]} {
		require.Equal(t, "legacy-model", row.RequestedModel)
		require.Nil(t, row.CompositeRouteID)
		require.Nil(t, row.CompositeRouteKind)
		require.Nil(t, row.CompositeFallbackChain)
		require.Zero(t, row.CacheCreation5mTokens)
		require.Zero(t, row.CacheCreation1hTokens)
		require.Zero(t, row.CacheCreation30mTokens)
		require.Zero(t, row.CacheCreation12hTokens)
		require.Nil(t, row.AccountStatsCost)
		require.Nil(t, row.AccountRateMultiplier)
		require.Equal(t, 1.0, row.TotalCost)
		require.Equal(t, 1.0, row.ActualCost)
	}

	plain := f.log(uuid.NewString(), "current-plain", "token")
	plain.CompositeRouteID, plain.CompositeRouteKind, plain.CompositeFallbackChain = nil, nil, nil
	_, err = f.repo.Create(ctx, plain)
	require.NoError(t, err)
	_, err = f.repo.Create(ctx, f.log(uuid.NewString(), "routed", "token"))
	require.NoError(t, err)
	rows, page, err := f.repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, usagestats.UsageLogFilters{GroupID: f.groupID, CompositeRoute: usagestats.CompositeRouteFilterNone})
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total, "NULL includes current unattributed rows and historical unknown rows")
	require.Len(t, rows, 2)
	ids := []int64{rows[0].ID, rows[1].ID}
	require.ElementsMatch(t, []int64{legacy.ID, plain.ID}, ids)
}

func TestCompositeRouteFallbackLegacyInsertCompatibility(t *testing.T) {
	f := newCompositeUsageFixture(t, false)
	ctx := context.Background()
	var id int64
	require.NoError(t, scanSingleRow(ctx, f.repo.sql, `INSERT INTO composite_model_routes
		(group_id, public_model, target_platform, upstream_model, endpoint)
		VALUES ($1, 'legacy-rule', 'openai', 'upstream-a', 'responses') RETURNING id`, []any{f.groupID}, &id))
	var fallbackJSON string
	require.NoError(t, scanSingleRow(ctx, f.repo.sql, "SELECT fallback_models::text FROM composite_model_routes WHERE id = $1", []any{id}, &fallbackJSON))
	require.JSONEq(t, "[]", fallbackJSON)
	routes, err := NewCompositeModelRouteRepository(f.client).ListByGroup(ctx, f.groupID, true)
	require.NoError(t, err)
	for _, route := range routes {
		if route.ID == id {
			require.Empty(t, route.FallbackModels)
			require.Equal(t, service.CompositeRouteRequestKindAny, route.RequestKind)
			return
		}
	}
	t.Fatal("legacy INSERT rule not returned")
}
