//go:build unit

package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type compositeV0222NativeUpstream struct {
	service.HTTPUpstream
	stream bool
	wire   []byte
}

func (u *compositeV0222NativeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var err error
	u.wire, err = io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	payload := `{"type":"message","id":"msg_alias","role":"assistant","model":"reported-native-model","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":3,"output_tokens":2}}`
	contentType := "application/json"
	if u.stream {
		contentType = "text/event-stream"
		payload = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_alias","role":"assistant","model":"reported-native-model","content":[],"usage":{"input_tokens":3}}}` + "\n\n" +
			"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}` + "\n\n" +
			"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
			"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
}

// Combines the real fallback middleware with the native gateway handler. A
// public alias must not overwrite observed/billing models or the attempt trace.
func TestCompositeFallbackV0222NativePublicAliasAndAttribution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			upstream := &compositeV0222NativeUpstream{stream: stream}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Security.URLAllowlist.Enabled = false
			gateway := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			account := &service.Account{ID: 812, Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "fixture-only", "api_protocol": service.APIProtocolAnthropic,
					"api_base_urls": map[string]any{service.APIProtocolAnthropic: "https://api.deepseek.com"}}}
			route := compositeFallbackRoute(service.CompositeRouteFallback{TargetPlatform: service.PlatformDeepseek, UpstreamModel: "deepseek-v4-pro"})
			var calls *[]compositeFallbackCall
			var result *service.OpenAIForwardResult
			var router *gin.Engine
			router, calls = newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if attempt == 0 {
					c.Request.URL.Path = "/attempt-private"
					c.Request.Header.Set("X-Attempt-Only", "primary")
					c.Header("X-Discarded-Attempt", "primary")
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "primary unavailable"}})
					return
				}
				require.Equal(t, "/v1/messages", c.Request.URL.Path)
				require.Empty(t, c.GetHeader("X-Attempt-Only"))
				var err error
				result, err = gateway.ForwardAsAnthropic(c.Request.Context(), c, account, []byte((*calls)[attempt].body), "", "")
				require.NoError(t, err)
			})
			rec := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-6.1-sol","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":`+strconv.FormatBool(stream)+`}`))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, request)
			require.Len(t, *calls, 2)
			require.NotNil(t, result)
			require.Equal(t, "deepseek-v4-pro", gjson.GetBytes(upstream.wire, "model").String())
			require.Equal(t, "deepseek-v4-pro", result.BillingModel)
			require.Equal(t, "deepseek-v4-pro", result.UpstreamModel)
			require.Equal(t, "reported-native-model", result.UpstreamResponseModel)
			require.Equal(t, "/v1/messages", result.UpstreamEndpoint)
			require.Equal(t, 3, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			require.NotSame(t, (*calls)[0].trace, (*calls)[1].trace)
			require.Empty(t, (*calls)[0].trace.FallbackChain())
			require.Equal(t, "gemini-3.8-flash-high (503) → deepseek/deepseek-v4-pro", (*calls)[1].trace.FallbackChain())
			require.Equal(t, int64(42), (*calls)[1].trace.RouteID)
			require.Empty(t, rec.Header().Get("X-Discarded-Attempt"))
			clientModel := gjson.GetBytes(rec.Body.Bytes(), "model").String()
			if stream {
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if data, ok := strings.CutPrefix(line, "data:"); ok && gjson.Get(data, "type").String() == "message_start" {
						clientModel = gjson.Get(data, "message.model").String()
					}
				}
			}
			require.Equal(t, "gpt-6.1-sol", clientModel, "native fallback must restore the public alias only on the client response")
		})
	}
}
