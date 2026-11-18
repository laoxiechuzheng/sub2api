//go:build unit

// C18/F01：用真实 Messages 转发与结算链验证失败用量；传输和账本均为合成。
package handler

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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	c18F01GroupID = int64(7101)
	c18F01Primary = int64(7102)
	c18F01Backup  = int64(7106)
	c18F01Public  = "gpt-4.1"
	c18F01Request = "gpt-4o"
	c18F01Sent    = "gpt-5.1"
	c18F01Echo    = "gpt-5.1-2025-11-13"
	c18F01UpID    = "fixture-upstream-primary"
	c18F01DownID  = "fixture-downstream"
	c18F01OKText  = "fixture backup answer"
	c18F01Balance = 100.0
	c18F01Usage   = `,"usage":{"input_tokens":123,"output_tokens":5,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":20,"cache_write_tokens_details":{"ttl_30m_tokens":7,"ttl_12h_tokens":13}}}`
	c18F01Cost    = 0.000142
	c18F01Write   = 0.000046
)

type c18F01Case struct {
	name                 string
	stream, cyber, usage bool
}

type c18F01Attempt struct {
	id            int64
	host, path    string
	method, model string
	stream, fast  bool
	valid         bool
}

// No HTTP client/delegate exists. The embedded nil interface makes an unexpected
// alternate transport method fail rather than leave the process.
type c18F01Transport struct {
	service.HTTPUpstream
	tc       c18F01Case
	attempts []c18F01Attempt // Do runs on the handler's forward stack.
}

func (u *c18F01Transport) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(io.LimitReader(req.Body, 32768))
	_ = req.Body.Close()
	u.attempts = append(u.attempts, c18F01Attempt{
		id: id, host: req.URL.Host, path: req.URL.Path, method: req.Method,
		model: gjson.GetBytes(b, "model").String(), stream: gjson.GetBytes(b, "stream").Type == gjson.True,
		fast: gjson.GetBytes(b, "service_tier").Exists(), valid: err == nil && gjson.ValidBytes(b),
	})
	idHeader := c18F01UpID
	h := http.Header{"Content-Type": {"text/event-stream"}}
	var event string
	switch id {
	case c18F01Primary:
		code, msg, usage := "rate_limit_exceeded", "fixture rate limit", ""
		if u.tc.cyber {
			code, msg = "cyber_policy", "fixture refusal"
		} else {
			h.Set("Retry-After", "7")
		}
		if u.tc.usage {
			usage = c18F01Usage
		}
		event = fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_fixture_first","status":"failed","model":%q,"error":{"code":%q,"message":%q}%s}}`, c18F01Echo, code, msg, usage)
	case c18F01Backup:
		idHeader = "fixture-upstream-backup"
		event = fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_fixture_backup","status":"completed","model":%q,"output":[{"id":"msg_fixture_backup","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":3,"output_tokens":4}}}`, c18F01Sent, c18F01OKText)
	default:
		return nil, fmt.Errorf("fixture unexpected account id %d", id)
	}
	h.Set("X-Request-Id", idHeader)
	h.Set("X-Fixture-Upstream-Request", idHeader)
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}, nil
}

// The existing scheduling fixture returns account values/copies. Construct
// distinct GroupIDs/Credentials/Extra maps for both rows; share no mutable maps.
type c18F01Accounts struct {
	openAIImagesFailoverAccountRepo
	rateLimited []int64
}

func (r *c18F01Accounts) SetRateLimited(_ context.Context, id int64, _ time.Time) error {
	r.rateLimited = append(r.rateLimited, id)
	return nil // leave eligibility/routing to the real scheduler + runtime blocker
}

// Existing probe records when the request and billing have both released the
// reservation; override the previously unused deduction for one shared cache.
type c18F01Cache struct{ *usageAttributionInflightCache }

func (c *c18F01Cache) DeductUserBalance(_ context.Context, _ int64, amount float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balance -= amount
	return nil
}

type c18F01Result struct {
	account int64
	result  *service.OpenAIForwardResult
	err     error
}

func TestC18F01MessagesPartialUsageBilling(t *testing.T) {
	for _, tc := range []c18F01Case{
		{name: "buffered_retryable", usage: true},
		{name: "buffered_cyber", cyber: true, usage: true},
		{name: "streaming_cyber", stream: true, cyber: true, usage: true},
		{name: "buffered_zero_still_switches"},
		{name: "buffered_cyber_zero", cyber: true},
		{name: "streaming_cyber_zero", stream: true, cyber: true},
	} {
		t.Run(tc.name, func(t *testing.T) { c18F01Run(t, tc) })
	}
}

func c18F01Run(t *testing.T, tc c18F01Case) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 3
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60, FailClosedOnUnpriced: true}
	makeAccount := func(id int64, priority int) service.Account {
		return service.Account{
			ID: id, GroupIDs: []int64{c18F01GroupID}, Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Priority: priority, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
			Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://c18.example.invalid", "api_protocol": service.APIProtocolResponses,
				"model_mapping": map[string]any{c18F01Request: c18F01Sent}},
			Extra: map[string]any{service.AccountExtraUpstreamRequestIDHeader: "X-Fixture-Upstream-Request"},
		}
	}
	accounts := &c18F01Accounts{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{
		accounts: []service.Account{makeAccount(c18F01Backup, 20), makeAccount(c18F01Primary, 10)},
	}}
	upstream := &c18F01Transport{tc: tc}
	repo := &usageAttributionRepo{created: make(chan usageAttributionReceipt, 16), diagnostics: make(chan *requestdiagnostic.Snapshot, 4),
		billing: &usageAttributionBillingRepo{balance: c18F01Balance}}
	cache := &c18F01Cache{&usageAttributionInflightCache{handlerInflightCache: newHandlerInflightCache(c18F01Balance), repo: repo,
		released: make(chan usageAttributionReservationRelease, 4)}}
	billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	input, output, read, write, long := 2e-6, 8e-6, 0.5e-6, 1e-6, 3e-6
	channels := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{ID: 7103, Name: "fixture", Status: service.StatusActive, GroupIDs: []int64{c18F01GroupID}, BillingModelSource: service.BillingModelSourceRequested,
			ModelPricing: []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{c18F01Public, c18F01Request, c18F01Sent}, BillingMode: service.BillingModeToken,
				InputPrice: &input, OutputPrice: &output, CacheReadPrice: &read, CacheWritePrice: &write, CacheWrite12hPrice: &long}}}},
		groupPlatforms: map[int64]string{c18F01GroupID: service.PlatformOpenAI},
	}, nil, nil, nil, nil)
	pricing := service.NewBillingService(cfg, nil)
	rate := service.NewRateLimitService(accounts, repo, cfg, nil, nil)
	gateway := service.NewOpenAIGatewayService(accounts, repo, repo.billing, nil, nil, nil, nil, cfg, nil, nil,
		pricing, rate, billingCache, upstream, &service.DeferredService{}, nil, nil,
		service.NewModelPricingResolver(channels, pricing), channels, nil, nil, nil)
	// 网关没有真实转发 delegate、DB、邮件或 OAuth；moderation 只做事后记录。
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, newFailedFrameCodeRule("rate_limit_exceeded"), nil, nil, cfg)
	moderation := &usageAttributionBlockingModeration{entered: make(chan struct{}), release: make(chan struct{})}
	settings := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:      "true",
		service.SettingKeyContentModerationConfig: `{"enabled":false,"all_groups":true,"base_url":"https://moderation.example.invalid","api_keys":[]}`,
	}}
	h.contentModerationService = service.NewContentModerationService(settings, moderation, nil, nil, nil, nil, nil, nil)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(moderation.release) }) }
	t.Cleanup(unblock)

	var results []c18F01Result
	h.forwardMessages = func(ctx context.Context, c *gin.Context, account *service.Account, body []byte, pk, mapped string) (*service.OpenAIForwardResult, error) {
		res, err := gateway.ForwardAsAnthropic(ctx, c, account, body, pk, mapped)
		results = append(results, c18F01Result{account.ID, res, err})
		return res, err
	}
	groupID := c18F01GroupID
	key := &service.APIKey{ID: 7104, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true,
			SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1, PeakRateEnabled: false},
		User: &service.User{ID: 7105, Status: service.StatusActive}}
	trace := &service.CompositeRouteTrace{RouteID: 7142, RequestKind: service.CompositeRouteRequestKindCompaction}
	trace.SetFallbackChain("fixture primary -> fixture backup")
	cap := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { repo.diagnostics <- s })
	ctx := service.WithCompositeRouteTrace(context.Background(), trace)
	ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, c18F01Public)
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "fixture-client")
	ctx = requestdiagnostic.WithCapture(ctx, cap)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(
		`{"model":%q,"stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"fixture input"}]}`, c18F01Request, tc.stream))).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Header("X-Request-Id", c18F01DownID)
	c.Set(string(middleware.ContextKeyAPIKey), key)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID})
	h.Messages(c)

	// Setup must be established before a failure may be called RED. No provider
	// payload, HTTP headers, fake key, or raw error is included in test receipts.
	require.NotEmpty(t, upstream.attempts, "F01-SETUP no upstream attempt")
	require.Len(t, results, len(upstream.attempts), "F01-SETUP Forward/transport cardinality")
	require.Equal(t, c18F01Primary, upstream.attempts[0].id, "F01-SETUP wrong first account")
	ids := make([]int64, 0, len(upstream.attempts))
	for i, a := range upstream.attempts {
		ids = append(ids, a.id)
		require.True(t, a.valid && a.stream && !a.fast && a.host == "c18.example.invalid" &&
			a.path == "/v1/responses" && a.method == http.MethodPost && a.model == c18F01Sent && results[i].account == a.id,
			"F01-SETUP unexpected outbound binding")
	}
	require.NotNil(t, results[0].err, "F01-SETUP first Forward did not fail")
	require.NotNil(t, service.InflightReservationFromContext(c.Request.Context()), "F01-SETUP no real reservation")

	if tc.cyber {
		select {
		case <-moderation.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("F01-SETUP Cyber task did not reach moderation gate")
		}
		assert.Equal(t, []int64{c18F01Primary}, ids, "F01-CYBER-REPLAY")
		mark := service.GetOpsCyberPolicy(c)
		require.NotNil(t, mark, "F01-SETUP genuine Cyber mark not set")
		require.Equal(t, "cyber_policy", mark.Code, "F01-SETUP wrong failure class")
		if tc.usage {
			require.Equal(t, 123, mark.UpstreamInTok, "F01-SETUP source reported input")
			require.Equal(t, 5, mark.UpstreamOutTok, "F01-SETUP source reported output")
		}
		require.True(t, trace.UsageCommitted(), "F01-SETUP Cyber did not submit before handler return")
		assert.Equal(t, 1, cache.count(), "F01-CYBER reservation released before billing")
		assert.Zero(t, repo.calls.Load(), "F01-CYBER ordinary usage raced dedicated task")
		repo.billing.mu.Lock()
		beforeApply := len(repo.billing.commands)
		repo.billing.mu.Unlock()
		assert.Zero(t, beforeApply, "F01-CYBER premature or duplicate Apply")
		unblock()
	} else {
		select {
		case <-moderation.entered:
			t.Error("F01-ORDINARY unexpectedly entered Cyber task")
		default:
		}
		var failover *service.UpstreamFailoverError
		require.ErrorAs(t, results[0].err, &failover, "F01-SETUP ordinary fixture did not enter original typed failure")
		require.Equal(t, http.StatusTooManyRequests, failover.StatusCode, "F01-SETUP ordinary semantic status")
		require.NotNil(t, failover.StreamFailedFrame, "F01-SETUP failed-frame rule input missing")
		require.Nil(t, service.GetOpsCyberPolicy(c), "F01-SETUP ordinary fixture marked Cyber")
		require.Equal(t, []int64{c18F01Primary}, accounts.rateLimited, "F01-SETUP semantic 429 side effect missing/duplicated")
	}

	// Most importantly, observe the old backup bill instead of timing out waiting
	// for a never-submitted primary bill. For another failure shape, a true release
	// with zero writes is an assertion failure, not a missing-harness timeout.
	var release usageAttributionReservationRelease
	select {
	case release = <-cache.released:
	case <-time.After(3 * time.Second):
		t.Fatal("F01-SETUP reservation completion not observed")
	}
	if !tc.cyber && tc.usage {
		assert.Equal(t, []int64{c18F01Primary}, ids, "F01-REPLAY consumed primary must finish without backup")
		assert.NotContains(t, rec.Body.String(), c18F01OKText, "F01-REPLAY backup answer became visible")
		if len(results) == 2 && ids[1] == c18F01Backup {
			require.NoError(t, results[1].err, "F01-SETUP replayed backup did not execute normally")
			require.Contains(t, rec.Body.String(), c18F01OKText, "F01-SETUP replay receipt missing actual backup output")
		}
		assert.Equal(t, http.StatusTeapot, rec.Code, "F01-ENVELOPE old exhausted mapping")
		assert.Equal(t, "7", rec.Header().Get("Retry-After"), "F01-ENVELOPE original retry header")
		assert.True(t, gjson.Valid(rec.Body.String()), "F01-ENVELOPE exactly one JSON value")
		assert.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String(), "F01-ENVELOPE root")
		assert.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String(), "F01-ENVELOPE type")
		assert.Equal(t, "fixture rate limit", gjson.Get(rec.Body.String(), "error.message").String(), "F01-ENVELOPE message")
	} else if !tc.cyber {
		require.Equal(t, []int64{c18F01Primary, c18F01Backup}, ids, "F01-ZERO ordinary fallback must actually send backup")
		require.NoError(t, results[1].err, "F01-ZERO backup Forward must succeed")
		require.Equal(t, http.StatusOK, rec.Code, "F01-ZERO backup status")
		require.Contains(t, rec.Body.String(), c18F01OKText, "F01-ZERO backup response absent")
	}
	if tc.cyber {
		assert.Empty(t, accounts.rateLimited, "F01-CYBER account side effect")
		assert.Len(t, moderation.logSnapshot(), 1, "F01-CYBER moderation count")
		body := rec.Body.String()
		if tc.stream {
			assert.Equal(t, http.StatusOK, rec.Code, "F01-CYBER SSE status")
			assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream"), "F01-CYBER SSE content type")
			assert.Equal(t, 1, strings.Count(body, "event: error\n"), "F01-CYBER must write one error event")
			for _, ln := range strings.Split(body, "\n") {
				if data, ok := strings.CutPrefix(ln, "data: "); ok {
					assert.Equal(t, "error", gjson.Get(data, "type").String(), "F01-CYBER SSE root")
					assert.Equal(t, "invalid_request_error", gjson.Get(data, "error.type").String(), "F01-CYBER SSE type")
					assert.Equal(t, "fixture refusal", gjson.Get(data, "error.message").String(), "F01-CYBER SSE message")
				}
			}
			assert.NotContains(t, body, "[DONE]", "F01-CYBER unexpected DONE suffix")
		} else {
			assert.Equal(t, http.StatusBadRequest, rec.Code, "F01-CYBER JSON status")
			assert.True(t, gjson.Valid(body), "F01-CYBER response must be exactly one JSON value")
			assert.Equal(t, "error", gjson.Get(body, "type").String(), "F01-CYBER JSON root")
			assert.Equal(t, "invalid_request_error", gjson.Get(body, "error.type").String(), "F01-CYBER JSON type")
			assert.Equal(t, "fixture refusal", gjson.Get(body, "error.message").String(), "F01-CYBER JSON message")
		}
	}

	repo.billing.mu.Lock()
	commands := append([]service.UsageBillingCommand(nil), repo.billing.commands...)
	balance := repo.billing.balance
	repo.billing.mu.Unlock()
	billAccount := int64(0)
	if len(commands) > 0 {
		billAccount = commands[0].AccountID
	}
	t.Logf("F01-RECEIPT attempts=%v code=%d limited=%v usage=%d apply=%d billed_account=%d balance=%.9f trace=%t", ids, rec.Code,
		accounts.rateLimited, release.usageWrites, release.charges, billAccount, balance, trace.UsageCommitted())
	label := "F01-ZERO"
	if tc.usage {
		label = "F01-USAGE"
	}
	assert.True(t, trace.UsageCommitted(), label+" trace not committed")
	assert.EqualValues(t, 1, cache.releases.Load(), label+" reservation release must be unique")
	assert.Zero(t, cache.count(), label+" reservation not drained")
	require.EqualValues(t, 1, release.usageWrites, label+" one usage Create before release")
	require.Equal(t, 1, release.charges, label+" one Apply before release")
	require.EqualValues(t, 1, repo.calls.Load(), label+" one usage Create overall")
	require.Len(t, commands, 1, label+" one non-deduplicating Apply overall")
	var got usageAttributionReceipt
	select {
	case got = <-repo.created:
	default:
		t.Fatal("F01-SETUP receipt absent after recorded Create")
	}
	log, cmd := got.log, commands[0]
	t.Logf("F01-RECEIPT buckets=%d,%d,%d,%d,%d,%d actual=%.9f command=%.9f full=%t",
		log.InputTokens, log.OutputTokens, log.CacheReadTokens, log.CacheCreationTokens, log.CacheCreation30mTokens, log.CacheCreation12hTokens,
		log.ActualCost, cmd.BalanceCost, results[0].result != nil)
	assert.Nil(t, got.ctxErr, label+" detached task canceled")
	assert.Same(t, cap, requestdiagnostic.FromContext(got.ctx), label+" lost diagnostic capture")
	assert.Equal(t, &trace.RouteID, log.CompositeRouteID, label+" lost composite route")
	chain := trace.FallbackChain()
	assert.Equal(t, &chain, log.CompositeFallbackChain, label+" lost fallback chain")
	assert.Equal(t, "client:fixture-client", log.RequestID, label+" canonical id changed")
	assert.Equal(t, log.RequestID, cmd.RequestID, label+" command id mismatch")
	assert.Equal(t, key.ID, cmd.APIKeyID, label+" command API key id")
	assert.Equal(t, key.User.ID, cmd.UserID, label+" command user id")
	assert.NotEmpty(t, cmd.RequestPayloadHash, label+" missing fingerprint")
	assert.Equal(t, c18F01Public, log.RequestedModel, label+" public model lost")
	if tc.cyber {
		assert.Equal(t, service.RequestTypeCyberBlocked, log.RequestType, label+" Cyber and ordinary mixed")
	} else {
		assert.NotEqual(t, service.RequestTypeCyberBlocked, log.RequestType, label+" ordinary request mislabeled")
	}

	wantAccount, wantInput, wantOutput, wantCost := c18F01Primary, 0, 0, 0.0
	wantRead, wantWrite, wantShort, wantLong := 0, 0, 0, 0
	if tc.usage {
		wantInput, wantOutput, wantCost = 3, 5, c18F01Cost
		wantRead, wantWrite, wantShort, wantLong = 100, 20, 7, 13
		assert.InDelta(t, c18F01Write, log.CacheCreationCost, 1e-12, "F01-USAGE TTL cost")
		if assert.NotNil(t, results[0].result, "F01-USAGE full result missing") {
			r := results[0].result
			assert.True(t, r.HasObservedUsage(), "F01-USAGE observed gate")
			assert.Equal(t, service.OpenAIUsage{InputTokens: 123, OutputTokens: 5, CacheReadInputTokens: 100,
				CacheCreationInputTokens: 20, CacheCreation30mInputTokens: 7, CacheCreation12hInputTokens: 13}, r.Usage, "F01-USAGE Forward snapshot buckets")
			assert.Equal(t, c18F01Sent, r.UpstreamModel, "F01-USAGE sent model")
			assert.Equal(t, c18F01Echo, r.UpstreamResponseModel, "F01-USAGE observed model")
		}
		assert.Equal(t, c18F01Request, log.Model, "F01-USAGE original forwarded model audit")
		sent, echo, upID := c18F01Sent, c18F01Echo, c18F01UpID
		assert.Equal(t, &sent, log.UpstreamModel, "F01-USAGE mapped model audit")
		assert.Equal(t, &echo, log.UpstreamResponseModel, "F01-USAGE observed model audit")
		assert.Equal(t, &upID, log.UpstreamRequestID, "F01-USAGE configured upstream id audit")
		assert.Empty(t, cmd.ServiceTier, "F01-USAGE accounting tier must remain base")
		assert.Empty(t, cmd.ReasoningEffort, "F01-USAGE effective effort must not affect failed price")
	} else {
		assert.Nil(t, results[0].result, "F01-ZERO old nil contract")
		if !tc.cyber {
			wantAccount, wantInput, wantOutput, wantCost = c18F01Backup, 3, 4, 0.000038
		}
	}
	assert.Equal(t, wantAccount, log.AccountID, label+" usage account")
	assert.Equal(t, wantAccount, cmd.AccountID, label+" command account")
	assert.Equal(t, wantInput, log.InputTokens, label+" actual input")
	assert.Equal(t, wantOutput, log.OutputTokens, label+" output")
	assert.Equal(t, wantRead, log.CacheReadTokens, label+" cache read")
	assert.Equal(t, wantWrite, log.CacheCreationTokens, label+" cache write")
	assert.Equal(t, wantShort, log.CacheCreation30mTokens, label+" 30m bucket")
	assert.Equal(t, wantLong, log.CacheCreation12hTokens, label+" 12h bucket")
	assert.Equal(t, wantInput, cmd.InputTokens, label+" command input")
	assert.Equal(t, wantOutput, cmd.OutputTokens, label+" command output")
	assert.Equal(t, wantRead, cmd.CacheReadTokens, label+" command cache read")
	assert.Equal(t, wantWrite, cmd.CacheCreationTokens, label+" command cache write")
	assert.InDelta(t, wantCost, log.ActualCost, 1e-12, label+" usage amount")
	assert.InDelta(t, wantCost, cmd.BalanceCost, 1e-12, label+" amount sent to Apply")
	assert.InDelta(t, c18F01Balance-wantCost, balance, 1e-12, label+" fake ledger balance")
	cacheBalance, err := cache.GetUserBalance(ctx, key.User.ID)
	require.NoError(t, err, "F01-SETUP in-memory cache")
	assert.InDelta(t, balance, cacheBalance, 1e-12, label+" balance cache not synchronized before release")
	inbound, outbound := "/v1/messages", "/v1/responses"
	assert.Equal(t, &inbound, log.InboundEndpoint, label+" inbound attribution")
	assert.Equal(t, &outbound, log.UpstreamEndpoint, label+" outbound attribution")
	cap.Finish(rec.Code)
	select {
	case snap := <-repo.diagnostics:
		assert.Equal(t, log.RequestID, snap.Binding.CanonicalID, label+" diagnostic canonical id")
		assert.Equal(t, log.APIKeyID, snap.Binding.APIKeyID, label+" diagnostic key")
	case <-time.After(3 * time.Second):
		t.Error("F01-SETUP diagnostic receipt not observed")
	}
}
