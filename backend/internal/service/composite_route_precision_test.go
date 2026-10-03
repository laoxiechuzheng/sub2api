package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompositeRoutePrecisionExplainAndNativeCompaction(t *testing.T) {
	route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: PlatformDeepseek, Endpoint: "any", Enabled: true, RequestKind: "compaction"}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
	match := CompositeRouteRequestMatch{Body: []byte(`{"input":"SECRET_CANARY"}`), NativeCompaction: true, Explain: true}
	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "responses", match)
	require.NoError(t, err)
	require.NotNil(t, decision.Route)
	require.NotNil(t, decision.RequestClassification)
	require.Equal(t, "native_endpoint", decision.RequestClassification.Source)
	require.Len(t, decision.ConditionEvaluations, 1)
	require.True(t, decision.ConditionEvaluations[0].Selected)
	require.True(t, decision.ConditionEvaluations[0].Matched)
	encoded, err := json.Marshal(decision)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "SECRET_CANARY")
	match.Explain = false
	live, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "responses", match)
	require.NoError(t, err)
	require.Nil(t, live.ConditionEvaluations)
	require.Nil(t, live.RequestClassification)
}

func TestCompositeRoutePrecisionRequestKindOnlyIsConditional(t *testing.T) {
	fallback := CompositeModelRoute{ID: 1, GroupID: 6, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: PlatformOpenAI, Endpoint: "any", Enabled: true}
	compact := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "claude", MatchType: "contains", TargetPlatform: PlatformDeepseek, Endpoint: "any", Enabled: true, RequestKind: "compaction"}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{fallback, compact}})
	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "responses", CompositeRouteRequestMatch{Body: []byte(`{"input":"work"}`), NativeCompaction: true})
	require.NoError(t, err)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(9), decision.Route.ID)
	decision, err = resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "responses", CompositeRouteRequestMatch{Body: []byte(`{"input":"work"}`)})
	require.NoError(t, err)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(1), decision.Route.ID)
	decision, err = resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "responses", CompositeRouteRequestMatch{IgnoreRequestConditions: true})
	require.NoError(t, err)
	require.NotNil(t, decision.Route, "catalog ignores request conditions")
}

func TestCompositeRoutePrecisionCurrentTurnDoesNotMatchHistory(t *testing.T) {
	var route CompositeModelRoute
	require.NoError(t, json.Unmarshal([]byte(`{"id":9,"group_id":6,"public_model":"claude-opus-5","match_type":"exact","target_platform":"deepseek","upstream_model":"deepseek-v4.1-flash","endpoint":"messages","enabled":true,"body_match_scope":"last_message","body_contains":"Your task is to create a detailed summary"}`), &route))
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "messages", CompositeRouteRequestMatch{
		Body: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"Old summary quotes: Your task is to create a detailed summary"},{"role":"assistant","content":"Acknowledged."},{"role":"user","content":"Continue the implementation."}]}`),
	})
	require.NoError(t, err)
	require.Nil(t, decision.Route, "a historical signature must not trigger a current-message rule")
}

func TestCompositeRoutePrecisionStructuredScopes(t *testing.T) {
	tests := []struct {
		name, endpoint, fields, body string
		want                         bool
	}{
		{"terminal user text", "messages", `"body_match_scope":"last_message"`, `{"messages":[{"role":"user","content":"marker"}]}`, true},
		{"terminal text blocks", "messages", `"body_match_scope":"last_message"`, `{"messages":[{"role":"user","content":[{"type":"text","text":"marker"}]}]}`, true},
		{"tool result is not user intent", "messages", `"body_match_scope":"last_message"`, `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"marker"}]}]}`, false},
		{"thinking is not user intent", "messages", `"body_match_scope":"last_message"`, `{"messages":[{"role":"user","content":[{"type":"thinking","thinking":"marker"}]}]}`, false},
		{"assistant tail is not latest user", "messages", `"body_match_scope":"last_message"`, `{"messages":[{"role":"user","content":"marker"},{"role":"assistant","content":"continue"}]}`, false},
		{"system tail is not latest user", "messages", `"body_match_scope":"last_message"`, `{"messages":[{"role":"user","content":"marker"},{"role":"system","content":"tool trace"}]}`, false},
		{"responses message", "responses", `"body_match_scope":"last_message"`, `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"marker"}]}]}`, true},
		{"responses tool tail", "responses", `"body_match_scope":"last_message"`, `{"input":[{"role":"user","content":"marker"},{"type":"function_call_output","output":"marker"}]}`, false},
		{"responses string input", "responses", `"body_match_scope":"last_message"`, `{"input":"marker"}`, true},
		{"instructions string", "responses", `"body_match_scope":"instructions"`, `{"instructions":"marker","input":"work"}`, true},
		{"anthropic system text", "messages", `"body_match_scope":"instructions"`, `{"system":[{"type":"text","text":"marker"}],"messages":[{"role":"user","content":"work"}]}`, true},
		{"chat system", "chat_completions", `"body_match_scope":"instructions"`, `{"messages":[{"role":"system","content":"marker"},{"role":"user","content":"work"}]}`, true},
		{"history is not instructions", "chat_completions", `"body_match_scope":"instructions"`, `{"messages":[{"role":"user","content":"old"},{"role":"system","content":"marker"},{"role":"user","content":"work"}]}`, false},
		{"current instructions", "responses", `"body_match_scope":"current_turn"`, `{"instructions":"marker","input":"work"}`, true},
		{"current ignores schemas", "responses", `"body_match_scope":"current_turn"`, `{"tools":[{"description":"marker"}],"input":"work"}`, false},
		{"legacy default still matches history", "messages", `"body_match_scope":"full_body"`, `{"messages":[{"role":"user","content":"marker"},{"role":"user","content":"work"}]}`, true},
		{"invalid structured JSON fails closed", "responses", `"body_match_scope":"current_turn"`, `{"input":"marker"`, false},
		{"unknown scope fails closed", "responses", `"body_match_scope":"typo"`, `{"input":"marker"}`, false},
		{"unknown mode fails closed", "responses", `"body_match_mode":"typo"`, `{"input":"marker"}`, false},
		{"prefix direct", "responses", `"body_match_scope":"last_message","body_match_mode":"prefix"`, `{"input":"  marker instruction"}`, true},
		{"prefix rejects quotation", "responses", `"body_match_scope":"last_message","body_match_mode":"prefix"`, `{"input":"Please explain marker instruction"}`, false},
		{"escaped unicode text", "responses", `"body_match_scope":"last_message"`, `{"input":"\u006darker"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var route CompositeModelRoute
			require.NoError(t, json.Unmarshal([]byte(`{"id":9,"group_id":6,"public_model":"claude-opus-5","match_type":"exact","target_platform":"deepseek","endpoint":"any","enabled":true,"body_contains":"marker",`+tt.fields+`}`), &route))
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
			d, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", tt.endpoint, CompositeRouteRequestMatch{Body: []byte(tt.body)})
			require.NoError(t, err)
			require.Equal(t, tt.want, d.Route != nil)
		})
	}
}

func TestCompositeRoutePrecisionCompactionProfiles(t *testing.T) {
	claude := "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of the conversation so far"
	codex := "You are a context summarization and handoff agent. Produce one accurate, self-contained record that allows another coding agent to continue the work without losing the user's intent, constraints, technical context, or latest working state."
	opencode := "You are a context summarization agent. You are given a conversation between a user and an agent. Your goal is to produce a structured summary matching the format specified so another coding agent can continue the work."
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	tests := []struct {
		name, endpoint, body string
		want                 bool
	}{
		{"Claude active terminal", "messages", `{"messages":[{"role":"user","content":` + quote(claude) + `}]}`, true},
		{"Claude quoted second block", "messages", `{"messages":[{"role":"user","content":[{"type":"text","text":"Explain this example:"},{"type":"text","text":` + quote(claude) + `}]}]}`, false},
		{"Claude directive split across blocks", "messages", `{"messages":[{"role":"user","content":[{"type":"text","text":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."},{"type":"text","text":"Your task is to create a detailed summary of the conversation so far"}]}]}`, true},
		{"Claude quote is not active instruction", "responses", `{"input":` + quote("Explain this prompt: "+claude) + `}`, false},
		{"Claude old user", "messages", `{"messages":[{"role":"user","content":` + quote(claude) + `},{"role":"user","content":"continue"}]}`, false},
		{"Claude old system", "chat_completions", `{"messages":[{"role":"user","content":"old"},{"role":"system","content":` + quote(claude) + `},{"role":"user","content":"continue"}]}`, false},
		{"Claude tool result", "messages", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":` + quote(claude) + `}]}]}`, false},
		{"Codex current instructions", "responses", `{"instructions":` + quote(codex) + `,"input":"history"}`, true},
		{"Codex current terminal", "responses", `{"input":[{"role":"user","content":` + quote(codex) + `}]}`, true},
		{"Codex quoted system docs", "responses", `{"instructions":` + quote("You are a coding agent. Example prompt: "+codex) + `,"input":"work"}`, false},
		{"Codex tool schema", "responses", `{"tools":[{"description":` + quote(codex) + `}],"input":"work"}`, false},
		{"Codex old input", "responses", `{"input":[{"role":"user","content":` + quote(codex) + `},{"role":"user","content":"work"}]}`, false},
		{"OpenCode current system", "chat_completions", `{"messages":[{"role":"system","content":` + quote(opencode) + `},{"role":"user","content":"history"}]}`, true},
		{"OpenCode quoted second system block", "messages", `{"system":[{"type":"text","text":"Coding-agent documentation examples:"},{"type":"text","text":` + quote(opencode) + `}],"messages":[{"role":"user","content":"work"}]}`, false},
		{"OpenCode old assistant", "messages", `{"messages":[{"role":"assistant","content":` + quote(opencode) + `},{"role":"user","content":"work"}]}`, false},
		{"unknown summary prose", "responses", `{"input":"Discuss automatic compaction and summaries."}`, false},
		{"malformed JSON", "responses", `{"input":"summary"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var route CompositeModelRoute
			require.NoError(t, json.Unmarshal([]byte(`{"id":9,"group_id":6,"public_model":"claude-opus-5","match_type":"exact","target_platform":"deepseek","endpoint":"any","enabled":true,"request_kind":"compaction"}`), &route))
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "claude-opus-5", tt.endpoint, CompositeRouteRequestMatch{Body: []byte(tt.body)})
			require.NoError(t, err)
			require.Equal(t, tt.want, d.Route != nil)
		})
	}
}

func TestCompositeRoutePrecisionAllRequiresEverySignature(t *testing.T) {
	var route CompositeModelRoute
	require.NoError(t, json.Unmarshal([]byte(`{"id":9,"group_id":6,"public_model":"claude-opus-5","match_type":"exact","target_platform":"deepseek","upstream_model":"deepseek-v4.1-flash","endpoint":"messages","enabled":true,"body_match_mode":"all","body_contains":"first marker\nsecond marker"}`), &route))
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "messages", CompositeRouteRequestMatch{
		Body: []byte(`{"messages":[{"role":"user","content":"first marker only"}]}`),
	})
	require.NoError(t, err)
	require.Nil(t, decision.Route, "all mode must not behave like legacy OR")
}

func TestCompositeRoutePrecisionExcludedTextVetoesRoute(t *testing.T) {
	var route CompositeModelRoute
	require.NoError(t, json.Unmarshal([]byte(`{"id":9,"group_id":6,"public_model":"claude-opus-5","match_type":"exact","target_platform":"deepseek","upstream_model":"deepseek-v4.1-flash","endpoint":"messages","enabled":true,"body_match_scope":"last_message","body_contains":"summary","body_not_contains":"quoted example"}`), &route))
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "messages", CompositeRouteRequestMatch{
		Body: []byte(`{"messages":[{"role":"user","content":"Explain this quoted example of a summary."}]}`),
	})
	require.NoError(t, err)
	require.Nil(t, decision.Route)
}

func TestCompositeRoutePrecisionCompactionMustNotUseHistoricalInstructions(t *testing.T) {
	var route CompositeModelRoute
	require.NoError(t, json.Unmarshal([]byte(`{"id":9,"group_id":6,"public_model":"claude-opus-5","match_type":"exact","target_platform":"deepseek","upstream_model":"deepseek-v4.1-flash","endpoint":"messages","enabled":true,"request_kind":"compaction"}`), &route))
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
	decision, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "messages", CompositeRouteRequestMatch{
		Body: []byte(`{"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of the conversation so far"},{"role":"assistant","content":"summary"},{"role":"user","content":"Fix the login error."}]}`),
	})
	require.NoError(t, err)
	require.Nil(t, decision.Route)
}
