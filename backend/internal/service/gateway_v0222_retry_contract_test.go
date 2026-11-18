//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A qualifying thinking 400 must be rectified before account policy sees it.
// Policies apply once to the final failed response; neither retry may loop.
func TestV0222GatewayRetryRectificationBeforeAccountPolicy(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions"} {
		for _, tc := range []struct {
			name        string
			rotate      bool
			finalStatus int
		}{
			{"rectification_succeeds", false, http.StatusOK},
			{"final_nondefault_status_disables", false, http.StatusGone},
			{"repeated_thinking_400_is_final", false, http.StatusBadRequest},
			{"rotation_then_rectification", true, http.StatusOK},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				account := &Account{
					ID: 711, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true, Concurrency: 1,
					Credentials: map[string]any{
						"api_key": "fixture-only", "custom_error_codes_enabled": true,
						"custom_error_codes":         []any{float64(http.StatusBadRequest), float64(http.StatusGone)},
						"temp_unschedulable_enabled": true,
						"temp_unschedulable_rules": []any{map[string]any{
							"scope": "account", "error_code": float64(http.StatusBadRequest),
							"keywords": []any{"final block"}, "duration_minutes": float64(10),
						}},
					},
				}
				cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
				repo := &rotatedClaudeTokenRepo{account: account}
				cache := newClaudeTokenCacheStub()
				latest := *account
				if tc.rotate {
					account.Type = AccountTypeOAuth
					account.Credentials["access_token"] = "old-token"
					account.Credentials["refresh_token"] = "fixture-refresh"
					latest = *account
					latest.Credentials = map[string]any{"access_token": "new-token", "refresh_token": "fixture-new-refresh"}
					require.NoError(t, cache.SetAccessToken(context.Background(), ClaudeTokenCacheKey(account), "old-token", time.Hour))
				}
				wantCalls := 2
				if tc.rotate {
					wantCalls++
				}
				var bodies [][]byte
				var rejected []*rotatedClaudeTokenBody
				calls := 0
				upstream := &systemOneHTTPUpstream{do: func(req *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					_ = req.Body.Close()
					bodies = append(bodies, body)
					if calls > wantCalls {
						return nil, fmt.Errorf("unexpected retry number %d", calls)
					}
					status, payload := http.StatusBadRequest, string(assistantThinkingTailErrorBody())
					if tc.rotate && calls == 1 {
						require.Equal(t, "Bearer old-token", getHeaderRaw(req.Header, "authorization"))
						repo.account = &latest
						status, payload = http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"fixture revoked"}}`
					} else if tc.rotate {
						require.Equal(t, "Bearer new-token", getHeaderRaw(req.Header, "authorization"))
					}
					if calls == wantCalls {
						status = tc.finalStatus
						if status == http.StatusOK {
							return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(thinkingTailAnthropicTextStream()))}, nil
						}
						if status == http.StatusGone {
							payload = `{"type":"error","error":{"type":"invalid_request_error","message":"final endpoint gone"}}`
						}
					}
					responseBody := &rotatedClaudeTokenBody{Reader: strings.NewReader(payload)}
					rejected = append(rejected, responseBody)
					return &http.Response{StatusCode: status, Header: http.Header{"X-Request-Id": {fmt.Sprintf("retry-attempt-%d", calls)}}, Body: responseBody}, nil
				}}
				svc := &GatewayService{
					cfg: cfg, accountRepo: repo, httpUpstream: upstream,
					rateLimitService: &RateLimitService{cfg: cfg, accountRepo: repo},
				}
				if tc.rotate {
					svc.claudeTokenProvider = NewClaudeTokenProvider(repo, cache, nil)
				}
				rec := httptest.NewRecorder()
				c := v0222FastPathContext(rec)
				body := []byte(`{"model":"claude-sonnet-4-6","input":"continue","reasoning":{"effort":"high"}}`)
				var result *ForwardResult
				var err error
				if endpoint == "responses" {
					result, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
				} else {
					body = []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"continue"}],"reasoning_effort":"high"}`)
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				}
				require.Equal(t, wantCalls, calls, "one token rotation and one rectification at most")
				require.Equal(t, "enabled", gjson.GetBytes(bodies[wantCalls-2], "thinking.type").String())
				require.NotEqual(t, "enabled", gjson.GetBytes(bodies[wantCalls-1], "thinking.type").String(), "rectification must change the actual wire request")
				for _, body := range rejected {
					require.True(t, body.closed, "every rejected response body must be closed")
				}
				if tc.rotate {
					require.Equal(t, bodies[0], bodies[1], "rotation preserves the original complete wire body before rectification")
					require.Equal(t, "old-token", account.GetCredential("access_token"), "keep the shared snapshot immutable")
				}
				if tc.finalStatus == http.StatusOK {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Contains(t, rec.Body.String(), "ok")
					require.Zero(t, repo.errorCalls, "recoverable thinking 400 must not disable the account")
					require.Zero(t, repo.cooldownCalls, "a successfully recovered 401 must not penalize the account")
				} else {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Equal(t, tc.finalStatus, failover.StatusCode)
					if tc.finalStatus == http.StatusBadRequest {
						require.Zero(t, repo.errorCalls)
						require.Equal(t, 1, repo.cooldownCalls, "explicit 400 rule applies only after rectification failed")
					} else {
						require.Equal(t, 1, repo.errorCalls, "only final status triggers account policy")
						require.Zero(t, repo.cooldownCalls)
					}
					require.Nil(t, result)
					require.Empty(t, rec.Body.String(), "leave final error available for account failover")
					if tc.finalStatus == http.StatusGone {
						require.Contains(t, string(failover.ResponseBody), "final endpoint gone", "policy and failover must use the final body")
					}
				}
			})
		}
	}
}
