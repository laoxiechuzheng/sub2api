//go:build unit

package handler

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const c18BoundaryUpstream = "web_search:fixture-upstream"
const c18BoundaryPrewritten = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
const c18BoundaryCacheOnly = "data: {\"type\":\"response.in_progress\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"input_tokens_details\":{\"cached_tokens\":100}}}\n\n"

var c18BoundaryFull = [6]int{3, 5, 100, 20, 7, 13}
var c18BoundaryScalar = [6]int{123, 5, 0, 0, 0, 0}

type c18BoundaryCounter struct {
	value, increments, resets atomic.Int64
}

func (p *c18BoundaryCounter) IncrementOpenAI403Count(_ context.Context, _ int64, _ int) (int64, error) {
	p.increments.Add(1)
	return p.value.Add(1), nil
}
func (p *c18BoundaryCounter) ResetOpenAI403Count(_ context.Context, _ int64) error {
	p.resets.Add(1)
	p.value.Store(0)
	return nil
}

type c18BoundaryAccounts struct {
	c18F01Accounts
	temp, permanent []int64
}

func (r *c18BoundaryAccounts) SetTempUnschedulable(_ context.Context, id int64, _ time.Time, _ string) error {
	r.temp = append(r.temp, id)
	return nil
}
func (r *c18BoundaryAccounts) SetError(_ context.Context, id int64, _ string) error {
	r.permanent = append(r.permanent, id)
	return nil
}

type c18BoundaryReadError struct{}

func (c18BoundaryReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type c18BoundaryOptions struct {
	stream, legacy, high, prewritten, cancel, pool, readError, cacheOnly bool
	client, local, downstream, code, message, usage, rule                string
	counter                                                              int64
	delegate                                                             service.UsageBillingRepository
}

type c18BoundaryTransport struct {
	service.HTTPUpstream
	opt      c18BoundaryOptions
	attempts []int64
	valid    []bool
}

func (u *c18BoundaryTransport) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(io.LimitReader(req.Body, 32768))
	_ = req.Body.Close()
	u.attempts = append(u.attempts, id)
	u.valid = append(u.valid, err == nil && req.URL.Host == "c18-boundary.example.invalid" && req.URL.Path == "/v1/responses" && req.Method == http.MethodPost && gjson.GetBytes(b, "model").String() == c18F01Sent && gjson.GetBytes(b, "stream").Bool() && (!u.opt.high || gjson.GetBytes(b, "reasoning.effort").String() == "high"))
	h := http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {c18BoundaryUpstream}, "X-Fixture-Upstream-Request": {c18BoundaryUpstream}}
	if u.opt.readError {
		progress := c18BoundaryCacheOnly
		if !u.opt.cacheOnly {
			progress = `data: {"type":"response.in_progress"` + c18F01Usage + "}\n\n"
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(io.MultiReader(strings.NewReader(progress), c18BoundaryReadError{}))}, nil
	}
	code, msg, use := u.opt.code, u.opt.message, u.opt.usage
	if len(u.attempts) > 1 {
		code = ""
		use = `,"usage":{"input_tokens":3,"output_tokens":4}`
	}
	var event string
	if code == "" {
		event = fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_fixture_complete","status":"completed","model":%q,"output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}]%s}}`, c18F01Echo, c18F01OKText, use)
	} else {
		event = fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_fixture_failure","status":"failed","model":%q,"error":{"code":%q,"message":%q}%s}}`, c18F01Echo, code, msg, use)
		if code == "rate_limit_exceeded" {
			h.Set("Retry-After", "7")
		}
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}, nil
}

type c18BoundaryApply struct {
	cmd             service.UsageBillingCommand
	result          *service.UsageBillingApplyResult
	err, contextErr error
	submitted       bool
	counter         int64
}

type c18BoundaryBilling struct {
	*usageAttributionBillingRepo
	delegate     service.UsageBillingRepository
	trace        *service.CompositeRouteTrace
	counter      *c18BoundaryCounter
	observations chan c18BoundaryApply
}

var _ service.UsageBillingRepository = (*c18BoundaryBilling)(nil)

func (b *c18BoundaryBilling) Apply(ctx context.Context, cmd *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	ob := c18BoundaryApply{cmd: *cmd, contextErr: ctx.Err(), submitted: b.trace.UsageCommitted(), counter: b.counter.value.Load()}
	if b.delegate == nil {
		ob.result, ob.err = b.usageAttributionBillingRepo.Apply(ctx, cmd)
	} else {
		ob.result, ob.err = b.delegate.Apply(ctx, cmd)
		b.usageAttributionBillingRepo.mu.Lock()
		b.usageAttributionBillingRepo.commands = append(b.usageAttributionBillingRepo.commands, ob.cmd)
		b.usageAttributionBillingRepo.mu.Unlock()
	}
	b.observations <- ob
	return ob.result, ob.err
}

type c18BoundaryAttempt struct {
	result                        *service.OpenAIForwardResult
	err                           error
	writtenBefore, committedAfter bool
}
type c18BoundaryOutcome struct {
	response                 *httptest.ResponseRecorder
	ctx                      *gin.Context
	attempts                 []c18BoundaryAttempt
	ids                      []int64
	apply                    c18BoundaryApply
	row                      *service.UsageLog
	counter                  [3]int64
	temp, permanent, limited []int64
	cacheBalance             float64
}

func runC18Boundary(t *testing.T, opt c18BoundaryOptions) c18BoundaryOutcome {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 3
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60, FailClosedOnUnpriced: true}
	cfg.Security.ResponseHeaders.Enabled = true
	cfg.Security.ResponseHeaders.ForceRemove = []string{"x-request-id"}
	makeAccount := func(id int64, priority int) service.Account {
		creds := map[string]any{"api_key": "fixture-only", "base_url": "https://c18-boundary.example.invalid", "api_protocol": service.APIProtocolResponses, "model_mapping": map[string]any{c18F01Request: c18F01Sent}}
		if opt.pool && id == c18F01Primary {
			creds["pool_mode"] = true
			creds["pool_mode_retry_count"] = 1
		}
		return service.Account{ID: id, Priority: priority, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{c18F01GroupID}, Credentials: creds, Extra: map[string]any{service.AccountExtraUpstreamRequestIDHeader: "X-Fixture-Upstream-Request"}}
	}
	accounts := &c18BoundaryAccounts{c18F01Accounts: c18F01Accounts{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: []service.Account{makeAccount(c18F01Primary, 10), makeAccount(c18F01Backup, 20)}}}}
	counter := &c18BoundaryCounter{}
	counter.value.Store(opt.counter)
	logs := &usageAttributionRepo{created: make(chan usageAttributionReceipt, 8), diagnostics: make(chan *requestdiagnostic.Snapshot, 4), billing: &usageAttributionBillingRepo{balance: c18F01Balance}}
	cache := &c18F01Cache{&usageAttributionInflightCache{handlerInflightCache: newHandlerInflightCache(c18F01Balance), repo: logs, released: make(chan usageAttributionReservationRelease, 4)}}
	billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	trace := &service.CompositeRouteTrace{RouteID: 7142, RequestKind: service.CompositeRouteRequestKindCompaction}
	billing := &c18BoundaryBilling{usageAttributionBillingRepo: logs.billing, delegate: opt.delegate, trace: trace, counter: counter, observations: make(chan c18BoundaryApply, 8)}
	u := &c18BoundaryTransport{opt: opt}
	input, output, read, write, long := 2e-6, 8e-6, .5e-6, 1e-6, 3e-6
	ch := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{ID: 7103, Name: "fixture", Status: service.StatusActive, GroupIDs: []int64{c18F01GroupID}, BillingModelSource: service.BillingModelSourceRequested,
			ModelPricing: []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{c18F01Public, c18F01Request, c18F01Sent}, BillingMode: service.BillingModeToken, InputPrice: &input, OutputPrice: &output, CacheReadPrice: &read, CacheWritePrice: &write, CacheWrite12hPrice: &long}}}},
		groupPlatforms: map[int64]string{c18F01GroupID: service.PlatformOpenAI},
	}, nil, nil, nil, nil)
	price := service.NewBillingService(cfg, nil)
	rate := service.NewRateLimitService(accounts, logs, cfg, nil, nil)
	rate.SetOpenAI403CounterCache(counter)
	gateway := service.NewOpenAIGatewayService(accounts, logs, billing, nil, nil, nil, nil, cfg, nil, nil, price, rate, billingCache, u, &service.DeferredService{}, nil, nil, service.NewModelPricingResolver(ch, price), ch, nil, nil, nil)
	var rules *service.ErrorPassthroughService
	if opt.rule != "" {
		rules = newFailedFrameCodeRule(opt.rule)
	}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, rules, nil, nil, cfg)
	ctx := service.WithCompositeRouteTrace(context.Background(), trace)
	ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, c18F01Public)
	if opt.client != "" {
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, opt.client)
	}
	if opt.local != "" {
		ctx = context.WithValue(ctx, ctxkey.RequestID, opt.local)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	out := c18BoundaryOutcome{response: rec, ctx: c}
	h.forwardMessages = func(ctx context.Context, c *gin.Context, account *service.Account, body []byte, pk, mapped string) (*service.OpenAIForwardResult, error) {
		before := c.Writer.Written()
		if opt.prewritten && len(out.attempts) == 0 {
			require.False(t, before, "F01-SETUP write must start after handler snapshot")
			c.Header("Content-Type", "text/event-stream")
			_, err := io.WriteString(c.Writer, c18BoundaryPrewritten)
			require.NoError(t, err)
			c.Writer.Flush()
		}
		r, err := gateway.ForwardAsAnthropic(ctx, c, account, body, pk, mapped)
		out.attempts = append(out.attempts, c18BoundaryAttempt{r, err, before, service.IsResponseCommitted(c)})
		if opt.cancel && len(out.attempts) == 1 {
			require.True(t, r.HasObservedUsage(), "F01-SETUP cancel after real consumption")
			cancel()
		}
		if opt.legacy {
			mark := service.GetOpsCyberPolicy(c)
			require.NotNil(t, mark, "F01-SETUP legacy reference must have a real mark")
			require.True(t, c.Writer.Written(), "F01-SETUP original rejection already written")
			require.Error(t, err)
			// 只复现旧 Forward 交给控制器的 nil 结果，保留实际错误、mark 和 writer。
			return nil, err
		}
		return r, err
	}
	extra := ""
	if opt.high {
		extra = `,"output_config":{"effort":"high"}`
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"fixture request"}]%s}`, c18F01Request, opt.stream, extra))).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	if opt.downstream != "" {
		c.Header("X-Request-Id", opt.downstream)
	}
	id := c18F01GroupID
	key := &service.APIKey{ID: 7104, GroupID: &id, Group: &service.Group{ID: id, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}, User: &service.User{ID: 7105, Status: service.StatusActive}}
	c.Set(string(middleware.ContextKeyAPIKey), key)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID})
	h.Messages(c)
	require.NotEmpty(t, u.attempts, "F01-SETUP actual Messages forwarded")
	require.Equal(t, c18F01Primary, u.attempts[0], "F01-SETUP primary first")
	for _, valid := range u.valid {
		require.True(t, valid, "F01-SETUP in-memory Responses request binding")
	}
	require.Len(t, out.attempts, len(u.attempts), "F01-SETUP Forward/transport cardinality")
	require.NotNil(t, service.InflightReservationFromContext(c.Request.Context()), "F01-SETUP real reservation")
	require.True(t, trace.UsageCommitted())
	select {
	case release := <-cache.released:
		require.Equal(t, usageAttributionReservationRelease{usageWrites: 1, charges: 1}, release)
	case <-time.After(3 * time.Second):
		t.Fatal("F01-SETUP reservation release missing")
	}
	require.Zero(t, cache.count())
	require.EqualValues(t, 1, cache.releases.Load())
	require.EqualValues(t, 1, logs.calls.Load())
	require.Len(t, logs.created, 1)
	require.Len(t, billing.observations, 1)
	got := <-logs.created
	require.NoError(t, got.ctxErr)
	out.row = got.log
	out.apply = <-billing.observations
	require.NoError(t, out.apply.contextErr)
	require.True(t, out.apply.submitted, "must mark submission before actual Apply")
	require.Equal(t, key.ID, out.apply.cmd.APIKeyID)
	require.Equal(t, key.User.ID, out.apply.cmd.UserID)
	require.Equal(t, out.row.RequestID, out.apply.cmd.RequestID)
	require.NotEmpty(t, out.apply.cmd.RequestPayloadHash)
	require.NotEmpty(t, out.apply.cmd.RequestFingerprint)
	out.ids = u.attempts
	out.counter = [3]int64{counter.value.Load(), counter.increments.Load(), counter.resets.Load()}
	out.temp, out.permanent, out.limited = accounts.temp, accounts.permanent, accounts.rateLimited
	cache.mu.Lock()
	out.cacheBalance = cache.balance
	cache.mu.Unlock()
	t.Logf("C18-RECEIPT handler attempts=%v status=%d actual=%.9f command=%.8f cache_balance=%.9f apply=1 create=1 counters=%v temp=%d hard=%d conflict=%t", out.ids, rec.Code, out.row.ActualCost, out.apply.cmd.BalanceCost, out.cacheBalance, out.counter, len(out.temp), len(out.permanent), out.apply.err != nil)
	return out
}

func c18BoundaryAssertBill(t *testing.T, out c18BoundaryOutcome, want [6]int, actual, command float64) {
	t.Helper()
	r, c := out.row, out.apply.cmd
	require.Equal(t, want, [6]int{r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.CacheCreation30mTokens, r.CacheCreation12hTokens})
	require.Equal(t, [4]int{want[0], want[1], want[2], want[3]}, [4]int{c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheCreationTokens})
	require.InDelta(t, actual, r.ActualCost, 1e-12)
	require.InDelta(t, command, c.BalanceCost, 1e-12)
	require.InDelta(t, c18F01Balance-actual, out.cacheBalance, 1e-12)
	t.Logf("C18-RECEIPT handler tokens=%v", want)
}

func c18BoundaryJSONError(t *testing.T, out c18BoundaryOutcome, status int, kind, message string) {
	t.Helper()
	require.Equal(t, status, out.response.Code)
	b := out.response.Body.Bytes()
	require.True(t, json.Valid(b), "exactly one JSON response")
	require.Equal(t, "error", gjson.GetBytes(b, "type").String())
	require.Equal(t, kind, gjson.GetBytes(b, "error.type").String())
	require.Equal(t, message, gjson.GetBytes(b, "error.message").String())
}

func c18BoundarySSEError(t *testing.T, out c18BoundaryOutcome, pre, kind, message string) {
	t.Helper()
	require.Equal(t, http.StatusOK, out.response.Code)
	require.True(t, strings.HasPrefix(out.response.Header().Get("Content-Type"), "text/event-stream"))
	body := out.response.Body.String()
	require.True(t, strings.HasPrefix(body, pre))
	data, ok := strings.CutPrefix(strings.TrimPrefix(body, pre), "event: error\ndata: ")
	require.True(t, ok)
	data, ok = strings.CutSuffix(data, "\n\n")
	require.True(t, ok)
	require.True(t, json.Valid([]byte(data)))
	require.Equal(t, "error", gjson.Get(data, "type").String())
	require.Equal(t, kind, gjson.Get(data, "error.type").String())
	require.Equal(t, message, gjson.Get(data, "error.message").String())
}

func c18BoundaryCy(t *testing.T, out c18BoundaryOutcome, opt c18BoundaryOptions, wantKey string, withUsage bool) {
	t.Helper()
	require.Equal(t, []int64{c18F01Primary}, out.ids)
	require.Empty(t, out.limited)
	mark := service.GetOpsCyberPolicy(out.ctx)
	require.NotNil(t, mark, "F01-SETUP actual refusal")
	require.Equal(t, "cyber_policy", mark.Code)
	if withUsage {
		require.Equal(t, [2]int{123, 5}, [2]int{mark.UpstreamInTok, mark.UpstreamOutTok})
		require.NotNil(t, out.attempts[0].result)
		require.Equal(t, c18BoundaryUpstream, out.attempts[0].result.RequestID, "F01-SETUP forced upstream prefix")
	} else {
		require.Equal(t, [2]int{}, [2]int{mark.UpstreamInTok, mark.UpstreamOutTok})
	}
	require.Equal(t, opt.downstream, out.ctx.Writer.Header().Get("X-Request-Id"), "F01-SETUP actual downstream header")
	require.Empty(t, out.response.Header().Get("X-Fixture-Upstream-Request"))
	if opt.stream {
		c18BoundarySSEError(t, out, "", "invalid_request_error", opt.message)
	} else {
		c18BoundaryJSONError(t, out, 400, "invalid_request_error", opt.message)
	}
	require.Equal(t, service.RequestTypeCyberBlocked, out.row.RequestType)
	if opt.legacy || !withUsage {
		require.Nil(t, out.row.UpstreamRequestID)
	} else {
		require.Equal(t, c18BoundaryUpstream, c18BoundaryValue(out.row.UpstreamRequestID))
		require.Equal(t, c18F01Echo, c18BoundaryValue(out.row.UpstreamResponseModel))
		require.Equal(t, c18F01Sent, c18BoundaryValue(out.row.UpstreamModel))
		if opt.high {
			require.Equal(t, "high", c18BoundaryValue(out.row.RequestedReasoningEffort))
			require.Nil(t, out.row.ReasoningEffort)
		}
	}
	if wantKey == "" {
		prefix, ok := strings.CutPrefix(out.apply.cmd.RequestID, "generated:")
		require.True(t, ok)
		require.NotEmpty(t, prefix)
	} else {
		require.Equal(t, wantKey, out.apply.cmd.RequestID)
	}
	require.NotEqual(t, c18BoundaryUpstream, out.apply.cmd.RequestID)
}

func c18BoundaryValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type c18BoundaryDifferentFingerprint string

func (f c18BoundaryDifferentFingerprint) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && len(s) == 64 && s != string(f)
}

func TestC18F01CompatIdentity(t *testing.T) {
	pair := func(t *testing.T, opt c18BoundaryOptions, wantKey string, conflict string) {
		t.Helper()
		opt.code, opt.message, opt.usage = "cyber_policy", "fixture refusal", c18F01Usage
		legacy := opt
		legacy.legacy = true
		old := runC18Boundary(t, legacy)
		c18BoundaryCy(t, old, legacy, wantKey, true)
		c18BoundaryAssertBill(t, old, c18BoundaryScalar, .000286, .000286)
		require.NoError(t, old.apply.err)
		require.True(t, old.apply.result.Applied)
		var mock sqlmock.Sqlmock
		if conflict != "" {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock = m
			actual := repository.NewUsageBillingRepository(nil, db)
			opt.delegate = actual
			o := old.apply.cmd
			require.Equal(t, [4]float64{}, [4]float64{o.SubscriptionCost, o.APIKeyQuotaCost, o.APIKeyRateLimitCost, o.AccountQuotaCost}, "F01-SETUP balance-only historical command")
			const insert = `^INSERT INTO usage_billing_dedup \(request_id, api_key_id, request_fingerprint\) VALUES \(\$1, \$2, \$3\) ON CONFLICT \(request_id, api_key_id\) DO NOTHING RETURNING id$`
			const active = `^SELECT request_fingerprint FROM usage_billing_dedup WHERE request_id = \$1 AND api_key_id = \$2$`
			const archive = `^SELECT request_fingerprint FROM usage_billing_dedup_archive WHERE request_id = \$1 AND api_key_id = \$2$`
			mock.ExpectBegin()
			mock.ExpectQuery(insert).WithArgs(o.RequestID, o.APIKeyID, o.RequestFingerprint).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
			mock.ExpectQuery(archive).WithArgs(o.RequestID, o.APIKeyID).WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}))
			mock.ExpectQuery(`^UPDATE users SET balance = balance - \$1, updated_at = NOW\(\) WHERE id = \$2 AND deleted_at IS NULL AND balance >= \$1 RETURNING balance$`).WithArgs(o.BalanceCost, o.UserID).WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(99.999714))
			mock.ExpectCommit()
			result, err := actual.Apply(context.Background(), &o)
			require.NoError(t, err)
			require.True(t, result.Applied)
			require.NotNil(t, result.NewBalance)
			require.InDelta(t, 99.999714, *result.NewBalance, 1e-12)
			require.NoError(t, mock.ExpectationsWereMet())
			mock.ExpectBegin()
			fresh := mock.ExpectQuery(insert).WithArgs(o.RequestID, o.APIKeyID, c18BoundaryDifferentFingerprint(o.RequestFingerprint))
			if conflict == "active" {
				fresh.WillReturnRows(sqlmock.NewRows([]string{"id"}))
				mock.ExpectQuery(active).WithArgs(o.RequestID, o.APIKeyID).WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow(o.RequestFingerprint))
			} else {
				// SQLmock 模拟已归档旧行；没有运行归档或实际数据库操作。
				fresh.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(2))
				mock.ExpectQuery(archive).WithArgs(o.RequestID, o.APIKeyID).WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow(o.RequestFingerprint))
			}
			mock.ExpectRollback()
			// 不为第二次请求声明任何金额/配额 SQL；错走这些语句时实际仓储不会返回 conflict。
			newResult := runC18Boundary(t, opt)
			c18BoundaryCy(t, newResult, opt, wantKey, true)
			c18BoundaryAssertBill(t, newResult, c18BoundaryFull, 0, .000142)
			require.ErrorIs(t, newResult.apply.err, service.ErrUsageBillingRequestConflict)
			require.Nil(t, newResult.apply.result)
			require.NoError(t, mock.ExpectationsWereMet())
			require.Equal(t, old.apply.cmd.RequestID, newResult.apply.cmd.RequestID)
			require.Equal(t, old.apply.cmd.APIKeyID, newResult.apply.cmd.APIKeyID)
			require.Equal(t, old.apply.cmd.RequestPayloadHash, newResult.apply.cmd.RequestPayloadHash)
			require.NotEqual(t, old.apply.cmd.RequestFingerprint, newResult.apply.cmd.RequestFingerprint)
			require.Equal(t, old.response.Body.String(), newResult.response.Body.String())
			t.Logf("C18-RECEIPT sqlmock branch=%s old_commit=true new_rollback=true second_effect=false fingerprints_differ=true", conflict)
			return
		}
		fresh := runC18Boundary(t, opt)
		c18BoundaryCy(t, fresh, opt, wantKey, true)
		c18BoundaryAssertBill(t, fresh, c18BoundaryFull, .000142, .000142)
		require.NoError(t, fresh.apply.err)
		require.True(t, fresh.apply.result.Applied)
		if wantKey != "" {
			require.Equal(t, old.apply.cmd.RequestID, fresh.apply.cmd.RequestID)
		}
		require.Equal(t, old.apply.cmd.APIKeyID, fresh.apply.cmd.APIKeyID)
		require.Equal(t, old.apply.cmd.RequestPayloadHash, fresh.apply.cmd.RequestPayloadHash)
		require.NotEqual(t, old.apply.cmd.RequestFingerprint, fresh.apply.cmd.RequestFingerprint)
		require.Equal(t, old.response.Body.String(), fresh.response.Body.String())
		t.Logf("C18-RECEIPT identity canonical=true fingerprints_differ=true independent_generated=%t", wantKey == "")
	}
	t.Run("client_wins", func(t *testing.T) {
		o := c18BoundaryOptions{client: "fixture-client", local: "fixture-local", downstream: c18F01DownID}
		pair(t, o, "client:fixture-client", "")
		o.high = true
		pair(t, o, "client:fixture-client", "")
	})
	t.Run("local_wins", func(t *testing.T) {
		pair(t, c18BoundaryOptions{stream: true, local: "fixture-local", downstream: c18F01DownID}, "local:fixture-local", "")
	})
	t.Run("downstream_fallback", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			pair(t, c18BoundaryOptions{stream: stream, downstream: c18F01DownID}, c18F01DownID, "")
		}
	})
	t.Run("generated_fallback", func(t *testing.T) { pair(t, c18BoundaryOptions{stream: true}, "", "") })
	t.Run("active_legacy_conflict", func(t *testing.T) {
		pair(t, c18BoundaryOptions{client: "fixture-client", downstream: c18F01DownID}, "client:fixture-client", "active")
	})
	t.Run("archive_legacy_conflict", func(t *testing.T) {
		pair(t, c18BoundaryOptions{stream: true, local: "fixture-local", downstream: c18F01DownID}, "local:fixture-local", "archive")
	})
}

func TestC18F01CompatTerminal(t *testing.T) {
	one := func(t *testing.T, out c18BoundaryOutcome, want [6]int, amount float64) {
		t.Helper()
		require.Equal(t, []int64{c18F01Primary}, out.ids)
		require.NoError(t, out.apply.err)
		require.True(t, out.apply.result.Applied)
		c18BoundaryAssertBill(t, out, want, amount, amount)
	}
	ordinary := func(code, message string) c18BoundaryOptions {
		return c18BoundaryOptions{code: code, message: message, usage: c18F01Usage}
	}
	rate := func() c18BoundaryOptions {
		o := ordinary("rate_limit_exceeded", "fixture rate limit")
		o.rule = o.code
		return o
	}
	t.Run("structured_account_403", func(t *testing.T) {
		out := runC18Boundary(t, ordinary("token_revoked", "fixture forbidden"))
		var typed *service.UpstreamFailoverError
		require.ErrorAs(t, out.attempts[0].err, &typed)
		require.Equal(t, 403, typed.StatusCode)
		require.Equal(t, [3]int64{1, 1, 0}, out.counter)
		require.EqualValues(t, 1, out.apply.counter)
		require.Equal(t, []int64{c18F01Primary}, out.temp)
		require.Empty(t, out.permanent)
		require.NotNil(t, typed.StreamFailedFrame)
		c18BoundaryJSONError(t, out, 502, "upstream_error", "Upstream access forbidden, please contact administrator")
		one(t, out, c18BoundaryFull, .000142)
	})
	t.Run("request_permission_403", func(t *testing.T) {
		o := ordinary("permission_error", "fixture forbidden")
		o.counter = 2
		out := runC18Boundary(t, o)
		require.Error(t, out.attempts[0].err)
		var typed *service.UpstreamFailoverError
		require.NotErrorAs(t, out.attempts[0].err, &typed)
		require.Equal(t, [3]int64{2, 0, 0}, out.counter)
		require.EqualValues(t, 2, out.apply.counter)
		require.Empty(t, out.temp)
		require.Empty(t, out.permanent)
		c18BoundaryJSONError(t, out, 502, "api_error", o.message)
		one(t, out, c18BoundaryFull, .000142)
	})
	t.Run("legacy_reset_paths", func(t *testing.T) {
		success := runC18Boundary(t, c18BoundaryOptions{counter: 2, usage: `,"usage":{"input_tokens":0,"output_tokens":0}`})
		require.NoError(t, success.attempts[0].err)
		require.Equal(t, 200, success.response.Code)
		require.Contains(t, success.response.Body.String(), c18F01OKText)
		one(t, success, [6]int{}, 0)
		o := ordinary("server_error", "fixture interrupted")
		o.counter, o.stream = 2, true
		stream := runC18Boundary(t, o)
		var typed *service.UpstreamFailoverError
		require.ErrorAs(t, stream.attempts[0].err, &typed)
		require.False(t, stream.attempts[0].result.IsRecoveredMessagesBufferedUsage())
		c18BoundaryJSONError(t, stream, 502, "api_error", "Upstream request failed")
		one(t, stream, c18BoundaryFull, .000142)
		o = ordinary("cyber_policy", "fixture refusal")
		o.counter, o.legacy = 2, true
		old := runC18Boundary(t, o)
		c18BoundaryCy(t, old, o, "", true)
		one(t, old, c18BoundaryScalar, .000286)
		o.legacy, o.usage = false, ""
		zero := runC18Boundary(t, o)
		c18BoundaryCy(t, zero, o, "", false)
		one(t, zero, [6]int{}, 0)
		for _, out := range []c18BoundaryOutcome{success, stream, old, zero} {
			require.Equal(t, [3]int64{0, 0, 1}, out.counter)
			require.Zero(t, out.apply.counter)
		}
	})
	t.Run("writer_committed_exhaustion", func(t *testing.T) {
		o := rate()
		o.prewritten = true
		out := runC18Boundary(t, o)
		var typed *service.UpstreamFailoverError
		require.ErrorAs(t, out.attempts[0].err, &typed)
		require.Equal(t, 429, typed.StatusCode)
		require.False(t, out.attempts[0].writtenBefore)
		require.True(t, out.attempts[0].result.IsRecoveredMessagesBufferedUsage())
		c18BoundarySSEError(t, out, c18BoundaryPrewritten, "upstream_error", o.message)
		one(t, out, c18BoundaryFull, .000142)
	})
	t.Run("cancel_before_terminal_mapping", func(t *testing.T) {
		for _, readError := range []bool{false, true} {
			o := rate()
			o.cancel, o.readError = true, readError
			out := runC18Boundary(t, o)
			require.ErrorIs(t, out.ctx.Request.Context().Err(), context.Canceled)
			require.False(t, out.ctx.Writer.Written())
			require.Empty(t, out.response.Body.String())
			var typed *service.UpstreamFailoverError
			if readError {
				require.Equal(t, io.ErrUnexpectedEOF, out.attempts[0].err)
				require.NotErrorAs(t, out.attempts[0].err, &typed)
			} else {
				require.ErrorAs(t, out.attempts[0].err, &typed)
				require.Equal(t, 429, typed.StatusCode)
			}
			one(t, out, c18BoundaryFull, .000142)
		}
	})
	t.Run("pool_no_same_account_replay", func(t *testing.T) {
		for _, consumed := range []bool{false, true} {
			o := rate()
			o.pool = true
			if !consumed {
				o.usage = ""
			}
			out := runC18Boundary(t, o)
			var typed *service.UpstreamFailoverError
			require.ErrorAs(t, out.attempts[0].err, &typed)
			require.True(t, typed.RetryableOnSameAccount, "F01-SETUP same-account actually eligible")
			require.True(t, typed.ShouldRetryNextAccount())
			require.Empty(t, out.limited)
			if consumed {
				c18BoundaryJSONError(t, out, 418, "upstream_error", o.message)
				one(t, out, c18BoundaryFull, .000142)
				continue
			}
			require.Equal(t, []int64{c18F01Primary, c18F01Primary}, out.ids)
			require.Nil(t, out.attempts[0].result)
			require.NoError(t, out.attempts[1].err)
			require.Equal(t, 200, out.response.Code)
			require.Contains(t, out.response.Body.String(), c18F01OKText)
			c18BoundaryAssertBill(t, out, [6]int{3, 4}, .000038, .000038)
		}
	})
	t.Run("rule_committed_no_double_write", func(t *testing.T) {
		for _, rule := range []bool{true, false} {
			o := ordinary("context_length_exceeded", "fixture context window exceeded")
			if rule {
				o.rule = o.code
			}
			out := runC18Boundary(t, o)
			var typed *service.UpstreamFailoverError
			require.Error(t, out.attempts[0].err)
			require.NotErrorAs(t, out.attempts[0].err, &typed)
			require.Equal(t, rule, out.attempts[0].committedAfter)
			if rule {
				c18BoundaryJSONError(t, out, 418, "upstream_error", o.message)
			} else {
				c18BoundaryJSONError(t, out, 502, "api_error", o.message)
			}
			one(t, out, c18BoundaryFull, .000142)
		}
	})
	t.Run("handler_read_error_settles_once", func(t *testing.T) {
		out := runC18Boundary(t, c18BoundaryOptions{readError: true, cacheOnly: true})
		var typed *service.UpstreamFailoverError
		require.Equal(t, io.ErrUnexpectedEOF, out.attempts[0].err)
		require.NotErrorAs(t, out.attempts[0].err, &typed)
		require.True(t, out.attempts[0].result.HasObservedUsage())
		require.False(t, out.attempts[0].committedAfter)
		c18BoundaryJSONError(t, out, 502, "api_error", "Upstream request failed")
		one(t, out, [6]int{0, 0, 100}, .000050)
	})
}
