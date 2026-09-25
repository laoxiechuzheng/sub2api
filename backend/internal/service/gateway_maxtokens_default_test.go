//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 面板出口只要客户端没带 max tokens，就统一补 81920：8192 会被 thinking 的
// reasoning 预算吃光，客户端只看到 max_output_tokens 截断。这里走真实的
// /v1/chat/completions 转发路径，覆盖非 Claude 上游与显式值优先两种情况。
func TestForwardAsChatCompletionsDefaultsMaxTokensForAllModels(t *testing.T) {
	cases := []struct {
		name   string
		alias  string
		mapped string
		body   string
		want   int64
	}{
		{name: "claude alias default", alias: "public-opus", mapped: "claude-opus-5-5", body: `{"model":"public-opus","messages":[{"role":"user","content":"hi"}]}`, want: 81920},
		{name: "deepseek default", alias: "public-fast", mapped: "deepseek-v4-flash", body: `{"model":"public-fast","messages":[{"role":"user","content":"hi"}]}`, want: 81920},
		{name: "glm default", alias: "public-glm", mapped: "glm-5.3", body: `{"model":"public-glm","messages":[{"role":"user","content":"hi"}]}`, want: 81920},
		{name: "explicit limit wins", alias: "public-fast", mapped: "deepseek-v4-flash", body: `{"model":"public-fast","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`, want: 4096},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))

			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(namespaceToolAnthropicStream())),
			}}
			account := &Account{
				ID:          1,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "fixture-key", "model_mapping": map[string]any{tc.alias: tc.mapped}},
			}
			svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream}

			_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(tc.body), nil)
			require.NoError(t, err)
			require.Equal(t, tc.mapped, gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, tc.want, gjson.GetBytes(upstream.lastBody, "max_tokens").Int())
		})
	}
}
