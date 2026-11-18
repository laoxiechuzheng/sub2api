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
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 由已实际运行的合成诊断迁移为产品真实路径回归；不依赖真实provider。
// 所有账户、token、会话、请求和响应均为合成 fixture；httpUpstreamRecorder 不发网络。
const v8CacheModel = "gpt-6.1-sol"

func v8CacheAccount(oauth bool) *Account {
	account := rawGPT56ResponsesAPIKeyAccount(v8CacheModel, v8CacheModel)
	account.ID = 8001
	account.Name = "synthetic-cache-account"
	account.Credentials["api_key"] = "synthetic-not-a-real-api-key"
	account.Credentials["base_url"] = "https://api.openai.com/v1"
	if oauth {
		account = rawGPT56ResponsesOAuthAccount(v8CacheModel, v8CacheModel)
		account.ID = 8001
		account.Name = "synthetic-cache-account"
		account.Credentials["access_token"] = "synthetic-not-a-real-oauth-token"
		account.Credentials["chatgpt_account_id"] = "synthetic-upstream-account"
	}
	return account
}

func v8CacheContext(body []byte, apiKeyID int64, explicit string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "synthetic-client/1.0")
	if explicit != "" {
		c.Request.Header.Set("session_id", explicit)
	}
	c.Set("api_key", &APIKey{ID: apiKeyID})
	return c, rec
}

func v8CacheCompleted(usage string, responseID string) *http.Response {
	response := fmt.Sprintf(`{"id":%q,"object":"response","model":%q,"status":"completed","output":[{"id":"synthetic-message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"synthetic-ok"}]}],"usage":%s}`, responseID, v8CacheModel, usage)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"synthetic-start\",\"model\":\"" + v8CacheModel + "\",\"output\":[]}}\n\n"
	body += "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"synthetic-upstream-id"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func v8CacheHash(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }

func v8CacheMetadata(session string) map[string]any {
	userID, _ := json.Marshal(map[string]string{
		"device_id":    strings.Repeat("a", 64),
		"account_uuid": "",
		"session_id":   session,
	})
	return map[string]any{"user_id": string(userID)}
}

// round=2 在 round=1 的真实历史后追加 assistant(thinking+tool_use)+user(tool_result+text)。
func v8CacheBody(t *testing.T, round int, initial int, metadata bool, rollingAnchor bool, stream bool) []byte {
	t.Helper()
	messages := make([]any, 0, initial+2)
	for i := 0; i < initial; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": []any{
			map[string]any{"type": "text", "text": fmt.Sprintf("synthetic-history-%02d-", i) + strings.Repeat("stable-prefix ", 128)},
		}})
	}
	if round == 2 {
		assistantText := map[string]any{"type": "text", "text": "synthetic-assistant-tool-turn"}
		if rollingAnchor {
			assistantText["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "synthetic-reasoning", "signature": "synthetic-provider-ciphertext"},
			assistantText,
			map[string]any{"type": "tool_use", "id": "synthetic-call-1", "name": "synthetic_lookup", "input": map[string]any{"query": "synthetic"}},
		}})
		followUp := map[string]any{"type": "text", "text": "synthetic-follow-up"}
		if rollingAnchor {
			followUp["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		messages = append(messages, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "synthetic-call-1", "content": "synthetic-result"},
			followUp,
		}})
	}
	request := map[string]any{
		"model": v8CacheModel, "max_tokens": 128, "stream": stream,
		"system":        []any{map[string]any{"type": "text", "text": strings.Repeat("synthetic-stable-system ", 128), "cache_control": map[string]any{"type": "ephemeral"}}},
		"tools":         []any{map[string]any{"name": "synthetic_lookup", "description": "synthetic tool", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}}},
		"thinking":      map[string]any{"type": "enabled", "budget_tokens": 64},
		"output_config": map[string]any{"effort": "high"}, "messages": messages,
	}
	if metadata {
		request["metadata"] = v8CacheMetadata("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	return body
}

func v8CacheClientUsage(t *testing.T, rec *httptest.ResponseRecorder, stream bool) gjson.Result {
	t.Helper()
	if !stream {
		return gjson.GetBytes(rec.Body.Bytes(), "usage")
	}
	var usage gjson.Result
	starts, deltas := 0, 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		value := gjson.Parse(strings.TrimPrefix(line, "data: "))
		switch value.Get("type").String() {
		case "message_start":
			starts++
		case "message_delta":
			deltas++
			if value.Get("usage").Exists() {
				usage = value.Get("usage")
			}
		}
	}
	require.Equal(t, 1, starts, "真实转换器应只输出一个 message_start")
	require.Equal(t, 1, deltas, "真实转换器应只输出一个终态 message_delta")
	require.True(t, usage.Exists())
	return usage
}

func TestV8OpenAICacheUsageRealPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                            string
		usage                           string
		input, read, write, short, long int
	}{
		{"ordinary_uncached_is_not_write", `{"input_tokens":103,"output_tokens":5,"input_tokens_details":{"cached_tokens":100}}`, 3, 100, 0, 0, 0},
		{"canonical_mixed_ttl", `{"input_tokens":123,"output_tokens":5,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":20,"cache_write_tokens_details":{"ttl_30m_tokens":7,"ttl_12h_tokens":13}}}`, 3, 100, 20, 7, 13},
		{"single_variable_remove_ttl_details", `{"input_tokens":123,"output_tokens":5,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":20}}`, 3, 100, 20, 0, 0},
		{"minimal_vendor_read_only", `{"input_tokens":103,"output_tokens":5,"cache_read_input_tokens":100}`, 3, 100, 0, 0, 0},
		{"minimal_read_nested_control", `{"input_tokens":103,"output_tokens":5,"input_tokens_details":{"cached_tokens":100}}`, 3, 100, 0, 0, 0},
		{"nested_zero_beats_read_alias", `{"input_tokens":103,"output_tokens":5,"cache_read_input_tokens":100,"input_tokens_details":{"cached_tokens":0}}`, 103, 0, 0, 0, 0},
		{"canonical_zero_beats_vendor_alias", `{"input_tokens":103,"output_tokens":5,"cache_creation_input_tokens":90,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":0}}`, 3, 100, 0, 0, 0},
		{"chat_usage_shape", `{"prompt_tokens":123,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":20}}`, 3, 100, 20, 0, 0},
		{"vendor_creation_alias", `{"input_tokens":123,"output_tokens":5,"cache_creation_input_tokens":20,"input_tokens_details":{"cached_tokens":100}}`, 3, 100, 20, 0, 0},
		{"vendor_read_alias", `{"input_tokens":123,"output_tokens":5,"cache_creation_input_tokens":20,"cache_read_input_tokens":100}`, 3, 100, 20, 0, 0},
	} {
		for _, oauth := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/oauth=%t/stream=%t", tc.name, oauth, stream), func(t *testing.T) {
					usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
					userRepo := &openAIRecordUsageUserRepoStub{}
					svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
					// 确定合成价格卡：input=2, output=8, read=0.5, write30m=2.5, write12h=5（每百万）。
					const groupID int64 = 8801
					var pricing ChannelModelPricing
					require.NoError(t, json.Unmarshal([]byte(`{"platform":"openai","models":["gpt-6.1-sol"],"billing_mode":"token","input_price":0.000002,"output_price":0.000008,"cache_read_price":0.0000005,"cache_write_price":0.0000025,"cache_write_12h_price":0.000005}`), &pricing))
					channel := Channel{ID: 8801, Status: StatusActive, GroupIDs: []int64{groupID}, ModelPricing: []ChannelModelPricing{pricing}, ApplyPricingToAccountStats: true}
					svc.channelService = &ChannelService{}
					svc.channelService.cache.Store(populateChannelCache([]Channel{channel}, map[int64]string{groupID: PlatformOpenAI}))
					svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
					upstream := &httpUpstreamRecorder{resp: v8CacheCompleted(tc.usage, "synthetic-completed")}
					svc.httpUpstream = upstream
					account := v8CacheAccount(oauth)
					body := v8CacheBody(t, 1, 1, true, false, stream)
					c, rec := v8CacheContext(body, 8801, "synthetic-session")
					result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, svc.ExtractSessionID(c, body), "")
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.requests, 1)
					assert.Equal(t, "/v1/responses", GetActualOpenAIUpstreamEndpoint(c))
					client := v8CacheClientUsage(t, rec, stream)
					assert.Equal(t, tc.input, int(client.Get("input_tokens").Int()), "Anthropic普通输入")
					assert.Equal(t, tc.read, int(client.Get("cache_read_input_tokens").Int()), "Anthropic缓存读取")
					assert.Equal(t, tc.write, int(client.Get("cache_creation_input_tokens").Int()), "Anthropic缓存写入")
					assert.Equal(t, tc.input+tc.read+tc.write, result.Usage.InputTokens, "真实 result 输入总量")
					assert.Equal(t, tc.read, result.Usage.CacheReadInputTokens, "真实 result 缓存读取")
					assert.Equal(t, tc.write, result.Usage.CacheCreationInputTokens, "真实 result 缓存写入")
					assert.Equal(t, tc.short, result.Usage.CacheCreation30mInputTokens, "终态 copy 不应丢新 TTL")
					assert.Equal(t, tc.long, result.Usage.CacheCreation12hInputTokens, "终态 copy 不应丢新 TTL")
					err = svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
						Result: result, Account: account, User: &User{ID: 8801},
						APIKey:          &APIKey{ID: 8801, GroupID: i64p(groupID), Group: &Group{ID: groupID, Platform: PlatformOpenAI, RateMultiplier: 1}},
						InboundEndpoint: "/v1/messages", UpstreamEndpoint: "/v1/responses",
						ChannelUsageFields: ChannelUsageFields{OriginalModel: v8CacheModel, BillingModelSource: BillingModelSourceRequested},
					})
					require.NoError(t, err)
					require.Equal(t, 1, usageRepo.calls)
					log := usageRepo.lastLog
					require.NotNil(t, log)
					assert.Equal(t, tc.input, log.InputTokens)
					assert.Equal(t, tc.read, log.CacheReadTokens)
					assert.Equal(t, tc.write, log.CacheCreationTokens)
					assert.Equal(t, tc.short, log.CacheCreation30mTokens)
					assert.Equal(t, tc.long, log.CacheCreation12hTokens)
					assert.Zero(t, log.CacheCreation5mTokens, "OpenAI TTL 不可误标为 Anthropic5m")
					assert.Zero(t, log.CacheCreation1hTokens, "OpenAI TTL 不可误标为 Anthropic1h")
					writeCost := float64(tc.write-tc.long)*2.5e-6 + float64(tc.long)*5e-6
					wantCost := float64(tc.input)*2e-6 + float64(tc.read)*0.5e-6 + writeCost + 5*8e-6
					assert.InDelta(t, writeCost, log.CacheCreationCost, 1e-12)
					assert.InDelta(t, wantCost, log.TotalCost, 1e-12, "互斥桶不得重复扣")
					assert.InDelta(t, wantCost, log.ActualCost, 1e-12)
					assert.InDelta(t, wantCost, userRepo.lastAmount, 1e-12, "真实 RecordUsage debit stub")
					t.Logf("synthetic usage_hash=%s request_hash=%s total=%d client_input=%d result_read=%d result_write=%d ttl30=%d ttl12=%d log_input=%d log_read=%d log_write=%d", v8CacheHash([]byte(tc.usage)), v8CacheHash(upstream.lastBody), result.Usage.InputTokens, client.Get("input_tokens").Int(), result.Usage.CacheReadInputTokens, result.Usage.CacheCreationInputTokens, result.Usage.CacheCreation30mInputTokens, result.Usage.CacheCreation12hInputTokens, log.InputTokens, log.CacheReadTokens, log.CacheCreationTokens)
				})
			}
		}
	}
}

func v8CachePrefixEqual(t *testing.T, before, after []byte) bool {
	t.Helper()
	first := gjson.GetBytes(before, "input").Array()
	second := gjson.GetBytes(after, "input").Array()
	matches := len(first) <= len(second)
	matched := 0
	for i := 0; i < len(first) && i < len(second); i++ {
		if first[i].Raw != second[i].Raw {
			matches = false
			break
		}
		matched++
	}
	t.Logf("synthetic prefix first_items=%d second_items=%d equal_items=%d first_hash=%s second_hash=%s", len(first), len(second), matched, v8CacheHash([]byte(gjson.GetBytes(before, "input").Raw)), v8CacheHash([]byte(gjson.GetBytes(after, "input").Raw)))
	return matches
}

func TestV8OpenAICachePrefixRealPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                                        string
		initial                                     int
		metadata, rolling, cold, disabled, explicit bool
	}{
		{"metadata_stable_stateless_control", 1, true, false, false, true, false},
		{"metadata_stable_tool_append", 11, true, false, false, true, false},
		{"metadata_cold_local_state_long_history", 11, true, false, true, false, false},
		{"single_variable_cold_short_history", 1, true, false, true, false, false},
		{"single_variable_warm_continuation_long_history", 11, true, false, false, false, false},
		{"single_variable_cold_disabled_continuation", 11, true, false, true, true, false},
		{"no_metadata_rolling_anchor", 1, false, true, false, false, false},
		{"single_variable_no_metadata_no_rolling_anchor", 1, false, false, false, false, false},
		{"single_variable_stable_metadata_rolling_anchor", 1, true, true, false, false, false},
		{"explicit_session_rolling_anchor", 1, false, true, false, true, true},
	} {
		for _, oauth := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oauth=%t", tc.name, oauth), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{responses: []*http.Response{
					v8CacheCompleted(`{"input_tokens":3,"output_tokens":5}`, "synthetic-r1"),
					v8CacheCompleted(`{"input_tokens":3,"output_tokens":5}`, "synthetic-r2"),
				}}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				account := v8CacheAccount(oauth)
				for round := 1; round <= 2; round++ {
					if round == 2 && tc.cold {
						// 模拟新实例/本地 session 缓存丢失；不伪造 provider 的缓存状态。
						svc = &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
					}
					body := v8CacheBody(t, round, tc.initial, tc.metadata, tc.rolling, false)
					explicit := ""
					if tc.explicit {
						explicit = "synthetic-explicit-session"
					}
					c, _ := v8CacheContext(body, 8801, explicit)
					key := svc.ExtractSessionID(c, body)
					if tc.disabled && !oauth {
						var req apicompat.AnthropicRequest
						require.NoError(t, json.Unmarshal(body, &req))
						if key == "" {
							key = promptCacheKeyFromAnthropicMetadataSession(&req)
						}
						if key == "" {
							key = deriveAnthropicCacheControlPromptCacheKey(&req)
						}
						svc.disableOpenAICompatSessionContinuation(context.Background(), c, account, key)
					}
					result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, key, "")
					require.NoError(t, err)
					require.NotNil(t, result)
				}
				require.Len(t, upstream.requests, 2)
				first, second := upstream.bodies[0], upstream.bodies[1]
				assert.JSONEq(t, gjson.GetBytes(first, "tools").Raw, gjson.GetBytes(second, "tools").Raw, "tool schema/order不变")
				assert.Equal(t, gjson.GetBytes(first, "instructions").Raw, gjson.GetBytes(second, "instructions").Raw, "instructions不变")
				assert.Equal(t, gjson.GetBytes(first, "reasoning").Raw, gjson.GetBytes(second, "reasoning").Raw, "reasoning policy不变")
				assert.Equal(t, upstream.requests[0].Header.Get("session_id"), upstream.requests[1].Header.Get("session_id"), "同一会话 append 不应改变上游session")
				if !oauth {
					assert.Equal(t, gjson.GetBytes(first, "prompt_cache_key").String(), gjson.GetBytes(second, "prompt_cache_key").String(), "同一会话 append 不应改变 prompt_cache_key")
				}
				if previous := gjson.GetBytes(second, "previous_response_id"); previous.Exists() {
					// 有效 provider continuation 应只发新一轮，不要求完整 replay 前缀。
					assert.False(t, oauth)
					assert.Equal(t, "synthetic-r1", previous.String())
					assert.False(t, tc.cold)
					assert.True(t, gjson.GetBytes(second, `input.#(type=="function_call")`).Exists())
					assert.True(t, gjson.GetBytes(second, `input.#(type=="function_call_output")`).Exists())
				} else {
					assert.True(t, v8CachePrefixEqual(t, first, second), "无 previous_response_id 时，append-only历史应保留完整已渲染 input 前缀")
				}
				t.Logf("synthetic identity first_key_hash=%s second_key_hash=%s first_session_hash=%s second_session_hash=%s continuation=%t", v8CacheHash([]byte(gjson.GetBytes(first, "prompt_cache_key").String())), v8CacheHash([]byte(gjson.GetBytes(second, "prompt_cache_key").String())), v8CacheHash([]byte(upstream.requests[0].Header.Get("session_id"))), v8CacheHash([]byte(upstream.requests[1].Header.Get("session_id"))), gjson.GetBytes(second, "previous_response_id").Exists())
			})
		}
	}
}

func TestV8OpenAICacheIsolationRealPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		v8CacheCompleted(`{"input_tokens":3,"output_tokens":5}`, "synthetic-r1"),
		v8CacheCompleted(`{"input_tokens":3,"output_tokens":5}`, "synthetic-r2"),
		v8CacheCompleted(`{"input_tokens":3,"output_tokens":5}`, "synthetic-r3"),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := v8CacheAccount(false)
	for _, tc := range []struct {
		keyID   int64
		session string
	}{{8801, "synthetic-shared-session"}, {8802, "synthetic-shared-session"}, {8801, "synthetic-new-session"}} {
		body := v8CacheBody(t, 1, 1, true, false, false)
		c, _ := v8CacheContext(body, tc.keyID, tc.session)
		_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, svc.ExtractSessionID(c, body), "")
		require.NoError(t, err)
	}
	require.Len(t, upstream.requests, 3)
	assert.NotEqual(t, upstream.requests[0].Header.Get("session_id"), upstream.requests[1].Header.Get("session_id"), "跨APIKey上游session隔离")
	assert.NotEqual(t, upstream.requests[0].Header.Get("session_id"), upstream.requests[2].Header.Get("session_id"), "新session隔离")
	for _, b := range upstream.bodies {
		assert.False(t, gjson.GetBytes(b, "previous_response_id").Exists(), "跨APIKey/新session不得引用他人response")
	}
	// 新模型官方 prompt_cache_key 支持租户核算隔离；与 session header 两者分别验证。
	assert.NotEqual(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String(), gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String(), "共享上游凭据下客户端APIKey的body key也应隔离；失败不等于已证明数据泄漏")
}
