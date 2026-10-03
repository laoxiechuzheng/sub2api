//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func nativeGuardChatResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chat-fixture","object":"chat.completion","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"fixture summary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		)),
	}
}

func nativeGuardChatStreamResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"id\":\"chat_guard\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture summary\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"chat_guard\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n",
		)),
	}
}

func TestResponsesChatFallbackRejectsNativeCompactionBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek, PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, direct := range []bool{false, true} {
			entry := "forward"
			if direct {
				entry = "direct_fallback"
			}
			for _, tt := range []struct {
				name, path, body string
				marked           bool
			}{
				{"legacy compact", "/v1/responses/compact", `{"model":"deepseek-v4.1-flash","input":"fixture","stream":false}`, false},
				{"legacy compact subpath", "/v1/responses/compact/detail", `{"model":"deepseek-v4.1-flash","input":"fixture"}`, false},
				{"native v2", "/v1/responses", `{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`, false},
				{"nonstream trigger", "/v1/responses", `{"model":"deepseek-v4.1-flash","stream":false,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`, false},
				{"trigger without stream", "/v1/responses", `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`, false},
				{"trigger with null stream", "/v1/responses", `{"model":"deepseek-v4.1-flash","stream":null,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`, false},
				{"native alias", "/backend-api/codex/responses", `{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`, false},
				{"request marker survives body normalization", "/v1/responses", `{"model":"deepseek-v4.1-flash","input":"fixture"}`, true},
			} {
				t.Run(platform+"/"+entry+"/"+tt.name, func(t *testing.T) {
					body := []byte(tt.body)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/json")
					if tt.marked {
						MarkOpenAINativeCompactionV2(c)
					}
					account := forceChatResponsesFallbackAccount()
					account.Platform = platform
					account.Credentials["api_protocol"] = APIProtocolChatCompletions
					resp := nativeGuardChatResponse()
					if gjson.GetBytes(body, "stream").Bool() {
						resp = nativeGuardChatStreamResponse()
					}
					upstream := &httpUpstreamRecorder{resp: resp}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
					var result *OpenAIForwardResult
					var err error
					if direct {
						result, err = svc.forwardResponsesViaRawChatCompletions(context.Background(), c, account, body)
					} else {
						result, err = svc.Forward(context.Background(), c, account, body)
					}
					require.Empty(t, upstream.requests, "native compaction must not be sent as an ordinary CC request")
					require.ErrorContains(t, err, "compaction")
					require.Nil(t, result)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "compact_not_supported", gjson.Get(recorder.Body.String(), "error.type").String())
				})
			}
		}
	}
}

func TestResponsesChatFallbackKeepsTextSummaryAndOrdinaryRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, text string
	}{
		{"ordinary", "fixture"},
		{"text summary", "Create a summary of the conversation so far."},
		{"quoted trigger", "The fixture mentions compaction_trigger as text."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"deepseek-v4.1-flash","stream":false,"input":"` + tt.text + `"}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("x-codex-beta-features", "remote_compaction_v2")
			account := forceChatResponsesFallbackAccount()
			account.Platform = PlatformDeepseek
			account.Credentials["api_protocol"] = APIProtocolChatCompletions
			upstream := &httpUpstreamRecorder{resp: nativeGuardChatResponse()}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

			result, err := svc.Forward(context.Background(), c, account, body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
			require.Equal(t, tt.text, gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
			require.Equal(t, http.StatusOK, recorder.Code)
		})
	}
}

func TestResponsesNativeGuardKeepsClaudeTextSummaryOnDeepSeekCC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"claude-summary-fixture","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"Create a summary of the conversation so far."}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	account := forceChatMessagesFallbackAccount()
	account.Platform = PlatformDeepseek
	account.Credentials["api_protocol"] = APIProtocolChatCompletions
	account.Credentials["model_mapping"] = map[string]any{"claude-summary-fixture": "deepseek-v4.1-flash"}
	upstream := &httpUpstreamRecorder{resp: nativeGuardChatResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
	require.Equal(t, "deepseek-v4.1-flash", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "Create a summary of the conversation so far.", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "fixture summary", gjson.Get(recorder.Body.String(), "content.0.text").String())
}
