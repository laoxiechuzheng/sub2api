//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 账号、凭据和响应均为合成值；transport 是唯一上游替身，真实服务负责构造和解析。
type v8OpenAIProbeTransport struct {
	requests             []*http.Request
	bodies               [][]byte
	rejectSyntheticCodex bool
}

func (u *v8OpenAIProbeTransport) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body []byte
	var err error
	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
	}
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	u.requests = append(u.requests, req)
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	if u.rejectSyntheticCodex && req.URL.Hostname() == "api.openai.com" &&
		(v8HeaderValue(req.Header, "Originator") != "" || v8HeaderValue(req.Header, "X-Codex-Window-ID") != "") {
		// 仅用于证明请求差分的影响，不是用户错误原文或 OpenAI 的真实拒绝策略。
		return v8ProbeResponse(http.StatusBadRequest, "application/json", `{"error":{"message":"SYNTHETIC_FIXTURE_REJECTED_CODEX_IDENTITY"}}`), nil
	}
	if req.Method == http.MethodGet {
		return v8ProbeResponse(http.StatusOK, "application/json", `{"object":"list","data":[{"id":"gpt-5.4","object":"model","owned_by":"openai"}]}`), nil
	}
	if strings.HasSuffix(req.URL.Path, "/images/generations") {
		return v8ProbeResponse(http.StatusOK, "application/json", `{"data":[{"b64_json":"aGk="}]}`), nil
	}
	if gjson.GetBytes(body, "messages").Exists() {
		return v8ProbeResponse(http.StatusOK, "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
	}
	if gjson.GetBytes(body, "input.#(type==compaction_trigger)").Exists() {
		return v8ProbeResponse(http.StatusOK, "text/event-stream", compactProbeSSESuccessBody), nil
	}
	if gjson.GetBytes(body, "stream").Bool() {
		return v8ProbeResponse(http.StatusOK, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_v8\",\"model\":\"gpt-5.4\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"), nil
	}
	return v8ProbeResponse(http.StatusOK, "application/json", `{"id":"resp_v8","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`), nil
}

func (u *v8OpenAIProbeTransport) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func v8ProbeResponse(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func v8ProbeAccount(accountType, base string) *Account {
	credentials := map[string]any{"api_key": "synthetic-v8-api-key"}
	if accountType != AccountTypeAPIKey {
		credentials = map[string]any{"access_token": "synthetic-v8-oauth-token"}
	}
	if base != "" {
		credentials["base_url"] = base
	}
	return &Account{ID: 38001, Name: "synthetic-openai-v8", Platform: PlatformOpenAI, Type: accountType, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: credentials}
}

func v8RunAccountProbe(t *testing.T, account *Account, transport *v8OpenAIProbeTransport, model, mode string) (error, string) {
	t.Helper()
	repo := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{*account}}}
	// 允许 HTTP 只为验证本策略不会把非 HTTPS 自定义目标认作官方；transport 不联网。
	svc := &AccountTestService{accountRepo: repo, httpUpstream: transport, cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/38001/test", nil)
	err := svc.TestAccountConnection(c, account.ID, model, "synthetic prompt", mode)
	return err, rec.Body.String()
}

// 覆写按真实 wire casing 保存，不能依赖 Header.Get 的 canonical map key。
func v8HeaderValue(header http.Header, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func v8LogWire(t *testing.T, label string, u *v8OpenAIProbeTransport) {
	t.Helper()
	require.NotEmpty(t, u.requests)
	i := len(u.requests) - 1
	req, body := u.requests[i], u.bodies[i]
	header := req.Header.Clone()
	if value := v8HeaderValue(header, "X-Codex-Window-ID"); value != "" {
		_, err := uuid.Parse(value)
		require.NoError(t, err)
		header.Set("X-Codex-Window-ID", "<synthetic-window-uuid>")
	}
	if header.Get("Authorization") != "" {
		header.Set("Authorization", "Bearer <synthetic-credential>")
	}
	for _, key := range []string{"Session_ID", "Conversation_ID"} {
		if header.Get(key) != "" {
			header.Set(key, "<synthetic-session>")
		}
	}
	var payload any
	if len(body) == 0 {
		payload = map[string]any{}
	} else {
		require.NoError(t, json.Unmarshal(body, &payload))
	}
	digest := sha256.Sum256(body)
	snapshot := map[string]any{"label": label, "url": req.URL.String(), "host": req.Host, "method": req.Method, "headers": header, "body": payload, "body_sha256": hex.EncodeToString(digest[:])}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	t.Logf("V8_WIRE %s", encoded)
}

func v8RequireNativeProbe(t *testing.T, u *v8OpenAIProbeTransport, model string) {
	t.Helper()
	require.NotEmpty(t, u.requests)
	i := len(u.requests) - 1
	for _, name := range []string{"Originator", "User-Agent", "Version", "OpenAI-Beta", "X-Codex-Window-ID"} {
		assert.Empty(t, v8HeaderValue(u.requests[i].Header, name), "不应自动注入 %s", name)
	}
	expected := map[string]any{
		"model": model, "stream": true,
		"input": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}}},
	}
	encoded, err := json.Marshal(expected)
	require.NoError(t, err)
	require.JSONEq(t, string(encoded), string(u.bodies[i]))
	require.Equal(t, "Bearer synthetic-v8-api-key", u.requests[i].Header.Get("Authorization"))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(u.requests[i].Context()))
}

func TestV8OpenAIProbeWireCharacterization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name, accountType, base, model, mode, wantURL, wantModel string
		extra                                                    map[string]any
		mapping                                                  map[string]any
		wantOAuth, wantCodex, wantNative, override               bool
	}{
		{name: "official_default", accountType: AccountTypeAPIKey, wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantNative: true},
		{name: "official_v1", accountType: AccountTypeAPIKey, base: "https://api.openai.com/v1", model: "gpt-5.4", wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantNative: true},
		{name: "official_mapped", accountType: AccountTypeAPIKey, model: "public-text", mapping: map[string]any{"public-text": "gpt-5.4"}, wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantNative: true},
		{name: "official_force_chat", accountType: AccountTypeAPIKey, model: "gpt-5.4", extra: map[string]any{openai_compat.ExtraKeyResponsesMode: "force_chat_completions", openai_compat.ExtraKeyResponsesSupported: true}, wantURL: "https://api.openai.com/v1/chat/completions", wantModel: "gpt-5.4"},
		{name: "official_force_responses", accountType: AccountTypeAPIKey, model: "gpt-5.4", extra: map[string]any{openai_compat.ExtraKeyResponsesMode: "force_responses", openai_compat.ExtraKeyResponsesSupported: false}, wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantNative: true},
		{name: "official_passthrough", accountType: AccountTypeAPIKey, model: "gpt-5.4", extra: map[string]any{"openai_passthrough": true}, wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantNative: true},
		{name: "oauth_default", accountType: AccountTypeOAuth, wantURL: chatgptCodexAPIURL, wantModel: "gpt-5.4", wantOAuth: true, wantCodex: true},
		{name: "setup_default", accountType: AccountTypeSetupToken, wantURL: chatgptCodexAPIURL, wantModel: "gpt-5.4", wantOAuth: true, wantCodex: true},
		{name: "oauth_codex_model", accountType: AccountTypeOAuth, model: "gpt-5.3-codex-spark", wantURL: chatgptCodexAPIURL, wantModel: "gpt-5.3-codex-spark", wantOAuth: true, wantCodex: true},
		{name: "official_explicit_codex_model", accountType: AccountTypeAPIKey, model: "gpt-5.3-codex-spark", wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.3-codex-spark", wantNative: true},
		{name: "custom_responses", accountType: AccountTypeAPIKey, base: "https://compat.example/v1", model: "gpt-5.4", wantURL: "https://compat.example/v1/responses", wantModel: "gpt-5.4", wantCodex: true},
		{name: "official_header_override", accountType: AccountTypeAPIKey, model: "gpt-5.4", override: true, wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantCodex: true, wantNative: true},
		{name: "official_compact", accountType: AccountTypeAPIKey, model: "gpt-5.4", mode: AccountTestModeCompact, wantURL: "https://api.openai.com/v1/responses", wantModel: "gpt-5.4", wantCodex: true},
		{name: "official_image", accountType: AccountTypeAPIKey, model: "gpt-image-1", wantURL: "https://api.openai.com/v1/images/generations", wantModel: "gpt-image-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := v8ProbeAccount(tc.accountType, tc.base)
			account.Extra = tc.extra
			if tc.mapping != nil {
				account.Credentials["model_mapping"] = tc.mapping
			}
			if tc.override {
				account.Credentials["header_override_enabled"] = true
				account.Credentials["header_overrides"] = map[string]any{"User-Agent": "synthetic-explicit-client/1.0", "Originator": "synthetic-explicit-client", "X-Custom-Probe": "keep"}
			}
			u := &v8OpenAIProbeTransport{}
			err, output := v8RunAccountProbe(t, account, u, tc.model, tc.mode)
			require.NoError(t, err)
			require.Len(t, u.requests, 1)
			require.Contains(t, output, `"success":true`)
			require.Equal(t, tc.wantURL, u.requests[0].URL.String())
			body := u.bodies[0]
			require.Equal(t, tc.wantModel, gjson.GetBytes(body, "model").String())
			require.Equal(t, tc.wantCodex, v8HeaderValue(u.requests[0].Header, "Originator") != "")
			if tc.override {
				require.Equal(t, "synthetic-explicit-client/1.0", v8HeaderValue(u.requests[0].Header, "User-Agent"))
				require.Equal(t, "keep", v8HeaderValue(u.requests[0].Header, "X-Custom-Probe"))
				require.Equal(t, "synthetic-explicit-client", v8HeaderValue(u.requests[0].Header, "Originator"))
				for _, name := range []string{"X-Codex-Window-ID", "OpenAI-Beta", "Version"} {
					require.Empty(t, v8HeaderValue(u.requests[0].Header, name))
				}
			} else if tc.wantNative {
				v8RequireNativeProbe(t, u, tc.wantModel)
			}
			if tc.wantOAuth {
				require.Equal(t, "chatgpt.com", u.requests[0].Host)
				require.True(t, gjson.GetBytes(body, "store").Exists())
				require.False(t, gjson.GetBytes(body, "store").Bool())
			}
			if strings.HasSuffix(tc.wantURL, "/responses") && tc.mode != AccountTestModeCompact {
				if tc.wantNative {
					require.False(t, gjson.GetBytes(body, "instructions").Exists())
				} else {
					require.Equal(t, openai.DefaultInstructions, gjson.GetBytes(body, "instructions").String())
				}
			}
			v8LogWire(t, tc.name, u)
		})
	}
}

func TestV8OpenAIOfficialAPIKeyProbeDoesNotInjectCodexIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &v8OpenAIProbeTransport{}
	err, _ := v8RunAccountProbe(t, v8ProbeAccount(AccountTypeAPIKey, ""), u, "gpt-5.4", AccountTestModeDefault)
	require.NoError(t, err)
	require.Len(t, u.requests, 1)
	v8LogWire(t, "official_contract", u)
	assert.Empty(t, v8HeaderValue(u.requests[0].Header, "Originator"), "普通官方 APIKey 测试不应自动宣称 Codex 身份")
	assert.Empty(t, v8HeaderValue(u.requests[0].Header, "X-Codex-Window-ID"), "不应自动伪造 Codex 引擎指纹")
	assert.NotEqual(t, openai.DefaultInstructions, gjson.GetBytes(u.bodies[0], "instructions").String(), "普通 APIKey 连通性测试不应强制 Codex 编程代理提示词")
	v8RequireNativeProbe(t, u, "gpt-5.4")
}

func TestV8OpenAIGroupAPIVersusAccountProbeSyntheticIdentityRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := v8ProbeAccount(AccountTypeAPIKey, "")
	u := &v8OpenAIProbeTransport{rejectSyntheticCodex: true}
	cfg := &config.Config{}
	gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: u, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
	body := []byte(`{"model":"gpt-5.4","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	groupID := int64(38002)
	c.Set("api_key", &APIKey{ID: 38003, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI, RateMultiplier: 1}})
	result, err := gateway.Forward(context.Background(), c, account, body)
	require.NoError(t, err, "合成 Group/API context 下真实 Forward 应可用")
	require.NotNil(t, result)
	v8LogWire(t, "group_api_forward", u)
	err, output := v8RunAccountProbe(t, v8ProbeAccount(AccountTypeAPIKey, ""), u, "gpt-5.4", AccountTestModeDefault)
	require.Len(t, u.requests, 2)
	v8LogWire(t, "account_probe", u)
	if err != nil {
		t.Logf("V8_SYNTHETIC_ONLY account_error=%v output=%s", err, output)
	}
	require.NoError(t, err, "同一合成 transport 下转发与测试均应成功；不代表 OpenAI 官方策略或用户确切错误")
	require.Contains(t, output, `"success":true`)
}

func TestV8OpenAIAccountModelPickerUsesAPIKeyCatalogAndMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := v8ProbeAccount(AccountTypeAPIKey, "")
	account.Credentials["model_mapping"] = map[string]any{"public-text": "gpt-5.4"}
	u := &v8OpenAIProbeTransport{}
	gateway := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: u}
	svc := &AccountTestService{openaiGatewayService: gateway}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, u.requests, 1)
	require.Equal(t, "https://api.openai.com/v1/models", u.requests[0].URL.String())
	require.Empty(t, u.requests[0].Header.Get("Originator"))
	require.Empty(t, u.requests[0].URL.Query().Get("client_version"))
	require.Len(t, models, 1)
	require.Equal(t, "public-text", models[0].ID)
	v8LogWire(t, "api_key_model_picker", u)
}

func TestV8OpenAIOfficialTargetBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name, base string
		wantNative bool
	}{
		{"default", "", true},
		{"root", "https://api.openai.com", true},
		{"v1", "https://api.openai.com/v1", true},
		{"endpoint", "https://api.openai.com/v1/responses", true},
		{"trailing_slash", "https://api.openai.com/v1/", true},
		{"default_https_port", "https://api.openai.com:443/v1", true},
		{"case_insensitive_host", "https://API.OPENAI.COM/v1", true},
		{"http_not_https", "http://api.openai.com/v1", false},
		{"custom_host", "https://compat.example/v1", false},
		{"lookalike_suffix", "https://api.openai.com.evil.example/v1", false},
		{"lookalike_prefix", "https://evil-api.openai.com/v1", false},
		{"userinfo_with_official_host", "https://synthetic@api.openai.com/v1", false},
		{"official_looking_userinfo", "https://api.openai.com@compat.example/v1", false},
		{"nonstandard_port", "https://api.openai.com:8443/v1", false},
		{"empty_port", "https://api.openai.com:/v1", false},
		{"dns_trailing_dot", "https://api.openai.com./v1", false},
		{"custom_prefix", "https://api.openai.com/relay/v1", false},
		{"alternate_version", "https://api.openai.com/v2", false},
		{"bare_responses", "https://api.openai.com/responses", false},
		{"case_sensitive_path", "https://api.openai.com/V1/responses", false},
		{"dot_segment", "https://api.openai.com/relay/../v1/responses", false},
		{"extra_query", "https://api.openai.com/v1?route=custom", false},
		{"empty_query", "https://api.openai.com/v1?", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &v8OpenAIProbeTransport{}
			err, output := v8RunAccountProbe(t, v8ProbeAccount(AccountTypeAPIKey, tc.base), u, "gpt-5.4", AccountTestModeDefault)
			require.NoError(t, err)
			require.Len(t, u.requests, 1)
			require.Contains(t, output, `"success":true`)
			if tc.wantNative {
				v8RequireNativeProbe(t, u, "gpt-5.4")
			} else {
				requireOpenAICodexProbeHeaders(t, u.requests[0].Header)
				require.Equal(t, openai.DefaultInstructions, gjson.GetBytes(u.bodies[0], "instructions").String())
			}
			v8LogWire(t, "boundary_"+tc.name, u)
		})
	}
}

func TestV8OpenAIExplicitCodexHeaderOverridesRemainFinal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := v8ProbeAccount(AccountTypeAPIKey, "")
	account.Credentials["header_override_enabled"] = true
	account.Credentials["header_overrides"] = map[string]any{
		"User-Agent": "codex-explicit/1.0", "Originator": "explicit-originator",
		"Version": "explicit-version", "OpenAI-Beta": "explicit-beta",
		"X-Codex-Window-ID": "00000000-0000-4000-8000-000000000038",
	}
	u := &v8OpenAIProbeTransport{}
	err, _ := v8RunAccountProbe(t, account, u, "gpt-5.4", AccountTestModeDefault)
	require.NoError(t, err)
	require.Len(t, u.requests, 1)
	for name, value := range account.Credentials["header_overrides"].(map[string]any) {
		require.Equal(t, value, v8HeaderValue(u.requests[0].Header, name))
	}
	require.Equal(t, "Bearer synthetic-v8-api-key", u.requests[0].Header.Get("Authorization"))
	require.False(t, gjson.GetBytes(u.bodies[0], "instructions").Exists())
}

func TestV8OpenAISharedPayloadCallersKeepCodexInstructions(t *testing.T) {
	for _, isOAuth := range []bool{false, true} {
		payload := createOpenAITestPayload("synthetic-caller-model", isOAuth)
		require.Equal(t, openai.DefaultInstructions, payload["instructions"])
		_, hasStore := payload["store"]
		require.Equal(t, isOAuth, hasStore)
	}
}
