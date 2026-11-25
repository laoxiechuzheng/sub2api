package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAnthropicPoolHTTP502RetainsRetryWithoutAccountCooldown(t *testing.T) {
	const upstreamError = `{"type":"error","error":{"type":"api_error","message":"upstream handshake failed"}}`
	const messages = `{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	const responses = `{"model":"claude-sonnet-4-5","stream":true,"input":"hello"}`

	for _, tc := range []struct {
		name        string
		path        string
		body        string
		passthrough bool
	}{
		{name: "messages", path: "/v1/messages", body: messages},
		{name: "messages passthrough", path: "/v1/messages", body: messages, passthrough: true},
		{name: "chat completions", path: "/v1/chat/completions", body: messages},
		{name: "responses", path: "/v1/responses", body: responses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra["anthropic_passthrough"] = tc.passthrough
			account.Credentials["pool_mode"] = true
			account.Credentials["pool_mode_retry_count"] = 3
			account.Credentials["pool_mode_retry_status_codes"] = []any{http.StatusBadGateway}
			repo := &gatewayForwardErrorPolicyRepoStub{}
			svc := newForwardPartialUsageServiceForTest(&anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(upstreamError)),
			}})
			svc.accountRepo = repo
			svc.rateLimitService.accountRepo = repo
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			body := []byte(tc.body)
			var err error
			switch tc.path {
			case "/v1/messages":
				parsed, parseErr := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				require.NoError(t, parseErr)
				_, err = svc.Forward(ctx, c, account, parsed)
			case "/v1/chat/completions":
				_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			case "/v1/responses":
				_, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
			}

			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, http.StatusBadGateway, failover.StatusCode)
			require.JSONEq(t, upstreamError, string(failover.ResponseBody))
			require.True(t, failover.RetryableOnSameAccount)
			require.False(t, failover.RequestScopedTransient)
			require.Zero(t, failover.SameAccountRetryDelay)
			require.Equal(t, 3, account.GetPoolModeRetryCount())
			require.True(t, failover.ShouldRetryNextAccount())
			require.False(t, c.Writer.Written())
			require.Zero(t, repo.tempCalls)

			// The handler invokes this after the 3 configured retries fail.
			svc.TempUnscheduleRetryableError(ctx, account.ID, failover)
			require.Zero(t, repo.tempCalls, "an HTTP 502 is not evidence of an empty stream")
		})
	}
}
