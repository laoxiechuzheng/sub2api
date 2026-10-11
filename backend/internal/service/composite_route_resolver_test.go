package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type compositeRouteRepoStub struct {
	routes []CompositeModelRoute
}

func TestCompositeRouteResolverCodexFilteringMatchesProductionPriority(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		routes []CompositeModelRoute
		want   []string
	}{
		{
			name: "endpoint beats priority",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointAny, TargetPlatform: PlatformOpenAI, Priority: 1},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformTypeSafe, Priority: 100},
			},
			want: []string{"unrouted"},
		},
		{
			name: "Responses OpenAI overrides any TypeSafe",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointAny, TargetPlatform: PlatformTypeSafe, Priority: 1},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformOpenAI, Priority: 100},
			},
			want: []string{"alias", "unrouted"},
		},
		{
			name: "longer prefix beats priority",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "a", MatchType: CompositeRouteMatchPrefix, TargetPlatform: PlatformOpenAI, Priority: 1},
				{ID: 2, PublicModel: "ali", MatchType: CompositeRouteMatchPrefix, TargetPlatform: PlatformTypeSafe, Priority: 100},
			},
			want: []string{"unrouted"},
		},
		{
			name: "exact beats endpoint prefix",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchPrefix, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformTypeSafe},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointAny, TargetPlatform: PlatformOpenAI},
			},
			want: []string{"alias", "unrouted"},
		},
		{
			name: "lower priority wins",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformTypeSafe, Priority: 100},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, Priority: 1},
			},
			want: []string{"alias", "unrouted"},
		},
		{
			name: "lower ID breaks tie",
			routes: []CompositeModelRoute{
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI},
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformTypeSafe},
			},
			want: []string{"unrouted"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for index := range scenario.routes {
				scenario.routes[index].Enabled = true
				scenario.routes[index].GroupID = 7
			}
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: scenario.routes})
			models, err := resolver.FilterCodexModels(context.Background(), 7, []string{"alias", "unrouted"})
			require.NoError(t, err)
			require.Equal(t, scenario.want, models)
			decision, err := resolver.Resolve(context.Background(), 7, "alias", CompositeRouteEndpointResponses)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, decision.TargetPlatform != PlatformTypeSafe, len(models) == 2)
			exactModels, err := resolver.ListExactPublicModels(context.Background(), 7, CompositeRouteEndpointResponses, false)
			require.NoError(t, err)
			if decision.TargetPlatform == PlatformTypeSafe {
				require.Empty(t, exactModels)
			} else {
				require.Contains(t, exactModels, "alias")
			}
		})
	}
}

func (s compositeRouteRepoStub) ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error) {
	routes := make([]CompositeModelRoute, 0, len(s.routes))
	for _, route := range s.routes {
		if route.GroupID != groupID {
			continue
		}
		if !includeDisabled && !route.Enabled {
			continue
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func (s compositeRouteRepoStub) Create(ctx context.Context, route *CompositeModelRoute) error {
	return nil
}

func (s compositeRouteRepoStub) Update(ctx context.Context, route *CompositeModelRoute) error {
	return nil
}

func (s compositeRouteRepoStub) Delete(ctx context.Context, id int64) error {
	return nil
}

func (s compositeRouteRepoStub) DeleteByGroup(ctx context.Context, groupID int64) error {
	return nil
}

func TestCompositeRouteCatalogFiltersConditionalSystemOneRoutes(t *testing.T) {
	for _, condition := range []struct {
		name  string
		route CompositeModelRoute
		match CompositeRouteRequestMatch
	}{
		{
			name:  "user agent",
			route: CompositeModelRoute{UserAgentContains: "codex"},
			match: CompositeRouteRequestMatch{UserAgent: "codex-cli"},
		},
		{
			name: "scoped body with exclusion",
			route: CompositeModelRoute{
				BodyContains: "summarize", BodyNotContains: "quoted example",
				BodyMatchScope: CompositeRouteBodyScopeLastMessage, BodyMatchMode: CompositeRouteBodyMatchPrefix,
			},
			match: CompositeRouteRequestMatch{Body: []byte(`{"input":"summarize this session"}`)},
		},
		{
			name:  "native compaction",
			route: CompositeModelRoute{RequestKind: CompositeRouteRequestKindCompaction},
			match: CompositeRouteRequestMatch{Body: []byte(`{"input":"summarize this session"}`), NativeCompaction: true},
		},
	} {
		for _, pattern := range []struct {
			matchType string
			model     string
		}{
			{CompositeRouteMatchExact, "shared-alias"},
			{CompositeRouteMatchPrefix, "shared-"},
			{CompositeRouteMatchContains, "alias"},
		} {
			t.Run(condition.name+"/"+pattern.matchType, func(t *testing.T) {
				route := condition.route
				route.ID, route.GroupID = 2, 7
				route.PublicModel, route.MatchType = pattern.model, pattern.matchType
				route.TargetPlatform = PlatformTypeSafe
				route.Endpoint, route.Enabled = CompositeRouteEndpointResponses, true
				resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{
					{ID: 1, GroupID: 7, PublicModel: "shared-alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Enabled: true},
					route,
					{ID: 3, GroupID: 7, PublicModel: "responses-control", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Enabled: true},
				}})

				models, err := resolver.ListExactPublicModels(context.Background(), 7, "", true)
				require.NoError(t, err)
				require.Contains(t, models, "shared-alias")
				require.Contains(t, models, "responses-control")

				models, err = resolver.ListExactPublicModels(context.Background(), 7, CompositeRouteEndpointResponses, false)
				require.NoError(t, err)
				require.Equal(t, []string{"responses-control"}, models)
				models, err = resolver.FilterCodexModels(context.Background(), 7, []string{"shared-alias", "responses-control", "unrouted"})
				require.NoError(t, err)
				require.Equal(t, []string{"responses-control", "unrouted"}, models)

				decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses)
				require.NoError(t, err)
				require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
				decision, err = resolver.ResolveWithMatch(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses, condition.match)
				require.NoError(t, err)
				require.Equal(t, PlatformTypeSafe, decision.TargetPlatform)
			})
		}
	}
}

func TestCompositeRouteCatalogConditionalOpenAICannotHideSystemOne(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		routes        []CompositeModelRoute
		typeSafeMatch CompositeRouteRequestMatch
		openAIMatch   CompositeRouteRequestMatch
	}{
		{
			name: "unconditional TypeSafe with conditional OpenAI",
			routes: []CompositeModelRoute{
				{TargetPlatform: PlatformTypeSafe, Endpoint: CompositeRouteEndpointAny},
				{TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, UserAgentContains: "curl"},
			},
			typeSafeMatch: CompositeRouteRequestMatch{UserAgent: "codex-cli"},
			openAIMatch:   CompositeRouteRequestMatch{UserAgent: "curl/8.6.0"},
		},
		{
			name: "conditional TypeSafe with unconditional OpenAI",
			routes: []CompositeModelRoute{
				{TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointAny},
				{TargetPlatform: PlatformTypeSafe, Endpoint: CompositeRouteEndpointResponses, UserAgentContains: "codex"},
			},
			typeSafeMatch: CompositeRouteRequestMatch{UserAgent: "codex-cli"},
			openAIMatch:   CompositeRouteRequestMatch{UserAgent: "curl/8.6.0"},
		},
		{
			name: "different conditional user agents",
			routes: []CompositeModelRoute{
				{TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Priority: 1, UserAgentContains: "curl"},
				{TargetPlatform: PlatformTypeSafe, Endpoint: CompositeRouteEndpointResponses, Priority: 100, UserAgentContains: "codex"},
			},
			typeSafeMatch: CompositeRouteRequestMatch{UserAgent: "codex-cli"},
			openAIMatch:   CompositeRouteRequestMatch{UserAgent: "curl/8.6.0"},
		},
		{
			name: "different conditional body signatures",
			routes: []CompositeModelRoute{
				{TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Priority: 1, BodyContains: "openai"},
				{TargetPlatform: PlatformTypeSafe, Endpoint: CompositeRouteEndpointResponses, Priority: 100, BodyContains: "systemone"},
			},
			typeSafeMatch: CompositeRouteRequestMatch{Body: []byte(`{"input":"use systemone"}`)},
			openAIMatch:   CompositeRouteRequestMatch{Body: []byte(`{"input":"use openai"}`)},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for i := range scenario.routes {
				scenario.routes[i].ID = int64(i + 1)
				scenario.routes[i].GroupID = 7
				scenario.routes[i].PublicModel = "shared-alias"
				scenario.routes[i].MatchType = CompositeRouteMatchExact
				scenario.routes[i].Enabled = true
			}
			routes := append(scenario.routes, CompositeModelRoute{
				ID: 3, GroupID: 7, PublicModel: "responses-control", MatchType: CompositeRouteMatchExact,
				TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Enabled: true,
				UserAgentContains: "curl",
			})
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})

			decision, err := resolver.ResolveWithMatch(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses, scenario.typeSafeMatch)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, PlatformTypeSafe, decision.TargetPlatform)
			decision, err = resolver.ResolveWithMatch(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses, scenario.openAIMatch)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, PlatformOpenAI, decision.TargetPlatform)

			models, err := resolver.ListExactPublicModels(context.Background(), 7, "", true)
			require.NoError(t, err)
			require.Contains(t, models, "shared-alias")
			require.Contains(t, models, "responses-control")

			t.Run("exact catalog", func(t *testing.T) {
				models, err := resolver.ListExactPublicModels(context.Background(), 7, CompositeRouteEndpointResponses, false)
				require.NoError(t, err)
				require.Equal(t, []string{"responses-control"}, models)
			})
			t.Run("Codex filter", func(t *testing.T) {
				models, err := resolver.FilterCodexModels(context.Background(), 7, []string{"shared-alias", "responses-control", "unrouted"})
				require.NoError(t, err)
				require.Equal(t, []string{"responses-control", "unrouted"}, models)
			})
		})
	}
}

func TestCompositeRouteResolverConditionalRouteOverridesExactRoute(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "gpt-5.6-sol",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformAnthropic,
				UpstreamModel:  "claude-opus-5",
				Endpoint:       CompositeRouteEndpointResponses,
				Enabled:        true,
			},
			{
				ID:                2,
				GroupID:           7,
				PublicModel:       "gpt",
				MatchType:         CompositeRouteMatchContains,
				TargetPlatform:    PlatformDeepseek,
				UpstreamModel:     "DeepSeek-V4-Flash",
				Endpoint:          CompositeRouteEndpointResponses,
				UserAgentContains: "codex",
				BodyContains:      "CONTEXT CHECKPOINT COMPACTION",
				Enabled:           true,
			},
		},
	})

	decision, err := resolver.ResolveWithMatch(
		context.Background(),
		7,
		"gpt-5.6-sol",
		CompositeRouteEndpointResponses,
		CompositeRouteRequestMatch{
			UserAgent: "Codex Desktop/0.155.0",
			Body:      []byte(`{"input":[{"text":"You are performing a CONTEXT CHECKPOINT COMPACTION"}]}`),
		},
	)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(2), decision.Route.ID)
	require.Equal(t, PlatformDeepseek, decision.TargetPlatform)
	require.Equal(t, "DeepSeek-V4-Flash", decision.UpstreamModel)
}

func TestCompositeRouteResolverConditionalRouteFallsBackWhenRequestDoesNotMatch(t *testing.T) {
	exactRoute := CompositeModelRoute{
		ID:             1,
		GroupID:        7,
		PublicModel:    "gpt-5.6-sol",
		MatchType:      CompositeRouteMatchExact,
		TargetPlatform: PlatformAnthropic,
		UpstreamModel:  "claude-opus-5",
		Endpoint:       CompositeRouteEndpointResponses,
		Enabled:        true,
	}
	conditionalRoute := CompositeModelRoute{
		ID:                2,
		GroupID:           7,
		PublicModel:       "gpt",
		MatchType:         CompositeRouteMatchContains,
		TargetPlatform:    PlatformDeepseek,
		UpstreamModel:     "DeepSeek-V4-Flash",
		Endpoint:          CompositeRouteEndpointResponses,
		UserAgentContains: "codex",
		BodyContains:      "CONTEXT CHECKPOINT COMPACTION",
		Enabled:           true,
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{exactRoute, conditionalRoute},
	})

	tests := []struct {
		name  string
		match CompositeRouteRequestMatch
	}{
		{name: "missing request facts"},
		{
			name: "wrong user agent",
			match: CompositeRouteRequestMatch{
				UserAgent: "curl/8.6.0",
				Body:      []byte(`CONTEXT CHECKPOINT COMPACTION`),
			},
		},
		{
			name: "missing body signature",
			match: CompositeRouteRequestMatch{
				UserAgent: "Codex Desktop/0.155.0",
				Body:      []byte(`{"input":[{"text":"hello"}]}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := resolver.ResolveWithMatch(
				context.Background(), 7, "gpt-5.6-sol", CompositeRouteEndpointResponses, tt.match,
			)

			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.NotNil(t, decision.Route)
			require.Equal(t, int64(1), decision.Route.ID)
			require.Equal(t, "claude-opus-5", decision.UpstreamModel)
		})
	}
}

func TestCompositeRouteResolverUserAgentConditionIgnoresCase(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:                1,
				GroupID:           7,
				PublicModel:       "claude",
				MatchType:         CompositeRouteMatchContains,
				TargetPlatform:    PlatformOpenAI,
				UpstreamModel:     "gpt-5.4-mini",
				Endpoint:          CompositeRouteEndpointAny,
				UserAgentContains: "Codex",
				Enabled:           true,
			},
		},
	})

	decision, err := resolver.ResolveWithMatch(
		context.Background(),
		7,
		"claude-sonnet-4-6",
		CompositeRouteEndpointMessages,
		CompositeRouteRequestMatch{UserAgent: "codex_cli_rs/0.155.0"},
	)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, "gpt-5.4-mini", decision.UpstreamModel)
}

func TestCompositeRouteResolverBodyConditionSupportsEscapedJSONAndMultipleLines(t *testing.T) {
	pattern := "He said \"hello\"\nsecond signature"
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "gpt",
				MatchType:      CompositeRouteMatchContains,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-5.4-mini",
				Endpoint:       CompositeRouteEndpointResponses,
				BodyContains:   "first signature\n" + pattern,
				Enabled:        true,
			},
		},
	})

	decision, err := resolver.ResolveWithMatch(
		context.Background(),
		7,
		"gpt-5.6-sol",
		CompositeRouteEndpointResponses,
		CompositeRouteRequestMatch{Body: []byte(`{"text":"He said \"hello\"\nsecond signature"}`)},
	)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, "gpt-5.4-mini", decision.UpstreamModel)
}

func TestCompositeRouteResolverExplicitExactRouteRewritesModel(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             10,
				GroupID:        7,
				PublicModel:    "openrouter/gpt-5",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-5",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "openrouter/gpt-5", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-5", decision.UpstreamModel)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(10), decision.Route.ID)
}

// Scenario: 唯一平台的精确别名可路由
func TestCompositeRouteResolverUsesAccountModelOwnershipForUnprefixedAlias(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(_ context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
		require.Equal(t, int64(7), groupID)
		require.Equal(t, "reasoning-alias", model)
		return CompositeModelOwnership{TargetPlatform: PlatformDeepseek, Matched: true}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "reasoning-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformDeepseek, decision.TargetPlatform)
	require.Equal(t, "reasoning-alias", decision.UpstreamModel)
}

func TestCompositeRouteResolverAccountOwnershipOverridesBuiltInDetector(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{TargetPlatform: PlatformDeepseek, Matched: true}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "gpt-5", CompositeRouteEndpointResponses)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformDeepseek, decision.TargetPlatform)
}

// Scenario: 显式路由保持最高优先级
func TestCompositeRouteResolverExplicitRouteBeatsAccountOwnership(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{{
			ID:             10,
			GroupID:        7,
			PublicModel:    "reasoning-alias",
			MatchType:      CompositeRouteMatchExact,
			TargetPlatform: PlatformOpenAI,
			UpstreamModel:  "gpt-5",
			Endpoint:       CompositeRouteEndpointAny,
			Enabled:        true,
		}},
	})
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{TargetPlatform: PlatformDeepseek, Matched: true}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "reasoning-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-5", decision.UpstreamModel)
}

// Scenario: 跨平台同名别名不被猜测
func TestCompositeRouteResolverDoesNotGuessAmbiguousAccountOwnership(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{Ambiguous: true}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.False(t, decision.Matched)
	require.Empty(t, decision.TargetPlatform)
	require.Equal(t, "model is exposed by multiple provider platforms", decision.Reason)
}

func TestCompositeRouteResolverOwnershipLookupErrorFallsBackOnlyForDetectableModels(t *testing.T) {
	lookupErr := errors.New("account catalog unavailable")
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{}, lookupErr
	})

	detected, err := resolver.Resolve(context.Background(), 7, "gpt-5", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.True(t, detected.Matched)
	require.Equal(t, CompositeRouteSourceDetector, detected.Source)
	require.Equal(t, PlatformOpenAI, detected.TargetPlatform)

	unknown, err := resolver.Resolve(context.Background(), 7, "company-model", CompositeRouteEndpointResponses)
	require.ErrorIs(t, err, lookupErr)
	require.False(t, unknown.Matched)
}

func TestCompositeRouteResolverPrefersEndpointSpecificLongestPrefix(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "router/",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformAnthropic,
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       10,
				Enabled:        true,
			},
			{
				ID:             2,
				GroupID:        7,
				PublicModel:    "router/gpt-",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-family",
				Endpoint:       CompositeRouteEndpointResponses,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "router/gpt-5", CompositeRouteEndpointResponses)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-family", decision.UpstreamModel)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(2), decision.Route.ID)
}

// TestCompositeRouteResolverPrefixEmptyUpstreamPassesThroughRequestedModel 验证：
// 前缀匹配路由留空 upstream_model 时，转发的是具体请求模型（各自原样），而不是
// 塌缩成 public_model。这是「留空 = 透传原始模型」语义的核心场景。
func TestCompositeRouteResolverPrefixEmptyUpstreamPassesThroughRequestedModel(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "deepseek-v4",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "", // 留空 = 透传
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro", "deepseek-v4"} {
		decision, err := resolver.Resolve(context.Background(), 7, model, CompositeRouteEndpointChatCompletions)
		require.NoError(t, err)
		require.True(t, decision.Matched, "model %q should match prefix route", model)
		require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
		require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
		require.Equal(t, model, decision.UpstreamModel, "model %q should pass through verbatim", model)
	}
}

// TestCompositeRouteResolverPrefixExplicitUpstreamStillFixed 验证：前缀匹配路由显式
// 填写 upstream_model 时，所有命中请求仍转发同一个固定上游模型（行为不变）。
func TestCompositeRouteResolverPrefixExplicitUpstreamStillFixed(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "deepseek-v4",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "deepseek-chat",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		decision, err := resolver.Resolve(context.Background(), 7, model, CompositeRouteEndpointChatCompletions)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, "deepseek-chat", decision.UpstreamModel)
	}
}

func TestCompositeRouteResolverIgnoresDisabledRoutesAndFallsBackToDetector(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "gpt-5",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformAnthropic,
				UpstreamModel:  "claude-sonnet-4-6",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        false,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "gpt-5", CompositeRouteEndpointAny)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceDetector, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-5", decision.UpstreamModel)
	require.Nil(t, decision.Route)
}

func TestCompositeRouteResolverDetectsKimiCodeBareModels(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)

	for _, model := range []string{"k3", "k3-256k", "kimi-code/k3"} {
		t.Run(model, func(t *testing.T) {
			decision, err := resolver.Resolve(context.Background(), 7, model, CompositeRouteEndpointMessages)

			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, CompositeRouteSourceDetector, decision.Source)
			require.Equal(t, PlatformKimi, decision.TargetPlatform)
			require.Equal(t, model, decision.UpstreamModel)
		})
	}
}

func TestCompositeRouteResolverExplicitRoutesCoverBucketTwoProviders(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "all/gpt-5",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-5",
				Endpoint:       CompositeRouteEndpointResponses,
				Priority:       100,
				Enabled:        true,
			},
			{
				ID:             2,
				GroupID:        7,
				PublicModel:    "all/claude-sonnet",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformAnthropic,
				UpstreamModel:  "claude-sonnet-4-6",
				Endpoint:       CompositeRouteEndpointMessages,
				Priority:       100,
				Enabled:        true,
			},
			{
				ID:             3,
				GroupID:        7,
				PublicModel:    "all/gemini-pro",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformGemini,
				UpstreamModel:  "gemini-2.5-pro",
				Endpoint:       CompositeRouteEndpointGemini,
				Priority:       100,
				Enabled:        true,
			},
			{
				ID:             4,
				GroupID:        7,
				PublicModel:    "all/grok",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformGrok,
				UpstreamModel:  "grok-4.3",
				Endpoint:       CompositeRouteEndpointResponses,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	tests := []struct {
		model        string
		endpoint     string
		wantPlatform string
		wantUpstream string
	}{
		{"all/gpt-5", CompositeRouteEndpointResponses, PlatformOpenAI, "gpt-5"},
		{"all/claude-sonnet", CompositeRouteEndpointMessages, PlatformAnthropic, "claude-sonnet-4-6"},
		{"all/gemini-pro", CompositeRouteEndpointGemini, PlatformGemini, "gemini-2.5-pro"},
		{"all/grok", CompositeRouteEndpointResponses, PlatformGrok, "grok-4.3"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			decision, err := resolver.Resolve(context.Background(), 7, tt.model, tt.endpoint)

			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
			require.Equal(t, tt.wantPlatform, decision.TargetPlatform)
			require.Equal(t, tt.wantUpstream, decision.UpstreamModel)
		})
	}
}
