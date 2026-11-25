package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type googleOneExactTestRepo struct {
	service.AccountRepository
	account *service.Account
	lookups int
}

func (r *googleOneExactTestRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.lookups++
	if r.account.ID == id {
		return r.account, nil
	}
	return nil, errors.New("missing fixture account")
}

type googleOneExactTestTransport func(*http.Request) (*http.Response, error)

func (f googleOneExactTestTransport) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return f(req)
}

func (f googleOneExactTestTransport) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return f(req)
}

func TestAccountHandlerGoogleOneExactTestBypassesMappingOnlyForThisProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		marker   = "upstream-result"
		private  = "private-fixture-detail"
		validSSE = `data: {"response":{"candidates":[{"content":{"parts":[{"text":"` + marker + `"}]},"finishReason":"STOP"}]}}` + "\n\n"
	)
	for _, tc := range []struct {
		name           string
		body           string
		oauthType      string
		missingProject bool
		upstreamStatus int
		upstreamBody   string
		wantModel      string
		wantError      string
		wantStatus     int
		wantLookups    int
	}{
		{name: "exact bypasses wildcard", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, wantModel: "gemini-4-neon", wantLookups: 1},
		{name: "explicit false maps", body: `{"model_id":"gemini-4-neon","google_one_exact_model":false}`, wantModel: "gemini-2.5-pro", wantLookups: 2},
		{name: "omitting the option maps", body: `{"model_id":"gemini-4-neon"}`, wantModel: "gemini-2.5-pro", wantLookups: 2},
		{name: "empty legacy body maps its default", wantModel: "gemini-2.5-pro", wantLookups: 2},
		{name: "other OAuth rejected before transport", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, oauthType: "code_assist", wantError: "only available for Google One OAuth accounts", wantLookups: 1},
		{name: "missing project cannot fall back to AI Studio", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, missingProject: true, wantError: "requires an existing Google Code Assist project", wantLookups: 1},
		{name: "invalid path not echoed", body: `{"model_id":"gemini-4/` + private + `","google_one_exact_model":true}`, wantError: "valid concrete Gemini model ID", wantLookups: 1},
		{name: "wildcard rejected", body: `{"model_id":"gemini-*","google_one_exact_model":true}`, wantError: "valid concrete Gemini model ID", wantLookups: 1},
		{name: "prefix alone rejected", body: `{"model_id":"gemini-","google_one_exact_model":true}`, wantError: "valid concrete Gemini model ID", wantLookups: 1},
		{name: "wrong flag type rejected", body: `{"model_id":"gemini-4-neon","google_one_exact_model":"` + private + `"}`, wantError: "Invalid account test request", wantStatus: http.StatusBadRequest},
		{name: "upstream HTTP body not echoed", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, upstreamStatus: 404, upstreamBody: private, wantModel: "gemini-4-neon", wantError: "Gemini returned HTTP 404", wantLookups: 1},
		{name: "upstream SSE message not echoed", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, upstreamBody: `data: {"error":{"code":403,"message":"` + private + `"}}` + "\n\n", wantModel: "gemini-4-neon", wantError: "Gemini returned a streaming error (403)", wantLookups: 1},
		{name: "terminal marker alone is not proof", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, upstreamBody: "data: [DONE]\n\n", wantModel: "gemini-4-neon", wantError: "Gemini did not return content", wantLookups: 1},
		{name: "thoughts alone are not visible content", body: `{"model_id":"gemini-4-neon","google_one_exact_model":true}`, upstreamBody: `data: {"response":{"candidates":[{"content":{"parts":[{"text":"` + private + `","thought":true}]},"finishReason":"STOP"}]}}` + "\n\n", wantModel: "gemini-4-neon", wantError: "Gemini did not return content", wantLookups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.oauthType == "" {
				tc.oauthType = "google_one"
			}
			repo := &googleOneExactTestRepo{account: &service.Account{
				ID: 71, Platform: service.PlatformGemini, Type: service.AccountTypeOAuth, Status: service.StatusActive,
				Credentials: map[string]any{
					"oauth_type": tc.oauthType, "access_token": "fixture-token", "project_id": private,
					"model_mapping": map[string]any{"gemini-*": "gemini-2.5-pro"},
				},
			}}
			if tc.missingProject {
				delete(repo.account.Credentials, "project_id")
			}
			requests := 0
			upstream := googleOneExactTestTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "/v1internal:streamGenerateContent", req.URL.Path)
				require.Equal(t, "sse", req.URL.Query().Get("alt"))
				require.Equal(t, "Bearer fixture-token", req.Header.Get("Authorization"))
				var sent struct {
					Model   string `json:"model"`
					Project string `json:"project"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&sent))
				require.Equal(t, tc.wantModel, sent.Model)
				require.Equal(t, private, sent.Project)
				status, body := tc.upstreamStatus, tc.upstreamBody
				if status == 0 {
					status = http.StatusOK
				}
				if body == "" {
					body = validSSE
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			cfg := &config.Config{}
			testSvc := service.NewAccountTestService(repo, &service.GeminiTokenProvider{}, nil, nil, nil, nil, upstream, cfg, nil)
			h := &AccountHandler{accountTestService: testSvc, rateLimitService: service.NewRateLimitService(repo, nil, cfg, nil, nil)}
			router := gin.New()
			router.POST("/api/v1/admin/accounts/:id/test", h.Test)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/71/test", strings.NewReader(tc.body)))

			if tc.wantStatus == 0 {
				tc.wantStatus = http.StatusOK
			}
			require.Equal(t, tc.wantStatus, rec.Code)
			require.Equal(t, tc.wantLookups, repo.lookups)
			if tc.wantModel == "" {
				require.Zero(t, requests)
			} else {
				require.Equal(t, 1, requests)
			}
			require.NotContains(t, rec.Body.String(), private)
			if tc.wantError == "" {
				if strings.Contains(tc.body, `"google_one_exact_model":true`) {
					require.Contains(t, rec.Body.String(), "Gemini returned content for this model.")
					require.NotContains(t, rec.Body.String(), marker)
				} else {
					require.Contains(t, rec.Body.String(), marker)
				}
				require.Contains(t, rec.Body.String(), `"success":true`)
			} else {
				require.Contains(t, rec.Body.String(), tc.wantError)
				require.NotContains(t, rec.Body.String(), `"success":true`)
			}
		})
	}
}
