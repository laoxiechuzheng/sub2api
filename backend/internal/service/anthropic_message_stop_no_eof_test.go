package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const anthropicMessageStopWithoutEOF = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stop\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5-5\",\"stop_reason\":\"\",\"usage\":{\"input_tokens\":10}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

func runAnthropicStreamWithoutEOF(t *testing.T, payload string, run func(*http.Response, *gin.Context) error) *httptest.ResponseRecorder {
	t.Helper()

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_message_stop"}},
		Body:       reader,
	}

	done := make(chan error, 1)
	go func() {
		done <- run(resp, c)
	}()

	_, err := io.WriteString(writer, payload)
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("message_stop must complete without waiting for upstream EOF")
	}

	return recorder
}

func TestGatewayService_ResponsesMessageStopCompletesWithoutEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, buffered := range []bool{false, true} {
		name := "streaming"
		if buffered {
			name = "buffered"
		}
		t.Run(name, func(t *testing.T) {
			svc := &GatewayService{}
			recorder := runAnthropicStreamWithoutEOF(t, anthropicMessageStopWithoutEOF, func(resp *http.Response, c *gin.Context) error {
				if buffered {
					_, err := svc.handleResponsesBufferedStreamingResponse(resp, c, "claude-opus-5-5", "claude-opus-5-5", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
					return err
				}
				_, err := svc.handleResponsesStreamingResponse(resp, c, "claude-opus-5-5", "claude-opus-5-5", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				return err
			})
			require.NotEmpty(t, recorder.Body.String())
		})
	}
}

func TestGatewayService_AnthropicPassthroughMessageStopCompletesWithoutEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := &GatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
	}
	account := &Account{ID: 1, Name: "anthropic-message-stop-test"}
	recorder := runAnthropicStreamWithoutEOF(t, anthropicMessageStopWithoutEOF, func(resp *http.Response, c *gin.Context) error {
		_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, account, time.Now(), "claude-opus-5-5")
		return err
	})
	require.Contains(t, recorder.Body.String(), `data: {"type":"message_stop"}`)
}
