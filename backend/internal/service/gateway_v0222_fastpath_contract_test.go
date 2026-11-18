//go:build unit

package service

import (
	"context"
	"errors"
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

// Deliberately generic: Claude CLI's noop-delta mode takes a different parse
// branch and would hide the named-event regressions introduced by v0.2.22.
func v0222FastPathContext(writer http.ResponseWriter) *gin.Context {
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "v0222-contract-client/1.0")
	return c
}

func v0222FastPathEvent(named bool, kind, payload string) string {
	if named {
		return "event: " + kind + "\ndata: " + payload + "\n\n"
	}
	return "data: " + payload + "\n\n"
}

const v0222FastPathText = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`
const v0222FastPathStart = `{"type":"message_start","message":{"model":"upstream-model","usage":{"input_tokens":12,"cache_read_input_tokens":9,"cache_creation_input_tokens":3}}}`
const v0222FastPathUsage = `{"type":"message_delta","usage":{"output_tokens":37,"cache_read_input_tokens":15,"cache_creation_input_tokens":6}}`
const v0222FastPathStop = `{"type":"message_stop"}`

func v0222RunFastPath(t *testing.T, body io.ReadCloser, c *gin.Context) (*streamingResult, error) {
	t.Helper()
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: &RateLimitService{},
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"v0222-attempt"}}, Body: body}
	t.Cleanup(func() { _ = body.Close() })
	type outcome struct {
		result *streamingResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "public-model", "upstream-model", false)
		done <- outcome{result, err}
	}()
	select {
	case got := <-done:
		return got.result, got.err
	case <-time.After(1500 * time.Millisecond):
		_ = body.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("handler did not join its reader after upstream body close")
		}
		t.Fatal("SSE handler exceeded fixture deadline; upstream EOF must not be needed")
		return nil, nil
	}
}

func TestV0222SSEFastPathVisibleOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type event struct{ kind, payload string }
	tool := event{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"clock","input":{}}}`}
	toolStop := event{"content_block_stop", `{"type":"content_block_stop","index":1}`}
	cases := []struct {
		name    string
		events  []event
		visible bool
		want    string
	}{
		{"text", []event{{"content_block_delta", v0222FastPathText}}, true, "answer"},
		{"tool_input", []event{tool, {"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"zone\":\"UTC\"}"}}`}}, true, "input_json_delta"},
		{"zero_arg_complete_tool", []event{tool, toolStop}, true, "clock"},
		{"incomplete_tool", []event{tool}, false, ""},
		{"thinking_only", []event{
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`},
		}, false, ""},
		{"server_tool_not_client_tool", []event{
			tool, // An open client tool at a different index must not lend its type.
			{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"server_tool_use","id":"srvtoolu_2","name":"web_search","input":{"query":"weather"}}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"weather\"}"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"server status"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":2}`},
		}, false, ""},
	}
	for _, named := range []bool{true, false} {
		format := "data_only"
		if named {
			format = "event_and_data"
		}
		for _, tc := range cases {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				payload := v0222FastPathEvent(named, "message_start", v0222FastPathStart)
				for _, e := range tc.events {
					payload += v0222FastPathEvent(named, e.kind, e.payload)
				}
				// A data-only terminal isolates S1 from S2's early tail exit.
				payload += v0222FastPathEvent(false, "message_delta", v0222FastPathUsage) + v0222FastPathEvent(false, "message_stop", v0222FastPathStop)
				rec := httptest.NewRecorder()
				result, err := v0222RunFastPath(t, io.NopCloser(strings.NewReader(payload)), v0222FastPathContext(rec))
				if !tc.visible {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result, "an uncommitted failed attempt must not be billed twice after failover")
					require.True(t, failover.SafeToFailoverAfterWrite)
					require.Contains(t, string(failover.ResponseBody), "empty_visible_output")
					require.Empty(t, rec.Body.String())
					require.Empty(t, rec.Header().Get("X-Request-Id"))
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, result.firstTokenMs)
				require.Equal(t, 37, result.usage.OutputTokens)
				require.Equal(t, 12, result.usage.InputTokens)
				require.Contains(t, rec.Body.String(), tc.want)
				require.Contains(t, rec.Body.String(), "public-model", "release staged metadata when visible output arrives")
				require.Equal(t, "v0222-attempt", rec.Header().Get("X-Request-Id"))
			})
		}
	}
}

func TestV0222SSEFastPathFirstVisibleBoundary(t *testing.T) {
	// Sequential large fixtures keep memory bounded on the 16 GiB build host.
	for _, named := range []bool{true, false} {
		format := "data_only"
		if named {
			format = "event_and_data"
		}
		for _, delta := range []int{0, 1} {
			name := "at_8MiB"
			if delta != 0 {
				name = "over_8MiB"
			}
			t.Run(format+"/"+name, func(t *testing.T) {
				const prefix = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`
				const suffix = `"}}`
				emptyEvent := v0222FastPathEvent(named, "content_block_delta", prefix+suffix)
				// The pending-line budget counts each line's newline, but not the
				// blank separator that dispatches the complete event.
				text := strings.Repeat("x", anthropicFirstVisibleOutputMaxBytes+1-len(emptyEvent)+delta)
				payload := v0222FastPathEvent(named, "content_block_delta", prefix+text+suffix)
				require.Equal(t, anthropicFirstVisibleOutputMaxBytes+delta, len(payload)-1)
				payload += v0222FastPathEvent(false, "message_stop", v0222FastPathStop)
				rec := httptest.NewRecorder()
				result, err := v0222RunFastPath(t, io.NopCloser(strings.NewReader(payload)), v0222FastPathContext(rec))
				if delta == 0 {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Contains(t, rec.Body.String(), text)
				} else {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Contains(t, string(failover.ResponseBody), "first_visible_output_buffer_overflow")
					require.Nil(t, result)
					require.Empty(t, rec.Body.String())
				}
			})
		}
		t.Run(format+"/visible_then_9MiB_line", func(t *testing.T) {
			large := strings.Repeat("z", 9*1024*1024)
			payload := v0222FastPathEvent(named, "content_block_delta", v0222FastPathText) +
				v0222FastPathEvent(named, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+large+`"}}`) +
				v0222FastPathEvent(false, "message_delta", v0222FastPathUsage) + v0222FastPathEvent(false, "message_stop", v0222FastPathStop)
			rec := httptest.NewRecorder()
			result, err := v0222RunFastPath(t, io.NopCloser(strings.NewReader(payload)), v0222FastPathContext(rec))
			require.NoError(t, err, "the scanner must release the first-visible guard before scanning the next event")
			require.Equal(t, 37, result.usage.OutputTokens)
			require.Contains(t, rec.Body.String(), large)
		})
		t.Run(format+"/unterminated_line_over_8MiB_without_EOF", func(t *testing.T) {
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
			producerDone := make(chan struct{})
			go func() {
				defer close(producerDone)
				prefix := ""
				if named {
					prefix = "event: content_block_delta\n"
				}
				_, _ = io.WriteString(pw, prefix+`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"`+strings.Repeat("x", anthropicFirstVisibleOutputMaxBytes+64*1024))
				// Do not close: the scanner must reject the still-incomplete line.
			}()
			rec := httptest.NewRecorder()
			result, err := v0222RunFastPath(t, pr, v0222FastPathContext(rec))
			v0222FastPathProducerStopped(t, producerDone)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Contains(t, string(failover.ResponseBody), "first_visible_output_buffer_overflow")
			require.Nil(t, result)
			require.Empty(t, rec.Body.String())
		})
	}
}

func v0222FastPathProducerStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("upstream producer did not stop after the reader was closed")
	}
}

func TestV0222SSEFastPathTerminalFixedTail(t *testing.T) {
	for _, tc := range []struct{ name, terminal string }{
		{"named_message_stop", v0222FastPathEvent(true, "message_stop", v0222FastPathStop)},
		{"data_only_message_stop", v0222FastPathEvent(false, "message_stop", v0222FastPathStop)},
		{"DONE", "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
			body := newSSELifecycleObservedBody(pr)
			producerDone := make(chan struct{})
			go func() {
				defer close(producerDone)
				// Data-only visibility isolates S2 from the independent S1 bug.
				if _, err := io.WriteString(pw, v0222FastPathEvent(false, "message_start", v0222FastPathStart)+v0222FastPathEvent(false, "content_block_delta", v0222FastPathText)+tc.terminal); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
				if _, err := io.WriteString(pw, v0222FastPathEvent(true, "message_delta", v0222FastPathUsage)); err != nil {
					return
				}
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for range ticker.C {
					if _, err := io.WriteString(pw, v0222FastPathEvent(true, "ping", `{"type":"ping"}`)+tc.terminal); err != nil {
						return
					}
				}
			}()
			rec := httptest.NewRecorder()
			start := time.Now()
			result, err := v0222RunFastPath(t, body, v0222FastPathContext(rec))
			elapsed := time.Since(start)
			// 立即固定退出状态，不能让 writer 的善后等待掩盖未 join。
			exit := body.exitState()
			v0222FastPathProducerStopped(t, producerDone)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 37, result.usage.OutputTokens, "usage arriving 50ms after stop must be collected")
			require.Equal(t, 15, result.usage.CacheReadInputTokens)
			require.GreaterOrEqual(t, elapsed, 450*time.Millisecond, "a stop must not skip the 500ms usage window")
			require.Less(t, elapsed, 800*time.Millisecond, "continuous pings/repeated stops must not extend the window")
			select {
			case <-body.closed:
			default:
				t.Error("terminal exit must close the upstream body")
			}
			if !exit.fullyReleased() {
				t.Errorf("terminal exit must complete the upstream reader and release its queue: %+v", exit)
			}
		})
	}
}

func TestV0222SSEFastPathStopReasonIsNotTailStart(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer func() { _ = pw.Close() }()
		if _, err := io.WriteString(pw, v0222FastPathEvent(false, "content_block_delta", v0222FastPathText)+
			v0222FastPathEvent(true, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)); err != nil {
			return
		}
		// A stop_reason is a success hint for compat readers, but is not the
		// message_stop/[DONE] boundary that starts a fixed usage tail.
		time.Sleep(anthropicTerminalUsageGrace + 100*time.Millisecond)
		_, _ = io.WriteString(pw, v0222FastPathEvent(true, "message_delta", v0222FastPathUsage)+v0222FastPathEvent(false, "message_stop", v0222FastPathStop))
	}()
	result, err := v0222RunFastPath(t, pr, v0222FastPathContext(httptest.NewRecorder()))
	v0222FastPathProducerStopped(t, producerDone)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 37, result.usage.OutputTokens, "stop_reason must not discard usage arriving before message_stop")
}

func TestV0222SSEFastPathEventNameCannotInventTerminal(t *testing.T) {
	payload := v0222FastPathEvent(false, "content_block_delta", v0222FastPathText) + v0222FastPathEvent(true, "message_stop", `{"type":"ping"}`)
	result, err := v0222RunFastPath(t, io.NopCloser(strings.NewReader(payload)), v0222FastPathContext(httptest.NewRecorder()))
	require.ErrorContains(t, err, "missing terminal event")
	require.NotNil(t, result)
}

func TestV0222SSEFastPathDisconnectPreservesUsageWithoutMoreWrites(t *testing.T) {
	writer := &gatewayForwardDrainFailWriter{ResponseRecorder: httptest.NewRecorder(), failAt: 1, failedCh: make(chan struct{})}
	body := &gatewayForwardDrainGatedBody{
		prefix: strings.NewReader(v0222FastPathEvent(true, "message_start", v0222FastPathStart) + v0222FastPathEvent(true, "content_block_delta", v0222FastPathText)),
		tail:   strings.NewReader(v0222FastPathEvent(true, "message_delta", v0222FastPathUsage) + v0222FastPathEvent(false, "message_stop", v0222FastPathStop)),
		gate:   writer.failedCh,
		closed: make(chan struct{}),
	}
	result, err := v0222RunFastPath(t, body, v0222FastPathContext(writer))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, writer.failed, "fixture must actually reach the downstream write boundary")
	require.True(t, result.clientDisconnect)
	require.Equal(t, 37, result.usage.OutputTokens)
	require.Equal(t, 12, result.usage.InputTokens)
	require.Equal(t, 15, result.usage.CacheReadInputTokens)
	require.Equal(t, 6, result.usage.CacheCreationInputTokens)
	require.Zero(t, writer.writesAfterFailure)
	require.Zero(t, writer.flushAfterFailure)
}

// Fail at the real converted tool event, after converter state already holds
// the open tool. This exercises finalization/recovery guards, not a fake parser.
type v0222FastPathToolFailWriter struct{ *gatewayForwardDrainFailWriter }

func (w *v0222FastPathToolFailWriter) Write(p []byte) (int, error) {
	if !w.failed && strings.Contains(string(p), "toolu_v0222") {
		w.failAt = w.writes + 1
	}
	return w.gatewayForwardDrainFailWriter.Write(p)
}

func TestV0222SSEFastPathFullReadersDisconnectToolTail(t *testing.T) {
	// These are the two full readers. The six status-only entry points retain
	// their separate terminal-first contract in TestAnthropicCompatStatusOnlyEntryPoints.
	handlers := gatewayForwardDrainHandlers()
	handlers["responses"] = func(s *GatewayService, resp *http.Response, c *gin.Context, start time.Time) (*ForwardResult, error) {
		// Keep an actual custom-tool restorer open at disconnect too, so that
		// later error/finalize paths cannot leak recovered tool events.
		return s.handleResponsesStreamingResponse(resp, c, "public-model", "claude-sonnet-4.5", nil, start, apicompat.ResponsesClientToolMapping{CustomTools: map[string]bool{"lookup": true}})
	}
	for name, handle := range handlers {
		for _, ending := range []string{"terminal", "post_terminal_error"} {
			t.Run(name+"/"+ending, func(t *testing.T) {
				writer := &v0222FastPathToolFailWriter{&gatewayForwardDrainFailWriter{ResponseRecorder: httptest.NewRecorder(), failedCh: make(chan struct{})}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				writer.cancel = cancel
				c := v0222FastPathContext(writer)
				c.Request = c.Request.WithContext(ctx)
				tail := v0222FastPathEvent(true, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}`) + gatewayForwardDrainDelta + gatewayForwardDrainStop
				if ending == "post_terminal_error" {
					tail += v0222FastPathEvent(true, "error", `{"type":"error","error":{"type":"overloaded_error","message":"v0222 fixture"}}`)
				}
				body := &gatewayForwardDrainGatedBody{
					prefix: strings.NewReader(gatewayForwardDrainStart + v0222FastPathEvent(true, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_v0222","name":"lookup","input":{}}}`)),
					tail:   strings.NewReader(tail),
					gate:   writer.failedCh,
					closed: make(chan struct{}),
				}
				defer func() { _ = body.Close() }()
				result, err := runGatewayForwardDrainHandler(t, handle, &GatewayService{}, &http.Response{Header: http.Header{}, Body: body}, c)
				if ending == "post_terminal_error" {
					require.ErrorContains(t, err, "upstream stream error event")
				} else {
					require.NoError(t, err)
				}
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "disconnected/committed requests must never be replayed")
				require.True(t, writer.failed, "must disconnect on converted tool output before reading its tail")
				require.NotNil(t, result)
				require.True(t, result.ClientDisconnect)
				require.Equal(t, 12, result.Usage.InputTokens)
				require.Equal(t, 27, result.Usage.OutputTokens)
				require.Equal(t, 15, result.Usage.CacheReadInputTokens)
				require.Equal(t, 6, result.Usage.CacheCreationInputTokens)
				require.Zero(t, writer.writesAfterFailure, "includes tool recovery, completion, final usage and DONE")
				require.Zero(t, writer.flushAfterFailure)
				require.NotContains(t, writer.Body.String(), "response.completed")
				require.NotContains(t, writer.Body.String(), "data: [DONE]")
			})
		}
	}
}

func TestV0222SSEFastPathFullReadersFinalizeWriteFailure(t *testing.T) {
	for name, handle := range gatewayForwardDrainHandlers() {
		t.Run(name, func(t *testing.T) {
			// Each full reader emits one initial chunk, then its final output.
			// No content events precede finalization, so failure #2 is a final
			// write, not an arbitrary earlier content write.
			writer := &gatewayForwardDrainFailWriter{ResponseRecorder: httptest.NewRecorder(), failAt: 2, failedCh: make(chan struct{})}
			body := io.NopCloser(strings.NewReader(gatewayForwardDrainStart + gatewayForwardDrainDelta))
			result, err := runGatewayForwardDrainHandler(t, handle, &GatewayService{}, &http.Response{Header: http.Header{}, Body: body}, v0222FastPathContext(writer))
			require.NoError(t, err)
			require.True(t, writer.failed, "fixture must reach finalization")
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, 27, result.Usage.OutputTokens)
			require.Zero(t, writer.writesAfterFailure)
			require.Zero(t, writer.flushAfterFailure)
		})
	}
}
