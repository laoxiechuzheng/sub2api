//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestKiroAccountsSupportPoolMode(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		account := &Account{Platform: PlatformKiro, Type: accountType, Credentials: map[string]any{
			"pool_mode":             true,
			"pool_mode_retry_count": 5,
		}}
		require.True(t, account.IsPoolMode(), accountType)
		require.Equal(t, 5, account.GetPoolModeRetryCount(), accountType)
	}
	// 其他平台的 OAuth 账号仍不支持池模式。
	require.False(t, (&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{"pool_mode": true}}).IsPoolMode())
	require.False(t, (&Account{Platform: PlatformKiro, Type: AccountTypeOAuth}).IsPoolMode())
	require.False(t, (*Account)(nil).IsPoolMode())
}

func TestHandleKiroHTTPErrorPoolModeRetriesSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		status    int
		poolMode  bool
		retryable bool
	}{
		{name: "pool 429 retries same account", status: http.StatusTooManyRequests, poolMode: true, retryable: true},
		{name: "pool 403 retries same account", status: http.StatusForbidden, poolMode: true, retryable: true},
		{name: "pool 402 quota still switches", status: http.StatusPaymentRequired, poolMode: true, retryable: false},
		{name: "pool 500 not in default codes", status: http.StatusInternalServerError, poolMode: true, retryable: false},
		{name: "non-pool 429 switches", status: http.StatusTooManyRequests, poolMode: false, retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			account := &Account{ID: 69, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{"pool_mode": tc.poolMode}}
			svc := &GatewayService{accountRepo: &recordingKiroTempUnschedRepo{}}
			resp := newJSONResponse(tc.status, `{"message":"upstream said no"}`)

			err := svc.handleKiroHTTPError(context.Background(), resp, c, account, "claude-opus-5-5", []byte(`{"model":"claude-opus-5-5"}`))

			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover))
			require.Equal(t, tc.status, failover.StatusCode)
			require.Equal(t, tc.retryable, failover.RetryableOnSameAccount)
		})
	}
}

// 池模式下 429 不写账号级 Kiro 冷却，留给 handler 在同账号上重试。
func TestExecuteKiroUpstreamPoolModeSkips429Cooldown(t *testing.T) {
	prevSleep := kiroRetrySleep
	kiroRetrySleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { kiroRetrySleep = prevSleep })

	payload, err := createTestPayload("claude-sonnet-4-6")
	require.NoError(t, err)
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)

	for _, poolMode := range []bool{true, false} {
		store := &stubKiroCooldownStore{mark429TTL: time.Minute}
		svc := &GatewayService{
			httpUpstream:        &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusTooManyRequests, `{"message":"rate limited"}`)}},
			kiroCooldownStore:   store,
			tlsFPProfileService: &TLSFingerprintProfileService{},
		}
		account := &Account{ID: 69, Platform: PlatformKiro, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1,
			Credentials: map[string]any{"api_region": "us-east-1", "pool_mode": poolMode}}
		parsed := &ParsedRequest{Group: &Group{Platform: PlatformKiro, KiroEndpointMode: KiroEndpointModeQ}}

		resp, _, err := svc.executeKiroUpstreamWithParsed(context.Background(), account, parsed, payloadBytes, "claude-sonnet-4-6", "claude-sonnet-4-6", "test-token", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		if poolMode {
			require.Empty(t, store.mark429Keys)
			require.Empty(t, resp.Header.Get("x-kiro-cooldown"))
		} else {
			require.NotEmpty(t, store.mark429Keys)
			require.NotEmpty(t, resp.Header.Get("x-kiro-cooldown"))
		}
	}
}
