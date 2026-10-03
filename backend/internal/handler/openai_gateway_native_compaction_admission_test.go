//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type nativeCompactionAdmissionSlots struct {
	grokMediaSlotsCache
	busyCC                      bool
	ccAcquireAttempts           int
	ccReleases                  int
	ccWaitIncrements            int
	ccWaitDecrements            int
	ccHeldWhenResponsesAcquired bool
}

func (s *nativeCompactionAdmissionSlots) AcquireAccountSlot(ctx context.Context, id int64, maxConcurrency int, request string) (bool, error) {
	s.mu.Lock()
	if id == 101 {
		s.ccAcquireAttempts++
		if s.busyCC {
			s.mu.Unlock()
			return false, nil
		}
	}
	if id == 102 {
		for _, heldAccount := range s.accounts {
			if heldAccount == 101 {
				s.ccHeldWhenResponsesAcquired = true
			}
		}
	}
	s.mu.Unlock()
	return s.grokMediaSlotsCache.AcquireAccountSlot(ctx, id, maxConcurrency, request)
}

func (s *nativeCompactionAdmissionSlots) ReleaseAccountSlot(ctx context.Context, id int64, request string) error {
	s.mu.Lock()
	if id == 101 {
		s.ccReleases++
	}
	s.mu.Unlock()
	return s.grokMediaSlotsCache.ReleaseAccountSlot(ctx, id, request)
}

func (s *nativeCompactionAdmissionSlots) IncrementAccountWaitCount(ctx context.Context, id int64, maxWait int) (bool, error) {
	s.mu.Lock()
	if id == 101 {
		s.ccWaitIncrements++
	}
	s.mu.Unlock()
	return s.grokMediaSlotsCache.IncrementAccountWaitCount(ctx, id, maxWait)
}

func (s *nativeCompactionAdmissionSlots) DecrementAccountWaitCount(ctx context.Context, id int64) error {
	s.mu.Lock()
	if id == 101 {
		s.ccWaitDecrements++
	}
	s.mu.Unlock()
	return s.grokMediaSlotsCache.DecrementAccountWaitCount(ctx, id)
}

type nativeCompactionAdmissionFixture struct {
	handler  *OpenAIGatewayHandler
	c        *gin.Context
	recorder *httptest.ResponseRecorder
	slots    *nativeCompactionAdmissionSlots
	upstream *nativeGuardResponsesUpstream
}

func newNativeCompactionAdmissionFixture(t *testing.T, busyCC, queueFull bool) nativeCompactionAdmissionFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(24)
	accounts := []service.Account{
		{ID: 101, Name: "admission-cc-fixture", Platform: service.PlatformDeepseek,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID},
			Credentials: map[string]any{
				"api_key":       "fixture-only",
				"base_url":      "https://upstream.example.invalid",
				"api_protocol":  service.APIProtocolChatCompletions,
				"model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"},
			}},
		{ID: 102, Name: "admission-responses-fixture", Platform: service.PlatformDeepseek,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID},
			Credentials: map[string]any{
				"api_key":       "fixture-only",
				"base_url":      "https://upstream.example.invalid",
				"api_protocol":  service.APIProtocolResponses,
				"model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"},
			}},
		{ID: 103, Name: "other-platform-responses-fixture", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Priority: -1, GroupIDs: []int64{groupID},
			Credentials: map[string]any{
				"api_key":       "fixture-only",
				"base_url":      "https://upstream.example.invalid",
				"model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"},
			}},
	}
	for i := range accounts {
		require.True(t, accounts[i].IsModelSupported("deepseek-v4.1-flash"), "fixture account %d must pass model admission", accounts[i].ID)
	}
	slots := &nativeCompactionAdmissionSlots{
		grokMediaSlotsCache: grokMediaSlotsCache{
			accounts: map[string]int64{}, users: map[string]int64{}, queueFull: queueFull,
		},
		busyCC: busyCC,
	}
	concurrency := service.NewConcurrencyService(slots)
	bindings := &grokMediaSlotBindings{}
	upstream := &nativeGuardResponsesUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	// The legacy scheduler must return a WaitPlan for the sticky, busy CC account.
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 25 * time.Millisecond
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 3
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 25 * time.Millisecond
	cfg.Concurrency.PingInterval = 60
	gateway := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, bindings, cfg, nil, concurrency, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(gateway, concurrency, billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 1
	body := `{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`
	group := &service.Group{ID: groupID, Platform: service.PlatformDeepseek,
		Status: service.StatusActive, Hydrated: true, RateMultiplier: 1,
		SubscriptionType: service.SubscriptionTypeStandard}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	_, hasCompositeSource := service.CompositeRouteSourceFromContext(ctx)
	require.False(t, hasCompositeSource, "direct-platform fixture must not claim composite alias ownership")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("session_id", "native-compaction-admission-fixture")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID, Group: group, User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 1})
	sessionHash := gateway.GenerateSessionHash(c, []byte(body))
	require.NotEmpty(t, sessionHash)
	require.NoError(t, gateway.BindStickySession(ctx, &groupID, sessionHash, 101))
	return nativeCompactionAdmissionFixture{handler: handler, c: c, recorder: recorder, slots: slots, upstream: upstream}
}

func assertNativeCompactionAdmissionForward(t *testing.T, fixture nativeCompactionAdmissionFixture) {
	t.Helper()
	require.Equal(t, http.StatusOK, fixture.recorder.Code, fixture.recorder.Body.String())
	fixture.upstream.mu.Lock()
	defer fixture.upstream.mu.Unlock()
	require.Equal(t, []int64{102}, fixture.upstream.accounts, "only the same-platform Responses account may receive compaction")
	require.Equal(t, []string{"/responses"}, fixture.upstream.paths)
	require.Len(t, fixture.upstream.bodies, 1)
	require.Equal(t, "deepseek-v4.1-flash", gjson.GetBytes(fixture.upstream.bodies[0], "model").String())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(fixture.upstream.bodies[0], "input.1.type").String())
	require.Contains(t, fixture.recorder.Body.String(), "encrypted_content")
}

func TestOpenAIResponsesNativeCompactionAdmissionSkipsBusyCCBeforeWait(t *testing.T) {
	for _, tt := range []struct {
		name      string
		queueFull bool
	}{
		{name: "CC wait queue available"},
		{name: "CC wait queue full", queueFull: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newNativeCompactionAdmissionFixture(t, true, tt.queueFull)
			groupID := int64(24)
			gateway := fixture.handler.gatewayService
			sessionHash := gateway.GenerateSessionHash(fixture.c, nil)
			// Prove this fixture reaches the real scheduler's unacquired-slot branch.
			selection, decision, err := gateway.SelectAccountWithSchedulerForCapability(
				fixture.c.Request.Context(), &groupID, "", sessionHash, "deepseek-v4.1-flash", nil,
				service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilityChatCompletions,
				false, false, true, service.PlatformDeepseek,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotNil(t, selection.Account)
			require.Equal(t, int64(101), selection.Account.ID)
			require.True(t, decision.StickySessionHit, "the real sticky CC candidate must reach the WaitPlan boundary")
			require.False(t, selection.Acquired)
			require.Nil(t, selection.ReleaseFunc)
			require.NotNil(t, selection.WaitPlan)
			require.Equal(t, int64(101), selection.WaitPlan.AccountID)
			fixture.slots.mu.Lock()
			attemptsBeforeHandler := fixture.slots.ccAcquireAttempts
			fixture.slots.mu.Unlock()

			fixture.handler.Responses(fixture.c)

			fixture.slots.assertReleased(t)
			func() {
				fixture.slots.mu.Lock()
				defer fixture.slots.mu.Unlock()
				require.Zero(t, fixture.slots.ccWaitIncrements, "a protocol-incompatible CC candidate must never enter its wait queue")
				require.Zero(t, fixture.slots.ccWaitDecrements, "no CC queue reservation should need cleanup")
				require.LessOrEqual(t, fixture.slots.ccAcquireAttempts-attemptsBeforeHandler, 1, "only the scheduler may probe the busy CC slot; admission must not wait or retry it")
				require.Zero(t, fixture.slots.ccReleases, "a busy CC slot was never acquired and must not be released")
				require.Equal(t, 1, fixture.slots.acquired, "only the Responses account should acquire a slot")
			}()
			assertNativeCompactionAdmissionForward(t, fixture)
		})
	}
}

func TestOpenAIResponsesNativeCompactionAdmissionReleasesPreAcquiredCCBeforeReselect(t *testing.T) {
	fixture := newNativeCompactionAdmissionFixture(t, false, false)

	fixture.handler.Responses(fixture.c)

	fixture.slots.assertReleased(t)
	func() {
		fixture.slots.mu.Lock()
		defer fixture.slots.mu.Unlock()
		require.Equal(t, 1, fixture.slots.ccAcquireAttempts, "the sticky CC slot is pre-acquired by the real scheduler")
		require.Equal(t, 1, fixture.slots.ccReleases, "protocol veto must release the pre-acquired CC slot exactly once")
		require.False(t, fixture.slots.ccHeldWhenResponsesAcquired, "release the CC slot before acquiring the same-platform Responses slot")
		require.Zero(t, fixture.slots.ccWaitIncrements)
		require.Equal(t, 2, fixture.slots.acquired, "the rejected CC slot and accepted Responses slot are both accounted for")
	}()
	assertNativeCompactionAdmissionForward(t, fixture)
}
