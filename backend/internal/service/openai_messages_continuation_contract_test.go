//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 由实际审查补证迁移的真实路径回归；所有transport均为合成内存stub。
const v10Model = "gpt-6.1-sol"

func v10Hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func v10JSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
func v10Account() *Account {
	a := rawGPT56ResponsesAPIKeyAccount(v10Model, v10Model)
	a.ID = 10001
	a.Name = "synthetic-cache-review"
	a.Credentials["api_key"] = "synthetic-not-a-secret"
	a.Credentials["base_url"] = "https://api.openai.com/v1"
	return a
}
func v10Context(body []byte, keyID int64) (*gin.Context, *httptest.ResponseRecorder) {
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", &APIKey{ID: keyID})
	return c, r
}
func v10Response(responseID, model, usage string, output []any) *http.Response {
	response := map[string]any{"id": responseID, "object": "response", "model": model, "status": "completed", "output": output, "usage": json.RawMessage(usage)}
	b, _ := json.Marshal(response)
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"synthetic-created\",\"output\":[]}}\n\n"
	sse += "data: {\"type\":\"response.completed\",\"response\":" + string(b) + "}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
}
func v10TextOutput(text string) []any {
	return []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
}
func v10HTTPError(message, code string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message, "code": code}})
	return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}
}
func v10Request(t *testing.T, messages []any, system string, toolName string) []byte {
	metadata := map[string]any{"user_id": `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"","session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`}
	r := map[string]any{"model": v10Model, "max_tokens": 128, "stream": false, "messages": messages, "system": system, "metadata": metadata}
	if toolName != "" {
		r["tools"] = []any{map[string]any{"name": toolName, "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}}}
	}
	return v10JSON(t, r)
}
func v10Message(role, content string) any { return map[string]any{"role": role, "content": content} }
func v10Forward(t *testing.T, s *OpenAIGatewayService, a *Account, b []byte, key string) (*OpenAIForwardResult, *httptest.ResponseRecorder) {
	t.Helper()
	c, r := v10Context(b, 10001)
	result, err := s.ForwardAsAnthropic(context.Background(), c, a, b, key, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, r
}

func TestV10CacheAContextErrorRealPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, message, code string
		explicitRule        bool
	}{
		{"structured_context", "maximum context length exceeded", "context_length_exceeded", false},
		{"ordinary_context_window", "Your input exceeds the context window of this model", "", false},
		{"explicit_temp_rule_context", "maximum context length exceeded", "context_length_exceeded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := v10Account()
			if tc.explicitRule {
				a.Credentials["temp_unschedulable_enabled"] = true
				a.Credentials["temp_unschedulable_rules"] = []any{map[string]any{"scope": "account", "error_code": float64(400), "keywords": []any{"maximum context length"}, "duration_minutes": float64(10)}}
			}
			repo := &rateLimitAccountRepoStub{}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), accountRepo: repo, rateLimitService: NewRateLimitService(repo, nil, rawChatCompletionsTestConfig(), nil, nil), httpUpstream: &httpUpstreamRecorder{resp: v10HTTPError(tc.message, tc.code)}}
			b := v10Request(t, []any{v10Message("user", "synthetic")}, "synthetic-system", "")
			c, r := v10Context(b, 10001)
			_, err := s.ForwardAsAnthropic(context.Background(), c, a, b, "synthetic-session", "")
			require.Error(t, err)
			var failover *UpstreamFailoverError
			if tc.explicitRule {
				assert.ErrorAs(t, err, &failover, "保留管理员显式临时不可调度 policy")
				assert.False(t, c.Writer.Written())
				assert.Equal(t, 1, repo.tempCalls)
			} else {
				assert.NotErrorAs(t, err, &failover)
				assert.Equal(t, 400, r.Code)
				assert.Equal(t, "invalid_request_error", gjson.GetBytes(r.Body.Bytes(), "error.type").String())
				assert.Equal(t, 0, repo.tempCalls)
			}
			assert.Equal(t, 0, repo.setErrorCalls)
			assert.Equal(t, 0, repo.rateLimitedCalls)
			assert.False(t, s.isOpenAIAccountRuntimeBlocked(a))
			t.Logf("synthetic code=%s explicit_rule=%t downstream=%d failover=%t error_marks=%d temp_marks=%d", tc.code, tc.explicitRule, r.Code, failover != nil, repo.setErrorCalls, repo.tempCalls)
		})
	}
}

func TestV10CacheARetryFullReplayRealPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"not_found", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			a := v10Account()
			messages := make([]any, 0, 15)
			for i := 0; i < 15; i++ {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				messages = append(messages, v10Message(role, fmt.Sprintf("synthetic-%02d", i)))
			}
			b := v10Request(t, messages, "synthetic-system", "")
			message, code := "Previous response with id synthetic-prev not found", "previous_response_not_found"
			if kind == "unsupported" {
				message = "Unsupported parameter: previous_response_id"
				code = "unsupported_parameter"
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{v10HTTPError(message, code), v10Response("synthetic-after-retry", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-answer"))}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, _ := v10Context(b, 10001)
			bindCompatProofForNextRequest(t, s, c, a, "synthetic-session", "synthetic-prev", b, v10Model)
			result, err := s.ForwardAsAnthropic(context.Background(), c, a, b, "synthetic-session", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.requests, 2)
			assert.Equal(t, "synthetic-prev", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
			assert.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
			assert.Equal(t, int64(17), gjson.GetBytes(upstream.bodies[1], "input.#").Int(), "15历史+system+guard必须全量")
			assert.Equal(t, "synthetic-00", gjson.GetBytes(upstream.bodies[1], "input.2.content.0.text").String())
			assert.Equal(t, kind == "unsupported", s.isOpenAICompatSessionContinuationDisabled(context.Background(), c, a, "synthetic-session"))
			t.Logf("synthetic retry=%s first_items=%d second_items=%d final_body_hash=%s", kind, gjson.GetBytes(upstream.bodies[0], "input.#").Int(), gjson.GetBytes(upstream.bodies[1], "input.#").Int(), v10Hash(upstream.bodies[1]))
		})
	}
}

func TestV10CacheBCanonicalParity(t *testing.T) {
	for _, tc := range []struct {
		name, raw                string
		read, write, short, long int
	}{
		{"canonical_zero", `{"input_tokens":20,"cache_read_input_tokens":9,"cache_creation_input_tokens":9,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, 0, 0, 0, 0},
		{"fieldwise_fallback_read", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":2},"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":9}}`, 5, 2, 0, 0},
		{"fieldwise_input_read_wins", `{"input_tokens":20,"input_tokens_details":{"cached_tokens":4},"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":9}}`, 4, 9, 0, 0},
		{"negative_canonical", `{"input_tokens":20,"cache_read_input_tokens":9,"input_tokens_details":{"cached_tokens":-3,"cache_write_tokens":-2}}`, 0, 0, 0, 0},
		{"ttl_valid", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":7}}}`, 0, 10, 3, 7},
		{"ttl_partial", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_12h_tokens":10}}}`, 0, 10, 0, 0},
		{"ttl_fraction", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":0.5,"ttl_12h_tokens":9.5}}}`, 0, 10, 0, 0},
		{"ttl_string", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":"0","ttl_12h_tokens":"10"}}}`, 0, 10, 0, 0},
		{"ttl_bad_sum", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":8}}}`, 0, 10, 0, 0},
		{"ttl_invalid_input_no_prompt_fallback", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{}},"prompt_tokens_details":{"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":7}}}`, 0, 10, 0, 0},
		{"ttl_absent_input_prompt_fallback", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10},"prompt_tokens_details":{"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":7}}}`, 0, 10, 3, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := openAIUsageFromGJSON(gjson.Parse(tc.raw))
			require.True(t, ok)
			assert.Equal(t, tc.read, parsed.CacheReadInputTokens)
			assert.Equal(t, tc.write, parsed.CacheCreationInputTokens)
			assert.Equal(t, tc.short, parsed.CacheCreation30mInputTokens)
			assert.Equal(t, tc.long, parsed.CacheCreation12hInputTokens)
			var typed apicompat.ResponsesUsage
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &typed))
			copied := copyOpenAIUsageFromResponsesUsage(&typed)
			assert.Equal(t, parsed.InputTokens, copied.InputTokens)
			assert.Equal(t, parsed.OutputTokens, copied.OutputTokens)
			assert.Equal(t, parsed.CacheReadInputTokens, copied.CacheReadInputTokens, "typed缓存字段应与已定义逐字段presence规则一致")
			assert.Equal(t, parsed.CacheCreationInputTokens, copied.CacheCreationInputTokens)
			// TTL单独已在v8真实path证明丢失；此表只补合法性与两候选差异，不再重做H4矩阵。
			t.Logf("synthetic usage_hash=%s gjson_read=%d typed_read=%d write=%d ttl30=%d ttl12=%d", v10Hash([]byte(tc.raw)), parsed.CacheReadInputTokens, copied.CacheReadInputTokens, parsed.CacheCreationInputTokens, parsed.CacheCreation30mInputTokens, parsed.CacheCreation12hInputTokens)
		})
	}
}

func TestV10CacheBActualJSONLibrarySemantics(t *testing.T) {
	for _, tc := range []struct{ name, raw, path string }{
		{"duplicate_read", `{"input_tokens":20,"input_tokens_details":{"cached_tokens":1,"cached_tokens":7}}`, "input_tokens_details.cached_tokens"},
		{"duplicate_ttl", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_30m_tokens":9,"ttl_12h_tokens":7}}}`, "input_tokens_details.cache_write_tokens_details.ttl_30m_tokens"},
		{"fraction_read", `{"input_tokens":20,"input_tokens_details":{"cached_tokens":2.5}}`, "input_tokens_details.cached_tokens"},
		{"string_read", `{"input_tokens":20,"input_tokens_details":{"cached_tokens":"2"}}`, "input_tokens_details.cached_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gjson.Get(tc.raw, tc.path)
			var typed apicompat.ResponsesUsage
			err := json.Unmarshal([]byte(tc.raw), &typed)
			parsed, ok := openAIUsageFromGJSON(gjson.Parse(tc.raw))
			require.True(t, ok)
			t.Logf("synthetic case=%s raw_node=%s gjson_int=%d typed_error=%t typed_read=%d gjson_read=%d ttl30=%d ttl12=%d", tc.name, g.Raw, g.Int(), err != nil, copyOpenAIUsageFromResponsesUsage(&typed).CacheReadInputTokens, parsed.CacheReadInputTokens, parsed.CacheCreation30mInputTokens, parsed.CacheCreation12hInputTokens)
			if strings.HasPrefix(tc.name, "duplicate") {
				require.NoError(t, err)
				if tc.name == "duplicate_read" {
					assert.Equal(t, int64(1), g.Int())
					assert.Equal(t, 1, typed.InputTokensDetails.CachedTokens, "cache字段共用gjson首值，重复键仍属不支持输入")
				}
				if tc.name == "duplicate_ttl" {
					assert.Equal(t, int64(3), g.Int())
					assert.Equal(t, 3, parsed.CacheCreation30mInputTokens)
					assert.Equal(t, 7, parsed.CacheCreation12hInputTokens)
				}
			} else {
				assert.Error(t, err)
				assert.Equal(t, 2, parsed.CacheReadInputTokens)
			}
		})
	}
}

func TestV10CacheBNonCacheCandidateCallerDifference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"messages_json", "messages_sse", "chat_json", "chat_sse"} {
		for _, zeroTerminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/zero=%t", route, zeroTerminal), func(t *testing.T) {
				// raw总output=2、独立reasoning=5、total17时gjson补到7；typed权威仍2。
				usage := `{"input_tokens":10,"output_tokens":2,"total_tokens":17,"output_tokens_details":{"reasoning_tokens":5}}`
				terminalUsage := usage
				if zeroTerminal {
					terminalUsage = `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`
				}
				resp := v10Response("synthetic-terminal", "grok-4.6", terminalUsage, v10TextOutput("synthetic-answer"))
				if zeroTerminal {
					b, _ := io.ReadAll(resp.Body)
					prefix := `data: {"type":"response.in_progress","response":{"usage":` + usage + "}}\n\n"
					resp.Body = io.NopCloser(strings.NewReader(prefix + string(b)))
				}
				s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
				c, r := v10Context(nil, 10001)
				account := &Account{ID: 10001, Platform: PlatformGrok, Type: AccountTypeAPIKey}
				var result *OpenAIForwardResult
				var err error
				switch route {
				case "messages_json":
					result, err = s.handleAnthropicBufferedStreamingResponse(resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now())
				case "messages_sse":
					result, err = s.handleAnthropicStreamingResponse(resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now())
				case "chat_json":
					result, err = s.handleChatBufferedStreamingResponse(resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now())
				case "chat_sse":
					result, err = s.handleChatStreamingResponse(resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(), 0)
				}
				if route == "chat_json" && zeroTerminal {
					// Grok Chat JSON既有无计费用量guard在全零terminal下返回failover，不能当fixture失败略去。
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					assert.Nil(t, result)
					var candidate OpenAIUsage
					s.parseSSEUsageBytesWithType([]byte(`{"type":"response.in_progress","response":{"usage":`+usage+`}}`), "response.in_progress", &candidate)
					s.parseSSEUsageBytesWithType([]byte(`{"type":"response.completed","response":{"usage":`+terminalUsage+`}}`), "response.completed", &candidate)
					assert.True(t, hasBillableGrokChatUsage(candidate), "换成gjson会让原missing-usage failover变成billable success")
					t.Logf("synthetic route=%s zero=%t current_failover=true gjson_candidate_total=%d gjson_candidate_output=%d", route, zeroTerminal, candidate.InputTokens, candidate.OutputTokens)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				wantOutput := 2
				if zeroTerminal {
					wantOutput = 0
				}
				assert.Equal(t, wantOutput, result.Usage.OutputTokens, "钉住当前非缓存output行为，不采纳无关reasoning补价")
				var gjsonCandidate OpenAIUsage
				if zeroTerminal {
					s.parseSSEUsageBytesWithType([]byte(`{"type":"response.in_progress","response":{"usage":`+usage+`}}`), "response.in_progress", &gjsonCandidate)
				}
				s.parseSSEUsageBytesWithType([]byte(`{"type":"response.completed","response":{"usage":`+terminalUsage+`}}`), "response.completed", &gjsonCandidate)
				assert.Equal(t, 7, gjsonCandidate.OutputTokens, "替代候选真实parser会改变非缓存output与全零权威")
				t.Logf("synthetic route=%s zero=%t current_total=%d current_output=%d gjson_candidate_total=%d gjson_candidate_output=%d downstream_hash=%s", route, zeroTerminal, result.Usage.InputTokens, result.Usage.OutputTokens, gjsonCandidate.InputTokens, gjsonCandidate.OutputTokens, v10Hash(r.Body.Bytes()))
			})
		}
	}
}

func v10Chain(t *testing.T, body []byte) string {
	t.Helper()
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(body, &req))
	return buildOpenAICompatAnthropicDigestChain(&req)
}

func TestV10CacheC3RealBranchIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name         string
		variant      int
		wantPrevious string
	}{
		{"same_request_retry", 0, ""},
		{"different_first_user_same_key", 1, ""},
		{"different_system_same_history", 2, ""},
		{"different_tools_same_history", 3, ""},
		{"fork_wrong_assistant_extends_prior_request", 4, ""},
		{"valid_linear_generated_echo", 5, "synthetic-A"},
		{"changed_tools_valid_echo_still_wrong_context", 6, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := []any{v10Message("user", "synthetic-root")}
			first := v10Request(t, root, "synthetic-system", "synthetic_tool")
			secondMessages := append([]any(nil), root...)
			system, tool := "synthetic-system", "synthetic_tool"
			switch tc.variant {
			case 1:
				secondMessages = []any{v10Message("user", "synthetic-other-root")}
			case 2:
				system = "synthetic-other-system"
			case 3:
				tool = "synthetic_other_tool"
			case 4:
				secondMessages = append(secondMessages, v10Message("assistant", "synthetic-FORK-B-not-generated-A"), v10Message("user", "synthetic-next"))
			case 5:
				secondMessages = append(secondMessages, v10Message("assistant", "synthetic-generated-A"), v10Message("user", "synthetic-next"))
			case 6:
				tool = "synthetic_other_tool"
				secondMessages = append(secondMessages, v10Message("assistant", "synthetic-generated-A"), v10Message("user", "synthetic-next"))
			}
			second := v10Request(t, secondMessages, system, tool)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				v10Response("synthetic-A", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-generated-A")),
				v10Response("synthetic-B", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-generated-B")),
			}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			_, client := v10Forward(t, s, v10Account(), first, "synthetic-same-session")
			require.Equal(t, "synthetic-generated-A", gjson.GetBytes(client.Body.Bytes(), "content.0.text").String())
			v10Forward(t, s, v10Account(), second, "synthetic-same-session")
			require.Len(t, upstream.requests, 2)
			got := gjson.GetBytes(upstream.bodies[1], "previous_response_id").String()
			assert.Equal(t, tc.wantPrevious, got, "不能只凭metadata/explicit key相同裁剪历史并续接错误分支")
			stored, next := v10Chain(t, first), v10Chain(t, second)
			candidate := strings.HasPrefix(next, stored+"-")
			t.Logf("synthetic case=%s actual_previous_present=%t history_chain_candidate=%t stored_chain_hash=%s next_chain_hash=%s request_items=%d", tc.name, got != "", candidate, v10Hash([]byte(stored)), v10Hash([]byte(next)), gjson.GetBytes(upstream.bodies[1], "input.#").Int())
			if tc.variant == 4 {
				assert.True(t, candidate, "历史提案只存request链会误接；实际P1必须拒绝")
			}
			if tc.variant == 6 {
				assert.True(t, candidate, "历史request链无tools；实际P1 fingerprint必须拒绝")
			}
			if tc.variant == 3 {
				assert.Equal(t, stored, next, "原chain不含tools，无法作为完整续接上下文证明")
			}
		})
	}
}

func TestV10CacheC3InterleavedBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		v10Response("synthetic-A1", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-A1-answer")),
		v10Response("synthetic-B1", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-B1-answer")),
		v10Response("synthetic-A2", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-A2-answer")),
	}}
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	requests := [][]any{
		{v10Message("user", "synthetic-root-A")},
		{v10Message("user", "synthetic-root-B")},
		{v10Message("user", "synthetic-root-A"), v10Message("assistant", "synthetic-A1-answer"), v10Message("user", "synthetic-A-next")},
	}
	for _, messages := range requests {
		v10Forward(t, s, v10Account(), v10Request(t, messages, "synthetic-system", ""), "synthetic-shared-session")
	}
	require.Len(t, upstream.requests, 3)
	assert.Empty(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").String(), "B不应挂A")
	assert.NotEqual(t, "synthetic-B1", gjson.GetBytes(upstream.bodies[2], "previous_response_id").String(), "A返回主线不应挂B；可恢复A1或full replay")
	t.Logf("synthetic interleave r1_previous=%t r2_previous=%t r3_previous_hash=%s", gjson.GetBytes(upstream.bodies[0], "previous_response_id").Exists(), gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists(), v10Hash([]byte(gjson.GetBytes(upstream.bodies[2], "previous_response_id").String())))
}

func TestV10CacheC3ToolBranchGeneratedEcho(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, sameArgs := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_args=%t", sameArgs), func(t *testing.T) {
			first := v10Request(t, []any{v10Message("user", "synthetic-root")}, "synthetic-system", "synthetic_tool")
			output := []any{map[string]any{"type": "function_call", "id": "fc_synthetic", "call_id": "call_synthetic", "name": "synthetic_tool", "arguments": `{"x":"A"}`, "status": "completed"}}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{v10Response("synthetic-tool-A", v10Model, `{"input_tokens":3,"output_tokens":5}`, output), v10Response("synthetic-tool-next", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done"))}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			_, client := v10Forward(t, s, v10Account(), first, "synthetic-same-session")
			var generated apicompat.AnthropicResponse
			require.NoError(t, json.Unmarshal(client.Body.Bytes(), &generated))
			require.Len(t, generated.Content, 1)
			if !sameArgs {
				generated.Content[0].Input = json.RawMessage(`{"x":"B"}`)
			}
			messages := []any{v10Message("user", "synthetic-root"), map[string]any{"role": "assistant", "content": generated.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": generated.Content[0].ID, "content": "synthetic-tool-result"}}}}
			second := v10Request(t, messages, "synthetic-system", "synthetic_tool")
			v10Forward(t, s, v10Account(), second, "synthetic-same-session")
			got := gjson.GetBytes(upstream.bodies[1], "previous_response_id").String()
			if sameArgs {
				assert.Equal(t, "synthetic-tool-A", got, "真实生成tool echo必须仍可有效续接")
			} else {
				assert.Empty(t, got, "same call_id但不同args不是上一response输出")
			}
			candidate := strings.HasPrefix(v10Chain(t, second), v10Chain(t, first)+"-")
			if !sameArgs {
				assert.True(t, candidate, "历史request链反例保持，实际P1不能挂该previous")
			}
			t.Logf("synthetic tool_echo_equal=%t previous_present=%t request_chain_candidate=%t", sameArgs, got != "", candidate)
		})
	}
}

// 每个并发请求单独body/response，channels只控制合成返回次序，不触真实网络。
type v10ConcurrentUpstream struct {
	mu       sync.Mutex
	requests map[string][]byte
	bEntered chan struct{}
	releaseB chan struct{}
}

func (u *v10ConcurrentUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	label := "A"
	if bytes.Contains(b, []byte("synthetic-root-B")) {
		label = "B"
	}
	u.mu.Lock()
	u.requests[label] = append([]byte(nil), b...)
	u.mu.Unlock()
	if label == "B" {
		close(u.bEntered)
		select {
		case <-u.releaseB:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return v10Response("synthetic-"+label, v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-"+label+"-answer")), nil
}
func (u *v10ConcurrentUpstream) DoWithTLS(req *http.Request, p string, a int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, a, n)
}

func TestV10CacheC3ConcurrentCompletionOverwrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &v10ConcurrentUpstream{requests: make(map[string][]byte), bEntered: make(chan struct{}), releaseB: make(chan struct{})}
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
	a := v10Account()
	rootB := v10Request(t, []any{v10Message("user", "synthetic-root-B")}, "synthetic-system", "")
	rootA := v10Request(t, []any{v10Message("user", "synthetic-root-A")}, "synthetic-system", "")
	bDone := make(chan error, 1)
	go func() {
		c, _ := v10Context(rootB, 10001)
		_, err := s.ForwardAsAnthropic(context.Background(), c, a, rootB, "synthetic-shared-session", "")
		bDone <- err
	}()
	select {
	case <-u.bEntered:
	case <-time.After(time.Second):
		t.Fatal("synthetic B did not reach fake transport")
	}
	v10Forward(t, s, a, rootA, "synthetic-shared-session")
	close(u.releaseB)
	require.NoError(t, <-bDone)
	next := v10Request(t, []any{v10Message("user", "synthetic-root-A"), v10Message("assistant", "synthetic-A-answer"), v10Message("user", "synthetic-next-A")}, "synthetic-system", "")
	u2 := &httpUpstreamRecorder{resp: v10Response("synthetic-A2", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done"))}
	s.httpUpstream = u2
	v10Forward(t, s, a, next, "synthetic-shared-session")
	got := gjson.GetBytes(u2.lastBody, "previous_response_id").String()
	assert.NotEqual(t, "synthetic-B", got, "慢分支B后完成不能使A下一轮带B的上下文")
	u.mu.Lock()
	aPrevious := gjson.GetBytes(u.requests["A"], "previous_response_id").Exists()
	bPrevious := gjson.GetBytes(u.requests["B"], "previous_response_id").Exists()
	u.mu.Unlock()
	assert.False(t, aPrevious)
	assert.False(t, bPrevious)
	t.Logf("synthetic concurrent_first_previous_A=%t B=%t next_wrong_B=%t", aPrevious, bPrevious, got == "synthetic-B")
}

func TestV10CacheC1AnchorGateFacts(t *testing.T) {
	for _, tc := range []struct {
		name, system, messages string
		want                   bool
	}{
		{"assistant_only", `"synthetic-system"`, `[{"role":"user","content":"synthetic-root"},{"role":"assistant","content":[{"type":"text","text":"synthetic-A","cache_control":{"type":"ephemeral"}}]}]`, true},
		{"user_only", `"synthetic-system"`, `[{"role":"user","content":[{"type":"text","text":"synthetic-U","cache_control":{"type":"ephemeral"}}]}]`, true},
		{"system_only", `[{"type":"text","text":"synthetic-system","cache_control":{"type":"ephemeral"}}]`, `[{"role":"user","content":"synthetic-U"}]`, true},
		{"none", `"synthetic-system"`, `[{"role":"user","content":"synthetic-U"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req apicompat.AnthropicRequest
			require.NoError(t, json.Unmarshal([]byte(`{"system":`+tc.system+`,"messages":`+tc.messages+`}`), &req))
			key := deriveAnthropicCacheControlPromptCacheKey(&req)
			assert.Equal(t, tc.want, key != "")
			t.Logf("synthetic case=%s anchor_enabled=%t key_hash=%s", tc.name, key != "", v10Hash([]byte(key)))
		})
	}
}
