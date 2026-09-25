//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 生产上游返回的结构性 400：assistant 消息末块不能是 thinking。
const assistantThinkingTailErrorMessage = "messages.77: The final block in an assistant message cannot be `thinking`."

func assistantThinkingTailErrorBody() []byte {
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": assistantThinkingTailErrorMessage,
		},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func TestIsThinkingBlockSignatureError_AssistantThinkingTail(t *testing.T) {
	t.Parallel()

	svc := &GatewayService{}
	require.True(t, svc.isAssistantThinkingTailError(assistantThinkingTailErrorBody()))
	require.True(t, svc.isThinkingBlockSignatureError(assistantThinkingTailErrorBody()))

	redacted := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.57: The final block in an assistant message cannot be ` + "`redacted_thinking`" + `."}}`)
	require.True(t, svc.isAssistantThinkingTailError(redacted))

	// 结构错误整流只对 anthropic-strict 协议族启用，第三方兼容上游保持透传。
	require.True(t, svc.shouldRectifyThinkingTailError(assistantThinkingTailErrorBody(), "claude-opus-5-5"))
	require.False(t, svc.shouldRectifyThinkingTailError(assistantThinkingTailErrorBody(), "deepseek-v4-pro"))

	require.False(t, svc.isAssistantThinkingTailError([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`)))
}

// 上游 Anthropic 兼容账号按顺序返回固定响应的测试桩，并记录每次出站 body。
type thinkingRectifierUpstreamStub struct {
	mu        sync.Mutex
	bodies    [][]byte
	responses []*http.Response
	calls     int
}

func (u *thinkingRectifierUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *thinkingRectifierUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	var body []byte
	if req != nil && req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies = append(u.bodies, body)
	if u.calls >= len(u.responses) {
		return nil, fmt.Errorf("unexpected upstream call %d", u.calls+1)
	}
	resp := u.responses[u.calls]
	u.calls++
	return resp, nil
}

func thinkingTailAnthropicTextStream() string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_tail","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","stop_reason":"","usage":{"input_tokens":5}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
}

// 转换路径（Responses→Anthropic）此前没有 /v1/messages 主路径上的 thinking 整流链：
// 上游因历史 thinking 结构返回 400 时整轮直接失败。这里验证命中结构错误后会清洗并重发
// 一次，且客户端拿到的是重试后的正常响应。
func TestForwardAsResponsesRetriesAfterAssistantThinkingTail400(t *testing.T) {
	gin.SetMode(gin.TestMode)

	thinkingBlock := map[string]any{
		"type":      "thinking",
		"thinking":  "truncated reasoning",
		"signature": "sig-thinking",
	}
	payload, err := json.Marshal(thinkingBlock)
	require.NoError(t, err)
	envelope := "anthropic-thinking-v1:" + base64.RawStdEncoding.EncodeToString(payload)

	body := fmt.Sprintf(`{
		"model":"claude-opus-5-5",
		"stream":false,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
			{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":%q},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`, envelope)

	upstream := &thinkingRectifierUpstreamStub{responses: []*http.Response{
		{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"x-request-id": []string{"rid_tail_400"}},
			Body:       io.NopCloser(bytes.NewReader(assistantThinkingTailErrorBody())),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"x-request-id": []string{"rid_tail_retry"}},
			Body:       io.NopCloser(strings.NewReader(thinkingTailAnthropicTextStream())),
		},
	}}

	account := &Account{
		ID:          77,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "fixture-key"},
	}
	// settingService 故意留空：结构错误整流不依赖面板签名整流开关，不应因此 panic 或跳过。
	svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, []byte(body), nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, upstream.bodies, 2, "结构错误应触发一次整流重试")
	require.Contains(t, string(upstream.bodies[0]), `"type":"thinking"`)
	require.NotContains(t, string(upstream.bodies[1]), `"type":"thinking"`)
	require.Contains(t, rec.Body.String(), "ok")
}
