package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyCompositeRouteTraceStampsUsageAndMarksRecorded(t *testing.T) {
	trace := &CompositeRouteTrace{RouteID: 42, RequestKind: CompositeRouteRequestKindCompaction}
	trace.SetFallbackChain("gemini-3.8-flash-high (429) → backup")
	ctx := WithCompositeRouteTrace(context.Background(), trace)
	usage := &UsageLog{}

	applyCompositeRouteTrace(ctx, usage)

	require.True(t, trace.UsageRecorded())
	require.Equal(t, int64(42), *usage.CompositeRouteID)
	require.Equal(t, CompositeRouteRequestKindCompaction, *usage.CompositeRouteKind)
	require.Equal(t, "gemini-3.8-flash-high (429) → backup", *usage.CompositeFallbackChain)

	plain := &UsageLog{}
	applyCompositeRouteTrace(context.Background(), plain)
	require.Nil(t, plain.CompositeRouteID)
	require.Nil(t, plain.CompositeRouteKind)
	require.Nil(t, plain.CompositeFallbackChain)
	require.Nil(t, CompositeRouteTraceFromContext(WithCompositeRouteTrace(context.Background(), &CompositeRouteTrace{})))
}

func TestCompositeRouteFromInputValidatesFallbacks(t *testing.T) {
	base := CompositeRouteInput{PublicModel: "gpt-6.1-sol", TargetPlatform: PlatformOpenAI, UpstreamModel: "gemini-3.8-flash-high", Enabled: true}

	input := base
	input.FallbackModels = []CompositeRouteFallback{
		{UpstreamModel: " gemini-3.8-flash "},
		{TargetPlatform: PlatformOpenAI, UpstreamModel: "gemini-3.8-flash"}, // same effective target as above
		{UpstreamModel: "gemini-3.8-flash-high"},                            // equals the primary
		{TargetPlatform: PlatformOpenAI, UpstreamModel: "gemini-3.8-flash-high"},
		{UpstreamModel: "  "},
		{TargetPlatform: PlatformDeepseek, UpstreamModel: "deepseek-v4-pro"},
		{TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-6-luna"}, // explicit platform is kept as written
	}
	route, err := compositeRouteFromInput(6, input)
	require.NoError(t, err)
	require.Equal(t, []CompositeRouteFallback{
		{UpstreamModel: "gemini-3.8-flash"},
		{TargetPlatform: PlatformDeepseek, UpstreamModel: "deepseek-v4-pro"},
		{TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-6-luna"},
	}, route.FallbackModels)

	input = base
	input.FallbackModels = []CompositeRouteFallback{{TargetPlatform: "composite", UpstreamModel: "x"}}
	_, err = compositeRouteFromInput(6, input)
	require.Error(t, err)

	input = base
	for i := 0; i < 9; i++ {
		input.FallbackModels = append(input.FallbackModels, CompositeRouteFallback{UpstreamModel: "m" + string(rune('a'+i))})
	}
	_, err = compositeRouteFromInput(6, input)
	require.Error(t, err)
}

func TestResolveExplicitRouteLabelsRequestKind(t *testing.T) {
	routes := []CompositeModelRoute{{ID: 1, GroupID: 6, PublicModel: "gpt-6.1-sol", MatchType: CompositeRouteMatchExact,
		TargetPlatform: PlatformOpenAI, UpstreamModel: "gemini-3.8-flash-high", Endpoint: CompositeRouteEndpointAny, Enabled: true}}
	resolver := NewCompositeRouteResolver(compositeRouteFallbackRepoStub{routes: routes})

	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "gpt-6.1-sol", CompositeRouteEndpointResponses, CompositeRouteRequestMatch{
		Body: []byte(`{"model":"gpt-6.1-sol","input":"hello"}`),
	})
	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteRequestKindConversation, decision.RequestKind)

	decision, err = resolver.ResolveWithMatch(context.Background(), 6, "gpt-6.1-sol", CompositeRouteEndpointResponses, CompositeRouteRequestMatch{
		Body: []byte(`{"model":"gpt-6.1-sol"}`), NativeCompaction: true,
	})
	require.NoError(t, err)
	require.Equal(t, CompositeRouteRequestKindCompaction, decision.RequestKind)

	decision, err = resolver.Resolve(context.Background(), 6, "gpt-6.1-sol", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.Empty(t, decision.RequestKind, "requests without facts are not classified")
}

type compositeRouteFallbackRepoStub struct {
	CompositeModelRouteRepository
	routes []CompositeModelRoute
}

func (s compositeRouteFallbackRepoStub) ListByGroup(context.Context, int64, bool) ([]CompositeModelRoute, error) {
	return s.routes, nil
}
