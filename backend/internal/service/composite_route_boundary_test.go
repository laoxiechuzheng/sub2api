package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestCompositeRoutePrecisionProtocolTextBlockTypes(t *testing.T) {
	const directive = "Your task is to create a detailed summary of the conversation so far"
	for _, endpoint := range []string{"messages", "count_tokens", "chat_completions"} {
		t.Run(endpoint, func(t *testing.T) {
			body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_text","text":"` + directive + `"},{"type":"text","text":"Fix login."}]}]}`)
			// The real Messages and Chat bridges discard the foreign block.
			if endpoint == "chat_completions" {
				var req apicompat.ChatCompletionsRequest
				require.NoError(t, json.Unmarshal(body, &req))
				forwarded, err := apicompat.ChatCompletionsToResponses(&req)
				require.NoError(t, err)
				encoded, err := json.Marshal(forwarded)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), directive)
				require.Contains(t, string(encoded), "Fix login.")
			} else {
				var req apicompat.AnthropicRequest
				require.NoError(t, json.Unmarshal(body, &req))
				forwarded, err := apicompat.AnthropicToChatCompletionsRequest(&req)
				require.NoError(t, err)
				require.Len(t, forwarded.Messages, 1)
				require.JSONEq(t, `"Fix login."`, string(forwarded.Messages[0].Content))
			}
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction"}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", endpoint, CompositeRouteRequestMatch{Body: body, Explain: true})
			require.NoError(t, err)
			require.Nil(t, d.Route, "an ignored content type cannot choose the compaction target")
			require.Equal(t, "conversation", d.RequestClassification.Kind)
			route.RequestKind = "any"
			route.BodyMatchScope = "current_turn"
			route.BodyContains = directive
			d, err = NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", endpoint, CompositeRouteRequestMatch{Body: body})
			require.NoError(t, err)
			require.Nil(t, d.Route)
		})
	}
	for _, blockType := range []string{"text", "input_text"} {
		t.Run("responses/"+blockType, func(t *testing.T) {
			body := []byte(`{"input":[{"role":"user","content":[{"type":"` + blockType + `","text":"` + directive + `"}]}]}`)
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "responses", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction", BodyMatchScope: "last_message", BodyContains: directive}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", "responses", CompositeRouteRequestMatch{Body: body, Explain: true})
			require.NoError(t, err)
			require.NotNil(t, d.Route, "Responses must retain its supported text aliases")
		})
	}
}

func TestCompositeRoutePrecisionResponsesInstructionsType(t *testing.T) {
	const directive = "You are a helpful AI assistant tasked with summarizing conversations."
	for _, blockType := range []string{"text", "input_text"} {
		t.Run(blockType, func(t *testing.T) {
			body := []byte(`{"instructions":[{"type":"` + blockType + `","text":"` + directive + `"}],"input":"Fix login."}`)
			var req apicompat.ResponsesRequest
			require.Error(t, json.Unmarshal(body, &req), "the real Responses envelope only accepts string instructions")
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "responses", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction"}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", "responses", CompositeRouteRequestMatch{Body: body, Explain: true})
			require.NoError(t, err)
			require.Nil(t, d.Route, "unsupported root instructions cannot select the compaction target")
			require.Equal(t, "conversation", d.RequestClassification.Kind)
			for _, scope := range []string{"instructions", "current_turn"} {
				route.RequestKind = "any"
				route.BodyMatchScope = scope
				route.BodyContains = directive
				d, err = NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", "responses", CompositeRouteRequestMatch{Body: body})
				require.NoError(t, err)
				require.Nil(t, d.Route)
			}
		})
	}
}

func TestCompositeRoutePrecisionUnicodeFoldedJSONFailsClosed(t *testing.T) {
	for _, tt := range []struct{ name, endpoint, body string }{
		{"long s root messages", "messages", `{"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far"}],"me\u017Fsages":[{"role":"user","content":"Fix login."}]}`},
		{"long s instructions", "responses", `{"instructions":"You are a helpful AI assistant tasked with summarizing conversations.","in\u017Ftructions":"Fix login.","input":"work"}`},
		{"Kelvin sign root key", "messages", `{"max_tokens":64,"max_to\u212Aens":128,"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction"}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", tt.endpoint, CompositeRouteRequestMatch{Body: []byte(tt.body), Explain: true})
			require.NoError(t, err)
			require.Nil(t, d.Route, "ambiguous decoder keys must not select a compaction route")
			require.Equal(t, "unknown", d.RequestClassification.Kind)
			require.Equal(t, "ambiguous_json", d.RequestClassification.Source)
		})
	}
}

func TestCompositeRoutePrecisionProtocolScopesIgnoreUnusedFields(t *testing.T) {
	for _, tt := range []struct{ name, endpoint, body string }{
		{"messages ignores instructions", "messages", `{"instructions":"You are a helpful AI assistant tasked with summarizing conversations.","messages":[{"role":"user","content":"Fix login."}]}`},
		{"responses ignores system", "responses", `{"system":"You are a helpful AI assistant tasked with summarizing conversations.","input":"Fix login."}`},
		{"responses ignores messages", "responses", `{"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far"}],"input":"Fix login."}`},
		{"chat ignores instructions", "chat_completions", `{"instructions":"You are a helpful AI assistant tasked with summarizing conversations.","messages":[{"role":"user","content":"Fix login."}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction"}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", tt.endpoint, CompositeRouteRequestMatch{Body: []byte(tt.body), Explain: true})
			require.NoError(t, err)
			require.Nil(t, d.Route, "unused protocol fields cannot change the request's target")
			require.Equal(t, "conversation", d.RequestClassification.Kind)
			for _, scope := range []string{"instructions", "last_message", "current_turn"} {
				route.RequestKind = ""
				route.BodyMatchScope = scope
				route.BodyContains = "summarizing conversations\nYour task is to create a detailed summary"
				d, err = NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", tt.endpoint, CompositeRouteRequestMatch{Body: []byte(tt.body)})
				require.NoError(t, err)
				require.Nil(t, d.Route, "unused fields cannot enter the %s scope", scope)
			}
		})
	}
}

func TestCompositeRoutePrecisionResponsesLegacyIngress(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far"}]}`,
		`{"input":null,"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far"}]}`,
		`{"prompt":"Your task is to create a detailed summary of the conversation so far"}`,
	} {
		route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "responses", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction", BodyMatchScope: "last_message", BodyContains: "Your task is to create a detailed summary"}
		d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", "responses", CompositeRouteRequestMatch{Body: []byte(body), Explain: true})
		require.NoError(t, err)
		require.NotNil(t, d.Route, "supported legacy Responses ingress must keep compaction and scoped body matching")
		require.Equal(t, "compaction", d.RequestClassification.Kind)
	}
}

func TestCompositeRoutePrecisionUnknownProtocolFailsClosed(t *testing.T) {
	for _, endpoint := range []string{"any", "images", "gemini"} {
		for _, kind := range []string{"compaction", "conversation"} {
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: kind}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", endpoint, CompositeRouteRequestMatch{Body: []byte(`{"input":"work"}`), Explain: true})
			require.NoError(t, err)
			require.Nil(t, d.Route, "an unsupported protocol cannot claim a request-kind match")
			require.Equal(t, "unknown", d.RequestClassification.Kind)
			require.Equal(t, "unsupported_endpoint", d.RequestClassification.Source)
		}
	}
}

func TestCompositeRoutePrecisionAmbiguousJSONFailsClosed(t *testing.T) {
	compact := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction"}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{compact}})
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far"}],"messages":[{"role":"user","content":"Fix login."}]}`,
		`{"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far","content":"Fix login."}]}`,
		`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"You are a helpful AI assistant tasked with summarizing conversations.","text":"Fix login."}]}]}`,
		`{"input":"work","stream":true,"stream":false}`,
	} {
		decision, err := resolver.ResolveWithMatch(context.Background(), 6, "m", "responses", CompositeRouteRequestMatch{Body: []byte(body), Explain: true})
		require.NoError(t, err)
		require.Nil(t, decision.Route)
		require.Equal(t, "unknown", decision.RequestClassification.Kind)
		require.Equal(t, "ambiguous_json", decision.RequestClassification.Source)
	}
}

func TestCompositeRoutePrecisionToolItemCannotBecomeInstructions(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"c1","output":"result","role":"developer","content":"You are a helpful AI assistant tasked with summarizing conversations."},{"type":"message","role":"user","content":"Fix login."}]}`)
	for _, route := range []CompositeModelRoute{
		{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, RequestKind: "compaction"},
		{ID: 9, GroupID: 6, PublicModel: "m", MatchType: "exact", Endpoint: "any", Enabled: true, TargetPlatform: PlatformDeepseek, BodyMatchScope: "instructions", BodyContains: "summarizing conversations"},
	} {
		decision, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "m", "responses", CompositeRouteRequestMatch{Body: body})
		require.NoError(t, err)
		require.Nil(t, decision.Route)
	}
}

func TestCompositeRoutePrecisionLiveShortCircuitsBeforeParsing(t *testing.T) {
	body := []byte(`{"input":"marker"}`)
	route := CompositeModelRoute{RequestKind: "compaction", UserAgentContains: "OnlyCompactClient", BodyMatchScope: "current_turn", BodyContains: "marker"}
	facts := newCompositeRequestFacts(CompositeRouteRequestMatch{Body: body})
	evaluation := evaluateCompositeRouteConditions(route, CompositeRouteRequestMatch{Body: body, UserAgent: "OtherClient", facts: facts})
	require.False(t, evaluation.Matched)
	require.False(t, facts.parsed, "a failed UA must avoid parsing and body scans on live requests")
	explanation := evaluateCompositeRouteConditions(route, CompositeRouteRequestMatch{Body: body, UserAgent: "OtherClient", Explain: true, facts: facts})
	require.False(t, explanation.Matched)
	require.True(t, facts.parsed, "preview still evaluates every configured check")
	require.Len(t, explanation.Checks, 3)
}
