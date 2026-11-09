//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAnthropicCompatStatusOnlyEntryPoints(t *testing.T) {
	gateway := &GatewayService{cfg: &config.Config{}}
	native := newNativeAnthropicHangTestService(1)
	forwardUsage := func(result *ForwardResult, err error) (OpenAIUsage, error) {
		if result == nil {
			return OpenAIUsage{}, err
		}
		return claudeUsageToOpenAIUsage(&result.Usage), err
	}
	nativeUsage := func(result *OpenAIForwardResult, err error) (OpenAIUsage, error) {
		if result == nil {
			return OpenAIUsage{}, err
		}
		return result.Usage, err
	}
	handlers := map[string]func(*http.Response, *gin.Context) (OpenAIUsage, error){
		"gateway_chat_buffered": func(resp *http.Response, c *gin.Context) (OpenAIUsage, error) {
			return forwardUsage(gateway.handleCCBufferedFromAnthropic(resp, c, "model", "model", nil, time.Now()))
		},
		"gateway_responses_buffered": func(resp *http.Response, c *gin.Context) (OpenAIUsage, error) {
			return forwardUsage(gateway.handleResponsesBufferedStreamingResponse(resp, c, "model", "model", nil, time.Now(), apicompat.ResponsesClientToolMapping{}))
		},
		"native_chat_buffered": func(resp *http.Response, c *gin.Context) (OpenAIUsage, error) {
			return nativeUsage(native.handleCCBufferedFromNativeAnthropic(resp, c, "model", "model", "model", nil, time.Now()))
		},
		"native_chat_streaming": func(resp *http.Response, c *gin.Context) (OpenAIUsage, error) {
			return nativeUsage(native.handleCCStreamingFromNativeAnthropic(resp, c, "model", "model", "model", nil, time.Now()))
		},
		"native_responses_buffered": func(resp *http.Response, c *gin.Context) (OpenAIUsage, error) {
			return nativeUsage(native.handleResponsesBufferedFromNativeAnthropic(resp, c, "model", "model", "model", nil, time.Now(), apicompat.ResponsesClientToolMapping{}))
		},
		"native_responses_streaming": func(resp *http.Response, c *gin.Context) (OpenAIUsage, error) {
			return nativeUsage(native.handleResponsesStreamingFromNativeAnthropic(resp, c, "model", "model", "model", nil, time.Now(), apicompat.ResponsesClientToolMapping{}))
		},
	}
	complete := miniAnthropicSSEStream()
	unfinished := strings.ReplaceAll(complete, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "")
	unfinished = strings.ReplaceAll(unfinished, `"stop_reason":"end_turn"`, `"stop_reason":null`)
	for name, handle := range handlers {
		for _, ending := range []string{"stop", "error", "missing_terminal"} {
			t.Run(name+"/"+ending, func(t *testing.T) {
				payload := unfinished
				if ending == "stop" {
					payload = complete
				} else if ending == "error" {
					payload += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"fixture\"}}\n\n"
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil)
				resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}
				defer resp.Body.Close()
				var usage OpenAIUsage
				var err error
				require.NotPanics(t, func() { usage, err = handle(resp, c) })
				if ending == "stop" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.NotContains(t, recorder.Body.String(), `"type":"response.completed"`)
					require.NotContains(t, recorder.Body.String(), "data: [DONE]")
				}
				require.Equal(t, 10, usage.InputTokens)
				require.Equal(t, 5, usage.OutputTokens)
			})
		}
	}
}
