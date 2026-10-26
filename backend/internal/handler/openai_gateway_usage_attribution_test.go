//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type usageAttributionReceipt struct {
	log    *service.UsageLog
	ctx    context.Context
	ctxErr error
}

type usageAttributionRepo struct {
	service.UsageLogRepository
	created     chan usageAttributionReceipt
	diagnostics chan *requestdiagnostic.Snapshot
	billing     *usageAttributionBillingRepo
	calls       atomic.Int32
}

func (r *usageAttributionRepo) Create(ctx context.Context, log *service.UsageLog) (bool, error) {
	r.calls.Add(1)
	log.ID = 718
	log.CreatedAt = time.Now().UTC()
	r.created <- usageAttributionReceipt{log: log, ctx: ctx, ctxErr: ctx.Err()}
	return true, nil
}

type usageAttributionUpstream struct {
	service.HTTPUpstream
	body    string
	calls   atomic.Int32
	account *service.Account
}

type usageAttributionBillingRepo struct {
	service.UsageBillingRepository
	mu       sync.Mutex
	commands []service.UsageBillingCommand
	balance  float64
}

func (r *usageAttributionBillingRepo) Apply(_ context.Context, cmd *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Deliberately no request-ID deduplication: duplicate handler submissions must fail the test.
	r.commands = append(r.commands, *cmd)
	r.balance -= cmd.BalanceCost
	remaining := r.balance
	return &service.UsageBillingApplyResult{Applied: true, NewBalance: &remaining}, nil
}

func (u *usageAttributionUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"upstream-attribution"}}, Body: io.NopCloser(strings.NewReader(u.body))}, nil
}

func newUsageAttributionHandler(t *testing.T, upstream *usageAttributionUpstream, pool *service.UsageRecordWorkerPool, endpoint string, passthrough bool) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder, *usageAttributionRepo, *service.CompositeRouteTrace) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	groupID := int64(7101)
	account := service.Account{ID: 7102, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://usage.example.invalid", "api_protocol": service.APIProtocolResponses}, Extra: map[string]any{"openai_passthrough": passthrough}}
	repo := &usageAttributionRepo{created: make(chan usageAttributionReceipt, 4), diagnostics: make(chan *requestdiagnostic.Snapshot, 1)}
	repo.billing = &usageAttributionBillingRepo{balance: 100}
	upstream.account = &account
	inputPrice, outputPrice := 2e-6, 8e-6
	channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{ID: 7103, Name: "attribution", Status: service.StatusActive, GroupIDs: []int64{groupID}, BillingModelSource: service.BillingModelSourceRequested,
			ModelPricing: []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{"gpt-4.1"}, BillingMode: service.BillingModeToken, InputPrice: &inputPrice, OutputPrice: &outputPrice}}}},
		groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
	}, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	backup := account
	backup.ID, backup.Priority = 7106, 1
	pricing := service.NewBillingService(cfg, nil)
	gw := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: []service.Account{account, backup}}, repo, repo.billing, nil, nil, nil, nil, cfg, nil, nil,
		pricing, nil, billing, upstream, &service.DeferredService{}, nil, nil, service.NewModelPricingResolver(channelSvc, pricing), channelSvc, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), pool, nil, nil, nil, cfg)
	apiKey := &service.APIKey{ID: 7104, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowMessagesDispatch: true, RateMultiplier: 1}, User: &service.User{ID: 7105, Status: service.StatusActive}}
	body := "{\"model\":\"gpt-4o\",\"stream\":true,\"input\":\"hello\"}"
	if endpoint == "messages" {
		body = "{\"model\":\"gpt-4o\",\"stream\":true,\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
	}
	trace := &service.CompositeRouteTrace{RouteID: 7142, RequestKind: service.CompositeRouteRequestKindCompaction}
	trace.SetFallbackChain("primary (429) -> backup")
	ctx := service.WithCompositeRouteTrace(context.Background(), trace)
	ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, "gpt-4.1")
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "attribution-client")
	ctx = requestdiagnostic.WithCapture(ctx, requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, func(snapshot *requestdiagnostic.Snapshot) { repo.diagnostics <- snapshot }))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID})
	return h, c, rec, repo, trace
}

const usageAttributionDelta = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
const usageAttributionCompleted = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_attribution\",\"status\":\"completed\",\"model\":\"gpt-4o\",\"output\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}}\n\n"
const usageAttributionFailed = "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_attribution\",\"status\":\"failed\",\"model\":\"gpt-4o\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream interrupted\"},\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}}\n\n"

func waitUsageAttribution(t *testing.T, repo *usageAttributionRepo) usageAttributionReceipt {
	t.Helper()
	select {
	case got := <-repo.created:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("usage record was not persisted")
		return usageAttributionReceipt{}
	}
}

func TestOpenAIUsageAttributionSnapshotsGinRequestBeforeWorker(t *testing.T) {
	for _, endpoint := range []string{"responses", "messages"} {
		for _, partial := range []bool{false, true} {
			name := endpoint + "/success"
			terminal := usageAttributionCompleted
			if partial {
				name = endpoint + "/partial"
				terminal = usageAttributionFailed
			}
			t.Run(name, func(t *testing.T) {
				pool := newUsageRecordTestPool(t)
				blocked, release := make(chan struct{}), make(chan struct{})
				var released atomic.Bool
				unblock := func() {
					if released.CompareAndSwap(false, true) {
						close(release)
					}
				}
				t.Cleanup(unblock)
				pool.Submit(func(context.Context) { close(blocked); <-release })
				<-blocked
				upstream := &usageAttributionUpstream{body: usageAttributionDelta + terminal}
				h, c, rec, repo, trace := newUsageAttributionHandler(t, upstream, pool, endpoint, false)
				capture := requestdiagnostic.FromContext(c.Request.Context())
				if endpoint == "messages" {
					h.Messages(c)
				} else {
					h.Responses(c)
				}
				require.EqualValues(t, 1, upstream.calls.Load(), rec.Body.String())
				require.True(t, trace.UsageCommitted(), "usage must be marked before returning to fallback")
				c.Request = httptest.NewRequest(http.MethodPost, "/reused", nil).WithContext(context.WithValue(context.Background(), ctxkey.RequestedPublicModel, "wrong-reused-model"))
				unblock()
				got := waitUsageAttribution(t, repo)
				require.Equal(t, "gpt-4.1", got.log.RequestedModel)
				require.Equal(t, 11, got.log.InputTokens)
				require.Equal(t, 3, got.log.OutputTokens)
				require.InDelta(t, 11*2e-6+3*8e-6, got.log.TotalCost, 1e-12)
				require.Equal(t, &trace.RouteID, got.log.CompositeRouteID)
				require.Same(t, capture, requestdiagnostic.FromContext(got.ctx))
				assertUsageAttributionDiagnosticBinding(t, capture, repo, got.log)
				require.EqualValues(t, 1, repo.calls.Load())
			})
		}
	}
}

func assertUsageAttributionDiagnosticBinding(t *testing.T, capture *requestdiagnostic.Capture, repo *usageAttributionRepo, log *service.UsageLog) {
	t.Helper()
	capture.Finish(http.StatusOK)
	select {
	case snapshot := <-repo.diagnostics:
		require.Equal(t, log.RequestID, snapshot.Binding.CanonicalID)
		require.Equal(t, log.APIKeyID, snapshot.Binding.APIKeyID)
	case <-time.After(3 * time.Second):
		t.Fatal("diagnostic was not bound to the persisted canonical usage key")
	}
}

type usageAttributionBlockingModeration struct {
	contentModerationHandlerTestRepo
	entered, release chan struct{}
}

func (r *usageAttributionBlockingModeration) CreateLog(ctx context.Context, log *service.ContentModerationLog) error {
	close(r.entered)
	select {
	case <-r.release:
		return r.contentModerationHandlerTestRepo.CreateLog(ctx, log)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestOpenAICyberUsagePreservesTraceAndMarksBeforeAsyncWrite(t *testing.T) {
	upstream := &usageAttributionUpstream{body: strings.ReplaceAll(usageAttributionFailed, "server_error", "cyber_policy")}
	h, c, _, repo, trace := newUsageAttributionHandler(t, upstream, nil, "responses", true)
	capture := requestdiagnostic.FromContext(c.Request.Context())
	moderation := &usageAttributionBlockingModeration{entered: make(chan struct{}), release: make(chan struct{})}
	settings := &contentModerationHandlerSettingRepo{values: map[string]string{service.SettingKeyRiskControlEnabled: "true"}}
	h.contentModerationService = service.NewContentModerationService(settings, moderation, nil, nil, nil, nil, nil, nil)
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(moderation.release)
		}
	}
	t.Cleanup(unblock)
	h.Responses(c)
	select {
	case <-moderation.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cyber event did not reach the async moderation step")
	}
	require.Zero(t, repo.calls.Load(), "billing is still blocked before its write")
	require.True(t, trace.UsageCommitted(), "cyber billing submission must synchronously prevent replay")
	c.Request = httptest.NewRequest(http.MethodPost, "/reused", nil)
	unblock()
	got := waitUsageAttribution(t, repo)
	require.Equal(t, &trace.RouteID, got.log.CompositeRouteID)
	require.Equal(t, service.CompositeRouteRequestKindCompaction, *got.log.CompositeRouteKind)
	require.Equal(t, trace.FallbackChain(), *got.log.CompositeFallbackChain)
	require.Equal(t, service.RequestTypeCyberBlocked, got.log.RequestType)
	require.Equal(t, 11, got.log.InputTokens)
	require.Equal(t, 3, got.log.OutputTokens)
	require.Positive(t, got.log.TotalCost)
	require.Same(t, capture, requestdiagnostic.FromContext(got.ctx))
	assertUsageAttributionDiagnosticBinding(t, capture, repo, got.log)
	require.EqualValues(t, 1, upstream.calls.Load())
	// A second handler observation of the same mark must stay idempotent.
	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-4o", true, nil, service.ChannelUsageFields{}, "")
	require.EqualValues(t, 1, repo.calls.Load())
}

func TestOpenAIUsagePartialBeforeOutputIsTerminal(t *testing.T) {
	for _, endpoint := range []string{"responses", "messages"} {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/passthrough=%t", endpoint, passthrough), func(t *testing.T) {
				upstream := &usageAttributionUpstream{body: usageAttributionFailed}
				h, c, rec, repo, trace := newUsageAttributionHandler(t, upstream, nil, endpoint, passthrough)
				if endpoint == "messages" {
					h.Messages(c)
				} else {
					h.Responses(c)
				}
				require.EqualValues(t, 1, upstream.calls.Load(), "a consumed failure must not retry the backup account: %s", rec.Body.String())
				require.True(t, trace.UsageCommitted())
				got := waitUsageAttribution(t, repo)
				require.Equal(t, 11, got.log.InputTokens)
				require.Equal(t, 3, got.log.OutputTokens)
				require.InDelta(t, 11*2e-6+3*8e-6, got.log.TotalCost, 1e-12)
				require.EqualValues(t, 1, repo.calls.Load())
			})
		}
	}
}

func TestOpenAIUsageImageCacheOnlyPartialErrorIsTerminal(t *testing.T) {
	for _, endpoint := range []string{"responses", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream := &usageAttributionUpstream{}
			h, c, rec, repo, trace := newUsageAttributionHandler(t, upstream, nil, endpoint, false)
			var attemptedAccounts []int64
			// Inject the parsed Forward contract; the generic SSE parser does not
			// currently produce an image-cache-only wire usage result.
			forward := func(_ context.Context, _ *gin.Context, account *service.Account, _ []byte) (*service.OpenAIForwardResult, error) {
				attemptedAccounts = append(attemptedAccounts, account.ID)
				return &service.OpenAIForwardResult{
					RequestID: "image-cache-only-partial", Model: "gpt-4o", Stream: true,
					Usage: service.OpenAIUsage{ImageCacheReadTokens: 7},
				}, &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}
			}
			if endpoint == "messages" {
				h.forwardMessages = func(ctx context.Context, c *gin.Context, account *service.Account, body []byte, _, _ string) (*service.OpenAIForwardResult, error) {
					return forward(ctx, c, account, body)
				}
				h.Messages(c)
			} else {
				h.forwardResponses = forward
				h.Responses(c)
			}
			require.Len(t, attemptedAccounts, 1, "image-cache consumption must prevent account failover: %s", rec.Body.String())
			require.True(t, trace.UsageCommitted())
			got := waitUsageAttribution(t, repo)
			require.Equal(t, "gpt-4.1", got.log.RequestedModel)
			require.Zero(t, got.log.InputTokens)
			require.Zero(t, got.log.OutputTokens)
			require.Zero(t, got.log.CacheReadTokens)
			require.Equal(t, 7, got.log.ImageSizeBreakdown["image_cache_read_tokens"])
			require.Equal(t, &trace.RouteID, got.log.CompositeRouteID)
			require.EqualValues(t, 1, repo.calls.Load())
		})
	}
}

func TestOpenAIUsageZeroConsumptionStillRetries(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			body := strings.ReplaceAll(strings.ReplaceAll(usageAttributionFailed, "\"input_tokens\":11", "\"input_tokens\":0"), "\"output_tokens\":3", "\"output_tokens\":0")
			upstream := &usageAttributionUpstream{body: body}
			h, c, _, repo, trace := newUsageAttributionHandler(t, upstream, nil, "responses", passthrough)
			h.Responses(c)
			require.EqualValues(t, 2, upstream.calls.Load(), "zero consumption may try the second available account")
			require.False(t, trace.UsageCommitted())
			require.Zero(t, repo.calls.Load())
		})
	}
}

func TestOpenAICyberPartialResultBilledOnce(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			body := strings.ReplaceAll(usageAttributionFailed, "server_error", "cyber_policy")
			// Verify the service's new non-nil partial contract on this exact wire fixture.
			contractUpstream := &usageAttributionUpstream{body: body}
			contractHandler, contractContext, _, _, _ := newUsageAttributionHandler(t, contractUpstream, nil, "responses", passthrough)
			requestBody, err := io.ReadAll(contractContext.Request.Body)
			require.NoError(t, err)
			partial, err := contractHandler.gatewayService.Forward(contractContext.Request.Context(), contractContext, contractUpstream.account, requestBody)
			require.Error(t, err)
			require.NotNil(t, partial, "cyber response.failed with observed usage must return a partial result")
			require.Equal(t, 11, partial.Usage.InputTokens)
			require.Equal(t, 3, partial.Usage.OutputTokens)
			require.NotNil(t, service.GetOpsCyberPolicy(contractContext))

			upstream := &usageAttributionUpstream{body: body}
			h, c, rec, repo, trace := newUsageAttributionHandler(t, upstream, nil, "responses", passthrough)
			h.cfg.RunMode = config.RunModeStandard
			preflightBilling := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, h.cfg, nil)
			t.Cleanup(preflightBilling.Stop)
			h.billingCacheService = preflightBilling
			moderation := &usageAttributionBlockingModeration{entered: make(chan struct{}), release: make(chan struct{})}
			settings := &contentModerationHandlerSettingRepo{values: map[string]string{service.SettingKeyRiskControlEnabled: "true"}}
			h.contentModerationService = service.NewContentModerationService(settings, moderation, nil, nil, nil, nil, nil, nil)
			var released atomic.Bool
			unblock := func() {
				if released.CompareAndSwap(false, true) {
					close(moderation.release)
				}
			}
			t.Cleanup(unblock)
			h.Responses(c)
			select {
			case <-moderation.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("cyber event did not reach the async moderation step")
			}
			require.EqualValues(t, 1, upstream.calls.Load(), rec.Body.String())
			require.True(t, trace.UsageCommitted())
			require.Zero(t, repo.calls.Load(), "the ordinary partial task must not run while dedicated cyber billing is blocked")
			unblock()
			got := waitUsageAttribution(t, repo)
			require.EqualValues(t, 1, repo.calls.Load())
			require.Equal(t, service.RequestTypeCyberBlocked, got.log.RequestType)
			require.Equal(t, "gpt-4.1", got.log.RequestedModel)
			require.Equal(t, 11, got.log.InputTokens)
			require.Equal(t, 3, got.log.OutputTokens)
			const expected = 11*2e-6 + 3*8e-6
			require.InDelta(t, expected, got.log.ActualCost, 1e-12)
			repo.billing.mu.Lock()
			commands := append([]service.UsageBillingCommand(nil), repo.billing.commands...)
			balance := repo.billing.balance
			repo.billing.mu.Unlock()
			require.Len(t, commands, 1, "the handler must not rely on duplicate request-ID suppression")
			require.Equal(t, 11, commands[0].InputTokens)
			require.Equal(t, 3, commands[0].OutputTokens)
			require.InDelta(t, expected, commands[0].BalanceCost, 1e-12)
			require.InDelta(t, 100-expected, balance, 1e-12)
		})
	}
}

type usageAttributionReservationRelease struct {
	usageWrites int32
	charges     int
}

type usageAttributionInflightCache struct {
	*handlerInflightCache
	repo     *usageAttributionRepo
	releases atomic.Int32
	released chan usageAttributionReservationRelease
}

func (c *usageAttributionInflightCache) ReleaseInflightBalance(ctx context.Context, userID int64, id string) error {
	err := c.handlerInflightCache.ReleaseInflightBalance(ctx, userID, id)
	c.repo.billing.mu.Lock()
	charges := len(c.repo.billing.commands)
	c.repo.billing.mu.Unlock()
	c.releases.Add(1)
	c.released <- usageAttributionReservationRelease{usageWrites: c.repo.calls.Load(), charges: charges}
	return err
}

func TestOpenAICyberPartialResultHoldsReservationUntilBillingCompletes(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			upstream := &usageAttributionUpstream{body: strings.ReplaceAll(usageAttributionFailed, "server_error", "cyber_policy")}
			h, c, rec, repo, trace := newUsageAttributionHandler(t, upstream, nil, "responses", passthrough)
			h.cfg.RunMode = config.RunModeStandard
			h.cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
			cache := &usageAttributionInflightCache{handlerInflightCache: newHandlerInflightCache(100), repo: repo, released: make(chan usageAttributionReservationRelease, 4)}
			billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, h.cfg, nil)
			t.Cleanup(billing.Stop)
			h.billingCacheService = billing
			requestCtx, cancel := context.WithCancel(c.Request.Context())
			t.Cleanup(cancel)
			// Use the fixture's explicitly priced model so the real handler reserves balance.
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","stream":true,"input":"hello","max_output_tokens":64}`)).WithContext(requestCtx)
			c.Request.Header.Set("Content-Type", "application/json")
			capture := requestdiagnostic.FromContext(requestCtx)
			moderation := &usageAttributionBlockingModeration{entered: make(chan struct{}), release: make(chan struct{})}
			settings := &contentModerationHandlerSettingRepo{values: map[string]string{service.SettingKeyRiskControlEnabled: "true"}}
			h.contentModerationService = service.NewContentModerationService(settings, moderation, nil, nil, nil, nil, nil, nil)
			var released atomic.Bool
			unblock := func() {
				if released.CompareAndSwap(false, true) {
					close(moderation.release)
				}
			}
			t.Cleanup(unblock)
			h.Responses(c)
			select {
			case <-moderation.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("cyber event did not reach the async moderation step")
			}
			reservation := service.InflightReservationFromContext(c.Request.Context())
			require.NotNil(t, reservation, "the handler must create a real reservation")
			require.Positive(t, reservation.Amount())
			require.EqualValues(t, 1, upstream.calls.Load(), rec.Body.String())
			require.True(t, trace.UsageCommitted())
			require.Zero(t, repo.calls.Load(), "billing is still blocked")
			require.Equal(t, 1, cache.count(), "handler return must not release the blocked cyber task's reservation")
			require.Zero(t, cache.releases.Load())
			cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/reused", nil)
			unblock()
			got := waitUsageAttribution(t, repo)
			select {
			case release := <-cache.released:
				require.EqualValues(t, 1, release.usageWrites, "release must follow the sole usage write")
				require.Equal(t, 1, release.charges, "release must follow the sole charge")
			case <-time.After(3 * time.Second):
				t.Fatal("the completed cyber task did not release its reservation")
			}
			require.Zero(t, cache.count())
			reservation.HandlerDone()
			require.EqualValues(t, 1, cache.releases.Load(), "only the final reference may release the reservation")
			require.EqualValues(t, 1, repo.calls.Load())
			require.NoError(t, got.ctxErr, "request cancellation must not cancel background billing")
			require.Same(t, capture, requestdiagnostic.FromContext(got.ctx))
			require.Equal(t, &trace.RouteID, got.log.CompositeRouteID)
			require.Equal(t, service.RequestTypeCyberBlocked, got.log.RequestType)
			require.Equal(t, 11, got.log.InputTokens)
			require.Equal(t, 3, got.log.OutputTokens)
			const expected = 11*2e-6 + 3*8e-6
			require.InDelta(t, expected, got.log.ActualCost, 1e-12)
			repo.billing.mu.Lock()
			commands := append([]service.UsageBillingCommand(nil), repo.billing.commands...)
			balance := repo.billing.balance
			repo.billing.mu.Unlock()
			require.Len(t, commands, 1)
			require.InDelta(t, expected, commands[0].BalanceCost, 1e-12)
			require.InDelta(t, 100-expected, balance, 1e-12)
		})
	}
}
