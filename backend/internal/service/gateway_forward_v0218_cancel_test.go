//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type gatewayForwardV0218ScanBody struct {
	*strings.Reader
	scanComplete chan struct{}
	once         sync.Once
}

func (b *gatewayForwardV0218ScanBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.once.Do(func() { close(b.scanComplete) })
	}
	return n, err
}

func (b *gatewayForwardV0218ScanBody) Close() error { return nil }

type gatewayForwardV0218CancelWriter struct {
	*httptest.ResponseRecorder
	ctx               context.Context
	afterWrite        func([]byte)
	afterFlush        func()
	writes            int
	flushes           int
	writesAfterCancel int
	flushAfterCancel  int
}

func (w *gatewayForwardV0218CancelWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.ctx.Err() != nil {
		w.writesAfterCancel++
	}
	n, err := w.ResponseRecorder.Write(p)
	if err == nil && w.afterWrite != nil {
		w.afterWrite(p)
	}
	return n, err
}

func (w *gatewayForwardV0218CancelWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *gatewayForwardV0218CancelWriter) Flush() {
	w.flushes++
	if w.ctx.Err() != nil {
		w.flushAfterCancel++
	}
	w.ResponseRecorder.Flush()
	if w.afterFlush != nil {
		w.afterFlush()
	}
}

func TestAnthropicCompatV0218_BufferedErrorAfterContextCancelPreservesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const fixture = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_v0218_cancel","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":17,"output_tokens":3}}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"fixture overload"}}` + "\n\n"
	for name, handle := range compatStreamHandlers() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &gatewayForwardV0218ScanBody{Reader: strings.NewReader(fixture), scanComplete: make(chan struct{})}
			writer := &gatewayForwardV0218CancelWriter{ResponseRecorder: httptest.NewRecorder(), ctx: ctx}
			writer.afterFlush = func() {
				// Six SSE lines fit in the pump's 16-line buffer. EOF proves
				// the complete error frame was enqueued before cancellation.
				<-body.scanComplete
				cancel()
			}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)

			result, err := handle(&GatewayService{}, &http.Response{Header: http.Header{}, Body: body}, c)

			require.ErrorIs(t, ctx.Err(), context.Canceled)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 17, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.True(t, result.ClientDisconnect)
			require.Zero(t, writer.writesAfterCancel)
			require.Zero(t, writer.flushAfterCancel)
			require.NotContains(t, writer.Body.String(), compatStreamFailureMarkers[name])
			require.NotContains(t, writer.Body.String(), compatStreamTerminalMarkers[name])
		})
	}
}

func TestHandleCCStreamingFromAnthropic_V0218CancelAfterFinalUsageSuppressesDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &gatewayForwardV0218CancelWriter{ResponseRecorder: httptest.NewRecorder(), ctx: ctx}
	usageWritten := false
	writer.afterWrite = func(p []byte) {
		for _, line := range strings.Split(string(p), "\n") {
			payload, ok := strings.CutPrefix(line, "data: ")
			if ok && gjson.Get(payload, "usage").IsObject() {
				usageWritten = true
				cancel()
			}
		}
	}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)
	resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(gatewayForwardDrainStart + gatewayForwardDrainDelta))}

	result, err := (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "public-model", "claude-sonnet-4.5", nil, time.Now())

	require.NoError(t, err)
	require.True(t, usageWritten, "the successful final usage write must trigger cancellation")
	require.NotNil(t, result)
	require.Equal(t, 27, result.Usage.OutputTokens)
	require.True(t, result.ClientDisconnect)
	require.Zero(t, writer.writesAfterCancel)
	require.Zero(t, writer.flushAfterCancel)
	require.NotContains(t, writer.Body.String(), "data: [DONE]")
}

func TestAnthropicCompatV0218_FailureFrameChecksLiveCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	failureWriters := map[string]func(*gin.Context, *anthropicCompatStreamFailure){
		"chat_completions": writeAnthropicCompatCCStreamFailure,
		"responses": func(c *gin.Context, failure *anthropicCompatStreamFailure) {
			state := apicompat.NewAnthropicEventToResponsesState()
			restorer := apicompat.NewResponsesClientToolStreamRestorer(apicompat.ResponsesClientToolMapping{})
			writeAnthropicCompatResponsesStreamFailure(c, state, restorer, failure)
		},
	}
	for name, writeFailure := range failureWriters {
		for _, timing := range []string{"before_write", "during_write"} {
			t.Run(name+"/"+timing, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				writer := &gatewayForwardV0218CancelWriter{ResponseRecorder: httptest.NewRecorder(), ctx: ctx}
				if timing == "before_write" {
					cancel()
				} else {
					writer.afterWrite = func([]byte) { cancel() }
				}
				c, _ := gin.CreateTestContext(writer)
				c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)
				failure := &anthropicCompatStreamFailure{err: io.ErrUnexpectedEOF, errType: "stream_read_error", message: "fixture read error"}
				// The result snapshot predates cancellation. The writer must
				// still consult the live request context at both boundaries.
				snapshot := &ForwardResult{Usage: ClaudeUsage{InputTokens: 17}}

				result, err := finishAnthropicCompatStreamFailure(c, c.Writer.Size(), failure, snapshot, func() {
					writeFailure(c, failure)
				})

				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				require.Same(t, snapshot, result)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.Zero(t, writer.writesAfterCancel)
				require.Zero(t, writer.flushAfterCancel)
				require.Zero(t, writer.flushes)
				if timing == "before_write" {
					require.Zero(t, writer.writes)
					require.Empty(t, writer.Body.String())
				} else {
					require.Equal(t, 1, writer.writes)
					require.Contains(t, writer.Body.String(), compatStreamFailureMarkers[name])
				}
			})
		}
	}
}
