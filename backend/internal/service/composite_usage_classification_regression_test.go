package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompositeUsageResponsesSummaryBodyOnAnyRoute(t *testing.T) {
	// Synthetic fixture built around the visible route signature, not production request content.
	const signature = "coding agent to continue the work without losing the user's intent, constraints, technical context, or latest working state."
	const fixture = "You are a context summarization and handoff agent. Produce one accurate, self-contained record that allows another " + signature
	for _, tc := range []struct {
		name  string
		input any
		want  string
	}{
		{"active summary task", fixture, CompositeRouteRequestKindCompaction},
		{"active structured task", []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": fixture}}}}, CompositeRouteRequestKindCompaction},
		{"historical task", []map[string]string{{"role": "user", "content": fixture}, {"role": "assistant", "content": "Previous summary."}, {"role": "user", "content": "Fix the login bug."}}, CompositeRouteRequestKindConversation},
		{"quoted material", "Explain this quoted prompt from documentation: " + fixture, CompositeRouteRequestKindConversation},
		{"ordinary conversation", "Fix the login bug and keep the existing API compatible.", CompositeRouteRequestKindConversation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"model": "fixture-public", "input": tc.input})
			require.NoError(t, err)
			route := CompositeModelRoute{ID: 42, GroupID: 6, PublicModel: "fixture-public", MatchType: CompositeRouteMatchExact,
				TargetPlatform: PlatformOpenAI, UpstreamModel: "fixture-upstream", Endpoint: CompositeRouteEndpointResponses,
				RequestKind: CompositeRouteRequestKindAny, Enabled: true}
			decision, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "fixture-public", "responses", CompositeRouteRequestMatch{Body: body, Explain: true})
			require.NoError(t, err)
			require.NotNil(t, decision.Route)
			require.Equal(t, tc.want, decision.RequestKind)
			require.Equal(t, tc.want, decision.RequestClassification.Kind)
			usage := &UsageLog{}
			trace := &CompositeRouteTrace{RouteID: decision.Route.ID, RequestKind: decision.RequestKind}
			applyCompositeRouteTrace(WithCompositeRouteTrace(context.Background(), trace), usage)
			require.Equal(t, decision.Route.ID, *usage.CompositeRouteID)
			require.Equal(t, tc.want, *usage.CompositeRouteKind)
			if tc.name == "historical task" || tc.name == "quoted material" {
				route.BodyContains = signature
				route.BodyMatchScope = CompositeRouteBodyScopeFullBody
				d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "fixture-public", "responses", CompositeRouteRequestMatch{Body: body})
				require.NoError(t, err)
				require.NotNil(t, d.Route, "a legacy full-body rule may match the signature in history or quotation")
				require.Equal(t, CompositeRouteRequestKindConversation, d.RequestKind, "matching a route does not make a historical/quoted directive active")
			}
			if tc.want == CompositeRouteRequestKindCompaction {
				// Reproduce the configured body-signature rule on ordinary /v1/responses.
				route.BodyContains = signature
				route.BodyMatchScope = CompositeRouteBodyScopeLastMessage
				d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "fixture-public", "responses", CompositeRouteRequestMatch{Body: body})
				require.NoError(t, err)
				require.NotNil(t, d.Route)
				require.Equal(t, CompositeRouteRequestKindCompaction, d.RequestKind)
			}
		})
	}
}
