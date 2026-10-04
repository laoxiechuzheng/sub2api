package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const claudeCompactionTestUA = "claude-cli/2.1.286 (external, claude-desktop-3p, agent-sdk/0.3.286)"

func TestCompositeClaudeCompactionCurrentMessageBoundaries(t *testing.T) {
	full := "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of the conversation so far"
	recent := "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of the RECENT portion of the conversation"
	earlier := "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of this conversation. This summary will be placed at the start of a continuing session; newer messages that build on this context will follow after your summary"
	reminder := "<system-reminder>\nCurrent date and workspace context.\n</system-reminder>"
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
	system := func(content any) map[string]any { return map[string]any{"role": "system", "content": content} }
	tests := []struct {
		name, ua, endpoint string
		messages           []map[string]any
		want               bool
	}{
		{"full positive control", claudeCompactionTestUA, "messages", []map[string]any{user(full)}, true},
		{"recent partial template", claudeCompactionTestUA, "messages", []map[string]any{user(recent)}, true},
		{"earlier partial template", claudeCompactionTestUA, "messages", []map[string]any{user(earlier)}, true},
		{"terminal system carrier", claudeCompactionTestUA, "messages", []map[string]any{user(full), system("<budget_tokens>16377</budget_tokens>")}, true},
		{"two terminal system carriers", claudeCompactionTestUA, "messages", []map[string]any{user(full), system(reminder), system("<budget_tokens>16377</budget_tokens>")}, true},
		{"merged leading reminder", claudeCompactionTestUA, "messages", []map[string]any{user(reminder + "\n" + full)}, true},
		{"separate leading reminder block", claudeCompactionTestUA, "messages", []map[string]any{user([]any{text(reminder), text(full)})}, true},
		{"reminder and system carrier", claudeCompactionTestUA, "messages", []map[string]any{user(reminder + "\n" + recent), system(reminder)}, true},
		{"tool result followed by direct prompt", claudeCompactionTestUA, "messages", []map[string]any{user([]any{map[string]any{"type": "tool_result", "content": "old tool data"}, text(full)}), system(reminder)}, true},
		{"non Claude keeps strict last message", "OtherClient", "messages", []map[string]any{user(full), system(reminder)}, false},
		{"Responses keeps strict last message", claudeCompactionTestUA, "responses", []map[string]any{user(full), system(reminder)}, false},
		{"Chat keeps strict last message", claudeCompactionTestUA, "chat_completions", []map[string]any{user(full), system(reminder)}, false},
		{"assistant boundary stops backtracking", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "assistant", "content": "summary"}, system(reminder)}, false},
		{"tool boundary stops backtracking", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "tool", "content": "output"}, system(reminder)}, false},
		{"typed tool cannot masquerade as carrier", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"type": "function_call_output", "role": "system", "content": reminder}}, false},
		{"unsupported system content is not carrier", claudeCompactionTestUA, "messages", []map[string]any{user(full), system([]any{map[string]any{"type": "tool_result", "content": reminder}})}, false},
		{"ordinary followup after historical template", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "assistant", "content": "summary"}, user("Continue fixing the code."), system(reminder)}, false},
		{"tool only latest user is not historical prompt", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "assistant", "content": "summary"}, user([]any{map[string]any{"type": "tool_result", "content": full}}), system(reminder)}, false},
		{"template inside leading reminder is not directive", claudeCompactionTestUA, "messages", []map[string]any{user("<system-reminder>\n" + full + "\n</system-reminder>\nContinue the implementation."), system(reminder)}, false},
		{"unclosed reminder fails closed", claudeCompactionTestUA, "messages", []map[string]any{user("<system-reminder>\n" + full)}, false},
		{"quoted later block stays ordinary", claudeCompactionTestUA, "messages", []map[string]any{user([]any{text("Explain this template:"), text(full)}), system(reminder)}, false},
		{"unmarked arbitrary prefix stays conservative", claudeCompactionTestUA, "messages", []map[string]any{user("Previous request.\n" + full)}, false},
		{"only critical warning is not compaction", claudeCompactionTestUA, "messages", []map[string]any{user("CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.")}, false},
		{"only vague partial phrase is not compaction", claudeCompactionTestUA, "messages", []map[string]any{user("Your task is to create a detailed summary of this conversation.")}, false},
		{"nested reminder hides history", claudeCompactionTestUA, "messages", []map[string]any{user("<system-reminder><system-reminder>note</system-reminder>" + full + "</system-reminder>\nContinue the implementation.")}, false},
		{"unclosed outer reminder hides history", claudeCompactionTestUA, "messages", []map[string]any{user("<system-reminder><system-reminder>note</system-reminder>" + full)}, false},
		{"nested complete reminder before directive", claudeCompactionTestUA, "messages", []map[string]any{user("<system-reminder><system-reminder>note</system-reminder>context</system-reminder>\n" + full)}, true},
		{"quoted recent partial is not directive", claudeCompactionTestUA, "messages", []map[string]any{user("CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nExplain this quoted prompt; do not execute it:\n<example>" + recent + "</example>")}, false},
		{"quoted earlier partial is not directive", claudeCompactionTestUA, "messages", []map[string]any{user("CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nExplain this quoted prompt; do not execute it:\n<example>" + earlier + "</example>")}, false},
		{"null carrier type is not omitted", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "system", "type": nil, "content": "budget"}}, false},
		{"empty carrier type is not omitted", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "system", "type": "", "content": "budget"}}, false},
		{"numeric carrier type is not message", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "system", "type": 0, "content": "budget"}}, false},
		{"boolean carrier type is not message", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "system", "type": false, "content": "budget"}}, false},
		{"explicit message carrier is accepted", claudeCompactionTestUA, "messages", []map[string]any{user(full), {"role": "system", "type": "message", "content": "budget"}}, true},
		{"other client partial keeps baseline", "OtherClient", "messages", []map[string]any{user(recent)}, false},
		{"other client Responses partial keeps baseline", "OtherClient", "responses", []map[string]any{user(recent)}, false},
		{"Claude Responses partial keeps baseline", claudeCompactionTestUA, "responses", []map[string]any{user(recent)}, false},
		{"Claude count tokens partial keeps baseline", claudeCompactionTestUA, "count_tokens", []map[string]any{user(recent)}, false},
		{"Claude Chat partial keeps baseline", claudeCompactionTestUA, "chat_completions", []map[string]any{user(recent)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := "messages"
			if tt.endpoint == "responses" {
				field = "input"
			}
			body, err := json.Marshal(map[string]any{field: tt.messages})
			require.NoError(t, err)
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: PlatformOpenAI, UpstreamModel: "gemini-3.8-flash-high", Endpoint: "any", Enabled: true, RequestKind: "compaction", BodyMatchScope: "last_message", BodyContains: "Your task is to create a detailed summary"}
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
			for _, explain := range []bool{false, true} {
				d, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", tt.endpoint, CompositeRouteRequestMatch{UserAgent: tt.ua, Body: body, Explain: explain})
				require.NoError(t, err)
				require.Equal(t, tt.want, d.Route != nil, "live/preview must agree")
				if explain {
					want := "conversation"
					if tt.want {
						want = "compaction"
					}
					require.Equal(t, want, d.RequestClassification.Kind)
				}
			}
		})
	}
}

func TestCompositeClaudeCompactionExplicitHintAndConservativeBody(t *testing.T) {
	for _, tt := range []struct {
		name, hint, ua, endpoint, body string
		want                           bool
	}{
		{"manual", "manual", claudeCompactionTestUA, "messages", `{"messages":[{"role":"user","content":"Merged prior user text and a custom summary request"}]}`, true},
		{"auto", "auto", claudeCompactionTestUA, "messages", `{"messages":[{"role":"user","content":"work"}]}`, true},
		{"reactive", "reactive", claudeCompactionTestUA, "messages", `{"messages":[{"role":"user","content":"work"}]}`, true},
		{"request class", "compaction", claudeCompactionTestUA, "messages", `{"messages":[{"role":"user","content":"work"}]}`, true},
		{"invalid hint", "true", claudeCompactionTestUA, "messages", `{"messages":[{"role":"user","content":"work"}]}`, false},
		{"unrelated UA", "manual", "OtherClient", "messages", `{"messages":[{"role":"user","content":"work"}]}`, false},
		{"wrong endpoint", "manual", claudeCompactionTestUA, "responses", `{"input":"work"}`, false},
		{"malformed body", "manual", claudeCompactionTestUA, "messages", `{"messages":`, false},
		{"ambiguous body", "manual", claudeCompactionTestUA, "messages", `{"messages":[],"messages":[]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Decode the public optional field so this regression compiles before its implementation.
			var match CompositeRouteRequestMatch
			encoded, err := json.Marshal(map[string]any{"ClaudeCompactionHint": tt.hint})
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &match))
			match.UserAgent = tt.ua
			match.Body = []byte(tt.body)
			match.Explain = true
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: PlatformOpenAI, Endpoint: "any", Enabled: true, RequestKind: "compaction"}
			d, err := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}}).ResolveWithMatch(context.Background(), 6, "claude-opus-5", tt.endpoint, match)
			require.NoError(t, err)
			require.Equal(t, tt.want, d.Route != nil)
			if tt.want {
				require.Equal(t, "claude_request_header", d.RequestClassification.Source)
			}
		})
	}
}

func TestCompositeClaudeCompactionHintKeepsBodyFiltersAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          bool
	}{
		{"positive control", "marker SECRET_CANARY", true},
		{"required marker missing", "ordinary SECRET_CANARY", false},
		{"excluded marker", "marker excluded SECRET_CANARY", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: PlatformOpenAI, Endpoint: "messages", Enabled: true, RequestKind: "compaction", BodyMatchScope: "last_message", BodyContains: "marker", BodyNotContains: "excluded"}
			body, err := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": tc.content}}})
			require.NoError(t, err)
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{route}})
			for _, explain := range []bool{false, true} {
				d, err := resolver.ResolveWithMatch(context.Background(), 6, "claude-opus-5", "messages", CompositeRouteRequestMatch{UserAgent: claudeCompactionTestUA, Body: body, ClaudeCompactionHint: "manual", Explain: explain})
				require.NoError(t, err)
				require.Equal(t, tc.want, d.Route != nil)
				encoded, err := json.Marshal(d)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "SECRET_CANARY")
				if explain {
					require.Equal(t, "claude_request_header", d.RequestClassification.Source)
				}
			}
		})
	}
}
