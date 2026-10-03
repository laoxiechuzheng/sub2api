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

type compositeDeepseekFailoverUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	accounts []int64
	models   []string
}

func TestCompositeMessagesExplicitMappingDoesNotUseGroupDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, source := range []string{service.CompositeRouteSourceAccount, service.CompositeRouteSourceExplicit} {
		t.Run(source, func(t *testing.T) {
			account := service.Account{ID: 53, Name: "mapping-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example.invalid", "model_mapping": map[string]any{"claude-sonnet-5-5": "gemini-3.8-flash-high"}}}
			upstream := &compositeDeepseekFailoverUpstream{}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			gateway := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			handler := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			body := []byte(`{"model":"claude-sonnet-5-5","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"Fix login."}]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{Matched: true, Source: source, GroupID: 6, PublicModel: "claude-sonnet-5-5", TargetPlatform: service.PlatformOpenAI, UpstreamModel: "claude-sonnet-5-5", Endpoint: "messages"}))
			id := int64(6)
			key := &service.APIKey{ID: 99, GroupID: &id, Group: &service.Group{ID: id, Platform: service.PlatformComposite, AllowMessagesDispatch: true}, User: &service.User{ID: 100}}
			c.Set(string(middleware2.ContextKeyAPIKey), key)
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
			require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(c, key, "claude-sonnet-5-5"), "explicit Composite routing must not inject the group default model")
			handler.Messages(c)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Equal(t, []int64{53}, upstream.accounts)
			require.Equal(t, []string{"gemini-3.8-flash-high"}, upstream.models)
			require.True(t, key.Group.AllowMessagesDispatch, "mapping must not change endpoint permissions")
		})
	}
}

func (u *compositeDeepseekFailoverUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.accounts = append(u.accounts, accountID)
	u.models = append(u.models, gjson.GetBytes(body, "model").String())
	u.mu.Unlock()
	if accountID == 34 {
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"overloaded_error","message":"upstream unavailable"}}`))}, nil
	}
	response := `{"id":"test-chat","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	if strings.HasSuffix(req.URL.Path, "/messages") {
		response = `{"id":"test-message","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`
	}
	if strings.HasSuffix(req.URL.Path, "/responses") {
		response = `{"id":"test-response","object":"response","status":"completed","model":"deepseek-flash","output":[{"id":"msg","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
	}
	contentType := "application/json"
	if strings.HasSuffix(req.URL.Path, "/responses") && gjson.GetBytes(body, "stream").Bool() {
		response = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
}

func TestCompositeDeepseekAccountFailoverRetainsRouteAndRemapsModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"messages", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			accounts := []service.Account{
				{ID: 34, Name: "primary-test", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 5, Credentials: map[string]any{"api_key": "test-only-primary", "base_url": "https://primary.example.invalid", "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-v4.1-flash"}}},
				{ID: 54, Name: "backup-test", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 10, Credentials: map[string]any{"api_key": "test-only-backup", "base_url": "https://backup.example.invalid", "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek-flash"}}},
			}
			upstream := &compositeDeepseekFailoverUpstream{}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			gateway := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: accounts}, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			handler := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			handler.maxAccountSwitches = 3
			body := `{"model":"deepseek-v4.1-flash","stream":false,"input":"ok"}`
			if endpoint == "messages" {
				body = `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"ok"}]}`
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{Matched: true, Source: "route", GroupID: 6, PublicModel: "claude-opus-5", TargetPlatform: service.PlatformDeepseek, UpstreamModel: "deepseek-v4.1-flash"}))
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = request
			id := int64(6)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 99, GroupID: &id, Group: &service.Group{ID: id, Platform: service.PlatformComposite}, User: &service.User{ID: 100}})
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
			if endpoint == "messages" {
				handler.Messages(c)
			} else {
				handler.Responses(c)
			}
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Equal(t, []int64{34, 54}, upstream.accounts)
			require.Equal(t, []string{"deepseek-v4.1-flash", "deepseek-flash"}, upstream.models)
		})
	}
}
