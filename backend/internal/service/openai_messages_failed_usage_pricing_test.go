//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	c18PriceA        = "fixture-public-a"
	c18PriceB        = "fixture-inbound-b"
	c18PriceC        = "fixture-mapped-c"
	c18PriceD        = "fixture-sent-d"
	c18PriceE        = "fixture-echo-e"
	c18PriceID       = "fixture-service-upstream"
	c18PriceUsage    = `{"input_tokens":123,"output_tokens":5,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":20,"cache_write_tokens_details":{"ttl_30m_tokens":7,"ttl_12h_tokens":13}}}`
	c18PriceProgress = `data: {"type":"response.in_progress","usage":` + c18PriceUsage + `,"response":{"id":"resp_fixture_nonterminal","model":"fixture-unconfirmed","service_tier":"flex"}}` + "\n\n"
)

var c18PriceFull = [6]int{3, 5, 100, 20, 7, 13}
var c18PriceScalar = [6]int{123, 5, 0, 0, 0, 0}

type c18PriceFixture struct {
	s        *OpenAIGatewayService
	key      *APIKey
	account  *Account
	log      *openAIRecordUsageLogRepoStub
	bill     *openAIRecordUsageBillingRepoStub
	upstream *httpUpstreamRecorder
}

func newC18PriceFixture(t *testing.T, platform, inbound, sent string, cards []ChannelModelPricing, ch *Channel) *c18PriceFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	logs := &openAIRecordUsageLogRepoStub{inserted: true}
	billing := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	s := newOpenAIRecordUsageServiceWithBillingRepoForTest(logs, billing, nil, nil, nil)
	s.cfg.RunMode = config.RunModeStandard
	s.cfg.Default.RateMultiplier = 1
	gid := int64(7101)
	key := &APIKey{ID: 7104, GroupID: &gid, Group: &Group{ID: gid, Platform: platform, RateMultiplier: 1, ModelPricing: cards}, User: &User{ID: 7105}}
	if ch != nil {
		// 填充正式 cache，空 source 也经真实归一化；不依赖手填价格索引。
		s.channelService = newTestChannelServiceForStats(t, ch, gid, platform)
		s.channelService.cache.Store(populateChannelCache([]Channel{*ch}, map[int64]string{gid: platform}))
	}
	s.resolver = NewModelPricingResolver(s.channelService, s.billingService)
	upstream := &httpUpstreamRecorder{}
	s.httpUpstream = upstream
	account := &Account{ID: 7102, Platform: platform, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://c18-price.example.invalid", "api_protocol": APIProtocolResponses, "model_mapping": map[string]any{inbound: sent}},
		Extra:       map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Fixture-Upstream-Request"}}
	return &c18PriceFixture{s, key, account, logs, billing, upstream}
}

func c18PriceCard(platform, name string, multiplier, write, long float64) ChannelModelPricing {
	return ChannelModelPricing{Platform: platform, Models: []string{name}, BillingMode: BillingModeToken,
		InputPrice: f64p(2e-6 * multiplier), OutputPrice: f64p(8e-6 * multiplier), CacheReadPrice: f64p(.5e-6 * multiplier),
		CacheWritePrice: f64p(write * 1e-6 * multiplier), CacheWrite12hPrice: f64p(long * 1e-6 * multiplier)}
}

func c18PriceChannel(source string, cards []ChannelModelPricing) *Channel {
	return &Channel{ID: 7103, Status: StatusActive, GroupIDs: []int64{7101}, BillingModelSource: source,
		ModelMapping: map[string]map[string]string{PlatformOpenAI: {c18PriceA: c18PriceC}}, ModelPricing: cards}
}

func c18PriceTerminal(echo, tier, code, usage string, search bool) string {
	status, tail := "failed", fmt.Sprintf(`,"error":{"code":%q,"message":"fixture refusal"}`, code)
	if code == "" {
		status = "completed"
		tail = `,"output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"fixture answer"}]}`
		if search {
			tail += `,{"id":"ws_fixture","type":"web_search_call","status":"completed"}`
		}
		tail += "]"
	}
	return fmt.Sprintf(`data: {"type":"response.%s","response":{"id":"resp_fixture","status":%q,"model":%q,"service_tier":%q,"usage":%s%s}}`, status, status, echo, tier, usage, tail) + "\n\n"
}

func (f *c18PriceFixture) forward(t *testing.T, inbound, sent, mapped string, reader io.ReadCloser, stream, fast, high bool, rules *ErrorPassthroughService) (*OpenAIForwardResult, error, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	extra := ""
	if high {
		extra = `,"output_config":{"effort":"high"}`
	}
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"fixture message"}]%s}`, inbound, stream, extra)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if fast {
		c.Request.Header.Set("anthropic-beta", claude.BetaFastMode)
	}
	if rules != nil {
		BindErrorPassthroughService(c, rules)
	}
	f.upstream.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {c18PriceID}, "X-Fixture-Upstream-Request": {c18PriceID}}, Body: reader}
	n := len(f.upstream.requests)
	r, err := f.s.ForwardAsAnthropic(c.Request.Context(), c, f.account, []byte(body), "", mapped)
	require.Len(t, f.upstream.requests, n+1, "F01-SETUP one in-memory Responses call")
	require.True(t, strings.HasSuffix(f.upstream.lastReq.URL.Path, "/responses"), "F01-SETUP Responses protocol")
	require.Equal(t, sent, gjson.GetBytes(f.upstream.lastBody, "model").String(), "F01-SETUP sent model")
	require.True(t, gjson.GetBytes(f.upstream.lastBody, "stream").Bool(), "F01-SETUP upstream SSE")
	if fast {
		require.Equal(t, "priority", gjson.GetBytes(f.upstream.lastBody, "service_tier").String(), "F01-SETUP outbound Fast")
	} else {
		require.False(t, gjson.GetBytes(f.upstream.lastBody, "service_tier").Exists(), "F01-SETUP untiered request")
	}
	if high {
		require.Equal(t, "high", gjson.GetBytes(f.upstream.lastBody, "reasoning.effort").String(), "F01-SETUP outbound effort")
	}
	return r, err, c, rec
}

func (f *c18PriceFixture) settle(t *testing.T, result *OpenAIForwardResult, mark *CyberPolicyMark, public, id string, fields ChannelUsageFields, at time.Time, want [6]int, amount float64, mode BillingMode) *UsageLog {
	t.Helper()
	nl, nb := f.log.calls, f.bill.calls
	if mark != nil {
		f.s.RecordCyberPolicyUsageLog(context.Background(), CyberPolicyUsageInput{Result: result, APIKey: f.key, Account: f.account, Model: public, RequestID: id, InputTokens: mark.UpstreamInTok, OutputTokens: mark.UpstreamOutTok, RequestPayloadHash: HashUsageRequestPayload([]byte("fixture payload")), ChannelUsageFields: fields})
	} else {
		require.NotNil(t, result)
		copyResult := *result
		usageCtx := context.WithValue(context.Background(), ctxkey.ClientRequestID, id)
		require.NoError(t, f.s.RecordUsage(usageCtx, &OpenAIRecordUsageInput{Result: &copyResult, APIKey: f.key, User: f.key.User, Account: f.account, PricingAt: at, ChannelUsageFields: fields, RequestPayloadHash: HashUsageRequestPayload([]byte("fixture payload"))}))
		id = "client:" + id
	}
	require.Equal(t, nl+1, f.log.calls)
	require.Equal(t, nb+1, f.bill.calls)
	row, cmd := f.log.lastLog, f.bill.lastCmd
	require.NotNil(t, row)
	require.NotNil(t, cmd)
	require.Equal(t, want, [6]int{row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreationTokens, row.CacheCreation30mTokens, row.CacheCreation12hTokens})
	require.Equal(t, [4]int{want[0], want[1], want[2], want[3]}, [4]int{cmd.InputTokens, cmd.OutputTokens, cmd.CacheReadTokens, cmd.CacheCreationTokens})
	require.InDelta(t, amount, row.ActualCost, 1e-12)
	require.InDelta(t, QuantizeUsageBillingAmount(amount), cmd.BalanceCost, 1e-12)
	require.Equal(t, id, cmd.RequestID)
	require.Equal(t, id, row.RequestID)
	require.NotEmpty(t, cmd.RequestFingerprint)
	require.Equal(t, string(mode), optionalStringValue(row.BillingMode))
	if mark != nil {
		require.Equal(t, RequestTypeCyberBlocked, row.RequestType)
	}
	t.Logf("C18-RECEIPT service amount=%.9f balance_cost=%.8f tokens=%v apply=1 create=1 cyber=%t", row.ActualCost, cmd.BalanceCost, want, mark != nil)
	return row
}

func c18PriceSSE(text string) io.ReadCloser { return io.NopCloser(strings.NewReader(text)) }

func c18PriceCyber(t *testing.T, c *gin.Context, rec *httptest.ResponseRecorder, stream bool, err error, in, out int) *CyberPolicyMark {
	t.Helper()
	require.Error(t, err)
	var typed *UpstreamFailoverError
	require.NotErrorAs(t, err, &typed)
	mark := GetOpsCyberPolicy(c)
	require.NotNil(t, mark, "F01-SETUP real Cyber mark")
	require.Equal(t, [2]int{in, out}, [2]int{mark.UpstreamInTok, mark.UpstreamOutTok})
	if stream {
		require.Equal(t, 200, rec.Code)
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error\n"))
	} else {
		require.Equal(t, 400, rec.Code)
		require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	}
	return mark
}

func c18PricePair(t *testing.T, f *c18PriceFixture, pub, inbound, sent, forwardMapped, echo string, fields ChannelUsageFields, usage string, legacy, recovered [6]int, oldCost, newCost float64, mode BillingMode) (*UsageLog, *UsageLog) {
	t.Helper()
	r, err, c, rec := f.forward(t, inbound, sent, forwardMapped, c18PriceSSE(c18PriceTerminal(echo, "", "cyber_policy", usage, false)), false, false, false, nil)
	mark := c18PriceCyber(t, c, rec, false, err, legacy[0], legacy[1])
	require.True(t, r.IsRecoveredMessagesBufferedUsage(), "F01-SETUP real buffered snapshot")
	old := f.settle(t, nil, mark, pub, "fixture-old", fields, time.Time{}, legacy, oldCost, mode)
	fresh := f.settle(t, r, mark, pub, "fixture-new", fields, time.Time{}, recovered, newCost, mode)
	require.Equal(t, inbound, fresh.Model)
	require.Equal(t, pub, fresh.RequestedModel)
	require.Equal(t, sent, optionalStringValue(fresh.UpstreamModel))
	require.Equal(t, echo, optionalStringValue(fresh.UpstreamResponseModel))
	require.Equal(t, c18PriceID, optionalStringValue(fresh.UpstreamRequestID))
	return old, fresh
}

func TestC18F01CompatPricing(t *testing.T) {
	all := func() []ChannelModelPricing {
		return []ChannelModelPricing{c18PriceCard(PlatformOpenAI, c18PriceA, 1, 1, 3), c18PriceCard(PlatformOpenAI, c18PriceC, 2, 1, 3), c18PriceCard(PlatformOpenAI, c18PriceD, 3, 1, 3), c18PriceCard(PlatformOpenAI, c18PriceE, .5, 1, 3)}
	}
	pair := func(t *testing.T, source string, cards []ChannelModelPricing, ch bool, oldCost, newCost float64) {
		t.Helper()
		inbound, mapped := c18PriceB, ""
		var channel *Channel
		var group []ChannelModelPricing
		if ch {
			inbound, mapped = c18PriceC, c18PriceC
			channel = c18PriceChannel(source, cards)
		} else {
			group = cards
		}
		f := newC18PriceFixture(t, PlatformOpenAI, inbound, c18PriceD, group, channel)
		m := f.s.ResolveChannelMapping(context.Background(), f.key.Group.ID, c18PriceA)
		wantSource := source
		if ch && source == "" {
			wantSource = BillingModelSourceChannelMapped
		}
		require.Equal(t, wantSource, m.BillingModelSource, "F01-SETUP original source")
		require.Equal(t, ch, m.Mapped, "F01-SETUP real channel mapping")
		if ch {
			require.Equal(t, c18PriceC, m.MappedModel)
		}
		c18PricePair(t, f, c18PriceA, inbound, c18PriceD, mapped, c18PriceE, m.ToUsageFields(c18PriceA, c18PriceD), c18PriceUsage, c18PriceScalar, c18PriceFull, oldCost, newCost, BillingModeToken)
	}
	t.Run("requested", func(t *testing.T) { pair(t, BillingModelSourceRequested, all(), true, .000286, .000142) })
	t.Run("channel_mapped", func(t *testing.T) { pair(t, BillingModelSourceChannelMapped, all(), true, .000572, .000284) })
	t.Run("upstream_and_no_channel", func(t *testing.T) {
		pair(t, BillingModelSourceUpstream, all(), true, .000286, .000142)
		pair(t, "", all(), false, .000286, .000142)
	})
	t.Run("default_channel_mapped", func(t *testing.T) { pair(t, "", all(), true, .000572, .000284) })
	t.Run("actual_price_fallback", func(t *testing.T) {
		cards := all()
		for _, tc := range []struct {
			source   string
			cards    []ChannelModelPricing
			missing  []string
			old, new float64
		}{
			{BillingModelSourceRequested, cards[1:], []string{c18PriceA}, .000572, .000284},
			{BillingModelSourceChannelMapped, []ChannelModelPricing{cards[0], cards[2], cards[3]}, []string{c18PriceC}, .000286, .000142},
			{BillingModelSourceRequested, cards[2:], []string{c18PriceA, c18PriceC}, 0, 0},
		} {
			f := newC18PriceFixture(t, PlatformOpenAI, c18PriceC, c18PriceD, nil, c18PriceChannel(tc.source, tc.cards))
			input := CostInput{Ctx: context.Background(), GroupID: f.key.GroupID, Group: f.key.Group, Resolver: f.s.resolver, Tokens: UsageTokens{InputTokens: 1}, RequestCount: 1, RateMultiplier: 1}
			for _, name := range tc.missing {
				input.Model = name
				_, err := f.s.billingService.CalculateCostUnified(input)
				require.ErrorIs(t, err, ErrModelPricingUnavailable, "F01-SETUP authentic missing candidate")
			}
			input.Model = c18PriceD
			cost, err := f.s.billingService.CalculateCostUnified(input)
			require.NoError(t, err, "F01-SETUP excluded sent model can actually price")
			require.InDelta(t, 6e-6, cost.ActualCost, 1e-12)
			pair(t, tc.source, tc.cards, true, tc.old, tc.new)
		}
	})
	t.Run("response_model_audit_only", func(t *testing.T) {
		pair(t, BillingModelSourceResponse, all(), true, .000286, .000142)
		f := newC18PriceFixture(t, PlatformOpenAI, c18PriceA, c18PriceA, nil, c18PriceChannel(BillingModelSourceResponse, all()))
		fields := f.s.ResolveChannelMapping(context.Background(), f.key.Group.ID, c18PriceA).ToUsageFields(c18PriceA, c18PriceA)
		for _, tc := range []struct {
			code, id string
			cost     float64
		}{{"rate_limit_exceeded", "fixture-failed", .000142}, {"", "fixture-completed", .000071}} {
			r, err, c, _ := f.forward(t, c18PriceA, c18PriceA, "", c18PriceSSE(c18PriceTerminal(c18PriceE, "", tc.code, c18PriceUsage, false)), false, false, false, nil)
			if tc.code == "" {
				require.NoError(t, err)
			} else {
				var typed *UpstreamFailoverError
				require.ErrorAs(t, err, &typed)
				require.True(t, r.IsRecoveredMessagesBufferedUsage())
			}
			require.Nil(t, GetOpsCyberPolicy(c))
			row := f.settle(t, r, nil, c18PriceA, tc.id, fields, time.Time{}, c18PriceFull, tc.cost, BillingModeToken)
			require.Equal(t, c18PriceE, optionalStringValue(row.UpstreamResponseModel))
		}
	})
	t.Run("oauth_long_context_both_ways", func(t *testing.T) {
		card := `{"litellm_provider":"openai","mode":"chat","input_cost_per_token":2e-6,"output_cost_per_token":10e-6,"input_cost_per_token_above_272k_tokens":4e-6,"output_cost_per_token_above_272k_tokens":15e-6}`
		for _, tc := range []struct {
			public, sent string
			amount       float64
			long         bool
		}{{"gpt-6.1-sol", "gpt-6-sol", .62, false}, {"gpt-6-sol", "gpt-6.1-sol", 1.23, true}} {
			f := newC18PriceFixture(t, PlatformOpenAI, tc.public, tc.sent, nil, nil)
			f.account.Type = AccountTypeOAuth
			f.account.Credentials["access_token"] = "fixture-only"
			f.account.Extra[openAILongContextBillingEnabledKey] = true
			f.key.Group.LongContextPricingEnabled = true
			f.s.billingService = NewBillingService(f.s.cfg, newStubPricingServiceFromJSON(t, `{"gpt-6.1-sol":`+card+`,"gpt-6-sol":`+card+`}`))
			f.s.resolver = NewModelPricingResolver(nil, f.s.billingService)
			fields := ChannelMappingResult{MappedModel: tc.public, BillingModelSource: BillingModelSourceRequested}.ToUsageFields(tc.public, tc.sent)
			buckets := [6]int{300000, 2000}
			old, row := c18PricePair(t, f, tc.public, tc.public, tc.sent, "", tc.sent, fields, `{"input_tokens":300000,"output_tokens":2000}`, buckets, buckets, tc.amount, tc.amount, BillingModeToken)
			require.Equal(t, tc.long, old.LongContextBillingApplied)
			require.Equal(t, tc.long, row.LongContextBillingApplied)
		}
	})
	t.Run("per_request_once", func(t *testing.T) {
		cards := []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{c18PriceA}, BillingMode: BillingModePerRequest, PerRequestPrice: f64p(.002)}}
		f := newC18PriceFixture(t, PlatformOpenAI, c18PriceA, c18PriceA, nil, c18PriceChannel(BillingModelSourceRequested, cards))
		fields := f.s.ResolveChannelMapping(context.Background(), f.key.Group.ID, c18PriceA).ToUsageFields(c18PriceA, c18PriceA)
		c18PricePair(t, f, c18PriceA, c18PriceA, c18PriceA, "", c18PriceA, fields, c18PriceUsage, c18PriceScalar, c18PriceFull, .002, .002, BillingModePerRequest)
		r, err, _, _ := f.forward(t, c18PriceA, c18PriceA, "", c18PriceSSE(c18PriceTerminal(c18PriceA, "", "", c18PriceUsage, false)), false, false, false, nil)
		require.NoError(t, err)
		f.settle(t, r, nil, c18PriceA, "fixture-success", fields, time.Time{}, c18PriceFull, .002, BillingModePerRequest)
	})
	t.Run("expensive_cache_direction", func(t *testing.T) {
		for _, tc := range []struct{ write, long, sum, writeCost float64 }{{1, 3, .000142, .000046}, {10, 40, .000686, .000590}} {
			f := newC18PriceFixture(t, PlatformOpenAI, c18PriceA, c18PriceA, []ChannelModelPricing{c18PriceCard(PlatformOpenAI, c18PriceA, 1, tc.write, tc.long)}, nil)
			fields := ChannelMappingResult{MappedModel: c18PriceA}.ToUsageFields(c18PriceA, c18PriceA)
			old, fresh := c18PricePair(t, f, c18PriceA, c18PriceA, c18PriceA, "", c18PriceA, fields, c18PriceUsage, c18PriceScalar, c18PriceFull, .000286, tc.sum, BillingModeToken)
			require.Zero(t, old.CacheCreationCost)
			require.InDelta(t, tc.writeCost, fresh.CacheCreationCost, 1e-12)
			require.InDelta(t, .000050, fresh.CacheReadCost, 1e-12)
			r, err, _, _ := f.forward(t, c18PriceA, c18PriceA, "", c18PriceSSE(c18PriceTerminal(c18PriceA, "", "rate_limit_exceeded", c18PriceUsage, false)), false, false, false, nil)
			var typed *UpstreamFailoverError
			require.ErrorAs(t, err, &typed)
			require.True(t, r.IsRecoveredMessagesBufferedUsage())
			row := f.settle(t, r, nil, c18PriceA, "fixture-noncyber", fields, time.Time{}, c18PriceFull, tc.sum, BillingModeToken)
			require.InDelta(t, tc.writeCost, row.CacheCreationCost, 1e-12)
			require.InDelta(t, .000050, row.CacheReadCost, 1e-12)
		}
	})
}

func TestC18F01CompatPriceSignals(t *testing.T) {
	signals := func(t *testing.T, fast, high bool, tier, billedTier string, success float64) {
		t.Helper()
		name := "gpt-5.4"
		card := c18PriceCard(PlatformOpenAI, name, 1, 1, 3)
		card.FastMultiplier, card.FlexMultiplier = f64p(2), f64p(.5)
		card.ReasoningEffortMultipliers = map[string]float64{"high": 1.7}
		f := newC18PriceFixture(t, PlatformOpenAI, name, name, []ChannelModelPricing{card}, nil)
		fields := ChannelMappingResult{MappedModel: name}.ToUsageFields(name, name)
		for _, tc := range []struct {
			id, code string
			stream   bool
		}{{"ordinary", "rate_limit_exceeded", false}, {"cyber-buffer", "cyber_policy", false}, {"cyber-stream", "cyber_policy", true}, {"success", "", false}} {
			r, err, c, rec := f.forward(t, name, name, "", c18PriceSSE(c18PriceTerminal(name, tier, tc.code, c18PriceUsage, false)), tc.stream, fast, high, nil)
			require.NotNil(t, r)
			if tc.code == "" {
				require.NoError(t, err)
				require.Equal(t, tier, r.UpstreamResponseServiceTier)
				if high {
					require.Equal(t, "high", optionalStringValue(r.ReasoningEffort))
				}
				row := f.settle(t, r, nil, name, tc.id, fields, time.Time{}, c18PriceFull, success, BillingModeToken)
				require.Equal(t, billedTier, optionalStringValue(row.ServiceTier))
				continue
			}
			var mark *CyberPolicyMark
			if tc.code == "cyber_policy" {
				mark = c18PriceCyber(t, c, rec, tc.stream, err, 123, 5)
			} else {
				var typed *UpstreamFailoverError
				require.ErrorAs(t, err, &typed)
				require.Nil(t, GetOpsCyberPolicy(c))
			}
			require.Nil(t, r.ServiceTier)
			require.Nil(t, r.ReasoningEffort)
			require.Empty(t, r.UpstreamResponseServiceTier)
			row := f.settle(t, r, mark, name, tc.id, fields, time.Time{}, c18PriceFull, .000142, BillingModeToken)
			require.Nil(t, row.ServiceTier)
			require.Nil(t, row.ReasoningEffort)
		}
	}
	t.Run("outbound_priority_and_effort", func(t *testing.T) { signals(t, true, true, "", "priority", .0004828) })
	t.Run("observed_flex_not_price", func(t *testing.T) {
		signals(t, true, true, "flex", "flex", .0001207)
		signals(t, false, false, "flex", "flex", .000071)
	})
	t.Run("grok_search_no_surcharge", func(t *testing.T) {
		name := "grok-4.6"
		f := newC18PriceFixture(t, PlatformGrok, name, name, []ChannelModelPricing{c18PriceCard(PlatformGrok, name, 1, 1, 3)}, nil)
		f.key.Group.SearchPricePer1k = f64p(20)
		fields := ChannelMappingResult{MappedModel: name}.ToUsageFields(name, name)
		const search = `data: {"type":"response.output_item.done","item":{"id":"ws_fixture","type":"web_search_call","status":"completed"}}` + "\n\n"
		for _, withSearch := range []bool{true, false} {
			prefix, want, count := "", .000142, 0
			if withSearch {
				prefix, want, count = search, .020142, 1
			}
			r, err, _, _ := f.forward(t, name, name, "", c18PriceSSE(prefix+c18PriceTerminal(name, "", "", c18PriceUsage, withSearch)), true, false, false, nil)
			require.NoError(t, err)
			require.Equal(t, count, r.SearchCount, "F01-SETUP real native search count")
			f.settle(t, r, nil, name, fmt.Sprintf("fixture-positive-%t", withSearch), fields, time.Time{}, c18PriceFull, want, BillingModeToken)
		}
		r, err, c, rec := f.forward(t, name, name, "", c18PriceSSE(search+c18PriceTerminal(name, "", "cyber_policy", c18PriceUsage, false)), true, false, false, nil)
		mark := c18PriceCyber(t, c, rec, true, err, 123, 5)
		require.NotNil(t, r)
		require.Zero(t, r.SearchCount)
		f.settle(t, r, mark, name, "fixture-rejected-search", fields, time.Time{}, c18PriceFull, .000142, BillingModeToken)
	})
	peak := func(t *testing.T, cyber bool) {
		for _, outOfPeak := range []bool{true, false} {
			f := newC18PriceFixture(t, PlatformOpenAI, c18PriceA, c18PriceA, []ChannelModelPricing{c18PriceCard(PlatformOpenAI, c18PriceA, 1, 1, 3)}, nil)
			synctest.Test(t, func(t *testing.T) {
				local := time.Now().In(timezone.Location())
				hour := 7
				if outOfPeak {
					hour = 8
				}
				target := time.Date(local.Year(), local.Month(), local.Day(), hour, 59, 55, 0, timezone.Location())
				if !target.After(local) {
					target = target.AddDate(0, 0, 1)
				}
				time.Sleep(time.Until(target))
				start := time.Now()
				g := f.key.Group
				g.SubscriptionType, g.PeakRateEnabled, g.PeakStart, g.PeakEnd, g.PeakRateMultiplier = SubscriptionTypeSubscription, true, "08:00", "09:00", 3
				fields := ChannelMappingResult{MappedModel: c18PriceA}.ToUsageFields(c18PriceA, c18PriceA)
				code := "rate_limit_exceeded"
				if cyber {
					code = "cyber_policy"
				}
				r, err, c, rec := f.forward(t, c18PriceA, c18PriceA, "", c18PriceSSE(c18PriceTerminal(c18PriceA, "", code, c18PriceUsage, false)), false, false, false, nil)
				require.True(t, r.IsRecoveredMessagesBufferedUsage())
				time.Sleep(10 * time.Second)
				entry, settlement := 1., 3.
				if outOfPeak {
					entry, settlement = 3, 1
				}
				require.Equal(t, entry, g.PeakMultiplierAt(start), "F01-SETUP request-side peak vector")
				require.Equal(t, settlement, g.PeakMultiplierAt(time.Now()), "F01-SETUP settlement-side peak vector")
				if cyber {
					mark := c18PriceCyber(t, c, rec, false, err, 123, 5)
					f.settle(t, nil, mark, c18PriceA, "fixture-cyber-old", fields, time.Time{}, c18PriceScalar, settlement*.000286, BillingModeToken)
					f.settle(t, r, mark, c18PriceA, "fixture-cyber-now", fields, time.Time{}, c18PriceFull, settlement*.000142, BillingModeToken)
				} else {
					var typed *UpstreamFailoverError
					require.ErrorAs(t, err, &typed)
					f.settle(t, r, nil, c18PriceA, "fixture-request-clock", fields, start, c18PriceFull, entry*.000142, BillingModeToken)
					f.settle(t, r, nil, c18PriceA, "fixture-current-control", fields, time.Time{}, c18PriceFull, settlement*.000142, BillingModeToken)
				}
			})
		}
	}
	t.Run("ordinary_peak_request_clock", func(t *testing.T) { peak(t, false) })
	t.Run("cyber_peak_settlement_clock", func(t *testing.T) { peak(t, true) })
}

type c18PriceIdleBody struct {
	source          *strings.Reader
	waiting, closed chan struct{}
	once            sync.Once
}

func (b *c18PriceIdleBody) Read(p []byte) (int, error) {
	if b.source.Len() > 0 {
		return b.source.Read(p)
	}
	close(b.waiting)
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *c18PriceIdleBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestC18F01CompatBufferedReader(t *testing.T) {
	fixture := func(t *testing.T) *c18PriceFixture {
		return newC18PriceFixture(t, PlatformOpenAI, c18PriceB, c18PriceD, nil, nil)
	}
	common := func(t *testing.T, r *OpenAIForwardResult, err error) {
		t.Helper()
		require.Error(t, err)
		var typed *UpstreamFailoverError
		require.NotErrorAs(t, err, &typed)
		require.True(t, r.IsRecoveredMessagesBufferedUsage())
		require.Equal(t, [6]int{123, 5, 100, 20, 7, 13}, [6]int{r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadInputTokens, r.Usage.CacheCreationInputTokens, r.Usage.CacheCreation30mInputTokens, r.Usage.CacheCreation12hInputTokens})
		require.Equal(t, c18PriceD, r.UpstreamModel)
		require.Equal(t, c18PriceID, r.RequestID)
		require.Equal(t, c18PriceID, r.UpstreamHeaders.Get("X-Fixture-Upstream-Request"))
		require.Empty(t, r.UpstreamResponseServiceTier)
		t.Logf("C18-RECEIPT reader tokens=[123 5 100 20 7 13] typed=false recovered=true")
	}
	noTerminal := func(t *testing.T, r *OpenAIForwardResult, err error) {
		t.Helper()
		common(t, r, err)
		require.Empty(t, r.ResponseID)
		require.Empty(t, r.UpstreamResponseModel)
	}
	t.Run("read_error_with_usage", func(t *testing.T) {
		r, err, c, rec := fixture(t).forward(t, c18PriceB, c18PriceD, "", c18PriceSSEReader(io.MultiReader(strings.NewReader(c18PriceProgress), passthroughErrReadCloser{err: io.ErrUnexpectedEOF})), false, false, false, nil)
		noTerminal(t, r, err)
		require.Equal(t, io.ErrUnexpectedEOF, err)
		require.False(t, c.Writer.Written())
		require.Empty(t, rec.Body.String())
	})
	t.Run("idle_timeout_with_usage", func(t *testing.T) {
		f := fixture(t)
		synctest.Test(t, func(t *testing.T) {
			f.s.cfg.Gateway.StreamDataIntervalTimeout = 1
			body := &c18PriceIdleBody{source: strings.NewReader(c18PriceProgress), waiting: make(chan struct{}), closed: make(chan struct{})}
			defer body.Close()
			var result *OpenAIForwardResult
			var err error
			var c *gin.Context
			var rec *httptest.ResponseRecorder
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, err, c, rec = f.forward(t, c18PriceB, c18PriceD, "", body, false, false, false, nil)
			}()
			synctest.Wait()
			select {
			case <-body.waiting:
			default:
				t.Fatal("F01-SETUP scanner never waited for next frame")
			}
			select {
			case <-done:
				t.Fatal("F01-SETUP Forward returned before fake idle timer")
			default:
			}
			<-done
			noTerminal(t, result, err)
			require.EqualError(t, err, "stream data interval timeout")
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
		})
	})
	missing := func(t *testing.T, tail string) {
		f := fixture(t)
		r, err, _, rec := f.forward(t, c18PriceB, c18PriceD, "", c18PriceSSE(c18PriceProgress+tail), false, false, false, nil)
		noTerminal(t, r, err)
		require.EqualError(t, err, "upstream stream ended without terminal event")
		require.Equal(t, 502, rec.Code)
		require.Equal(t, "api_error", gjson.Get(rec.Body.String(), "error.type").String())
	}
	t.Run("eof_without_terminal", func(t *testing.T) { missing(t, "") })
	t.Run("done_without_terminal", func(t *testing.T) { missing(t, "data: [DONE]\n\n") })
	t.Run("rule_committed_failure", func(t *testing.T) {
		status := http.StatusTeapot
		rule := &model.ErrorPassthroughRule{ID: 1, Name: "fixture", Enabled: true, Priority: 1, Keywords: []string{"context_length_exceeded"}, MatchMode: model.MatchModeAny, Platforms: []string{PlatformOpenAI}, ResponseCode: &status, PassthroughBody: true}
		svc := NewErrorPassthroughService(&mockErrorPassthroughRepo{rules: []*model.ErrorPassthroughRule{rule}}, nil)
		r, err, c, rec := fixture(t).forward(t, c18PriceB, c18PriceD, "", c18PriceSSE(c18PriceTerminal(c18PriceE, "", "context_length_exceeded", c18PriceUsage, false)), false, false, false, svc)
		common(t, r, err)
		require.True(t, IsResponseCommitted(c))
		require.Equal(t, status, rec.Code)
		require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
		require.Equal(t, "fixture refusal", gjson.Get(rec.Body.String(), "error.message").String())
	})
}

func c18PriceSSEReader(r io.Reader) io.ReadCloser { return io.NopCloser(r) }
