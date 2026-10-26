package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type compositeFallbackCall struct {
	platform string
	model    string
	body     string
	trace    *service.CompositeRouteTrace
}

func compositeFallbackRoute(fallbacks ...service.CompositeRouteFallback) service.CompositeModelRoute {
	return service.CompositeModelRoute{
		ID: 42, GroupID: 6, PublicModel: "gpt-6.1-sol", MatchType: service.CompositeRouteMatchExact,
		TargetPlatform: service.PlatformOpenAI, UpstreamModel: "gemini-3.8-flash-high", Endpoint: service.CompositeRouteEndpointAny,
		RequestKind: service.CompositeRouteRequestKindAny, FallbackModels: fallbacks, Priority: 100, Enabled: true,
	}
}

// newCompositeFallbackRouter wires the real composite middleware in front of a
// handler that answers per attempt; it records what every attempt observed.
func newCompositeFallbackRouter(t *testing.T, route service.CompositeModelRoute, handle func(c *gin.Context, attempt int), before ...gin.HandlerFunc) (*gin.Engine, *[]compositeFallbackCall) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	calls := []compositeFallbackCall{}
	router := gin.New()
	router.Use(before...)
	router.Use(gin.HandlerFunc(servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		groupID := int64(6)
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformComposite}})
		c.Set("pre_route_key", "kept")
		c.Next()
	})))
	router.Use(compositeTargetPlatformMiddleware(service.NewCompositeRouteResolver(compositeRouteRepoStub{routes: []service.CompositeModelRoute{route}})))
	handler := func(c *gin.Context) {
		platform, _ := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		model, _ := service.ResolvedUpstreamModelFromContext(c.Request.Context())
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		_, leaked := c.Get("attempt_scoped_key")
		require.False(t, leaked, "attempt-scoped gin keys must not leak into a replay")
		require.Equal(t, "kept", c.GetString("pre_route_key"))
		c.Set("attempt_scoped_key", true)
		mu.Lock()
		calls = append(calls, compositeFallbackCall{platform: platform, model: model, body: string(body), trace: service.CompositeRouteTraceFromContext(c.Request.Context())})
		attempt := len(calls) - 1
		mu.Unlock()
		handle(c, attempt)
	}
	router.POST("/v1/responses", handler)
	router.POST("/v1/responses/compact", handler)
	router.POST("/v1/chat/completions", handler)
	router.POST("/v1/messages", handler)
	return router, &calls
}

func serveCompositeFallback(router *gin.Engine, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestCompositeFallbackReplaysJSONFailureOnNextTarget(t *testing.T) {
	route := compositeFallbackRoute(
		service.CompositeRouteFallback{UpstreamModel: "gemini-3.8-flash"},
		service.CompositeRouteFallback{TargetPlatform: service.PlatformDeepseek, UpstreamModel: "deepseek-v4-pro"},
	)
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		switch attempt {
		case 0:
			service.SetOpsUpstreamError(c, http.StatusTooManyRequests, "primary limited", "")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "rate_limit_error", "message": "primary limited"}})
		case 1:
			c.Header("X-Attempt", "1")
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "second failed"}})
		default:
			c.JSON(http.StatusOK, gin.H{"id": "resp_ok", "output_text": "summary"})
		}
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","input":"hi"}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"id":"resp_ok","output_text":"summary"}`, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("X-Attempt"), "headers of a discarded attempt must not leak")
	require.Len(t, *calls, 3)
	require.Equal(t, service.PlatformOpenAI, (*calls)[0].platform)
	require.Equal(t, "gemini-3.8-flash-high", (*calls)[0].model)
	require.Equal(t, "gemini-3.8-flash-high", gjson.Get((*calls)[0].body, "model").String())
	require.Equal(t, service.PlatformOpenAI, (*calls)[1].platform)
	require.Equal(t, "gemini-3.8-flash", gjson.Get((*calls)[1].body, "model").String())
	require.Equal(t, service.PlatformDeepseek, (*calls)[2].platform)
	require.Equal(t, "deepseek-v4-pro", gjson.Get((*calls)[2].body, "model").String())
	require.Equal(t, "hi", gjson.Get((*calls)[2].body, "input").String())

	// Every attempt carries its own trace: usage of a failed attempt is never
	// labelled with a chain that was decided after it.
	require.Empty(t, (*calls)[0].trace.FallbackChain())
	require.Equal(t, "gemini-3.8-flash-high (429) → gemini-3.8-flash", (*calls)[1].trace.FallbackChain())
	require.NotSame(t, (*calls)[0].trace, (*calls)[1].trace)
	trace := (*calls)[2].trace
	require.NotNil(t, trace)
	require.Equal(t, int64(42), trace.RouteID)
	require.Equal(t, service.CompositeRouteRequestKindConversation, trace.RequestKind)
	require.Equal(t, "gemini-3.8-flash-high (429) → gemini-3.8-flash (502) → deepseek/deepseek-v4-pro", trace.FallbackChain())
}

func TestCompositeFallbackReturnsLastFailureWhenAllTargetsFail(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "primary down"}})
			return
		}
		service.SetOpsUpstreamError(c, http.StatusTooManyRequests, "backup limited", "")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "backup limited"}})
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol"}`)

	require.Len(t, *calls, 2)
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, "backup limited", gjson.Get(recorder.Body.String(), "error.message").String())
	require.NotContains(t, recorder.Body.String(), "primary down")
}

// Caller-side rejections fail identically on every target, so they are
// returned at once instead of being replayed through the whole chain.
func TestCompositeFallbackSkipsFinalFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		handle func(c *gin.Context)
		status int
	}{
		{"local authentication", func(c *gin.Context) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "bad key"}})
		}, http.StatusUnauthorized},
		{"group unassigned", func(c *gin.Context) {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonAPIKeyGroupUnassigned)
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "no group"}})
		}, http.StatusForbidden},
		{"payment", func(c *gin.Context) {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": gin.H{"message": "insufficient balance"}})
		}, http.StatusPaymentRequired},
		{"user concurrency", func(c *gin.Context) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "rate_limit_error", "message": "Concurrency limit exceeded"}})
		}, http.StatusTooManyRequests},
		{"quota", func(c *gin.Context) {
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "billing_error", "message": "quota exhausted"}})
		}, http.StatusForbidden},
		{"local validation", func(c *gin.Context) {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "messages is required"}})
		}, http.StatusBadRequest},
		{"content policy", func(c *gin.Context) {
			service.SetOpsUpstreamError(c, http.StatusBadRequest, "blocked", "")
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "blocked by policy"}})
		}, http.StatusBadRequest},
		{"request scoped", func(c *gin.Context) {
			service.SetOpsUpstreamError(c, http.StatusNotFound, "cache missing", "")
			service.MarkOpsRequestScopedError(c, "invalid_request_error")
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "cached content not found"}})
		}, http.StatusNotFound},
		{"too large", func(c *gin.Context) {
			service.SetOpsUpstreamError(c, http.StatusRequestEntityTooLarge, "too large", "")
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"message": "too large"}})
		}, http.StatusRequestEntityTooLarge},
		{"usage already submitted", func(c *gin.Context) {
			service.SetOpsUpstreamError(c, http.StatusBadRequest, "failed after partial usage", "")
			service.CompositeRouteTraceFromContext(c.Request.Context()).MarkUsageSubmitted()
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "failed after partial usage"}})
		}, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) { tt.handle(c) })
			recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol"}`)
			require.Len(t, *calls, 1)
			require.Equal(t, tt.status, recorder.Code)
		})
	}
}

// Upstream/account-side failures and an empty account pool are what another
// target can fix, whatever the status code.
func TestCompositeFallbackRetriesUpstreamAndCapacityFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		handle func(c *gin.Context)
	}{
		{"upstream auth", func(c *gin.Context) {
			service.SetOpsUpstreamError(c, http.StatusUnauthorized, "account token revoked", "")
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream auth failed"}})
		}},
		{"upstream 400", func(c *gin.Context) {
			service.SetOpsUpstreamError(c, http.StatusBadRequest, "unsupported parameter", "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "unsupported parameter"}})
		}},
		{"all accounts rate limited", func(c *gin.Context) {
			c.Set(service.OpsRoutingCapacityLimitedKey, true)
			c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "rate_limit_error"}})
		}},
		{"server error", func(c *gin.Context) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "boom"}})
		}},
		{"feature gate", func(c *gin.Context) {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "not enabled for platform"}})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if attempt == 0 {
					tt.handle(c)
					return
				}
				require.False(t, service.HasOpsUpstreamFailure(c), "the replay starts from the pre-route gin keys")
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
			recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol"}`)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Len(t, *calls, 2)
		})
	}
}

// A gateway-authored in-band error (e.g. a concurrency wait timeout on an
// already committed stream) is caller-side; an upstream one is replayed.
func TestCompositeFallbackInBandErrorOrigin(t *testing.T) {
	for _, tt := range []struct {
		name     string
		local    bool
		attempts int
	}{
		{"gateway authored", true, 1},
		{"upstream stream error", false, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if attempt == 0 {
					if tt.local {
						service.MarkOpsStreamError(c, "rate_limit_error", "Concurrency limit exceeded", http.StatusTooManyRequests)
					}
					writeSSE(c, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"limited\"}}\n\n")
					return
				}
				writeSSE(c, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"ok\"}}\n\n")
			})
			serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)
			require.Len(t, *calls, tt.attempts)
		})
	}
}

// Handlers may edit request headers (e.g. Bedrock beta filtering); a replay
// must see the client's original headers.
func TestCompositeFallbackIsolatesRequestHeadersPerAttempt(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	seen := []string{}
	router, _ := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		seen = append(seen, c.Request.Header.Get("Anthropic-Beta"))
		c.Request.Header.Del("Anthropic-Beta")
		if attempt == 0 {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "down"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"context-1m-2025-08-07", "context-1m-2025-08-07"}, seen)
}

// A final attempt that only sets a status keeps it, as without the wrapper.
func TestCompositeFallbackKeepsHeaderOnlyFinalStatus(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, _ := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "down"}})
			return
		}
		c.Header("X-Final", "yes")
		c.Status(http.StatusNoContent)
	})
	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol"}`)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, "yes", recorder.Header().Get("X-Final"))
}

func TestCompositeFallbackRetriesModelScopedLocalRejection(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{TargetPlatform: service.PlatformAnthropic, UpstreamModel: "claude-sonnet-5"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"type": "model_not_found"}})
			return
		}
		require.False(t, service.HasOpsClientBusinessLimited(c), "the replay starts from the pre-route gin keys")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol"}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, *calls, 2)
	require.Equal(t, service.PlatformAnthropic, (*calls)[1].platform)
	require.Equal(t, "gemini-3.8-flash-high (404) → anthropic/claude-sonnet-5", (*calls)[1].trace.FallbackChain())
}

func writeSSE(c *gin.Context, frames ...string) {
	c.Header("Content-Type", "text/event-stream")
	c.Status(http.StatusOK)
	for _, frame := range frames {
		_, _ = c.Writer.WriteString(frame)
		c.Writer.Flush()
	}
}

func TestCompositeFallbackReplaysStreamFailureBeforeVisibleOutput(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			writeSSE(c,
				": keepalive\n\n",
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary\"}}\n\n",
				"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}}\n\n",
			)
			return
		}
		writeSSE(c,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_backup\"}}\n\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"summary\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		)
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)

	require.Len(t, *calls, 2)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	body := recorder.Body.String()
	require.True(t, strings.HasPrefix(body, ": keepalive\n\n"), body)
	require.NotContains(t, body, "resp_primary")
	require.NotContains(t, body, "response.failed")
	require.Contains(t, body, "resp_backup")
	require.Contains(t, body, `"delta":"summary"`)
	require.Contains(t, body, "response.completed")
	require.Equal(t, "gemini-3.8-flash-high (429) → backup", (*calls)[1].trace.FallbackChain())
}

// When a keepalive already committed the stream, the final JSON failure is
// turned into the client protocol's terminal event — response.failed for
// Responses (Codex reconnects blindly on a bare error frame).
func TestCompositeFallbackCommittedStreamEndsWithProtocolTerminalEvent(t *testing.T) {
	for _, tt := range []struct {
		path   string
		assert func(t *testing.T, body string)
	}{
		{"/v1/responses", func(t *testing.T, body string) {
			require.Contains(t, body, "event: response.failed\ndata: ")
			data := strings.TrimSuffix(body[strings.Index(body, "data: ")+len("data: "):], "\n\n")
			require.Equal(t, "response.failed", gjson.Get(data, "type").String())
			require.Equal(t, "failed", gjson.Get(data, "response.status").String())
			require.Equal(t, "api_error", gjson.Get(data, "response.error.code").String())
			require.Equal(t, "no accounts", gjson.Get(data, "response.error.message").String())
			require.True(t, strings.HasPrefix(gjson.Get(data, "response.id").String(), "resp_"))
			require.Positive(t, gjson.Get(data, "response.created_at").Int())
		}},
		{"/v1/messages", func(t *testing.T, body string) {
			require.Contains(t, body, "event: error\ndata: {\"error\":{\"message\":\"no accounts\",\"type\":\"api_error\"},\"type\":\"error\"}\n\n")
		}},
		{"/v1/chat/completions", func(t *testing.T, body string) {
			require.Contains(t, body, "data: {\"error\":{\"code\":503,\"message\":\"no accounts\",\"type\":\"api_error\"}}\n\n")
			require.NotContains(t, body, "event:")
		}},
	} {
		t.Run(tt.path, func(t *testing.T) {
			route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
			router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
				if attempt == 0 {
					writeSSE(c, ": keepalive\n\n", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "api_error", "message": "no accounts"}})
			})
			recorder := serveCompositeFallback(router, tt.path, `{"model":"gpt-6.1-sol","stream":true}`)

			require.Len(t, *calls, 2)
			require.Equal(t, http.StatusOK, recorder.Code, "status was already committed by the keepalive")
			body := recorder.Body.String()
			require.True(t, strings.HasPrefix(body, ": keepalive\n\n"), body)
			require.NotContains(t, body, "busy")
			tt.assert(t, strings.TrimPrefix(body, ": keepalive\n\n"))
		})
	}
}

// If the final attempt writes nothing, a committed stream ends with the last
// upstream error event — without re-sending the discarded attempt's preamble.
func TestCompositeFallbackCommittedStreamReusesOnlyErrorEvents(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			writeSSE(c, ": keepalive\n\n",
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary\"}}\n\n",
				"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"primary broke\"}}}\n\n")
			return
		}
		// The backup handler returns without writing anything (e.g. client gone mid-way).
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)

	require.Len(t, *calls, 2)
	body := recorder.Body.String()
	require.NotContains(t, body, "resp_primary")
	require.Contains(t, body, "primary broke")
}

func TestCompositeFallbackDoesNotReplayAfterVisibleOutput(t *testing.T) {
	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		writeSSE(c,
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n",
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
			"data: {\"error\":{\"message\":\"stream broke\"}}\n\n",
		)
	})

	recorder := serveCompositeFallback(router, "/v1/chat/completions", `{"model":"gpt-6.1-sol","stream":true}`)

	require.Len(t, *calls, 1)
	body := recorder.Body.String()
	require.Contains(t, body, "partial")
	require.Contains(t, body, "stream broke")
	require.Empty(t, (*calls)[0].trace.FallbackChain())
}

func TestCompositeFallbackHoldsPreambleBehindKeepalive(t *testing.T) {
	previous := compositeFallbackKeepaliveInterval
	compositeFallbackKeepaliveInterval = 60 * time.Millisecond
	t.Cleanup(func() { compositeFallbackKeepaliveInterval = previous })

	route := compositeFallbackRoute(service.CompositeRouteFallback{UpstreamModel: "backup"})
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		if attempt == 0 {
			writeSSE(c, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary\"}}\n\n")
			time.Sleep(250 * time.Millisecond)
			_, _ = c.Writer.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n")
			return
		}
		writeSSE(c, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol","stream":true}`)

	require.Len(t, *calls, 2)
	body := recorder.Body.String()
	require.Contains(t, body, ": keepalive\n\n")
	require.NotContains(t, body, "resp_primary")
	require.Contains(t, body, `"delta":"ok"`)
}

func TestCompositeExplicitRouteWithoutFallbackStillTracesUsage(t *testing.T) {
	route := compositeFallbackRoute()
	router, calls := newCompositeFallbackRouter(t, route, func(c *gin.Context, attempt int) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "limited"}})
	})

	recorder := serveCompositeFallback(router, "/v1/responses", `{"model":"gpt-6.1-sol"}`)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Len(t, *calls, 1)
	require.NotNil(t, (*calls)[0].trace)
	require.Equal(t, int64(42), (*calls)[0].trace.RouteID)
	require.Empty(t, (*calls)[0].trace.FallbackChain())
}

func TestClassifyCompositeSSEEvent(t *testing.T) {
	for _, tt := range []struct {
		name  string
		event string
		want  compositeSSEClass
	}{
		{"comment", ": ping\n\n", compositeSSEComment},
		{"done", "data: [DONE]\n\n", compositeSSEPreamble},
		{"anthropic ping", "event: ping\ndata: {\"type\":\"ping\"}\n\n", compositeSSEPreamble},
		{"message start", "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", compositeSSEPreamble},
		{"role chunk", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n", compositeSSEPreamble},
		{"content chunk", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", compositeSSEVisible},
		{"tool chunk", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\n", compositeSSEVisible},
		{"anthropic error", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", compositeSSEError},
		{"openai error", "data: {\"error\":{\"message\":\"x\"}}\n\n", compositeSSEError},
		{"gemini candidate", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n\n", compositeSSEVisible},
		{"content block", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n", compositeSSEVisible},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyCompositeSSEEvent([]byte(tt.event))
			require.Equal(t, tt.want, got)
		})
	}
	_, status := classifyCompositeSSEEvent([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"))
	require.Equal(t, http.StatusTooManyRequests, status)
	_, status = classifyCompositeSSEEvent([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"))
	require.Equal(t, 529, status)
}
