//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// #114995: OpenRouter 原生 Responses（store=false，Claude）回放历史时，input 中的
// reasoning item（encrypted_content=null、content=[{type:"reasoning_text"}]）
// 必须原样到达上游；OpenAI 端点专用的 replay 清理不得作用于该端点。
const openRouterReasoningReplayTestBody = `{"model":"anthropic/claude-sonnet-4.5","stream":false,"store":false,"instructions":"be brief","reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"id":"msg_or_1"},{"type":"reasoning","id":"rs_or_1","encrypted_content":null,"summary":[],"content":[{"type":"reasoning_text","text":"snapshot reasoning"}]},{"type":"function_call","id":"fc_or_1","call_id":"call_or_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","id":"fco_or_1","call_id":"call_or_1","output":"ok"},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}],"id":"msg_or_2"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`

func newOpenRouterReasoningReplayTestService() (*OpenAIGatewayService, *httpUpstreamRecorder) {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":3,"output_tokens":4}}`)),
	}}
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}, upstream
}

func newOpenRouterReasoningReplayTestAccount(baseURL string, extra map[string]any) *Account {
	credentials := map[string]any{"api_key": "sk-or-v1-test"}
	if strings.TrimSpace(baseURL) != "" {
		credentials["base_url"] = baseURL
	}
	merged := map[string]any{"use_responses_api": true, "openai_responses_supported": true}
	for key, value := range extra {
		merged[key] = value
	}
	return &Account{
		ID:          6100,
		Name:        "openrouter-claude-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: credentials,
		Extra:       merged,
		Status:      StatusActive,
		Schedulable: true,
	}
}

func newOpenRouterReasoningReplayTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(nil))
	c.Request.Header.Set("Content-Type", "application/json")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c
}

func requireOpenRouterClaudeReasoningReplayOnWire(t *testing.T, out []byte) {
	t.Helper()
	require.Equal(t, "max", gjson.GetBytes(out, "reasoning.effort").String(), string(out))
	require.False(t, gjson.GetBytes(out, "max_output_tokens").Exists(), string(out))

	input := gjson.GetBytes(out, "input")
	require.True(t, input.IsArray(), string(out))
	require.Len(t, input.Array(), 5, string(out))
	want := [][2]string{
		{"message", "msg_or_1"},
		{"reasoning", "rs_or_1"},
		{"function_call", "fc_or_1"},
		{"function_call_output", "fco_or_1"},
		{"message", "msg_or_2"},
	}
	for i, pair := range want {
		require.Equal(t, pair[0], gjson.GetBytes(out, fmt.Sprintf("input.%d.type", i)).String(), string(out))
		require.Equal(t, pair[1], gjson.GetBytes(out, fmt.Sprintf("input.%d.id", i)).String(), string(out))
	}
	require.Equal(t, "reasoning_text", gjson.GetBytes(out, "input.1.content.0.type").String(), string(out))
	require.Equal(t, "snapshot reasoning", gjson.GetBytes(out, "input.1.content.0.text").String(), string(out))
	encrypted := gjson.GetBytes(out, "input.1.encrypted_content")
	require.True(t, encrypted.Exists(), string(out))
	require.Equal(t, gjson.Null, encrypted.Type, string(out))
	summary := gjson.GetBytes(out, "input.1.summary")
	require.True(t, summary.IsArray(), string(out))
	require.Len(t, summary.Array(), 0, string(out))
	require.Len(t, gjson.GetBytes(out, "tools").Array(), 1, string(out))
	require.Equal(t, "lookup", gjson.GetBytes(out, "tools.0.name").String(), string(out))
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "cache_control.type").String(), string(out))
	require.Equal(t, "1h", gjson.GetBytes(out, "cache_control.ttl").String(), string(out))
}

func requireOpenRouterReasoningItemCleaned(t *testing.T, out []byte) {
	t.Helper()
	input := gjson.GetBytes(out, "input")
	require.True(t, input.IsArray(), string(out))
	input.ForEach(func(_, item gjson.Result) bool {
		require.NotEqual(t, "reasoning", item.Get("type").String(), string(out))
		return true
	})
}

func TestOpenRouterClaudeResponsesReasoningReplayPreservedOnForward(t *testing.T) {
	body := []byte(openRouterReasoningReplayTestBody)
	svc, upstream := newOpenRouterReasoningReplayTestService()
	account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", nil)

	result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
	require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
	requireOpenRouterClaudeReasoningReplayOnWire(t, upstream.lastBody)
}

func TestOpenRouterClaudeResponsesReasoningReplayPreservedOnPassthroughForward(t *testing.T) {
	body := []byte(openRouterReasoningReplayTestBody)
	svc, upstream := newOpenRouterReasoningReplayTestService()
	account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", map[string]any{"openai_passthrough": true})

	result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
	require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
	requireOpenRouterClaudeReasoningReplayOnWire(t, upstream.lastBody)
}

func TestOpenRouterClaudeResponsesReasoningReplayCleanupStillAppliesOutsideGate(t *testing.T) {
	t.Run("lookalike host keeps OpenAI cleanup", func(t *testing.T) {
		body := []byte(openRouterReasoningReplayTestBody)
		svc, upstream := newOpenRouterReasoningReplayTestService()
		account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai.evil/api", nil)

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "openrouter.ai.evil", upstream.lastReq.URL.Hostname())
		require.False(t, gjson.GetBytes(upstream.lastBody, "cache_control").Exists(), string(upstream.lastBody))
		requireOpenRouterReasoningItemCleaned(t, upstream.lastBody)
	})

	t.Run("openai direct keeps OpenAI cleanup", func(t *testing.T) {
		body := []byte(openRouterReasoningReplayTestBody)
		svc, upstream := newOpenRouterReasoningReplayTestService()
		account := newOpenRouterReasoningReplayTestAccount("", nil)

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "api.openai.com", upstream.lastReq.URL.Hostname())
		require.False(t, gjson.GetBytes(upstream.lastBody, "cache_control").Exists(), string(upstream.lastBody))
		requireOpenRouterReasoningItemCleaned(t, upstream.lastBody)
	})

	t.Run("raw chat completions fallback unchanged", func(t *testing.T) {
		body := []byte(openRouterReasoningReplayTestBody)
		cfg := &config.Config{}
		cfg.Security.URLAllowlist.Enabled = false
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"cc_or_1","object":"chat.completion","created":1,"model":"anthropic/claude-sonnet-4.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			)),
		}}
		svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
		account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", map[string]any{"openai_responses_supported": false})

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
		require.Equal(t, "/api/v1/chat/completions", upstream.lastReq.URL.Path)
		require.True(t, gjson.GetBytes(upstream.lastBody, "messages").IsArray(), string(upstream.lastBody))
	})
}

func TestNormalizeOpenAIResponsesCompatibilityBodyWithReasoningReplay(t *testing.T) {
	account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", nil)
	body, err := sjson.SetBytes([]byte(openRouterReasoningReplayTestBody), "input.0.id", "not-a-msg-id")
	require.NoError(t, err)

	t.Run("preserve reasoning replay keeps content and other handling", func(t *testing.T) {
		out, _, normalizeErr := normalizeOpenAIResponsesCompatibilityBodyWithReasoningReplay(body, account, false, true)
		require.NoError(t, normalizeErr)
		require.Len(t, gjson.GetBytes(out, "input").Array(), 5, string(out))
		require.Equal(t, "reasoning", gjson.GetBytes(out, "input.1.type").String(), string(out))
		require.Equal(t, "snapshot reasoning", gjson.GetBytes(out, "input.1.content.0.text").String(), string(out))
		// Invalid replayed IDs are still sanitized.
		require.False(t, gjson.GetBytes(out, "input.0.id").Exists(), string(out))
	})

	t.Run("legacy wrapper still removes reasoning replay", func(t *testing.T) {
		out, changed, normalizeErr := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, account, false)
		require.NoError(t, normalizeErr)
		require.True(t, changed, string(out))
		requireOpenRouterReasoningItemCleaned(t, out)
		require.False(t, gjson.GetBytes(out, "input.0.id").Exists(), string(out))
	})
}

func TestOpenRouterClaudeResponsesReasoningReplayMappedModelGate(t *testing.T) {
	t.Run("alias mapped to claude keeps reasoning replay", func(t *testing.T) {
		body, err := sjson.SetBytes([]byte(openRouterReasoningReplayTestBody), "model", "or-claude-alias")
		require.NoError(t, err)
		svc, upstream := newOpenRouterReasoningReplayTestService()
		account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", nil)
		account.Credentials["model_mapping"] = map[string]any{"or-claude-alias": "anthropic/claude-sonnet-4.5"}

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
		require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
		require.Equal(t, "anthropic/claude-sonnet-4.5", gjson.GetBytes(upstream.lastBody, "model").String(), string(upstream.lastBody))
		requireOpenRouterClaudeReasoningReplayOnWire(t, upstream.lastBody)
	})

	t.Run("claude alias mapped to non claude keeps OpenAI cleanup", func(t *testing.T) {
		body, err := sjson.SetBytes([]byte(openRouterReasoningReplayTestBody), "model", "or-claude-alias-2")
		require.NoError(t, err)
		svc, upstream := newOpenRouterReasoningReplayTestService()
		account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", nil)
		account.Credentials["model_mapping"] = map[string]any{"or-claude-alias-2": "openai/gpt-5.6"}

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
		require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
		require.Equal(t, "openai/gpt-5.6", gjson.GetBytes(upstream.lastBody, "model").String(), string(upstream.lastBody))
		require.False(t, gjson.GetBytes(upstream.lastBody, "cache_control").Exists(), string(upstream.lastBody))
		requireOpenRouterReasoningItemCleaned(t, upstream.lastBody)
	})
}

// 透传账号只替换认证：线上实际发送的是请求 model，账号 model_mapping 不参与
// 出站改写。门控必须以该真实线上模型判断是否 Claude，而不是账号映射结果。
func TestOpenRouterClaudeResponsesReasoningReplayPassthroughIgnoresAccountMapping(t *testing.T) {
	t.Run("claude request mapped to gpt still keeps replay", func(t *testing.T) {
		body := []byte(openRouterReasoningReplayTestBody)
		svc, upstream := newOpenRouterReasoningReplayTestService()
		account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", map[string]any{"openai_passthrough": true})
		account.Credentials["model_mapping"] = map[string]any{"anthropic/claude-sonnet-4.5": "openai/gpt-5.6"}

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
		require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
		require.Equal(t, "anthropic/claude-sonnet-4.5", gjson.GetBytes(upstream.lastBody, "model").String(), string(upstream.lastBody))
		requireOpenRouterClaudeReasoningReplayOnWire(t, upstream.lastBody)
	})

	t.Run("gpt request mapped to claude keeps OpenAI cleanup", func(t *testing.T) {
		body, err := sjson.SetBytes([]byte(openRouterReasoningReplayTestBody), "model", "openai/gpt-5.6")
		require.NoError(t, err)
		svc, upstream := newOpenRouterReasoningReplayTestService()
		account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", map[string]any{"openai_passthrough": true})
		account.Credentials["model_mapping"] = map[string]any{"openai/gpt-5.6": "anthropic/claude-sonnet-4.5"}

		result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "openrouter.ai", upstream.lastReq.URL.Hostname())
		require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
		require.Equal(t, "openai/gpt-5.6", gjson.GetBytes(upstream.lastBody, "model").String(), string(upstream.lastBody))
		require.False(t, gjson.GetBytes(upstream.lastBody, "cache_control").Exists(), string(upstream.lastBody))
		requireOpenRouterReasoningItemCleaned(t, upstream.lastBody)
	})
}
