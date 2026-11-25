package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func googleOneModelSyncTestService(upstream *httpUpstreamRecorder) *AccountTestService {
	return &AccountTestService{
		cfg:                 upstreamModelSyncTestConfig(),
		httpUpstream:        upstream,
		geminiTokenProvider: NewGeminiTokenProvider(nil, &vertexModelsTokenCacheStub{token: "fixture-bearer"}, nil),
	}
}

func googleOneModelSyncTestAccount() *Account {
	proxyID := int64(17)
	return &Account{
		ID: 71, Platform: PlatformGemini, Type: AccountTypeOAuth,
		Credentials: map[string]any{
			"oauth_type":    "google_one",
			"project_id":    "fixture-project",
			"tier_id":       "google_ai_ultra",
			"base_url":      "https://unrelated.example",
			"model_mapping": map[string]any{"gemini-local-configured": "gemini-local-configured"},
		},
		ProxyID: &proxyID,
		Proxy:   &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example", Port: 8080},
	}
}

func TestGoogleOneModelSyncUsesAccountQuotaWithoutGeneration(t *testing.T) {
	const payload = `{"buckets":[
		{"modelId":"gemini-4-carbon","remainingAmount":"quota-only-fixture"},
		{"modelId":"gemini-4-argon","remainingFraction":0},
		{"modelId":"gemini-4-argon"},
		{"modelId":" models/gemini-2.5-flash "},
		{"modelId":"gemini-4-carbon/child"},{"modelId":"gemini-"},{"modelId":"text-bison"},
		{"modelId":19},{"remainingFraction":1},null
	],"models":[{"id":"gemini-unverified"}],"project":"fixture-project"}`
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		vertexModelsJSONResponse(http.StatusOK, payload),
		vertexModelsJSONResponse(http.StatusOK, payload),
	}}
	svc := googleOneModelSyncTestService(upstream)
	repo := &upstreamModelMetadataRepoStub{}
	svc.accountRepo = repo
	account := googleOneModelSyncTestAccount()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := []string{"gemini-2.5-flash", "gemini-4-argon", "gemini-4-carbon"}
	models, err := svc.FetchUpstreamSupportedModels(ctx, account)
	require.NoError(t, err)
	require.Equal(t, want, models)

	catalog, err := svc.SyncUpstreamModelCatalog(ctx, account)
	require.NoError(t, err)
	require.Equal(t, want, catalog.Models)
	require.Equal(t, "gemini_code_assist_quota", catalog.DiscoverySource)
	require.Empty(t, catalog.Metadata)
	require.Empty(t, repo.updates, "a quota lookup should not change normalized model metadata")
	require.Len(t, upstream.requests, 2, "no generation or separate registry request")
	for _, req := range upstream.requests {
		require.Same(t, ctx, req.Context())
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", req.URL.String())
		require.Equal(t, "Bearer fixture-bearer", req.Header.Get("Authorization"))
		require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	}
	require.JSONEq(t, `{"project":"fixture-project"}`, string(upstream.lastBody))
	require.Equal(t, "http://proxy.example:8080", upstream.lastProxyURL)
}

func TestGoogleOneModelSyncRejectsUnverifiedModels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		msg    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"upstream-private-data"}}`, "HTTP 401"},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"upstream-private-data"}}`, "HTTP 403"},
		{"unsupported", http.StatusNotFound, `{"error":{"message":"upstream-private-data"}}`, "HTTP 404"},
		{"empty", http.StatusOK, `{"buckets":[],"models":[{"name":"gemini-4-argon"}]}`, "no Gemini models"},
		{"invalid IDs", http.StatusOK, `{"buckets":[{"modelId":"private-segment"},{"modelId":"gemini/invalid"},{"modelId":3}]}`, "no Gemini models"},
		{"malformed", http.StatusOK, `{"buckets":"upstream-private-data"}`, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: vertexModelsJSONResponse(tc.status, tc.body)}
			svc := googleOneModelSyncTestService(upstream)
			catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), googleOneModelSyncTestAccount())
			var syncErr *UpstreamModelSyncError
			require.ErrorAs(t, err, &syncErr)
			require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
			if tc.status != http.StatusOK {
				require.Equal(t, tc.status, syncErr.StatusCode)
			}
			require.ErrorContains(t, err, tc.msg)
			require.NotContains(t, err.Error(), "upstream-private-data")
			require.Nil(t, catalog)
			require.Len(t, upstream.requests, 1)
		})
	}
}

func TestGoogleOneModelSyncHonorsResponseLimitAndAccountType(t *testing.T) {
	t.Run("oversized response", func(t *testing.T) {
		upstream := &httpUpstreamRecorder{resp: vertexModelsJSONResponse(http.StatusOK, `{"buckets":[{"modelId":"gemini-4-argon"}]}`)}
		svc := googleOneModelSyncTestService(upstream)
		svc.cfg.Gateway.ModelsListReadMaxBytes = 16
		models, err := svc.FetchUpstreamSupportedModels(context.Background(), googleOneModelSyncTestAccount())
		require.ErrorContains(t, err, "too large")
		require.Empty(t, models)
	})
	t.Run("missing project", func(t *testing.T) {
		upstream := &httpUpstreamRecorder{resp: vertexModelsJSONResponse(http.StatusBadRequest, "{}")}
		svc := googleOneModelSyncTestService(upstream)
		account := googleOneModelSyncTestAccount()
		delete(account.Credentials, "project_id")
		_, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
		require.ErrorContains(t, err, "project")
		require.Empty(t, upstream.requests)
	})
	t.Run("Code Assist unchanged", func(t *testing.T) {
		upstream := &httpUpstreamRecorder{resp: vertexModelsJSONResponse(http.StatusBadRequest, "{}")}
		svc := googleOneModelSyncTestService(upstream)
		account := googleOneModelSyncTestAccount()
		account.Credentials["oauth_type"] = "code_assist"
		_, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
		var syncErr *UpstreamModelSyncError
		require.ErrorAs(t, err, &syncErr)
		require.Equal(t, UpstreamModelSyncErrorUnsupported, syncErr.Kind)
		require.Empty(t, upstream.requests)
	})
}
