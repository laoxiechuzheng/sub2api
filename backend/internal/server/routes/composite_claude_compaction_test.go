package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCompositeTargetPlatformMiddlewareClaudeCompactionHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, header, value, ua string
		want                    bool
	}{
		{"manual", "x-claude-code-compaction", "manual", "claude-cli/2.1.286", true},
		{"auto", "x-claude-code-compaction", "auto", "claude-cli/2.1.286", true},
		{"reactive", "x-claude-code-compaction", "reactive", "claude-cli/2.1.286", true},
		{"request class", "x-claude-code-request-class", "compaction", "claude-cli/2.1.286", true},
		{"first party alias", "x-cc-compaction-request", "manual", "claude-code/2.1.286", true},
		{"context already compacted is not current compaction", "x-claude-code-context-compacted", "manual", "claude-cli/2.1.286", false},
		{"generic boolean is not a compaction kind", "x-claude-code-compaction", "true", "claude-cli/2.1.286", false},
		{"duplicate value is not one hint", "x-claude-code-compaction", "manual, auto", "claude-cli/2.1.286", false},
		{"unrelated client", "x-claude-code-compaction", "manual", "OtherClient", false},
		{"duplicate same value", "x-claude-code-compaction", "manual", "claude-cli/2.1.286", false},
		{"duplicate different value", "x-claude-code-compaction", "manual", "claude-cli/2.1.286", false},
		{"conflicting aliases", "x-claude-code-compaction", "manual", "claude-cli/2.1.286", false},
		{"conflicting request class", "x-claude-code-compaction", "manual", "claude-cli/2.1.286", false},
		{"unknown alias value", "x-claude-code-compaction", "manual", "claude-cli/2.1.286", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := service.CompositeModelRoute{ID: 9, GroupID: 6, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: service.PlatformOpenAI, UpstreamModel: "gemini-3.8-flash-high", Endpoint: "messages", RequestKind: "compaction", Enabled: true}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				id := int64(6)
				c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{GroupID: &id, Group: &service.Group{ID: id, Platform: service.PlatformComposite}})
				c.Next()
			})
			router.Use(compositeTargetPlatformMiddleware(service.NewCompositeRouteResolver(compositeRouteRepoStub{routes: []service.CompositeModelRoute{route}})))
			router.POST("/v1/messages", func(c *gin.Context) {
				source, ok := service.CompositeRouteSourceFromContext(c.Request.Context())
				require.True(t, ok)
				require.Equal(t, tt.want, source == service.CompositeRouteSourceExplicit)
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				if tt.want {
					require.Contains(t, string(body), `"model":"gemini-3.8-flash-high"`)
					require.NotContains(t, string(body), `"compaction_trigger"`)
					require.NotContains(t, string(body), `"context_management"`)
				} else {
					require.Contains(t, string(body), `"model":"claude-opus-5"`)
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"Current request with custom instructions"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", tt.ua)
			req.Header.Set(tt.header, tt.value)
			switch tt.name {
			case "duplicate same value":
				req.Header.Add(tt.header, "manual")
			case "duplicate different value":
				req.Header.Add(tt.header, "auto")
			case "conflicting aliases":
				req.Header.Set("x-cc-compaction-request", "auto")
			case "conflicting request class":
				req.Header.Set("x-claude-code-request-class", "conversation")
			case "unknown alias value":
				req.Header.Set("x-cc-compaction-request", "true")
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusNoContent, rec.Code)
		})
	}
}
