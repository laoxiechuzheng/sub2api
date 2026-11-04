//go:build unit

package service

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Capture at the transport boundary: all routing, credentials, model mapping,
// payload construction and header overrides still run in the real service.
type anthropicAccountTestUpstream struct {
	req         *http.Request
	body        []byte
	proxyURL    string
	accountID   int64
	concurrency int
	profile     *tlsfingerprint.Profile
	response    *http.Response
}

func (u *anthropicAccountTestUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, concurrency, nil)
}

func (u *anthropicAccountTestUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.req, u.proxyURL, u.accountID, u.concurrency, u.profile = req, proxyURL, accountID, concurrency, profile
	var err error
	u.body, err = io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return u.response, nil
}

func newAnthropicAccountTestService(account *Account, response *http.Response) (*AccountTestService, *anthropicAccountTestUpstream, *openAIAccountTestRepo) {
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{
		accountsByID: map[int64]*Account{account.ID: account},
	}}
	upstream := &anthropicAccountTestUpstream{response: response}
	return &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: rawChatCompletionsTestConfig()}, upstream, repo
}

func anthropicAccountTestAPIKeyAccount() *Account {
	return &Account{ID: 751, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Concurrency: 3, Credentials: map[string]any{"api_key": "test-anthropic-key"}}
}

// A native API key probe must remain ordinary API traffic even when automatic
// passthrough is disabled, rather than accidentally selecting the OAuth probe.
func TestAccountTestService_AnthropicAPIKeyUsesNativeRequest(t *testing.T) {
	cases := []struct {
		name        string
		passthrough bool
		baseURL     string
		model       string
		mapping     map[string]any
		wantURL     string
		wantModel   string
	}{
		{"passthrough enabled", true, "", "claude-haiku-5-5", nil, "https://api.anthropic.com/v1/messages", "claude-haiku-5-5"},
		{"passthrough disabled", false, "", "claude-haiku-5-5", nil, "https://api.anthropic.com/v1/messages", "claude-haiku-5-5"},
		{"custom base and wildcard mapping", false, "https://relay.example/anthropic/", "client-haiku", map[string]any{"client-*": "claude-haiku-5-5"}, "https://relay.example/anthropic/v1/messages", "claude-haiku-5-5"},
		{"default model", true, "", "", nil, "https://api.anthropic.com/v1/messages", "claude-sonnet-4-5-20250929"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := anthropicAccountTestAPIKeyAccount()
			account.Extra = map[string]any{"anthropic_passthrough": tc.passthrough}
			account.Credentials["base_url"], account.Credentials["model_mapping"] = tc.baseURL, tc.mapping
			proxyID := int64(82)
			account.ProxyID, account.Proxy = &proxyID, &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 18888}
			svc, upstream, _ := newAnthropicAccountTestService(account, adaptiveCNAnthropicTestResponse())
			ctx, recorder := newTestContext()

			require.NoError(t, svc.TestAccountConnection(ctx, account.ID, tc.model, "", ""))
			require.NotNil(t, upstream.req)
			assert.Equal(t, http.MethodPost, upstream.req.Method)
			assert.Equal(t, tc.wantURL, upstream.req.URL.String())
			assert.Empty(t, upstream.req.URL.RawQuery)
			assert.Equal(t, http.Header{
				"Content-Type": {"application/json"}, "Anthropic-Version": {"2023-06-01"}, "X-Api-Key": {"test-anthropic-key"},
			}, upstream.req.Header)
			assert.JSONEq(t, `{"model":"`+tc.wantModel+`","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, string(upstream.body))
			assert.Equal(t, "http://127.0.0.1:18888", upstream.proxyURL)
			assert.Equal(t, account.ID, upstream.accountID)
			assert.Equal(t, 3, upstream.concurrency)
			assert.Nil(t, upstream.profile)
			assert.Contains(t, recorder.Body.String(), `"text":"anthropic ok"`)
			assert.Contains(t, recorder.Body.String(), `"success":true`)
		})
	}
}

func TestAccountTestService_AnthropicAPIKeyKeepsAuthAndExplicitHeaderOverrides(t *testing.T) {
	for _, tc := range []struct{ name, baseURL, scheme string }{
		{"configured bearer", "https://api.anthropic.com", "authorization_bearer"},
		{"Ollama requires bearer", "https://ollama.com", "x_api_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := anthropicAccountTestAPIKeyAccount()
			account.Credentials["base_url"] = tc.baseURL
			account.Extra = map[string]any{"anthropic_apikey_auth_scheme": tc.scheme}
			account.Credentials["header_override_enabled"] = true
			account.Credentials["header_overrides"] = map[string]any{
				"user-agent": "explicit-test-agent", "anthropic-beta": "explicit-beta", "anthropic-version": "2023-06-01", "x-test-header": "configured",
			}
			svc, upstream, _ := newAnthropicAccountTestService(account, adaptiveCNAnthropicTestResponse())
			ctx, _ := newTestContext()

			require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "claude-haiku-5-5", "", ""))
			assert.Equal(t, "Bearer test-anthropic-key", getHeaderRaw(upstream.req.Header, "authorization"))
			assert.Empty(t, getHeaderRaw(upstream.req.Header, "x-api-key"))
			assert.Equal(t, "explicit-test-agent", getHeaderRaw(upstream.req.Header, "user-agent"))
			assert.Equal(t, "explicit-beta", getHeaderRaw(upstream.req.Header, "anthropic-beta"))
			assert.Equal(t, "configured", getHeaderRaw(upstream.req.Header, "x-test-header"))
			assert.Empty(t, getHeaderRaw(upstream.req.Header, "x-app"))
			assert.Empty(t, getHeaderRaw(upstream.req.Header, "x-stainless-lang"))
		})
	}
}

func TestAccountTestService_AnthropicAPIKeyRejectsInvalidBaseBeforeUpstream(t *testing.T) {
	account := anthropicAccountTestAPIKeyAccount()
	account.Credentials["base_url"] = "file:///invalid-upstream"
	svc, upstream, _ := newAnthropicAccountTestService(account, adaptiveCNAnthropicTestResponse())
	ctx, recorder := newTestContext()

	require.ErrorContains(t, svc.TestAccountConnection(ctx, account.ID, "claude-haiku-5-5", "", ""), "Invalid base URL")
	assert.Nil(t, upstream.req)
	assert.Contains(t, recorder.Body.String(), `"type":"error"`)
}

func TestAccountTestService_AnthropicAPIKeyPreservesUpstreamFailures(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			account := anthropicAccountTestAPIKeyAccount()
			body := `{"type":"error","error":{"type":"invalid_request_error","message":"probe rejected"}}`
			svc, _, repo := newAnthropicAccountTestService(account, newJSONResponse(status, body))
			ctx, recorder := newTestContext()

			require.ErrorContains(t, svc.TestAccountConnection(ctx, account.ID, "claude-haiku-5-5", "", ""), "probe rejected")
			assert.Contains(t, recorder.Body.String(), `"type":"error"`)
			assert.NotContains(t, recorder.Body.String(), `"success":true`)
			if status == http.StatusForbidden {
				assert.Equal(t, account.ID, repo.setErrorID)
				assert.Contains(t, repo.setErrorMsg, "API returned 403")
			} else {
				assert.Zero(t, repo.setErrorID)
			}
		})
	}
}

// Freezing both entropy sources and the runtime CLI version makes an accidental
// change to the existing OAuth/SetupToken request observable byte for byte.
func TestAccountTestService_AnthropicOAuthRequestRemainsByteIdentical(t *testing.T) {
	originalReader := rand.Reader
	claude.SetCLIVersionResolver(func() string { return "2.1.258" })
	t.Cleanup(func() {
		rand.Reader = originalReader
		uuid.SetRand(nil)
		claude.SetCLIVersionResolver(nil)
	})
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			// The request logger also allocates a UUID before the session ID.
			// Constant entropy keeps both IDs deterministic without coupling the
			// fixture to the logger's number of random reads.
			rand.Reader = bytes.NewReader(make([]byte, 4096))
			uuid.SetRand(bytes.NewReader(bytes.Repeat([]byte{0x11}, 4096)))
			account := &Account{ID: 752, Platform: PlatformAnthropic, Type: accountType,
				Concurrency: 2, Credentials: map[string]any{"access_token": "test-oauth-token"},
				Extra: map[string]any{"anthropic_passthrough": true, "enable_tls_fingerprint": true}}
			svc, upstream, _ := newAnthropicAccountTestService(account, adaptiveCNAnthropicTestResponse())
			ctx, _ := newTestContext()

			require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "claude-haiku-5-5", "", ""))
			assert.Equal(t, "https://api.anthropic.com/v1/messages?beta=true", upstream.req.URL.String())
			assert.Equal(t, http.MethodPost, upstream.req.Method)
			assert.Equal(t, http.Header{
				"Content-Type": {"application/json"}, "Anthropic-Version": {"2023-06-01"},
				"Authorization":  {"Bearer test-oauth-token"},
				"Anthropic-Beta": {"claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"},
				"User-Agent":     {"claude-cli/2.1.258 (external, cli)"}, "X-App": {"cli"},
				"X-Stainless-Lang": {"js"}, "X-Stainless-Package-Version": {"0.94.0"},
				"X-Stainless-Os": {"Linux"}, "X-Stainless-Arch": {"arm64"}, "X-Stainless-Runtime": {"node"},
				"X-Stainless-Runtime-Version": {"v24.3.0"}, "X-Stainless-Retry-Count": {"0"}, "X-Stainless-Timeout": {"600"},
				"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
			}, upstream.req.Header)
			const wantBody = `{"max_tokens":1024,"messages":[{"content":[{"cache_control":{"type":"ephemeral"},"text":"hi","type":"text"}],"role":"user"}],"metadata":{"user_id":"{\"device_id\":\"0000000000000000000000000000000000000000000000000000000000000000\",\"account_uuid\":\"\",\"session_id\":\"11111111-1111-4111-9111-111111111111\"}"},"model":"claude-haiku-5-5","stream":true,"system":[{"cache_control":{"type":"ephemeral"},"text":"You are Claude Code, Anthropic's official CLI for Claude.","type":"text"}],"temperature":1}`
			assert.Equal(t, []byte(wantBody), upstream.body)
			assert.Equal(t, "", upstream.proxyURL)
			assert.Equal(t, int64(752), upstream.accountID)
			assert.Equal(t, 2, upstream.concurrency)
			require.NotNil(t, upstream.profile)
			assert.Equal(t, "Built-in Default (Node.js 24.x)", upstream.profile.Name)
			assert.True(t, json.Valid(upstream.body))
		})
	}
}
