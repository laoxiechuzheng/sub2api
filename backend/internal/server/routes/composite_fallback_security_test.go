package routes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCompositeFallbackNativeCompactionSkipsLossyTargets(t *testing.T) {
	for _, tt := range []struct{ name, path, body string }{
		{"native v2", "/v1/responses", `{"model":"gpt-6.1-sol","stream":true,"input":[{"type":"compaction_trigger"}]}`},
		{"compact path", "/v1/responses/compact", `{"model":"gpt-6.1-sol","input":[]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{TargetPlatform: service.PlatformAnthropic, UpstreamModel: "claude-sonnet-5"})
			lossyCalls := 0
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if getGroupPlatform(c) == service.PlatformAnthropic {
					lossyCalls++
					c.JSON(http.StatusOK, gin.H{"summary": "lost native compaction"})
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "api_error", "message": "primary down"}})
			})
			recorder := serveCompositeFallback(router, tt.path, tt.body)
			require.Zero(t, lossyCalls, "native compaction must never enter the Anthropic conversion handler")
			require.Len(t, *calls, 1)
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			require.Equal(t, "compact_not_supported", gjson.Get(recorder.Body.String(), "error.type").String())
		})
	}
}

func TestCompositeFallbackOrdinarySummaryStillConverts(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{TargetPlatform: service.PlatformAnthropic, UpstreamModel: "claude-sonnet-5"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "primary down"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"summary": "ok"})
	})
	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","input":"summarize this conversation"}`)
	require.Len(t, *calls, 2)
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestCompositeFallbackNativeCompactionCanReachCompatibleTargetAfterSkip(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "gpt-backup", TargetPlatform: service.PlatformOpenAI})
	route.TargetPlatform = service.PlatformAnthropic
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		require.Equal(t, service.PlatformOpenAI, getGroupPlatform(c))
		c.JSON(http.StatusOK, gin.H{"object": "response.compaction"})
	})
	recorder := serveCompositeFallback(router, "/v1/responses/compact", "{\"model\":\"gpt-6.1-sol\",\"input\":[]}")
	require.Len(t, *calls, 1)
	require.Equal(t, "gpt-backup", (*calls)[0].model)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "response.compaction")
}

func TestCompositeFallbackNativeCompactionWithoutCompatibleTargetIsFinal(t *testing.T) {
	route := compositeFallbackRoute()
	route.TargetPlatform = service.PlatformAnthropic
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		c.JSON(http.StatusOK, gin.H{"summary": "lossy"})
	})
	recorder := serveCompositeFallback(router, "/v1/responses/compact", "{\"model\":\"gpt-6.1-sol\",\"input\":[]}")
	require.Empty(t, *calls)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Equal(t, "compact_not_supported", gjson.Get(recorder.Body.String(), "error.type").String())
}

func TestCompositeFallbackCyberPolicyIsFinalWhileBillingWorkerBlocked(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 8})
	release, started, billed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release); pool.Stop() })
	pool.Submit(func(context.Context) { close(started); <-release })
	<-started
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "blocked", UpstreamInTok: 11})
			pool.Submit(func(context.Context) { close(billed) })
			writeSSE(c, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"cyber_policy\",\"message\":\"blocked\"}}}\n\n")
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)
	require.Len(t, *calls, 1)
	require.Contains(t, recorder.Body.String(), "cyber_policy")
	select {
	case <-billed:
		t.Fatal("billing must remain blocked while the request reaches its final decision")
	default:
	}
}

func TestCompositeFallbackTimingStackDoesNotDeadlock(t *testing.T) {
	previous := compositeFallbackKeepaliveInterval
	compositeFallbackKeepaliveInterval = 15 * time.Millisecond
	t.Cleanup(func() { compositeFallbackKeepaliveInterval = previous })
	for _, streaming := range []bool{false, true} {
		name := "JSON first commit"
		if streaming {
			name = "SSE heartbeat"
		}
		t.Run(name, func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, _ := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if streaming {
					writeSSE(c, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
					time.Sleep(45 * time.Millisecond)
					writeSSE(c, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
					return
				}
				c.JSON(http.StatusOK, gin.H{"ok": true})
			}, servermiddleware.ServerTiming(true), handler.OpsErrorLoggerMiddleware(nil), func(c *gin.Context) {
				c.Set(string(servermiddleware.ContextKeyUserRole), "admin")
				c.Next()
			})
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true}`))
			req.Header.Set("X-Admin-UI-Request", "1")
			done := make(chan struct{})
			go func() { defer close(done); router.ServeHTTP(recorder, req) }()
			select {
			case <-done:
				require.NotEmpty(t, recorder.Header().Get("Server-Timing"))
				require.Contains(t, recorder.Body.String(), "ok")
				if streaming {
					require.Contains(t, recorder.Body.String(), ": keepalive")
				}
			case <-time.After(time.Second):
				t.Fatal("ServerTiming + Ops + Composite deadlocked on response commit")
			}
		})
	}
}

type compositeFailingDownstream struct {
	*httptest.ResponseRecorder
	mu              sync.Mutex
	err             error
	short           bool
	writes, flushes int
	failed          chan struct{}
	once            sync.Once
}

func (w *compositeFailingDownstream) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	w.once.Do(func() { close(w.failed) })
	if w.short {
		return len(data) - 1, nil
	}
	return 0, w.err
}

func (w *compositeFailingDownstream) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes++
}

func TestCompositeFallbackDownstreamFailureStopsWritesAndReplay(t *testing.T) {
	previous := compositeFallbackKeepaliveInterval
	compositeFallbackKeepaliveInterval = 15 * time.Millisecond
	t.Cleanup(func() { compositeFallbackKeepaliveInterval = previous })
	for _, mode := range []string{"comment", "visible", "heartbeat"} {
		for _, short := range []bool{false, true} {
			name := mode + "/closed pipe"
			if short {
				name = mode + "/short write"
			}
			t.Run(name, func(t *testing.T) {
				base := &compositeFailingDownstream{ResponseRecorder: httptest.NewRecorder(), err: io.ErrClosedPipe, short: short, failed: make(chan struct{})}
				wantErr := io.ErrClosedPipe
				if short {
					wantErr = io.ErrShortWrite
				}
				route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
				router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
					c.Header("Content-Type", "text/event-stream")
					frame := ": keepalive\n\n"
					if mode == "visible" {
						frame = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
					}
					if mode == "heartbeat" {
						_, err := c.Writer.WriteString("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
						require.NoError(t, err)
						select {
						case <-base.failed:
							time.Sleep(45 * time.Millisecond)
						case <-time.After(time.Second):
							t.Fatal("heartbeat never attempted a downstream write")
						}
					} else {
						_, err := c.Writer.WriteString(frame)
						require.ErrorIs(t, err, wantErr)
					}
					_, err := c.Writer.WriteString("event: error\ndata: {\"error\":{\"message\":\"busy\"}}\n\n")
					require.ErrorIs(t, err, wantErr, "downstream failure must persist across subsequent writes")
					c.Writer.Flush()
				})
				router.ServeHTTP(base, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true}`)))
				require.Len(t, *calls, 1)
				base.mu.Lock()
				defer base.mu.Unlock()
				require.Equal(t, 1, base.writes, "no subsequent Write may reach the failed downstream")
				require.Zero(t, base.flushes, "no Flush may reach the failed downstream")
			})
		}
	}
}

func TestCompositeFallbackStructuredStreamStatusPreventsReplay(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusRequestEntityTooLarge, 499} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if attempt > 0 {
					c.JSON(http.StatusOK, gin.H{"ok": true})
					return
				}
				writeSSE(c, ": keepalive\n\n")
				if status != http.StatusForbidden {
					service.SetOpsUpstreamError(c, status, "upstream rejected", "")
				}
				errType := "invalid_request_error"
				if status == http.StatusForbidden {
					errType = "billing_error"
				}
				service.MarkOpsStreamError(c, errType, "request rejected", status)
				writeSSE(c, fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":\"request rejected\"}}\n\n", errType))
			})
			recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)
			require.Len(t, *calls, 1)
			require.Contains(t, recorder.Body.String(), "request rejected")
		})
	}
}

func TestCompositeFallbackStructuredUpstreamStatusPreventsReplay(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, 499} {
		for _, intendedStatus := range []int{0, http.StatusBadGateway} {
			t.Run(fmt.Sprintf("%d/intended=%d", status, intendedStatus), func(t *testing.T) {
				route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
				router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
					writeSSE(c, ": keepalive\n\n")
					service.SetOpsUpstreamError(c, status, "upstream rejected", "")
					if intendedStatus > 0 {
						service.MarkOpsStreamError(c, "upstream_error", "request rejected", intendedStatus)
					}
					writeSSE(c, "event: error\ndata: {\"error\":{\"message\":\"request rejected\"}}\n\n")
				})
				serveCompositeFallback(router, "/v1/responses", "{\"model\":\"gpt-6.1-sol\",\"stream\":true}")
				require.Len(t, *calls, 1)
			})
		}
	}
}

func TestCompositeFallbackStructuredUpstreamFailureStillRetries(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if attempt > 0 {
					c.JSON(http.StatusOK, gin.H{"ok": true})
					return
				}
				writeSSE(c, ": keepalive\n\n")
				service.SetOpsUpstreamError(c, status, "upstream rejected", "")
				service.MarkOpsStreamFailure(c, "upstream_error", "server_error", "busy", status)
				writeSSE(c, "event: error\ndata: {\"error\":{\"message\":\"busy\"}}\n\n")
			})
			recorder := serveCompositeFallback(router, "/v1/responses", "{\"model\":\"gpt-6.1-sol\",\"stream\":true}")
			require.Len(t, *calls, 2)
			require.Contains(t, recorder.Body.String(), "ok")
		})
	}
}

func TestCompositeFallbackFinalSSEUsesOneProtocolTerminal(t *testing.T) {
	for _, heartbeat := range []bool{false, true} {
		for _, validTerminal := range []bool{false, true} {
			name := "bare error"
			if validTerminal {
				name = "existing response.failed"
			}
			if heartbeat {
				name += "/heartbeat"
			}
			t.Run(name, func(t *testing.T) {
				route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
				router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
					if heartbeat {
						writeSSE(c, ": keepalive\n\n")
					}
					writeSSE(c, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_discard_preamble\"}}\n\n")
					if validTerminal {
						writeSSE(c, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failure\",\"object\":\"response\",\"status\":\"failed\",\"output\":[],\"error\":{\"code\":\"server_error\",\"message\":\"final failure\"}}}\n\n")
					} else {
						writeSSE(c, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"final failure\"}}\n\n")
					}
				})
				recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)
				require.Len(t, *calls, 2)
				body := recorder.Body.String()
				require.Equal(t, 1, strings.Count(body, "event: response.failed\n"))
				require.NotContains(t, body, "event: error\n")
				require.NotContains(t, body, "response.created")
				require.NotContains(t, body, "resp_discard_preamble")
				require.Contains(t, body, "final failure")
				if validTerminal {
					require.Contains(t, body, "resp_failure")
				}
			})
		}
	}
}

func TestCompositeFallbackFinalTerminalCompletesSSEEnvelope(t *testing.T) {
	payload := "{\"type\":\"response.failed\",\"response\":{\"id\":\"resp_envelope\",\"object\":\"response\",\"status\":\"failed\",\"output\":[],\"usage\":{\"input_tokens\":11},\"error\":{\"code\":\"server_error\",\"message\":\"failed\"}}}"
	for _, frame := range []string{"event: response.failed\ndata: " + payload, "event: error\ndata: " + payload + "\n\n"} {
		t.Run(frame[:strings.Index(frame, "\n")], func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString(frame)
			})
			recorder := serveCompositeFallback(router, "/v1/responses", "{\"model\":\"gpt-6.1-sol\",\"stream\":true}")
			require.Len(t, *calls, 2)
			body := recorder.Body.String()
			require.True(t, strings.HasSuffix(body, "\n\n"), "terminal frame must have a complete SSE delimiter")
			require.Contains(t, body, "event: response.failed\n")
			require.NotContains(t, body, "event: error\n")
			require.Contains(t, body, payload, "valid terminal billing payload must survive envelope repair")
		})
	}
}

func TestCompositeFallbackFinalBareSSEMatchesIngressProtocol(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		for _, heartbeat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/heartbeat=%t", path, heartbeat), func(t *testing.T) {
				route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
				router, _ := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
					if heartbeat {
						writeSSE(c, ": keepalive\n\n")
					}
					writeSSE(c, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
						"event: error\ndata: {\"error\":{\"type\":\"server_error\",\"message\":\"final failure\"}}\n\n")
				})
				recorder := serveCompositeFallback(router, path, "{\"model\":\"gpt-6.1-sol\",\"stream\":true}")
				body := recorder.Body.String()
				require.NotContains(t, body, "response.created")
				require.Equal(t, 1, strings.Count(body, "final failure"))
				if path == "/v1/messages" {
					require.Contains(t, body, "event: error\n")
					require.Contains(t, body, "\"type\":\"error\"")
				} else {
					require.NotContains(t, body, "event:")
					require.Contains(t, body, "data: {\"error\":")
				}
			})
		}
	}
}
