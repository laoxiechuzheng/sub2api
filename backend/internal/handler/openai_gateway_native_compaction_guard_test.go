//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesNativeCompactionCapabilityDoesNotOverrideProtocolRules(t *testing.T) {
	for _, platform := range []string{
		service.PlatformOpenAI,
		service.PlatformDeepseek,
		service.PlatformKimi,
		service.PlatformZhipu,
		service.PlatformMiniMax,
		service.PlatformOpenCodeGo,
	} {
		t.Run(platform, func(t *testing.T) {
			want := service.OpenAIEndpointCapabilityChatCompletions
			if platform == service.PlatformOpenAI {
				want = service.OpenAIEndpointCapabilityResponses
			}
			require.Equal(t, want,
				openAIResponsesRequiredCapabilityForRequest(false, true, platform))
			require.Equal(t, service.OpenAIEndpointCapabilityChatCompletions,
				openAIResponsesRequiredCapabilityForRequest(false, false, platform))
		})
	}
	// Grok compaction uses its existing dedicated Responses bridge.
	require.Equal(t, service.OpenAIEndpointCapabilityChatCompletions,
		openAIResponsesRequiredCapabilityForRequest(false, true, service.PlatformGrok))
}

type nativeGuardResponsesUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	accounts []int64
	paths    []string
	bodies   [][]byte
}

func (u *nativeGuardResponsesUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.accounts = append(u.accounts, accountID)
	u.paths = append(u.paths, req.URL.Path)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	response := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_guard\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"deepseek-v4.1-flash\",\"output\":[{\"id\":\"cmp_guard\",\"type\":\"compaction\",\"encrypted_content\":\"fixture-only\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	if strings.HasSuffix(req.URL.Path, "/chat/completions") {
		response = "data: {\"id\":\"chat_guard\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture summary\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chat_guard\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"
	} else if strings.HasSuffix(req.URL.Path, "/messages") {
		response = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_guard\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"deepseek-v4.1-flash\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"fixture summary\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(response)),
	}, nil
}

func TestOpenAIResponsesNativeCompactionSkipsCCForSamePlatformResponsesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	accounts := []service.Account{
		{ID: 101, Name: "cc-fixture", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Priority: 0,
			Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example.invalid", "api_protocol": service.APIProtocolChatCompletions, "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"}}},
		{ID: 102, Name: "responses-fixture", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Priority: 1,
			Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example.invalid", "api_protocol": service.APIProtocolResponses, "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"}}},
	}
	upstream := &nativeGuardResponsesUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 1
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched: true, Source: "route", GroupID: 6, PublicModel: "public-fixture",
		TargetPlatform: service.PlatformDeepseek, UpstreamModel: "deepseek-v4.1-flash",
	}))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	groupID := int64(6)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})

	handler.Responses(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Equal(t, []int64{102}, upstream.accounts, "only the same-platform native Responses account may receive compaction")
	require.Equal(t, []string{"/responses"}, upstream.paths)
	require.Equal(t, "compaction_trigger", gjson.GetBytes(upstream.bodies[0], "input.1.type").String())
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "encrypted_content")
}

func TestOpenAIResponsesNativeCompactionRejectsCCAccountBeforeForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := service.Account{
		ID:          101,
		Name:        "native-guard-fixture",
		Platform:    service.PlatformDeepseek,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key":       "fixture-only",
			"base_url":      "https://upstream.example.invalid",
			"api_protocol":  service.APIProtocolChatCompletions,
			"model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"},
		},
	}
	// Unknown probes pass the generic capability check; explicit protocols
	// must still be checked before forwarding native compaction.
	require.True(t, account.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilityResponses))
	upstream := &openAIImagesFailoverHTTPUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: []service.Account{account}},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 1
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched: true, Source: "route", GroupID: 6, PublicModel: "public-fixture",
		TargetPlatform: service.PlatformDeepseek, UpstreamModel: "deepseek-v4.1-flash",
	}))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	groupID := int64(6)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})

	handler.Responses(c)

	require.Empty(t, upstream.calls(), "native compaction must reject CC-only accounts before forwarding")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "compaction")
}

func runNativeGuardHandlerRequest(t *testing.T, accounts []service.Account, platform, path, body string) (*httptest.ResponseRecorder, *nativeGuardResponsesUpstream) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &nativeGuardResponsesUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 1
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched: true, Source: "route", GroupID: 6, PublicModel: "public-fixture",
		TargetPlatform: platform, UpstreamModel: "deepseek-v4.1-flash",
	}))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	groupID := int64(6)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
	handler.Responses(c)
	return recorder, upstream
}

func TestOpenAIResponsesNativeCompactionRejectsOtherConvertedProtocolsBeforeForward(t *testing.T) {
	for _, tt := range []struct {
		name, platform, protocol, mappedProtocol string
	}{
		{"deepseek default CC", service.PlatformDeepseek, "", ""},
		{"kimi CC", service.PlatformKimi, service.APIProtocolChatCompletions, ""},
		{"minimax CC", service.PlatformMiniMax, service.APIProtocolChatCompletions, ""},
		{"deepseek Anthropic", service.PlatformDeepseek, service.APIProtocolAnthropic, ""},
		{"zhipu adaptive", service.PlatformZhipu, service.APIProtocolAdaptive, ""},
		{"opencode pinned CC", service.PlatformOpenCodeGo, service.APIProtocolChatCompletions, ""},
		{"opencode mapped CC", service.PlatformOpenCodeGo, service.APIProtocolAdaptive, service.APIProtocolChatCompletions},
		{"opencode mapped Anthropic", service.PlatformOpenCodeGo, service.APIProtocolAdaptive, service.APIProtocolAnthropic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			credentials := map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example.invalid", "api_protocol": tt.protocol, "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"}}
			if tt.mappedProtocol != "" {
				credentials["model_mapping"] = map[string]any{"deepseek-v4.1-flash": "mapped-fixture"}
				credentials["protocol_rules"] = []any{
					map[string]any{"pattern": "deepseek-v4.1-flash", "protocol": service.APIProtocolResponses},
					map[string]any{"pattern": "mapped-fixture", "protocol": tt.mappedProtocol},
				}
			}
			account := service.Account{ID: 101, Name: "native-guard-fixture", Platform: tt.platform,
				Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: credentials}
			body := `{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`
			recorder, upstream := runNativeGuardHandlerRequest(t, []service.Account{account}, tt.platform, "/v1/responses", body)
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			require.Empty(t, upstream.accounts, "native compaction must not be converted to a different upstream protocol")
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			require.Equal(t, "compact_not_supported", gjson.Get(recorder.Body.String(), "error.type").String())
			require.Contains(t, recorder.Body.String(), "compaction")
		})
	}
}

func TestOpenAIResponsesNativeCompactionUsesOpenCodeGoMappedResponsesProtocol(t *testing.T) {
	account := service.Account{
		ID: 101, Name: "opencode-responses-fixture", Platform: service.PlatformOpenCodeGo,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key": "fixture-only", "base_url": "https://upstream.example.invalid",
			"api_protocol":  service.APIProtocolAdaptive,
			"api_base_urls": map[string]any{service.APIProtocolResponses: "https://upstream.example.invalid"},
			"model_mapping": map[string]any{"deepseek-v4.1-flash": "mapped-fixture"},
			"protocol_rules": []any{
				map[string]any{"pattern": "deepseek-v4.1-flash", "protocol": service.APIProtocolChatCompletions},
				map[string]any{"pattern": "mapped-fixture", "protocol": service.APIProtocolResponses},
			},
		},
		Extra: map[string]any{"openai_responses_supported": false},
	}
	body := `{"model":"deepseek-v4.1-flash","stream":true,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`
	recorder, upstream := runNativeGuardHandlerRequest(t, []service.Account{account}, service.PlatformOpenCodeGo, "/v1/responses", body)
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Equal(t, []int64{101}, upstream.accounts)
	require.Equal(t, []string{"/v1/responses"}, upstream.paths)
	require.Equal(t, "mapped-fixture", gjson.GetBytes(upstream.bodies[0], "model").String())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(upstream.bodies[0], "input.1.type").String())
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestOpenAIResponsesNativeCompactionRejectsLegacyCompactOnDeepSeekCC(t *testing.T) {
	account := service.Account{ID: 101, Name: "cc-fixture", Platform: service.PlatformDeepseek,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example.invalid", "api_protocol": service.APIProtocolChatCompletions, "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"}}}
	for _, tt := range []struct {
		name, path, body string
	}{
		{"legacy endpoint", "/v1/responses/compact", `{"model":"deepseek-v4.1-flash","input":"fixture"}`},
		{"promoted trigger", "/v1/responses", `{"model":"deepseek-v4.1-flash","stream":false,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder, upstream := runNativeGuardHandlerRequest(t, []service.Account{account}, service.PlatformDeepseek, tt.path, tt.body)
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			require.Empty(t, upstream.accounts)
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			require.Equal(t, "compact_not_supported", gjson.Get(recorder.Body.String(), "error.type").String())
		})
	}
}
