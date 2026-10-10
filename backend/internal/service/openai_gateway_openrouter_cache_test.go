//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openRouterCacheTestClaudeBody = `{"model":"anthropic/claude-sonnet-4.5","input":[{"type":"message","role":"user","content":"hello"}],"instructions":"be brief","reasoning":{"effort":"high","summary":"auto"},"max_output_tokens":4096,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"prompt_cache_key":"sess-openrouter","stream":true}`

func newOpenRouterCacheTestContext(t *testing.T, requestPath string, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, requestPath, bytes.NewReader(body))
	return c
}

func newOpenRouterNativeResponsesTestAccount(baseURL string) *Account {
	return &Account{
		ID:       4242,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": baseURL,
			"api_key":  "sk-or-v1-test",
		},
	}
}

func readOpenRouterCacheUpstreamBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())
	req.Body = io.NopCloser(bytes.NewReader(raw))
	return raw
}

func TestBuildUpstreamRequestOpenRouterClaudeResponsesInjectsAutoCacheControl(t *testing.T) {
	body := []byte(openRouterCacheTestClaudeBody)
	c := newOpenRouterCacheTestContext(t, "/v1/responses", body)
	svc := &OpenAIGatewayService{}
	account := newOpenRouterNativeResponsesTestAccount("https://openrouter.ai/api")

	req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "or-token", true, "sess-openrouter", false)
	require.NoError(t, err)
	require.Equal(t, "openrouter.ai", req.URL.Hostname())
	require.Equal(t, "/api/v1/responses", req.URL.Path)

	upstreamBody := readOpenRouterCacheUpstreamBody(t, req)
	require.Equal(t, "ephemeral", gjson.GetBytes(upstreamBody, "cache_control.type").String())
	require.Equal(t, "1h", gjson.GetBytes(upstreamBody, "cache_control.ttl").String())
}

func TestBuildUpstreamRequestOpenAIPassthroughOpenRouterClaudeResponsesInjectsAutoCacheControl(t *testing.T) {
	body := []byte(openRouterCacheTestClaudeBody)
	c := newOpenRouterCacheTestContext(t, "/v1/responses", body)
	svc := &OpenAIGatewayService{}
	account := newOpenRouterNativeResponsesTestAccount("https://openrouter.ai/api")

	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "or-token")
	require.NoError(t, err)
	require.Equal(t, "openrouter.ai", req.URL.Hostname())
	require.Equal(t, "/api/v1/responses", req.URL.Path)

	upstreamBody := readOpenRouterCacheUpstreamBody(t, req)
	require.Equal(t, "ephemeral", gjson.GetBytes(upstreamBody, "cache_control.type").String())
	require.Equal(t, "1h", gjson.GetBytes(upstreamBody, "cache_control.ttl").String())
}

const openRouterCacheTestResponsesURL = "https://openrouter.ai/api/v1/responses"

func TestOpenRouterAutoCacheControlGateScopesHostPathAndModel(t *testing.T) {
	claudeBody := []byte(openRouterCacheTestClaudeBody)
	tests := []struct {
		name      string
		targetURL string
		body      []byte
		injected  bool
	}{
		{name: "openrouter api v1 path", targetURL: openRouterCacheTestResponsesURL, body: claudeBody, injected: true},
		{name: "openrouter bare v1 path", targetURL: "https://openrouter.ai/v1/responses", body: claudeBody, injected: true},
		{name: "lookalike host", targetURL: "https://openrouter.ai.evil/api/v1/responses", body: claudeBody},
		{name: "subdomain host", targetURL: "https://proxy.openrouter.ai/api/v1/responses", body: claudeBody},
		{name: "other host", targetURL: "https://api.openai.com/v1/responses", body: claudeBody},
		{name: "compact subpath", targetURL: openRouterCacheTestResponsesURL + "/compact", body: claudeBody},
		{name: "input tokens subpath", targetURL: openRouterCacheTestResponsesURL + "/input_tokens", body: claudeBody},
		{name: "trailing slash", targetURL: openRouterCacheTestResponsesURL + "/", body: claudeBody},
		{name: "non claude model", targetURL: openRouterCacheTestResponsesURL, body: []byte(`{"model":"openai/gpt-5.6","input":[]}`)},
		{name: "api model prefix without claude", targetURL: openRouterCacheTestResponsesURL, body: []byte(`{"model":"deepseek/deepseek-chat","input":[]}`)},
		{name: "missing model", targetURL: openRouterCacheTestResponsesURL, body: []byte(`{"input":[]}`)},
		{name: "non json body", targetURL: openRouterCacheTestResponsesURL, body: []byte(`not json at all`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := injectOpenRouterClaudeAutoCacheControl(tt.targetURL, tt.body)
			require.NoError(t, err)
			if tt.injected {
				require.Equal(t, "ephemeral", gjson.GetBytes(out, "cache_control.type").String(), string(out))
				require.Equal(t, "1h", gjson.GetBytes(out, "cache_control.ttl").String(), string(out))
				return
			}
			require.Equal(t, tt.body, out, "gated request must keep body bytes unchanged")
		})
	}
}

func TestOpenRouterAutoCacheControlTTLDefaultAndPreservation(t *testing.T) {
	target := openRouterCacheTestResponsesURL
	const prefix = `{"model":"anthropic/claude-sonnet-4.5","input":[]`

	t.Run("missing directive injects ephemeral 1h", func(t *testing.T) {
		body := []byte(prefix + `}`)
		out, err := injectOpenRouterClaudeAutoCacheControl(target, body)
		require.NoError(t, err)
		require.Equal(t, `{"type":"ephemeral","ttl":"1h"}`, gjson.GetBytes(out, "cache_control").Raw)
	})

	t.Run("exact ephemeral without ttl fills 1h only", func(t *testing.T) {
		body := []byte(prefix + `,"cache_control":{"type":"ephemeral"}}`)
		out, err := injectOpenRouterClaudeAutoCacheControl(target, body)
		require.NoError(t, err)
		require.Equal(t, "ephemeral", gjson.GetBytes(out, "cache_control.type").String())
		require.Equal(t, "1h", gjson.GetBytes(out, "cache_control.ttl").String())
	})

	preserved := []struct {
		name       string
		cacheField string
	}{
		{name: "explicit ttl wins", cacheField: `,"cache_control":{"type":"ephemeral","ttl":"5m"}`},
		{name: "client null ttl wins", cacheField: `,"cache_control":{"type":"ephemeral","ttl":null}`},
		{name: "client null directive wins", cacheField: `,"cache_control":null`},
		{name: "client false directive wins", cacheField: `,"cache_control":false`},
		{name: "unsupported type wins", cacheField: `,"cache_control":{"type":"customer"}`},
		{name: "empty object wins", cacheField: `,"cache_control":{}}`},
		{name: "string directive wins", cacheField: `,"cache_control":"ephemeral"`},
		{name: "uppercase ephemeral type wins", cacheField: `,"cache_control":{"type":"EPHEMERAL"}`},
		{name: "mixed case ephemeral type wins", cacheField: `,"cache_control":{"type":"Ephemeral"}`},
	}
	for _, tt := range preserved {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(prefix + tt.cacheField + `}`)
			out, err := injectOpenRouterClaudeAutoCacheControl(target, body)
			require.NoError(t, err)
			require.Equal(t, body, out, "explicit client directive must keep body bytes unchanged")
		})
	}
}

func TestOpenRouterAutoCacheControlPreservesNonExactEphemeralTypeCasing(t *testing.T) {
	for _, cacheField := range []string{
		`{"type":"EPHEMERAL"}`,
		`{"type":"Ephemeral"}`,
		`{"type":" ephemeral"}`,
	} {
		body := []byte(`{"model":"anthropic/claude-sonnet-4.5","input":[],"cache_control":` + cacheField + `}`)
		out, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, body)
		require.NoError(t, err)
		require.Equal(t, body, out, cacheField)
		require.False(t, gjson.GetBytes(out, "cache_control.ttl").Exists(), cacheField)
	}
}

func TestOpenRouterAutoCacheControlIdempotent(t *testing.T) {
	first, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, []byte(`{"model":"anthropic/claude-sonnet-4.5","input":[]}`))
	require.NoError(t, err)
	second, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, first)
	require.NoError(t, err)
	require.Equal(t, first, second)

	seeded, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, []byte(`{"model":"anthropic/claude-sonnet-4.5","input":[],"cache_control":{"type":"ephemeral"}}`))
	require.NoError(t, err)
	require.Equal(t, "1h", gjson.GetBytes(seeded, "cache_control.ttl").String())
	again, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, seeded)
	require.NoError(t, err)
	require.Equal(t, seeded, again)
}

func TestOpenRouterAutoCacheControlSkipsExplicitPromptCacheOptionsMode(t *testing.T) {
	plain := []byte(`{"model":"anthropic/claude-sonnet-4.5","input":[],"prompt_cache_options":{"mode":"explicit"}}`)
	out, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, plain)
	require.NoError(t, err)
	require.Equal(t, plain, out)

	withBreakpoint := []byte(`{"model":"anthropic/claude-sonnet-4.5","input":[],"prompt_cache_options":{"mode":"explicit"},"cache_control":{"type":"ephemeral"}}`)
	kept, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, withBreakpoint)
	require.NoError(t, err)
	require.Equal(t, withBreakpoint, kept)
}

func TestOpenRouterAutoCacheControlPreservesNativeResponsesPayload(t *testing.T) {
	body := []byte(openRouterCacheTestClaudeBody)
	c := newOpenRouterCacheTestContext(t, "/v1/responses", body)
	svc := &OpenAIGatewayService{}
	account := newOpenRouterNativeResponsesTestAccount("https://openrouter.ai/api")

	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "or-token")
	require.NoError(t, err)
	upstreamBody := readOpenRouterCacheUpstreamBody(t, req)

	require.Equal(t, "anthropic/claude-sonnet-4.5", gjson.GetBytes(upstreamBody, "model").String())
	require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(upstreamBody, "input").Raw)
	require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(upstreamBody, "tools").Raw)
	require.Equal(t, gjson.GetBytes(body, "reasoning").Raw, gjson.GetBytes(upstreamBody, "reasoning").Raw)
	require.Equal(t, "high", gjson.GetBytes(upstreamBody, "reasoning.effort").String())
	require.Equal(t, int64(4096), gjson.GetBytes(upstreamBody, "max_output_tokens").Int())
	require.Equal(t, "be brief", gjson.GetBytes(upstreamBody, "instructions").String())
	require.Equal(t, "sess-openrouter", gjson.GetBytes(upstreamBody, "prompt_cache_key").String())
	require.True(t, gjson.GetBytes(upstreamBody, "stream").Bool())

	stripped, err := sjson.DeleteBytes(upstreamBody, "cache_control")
	require.NoError(t, err)
	require.Equal(t, body, stripped, "removing the injected directive must restore the original payload bytes")
}

func TestOpenRouterAutoCacheControlDoesNotAddUnrequestedFields(t *testing.T) {
	body := []byte(`{"model":"anthropic/claude-sonnet-4.5","input":[],"prompt_cache_key":"k"}`)
	out, err := injectOpenRouterClaudeAutoCacheControl(openRouterCacheTestResponsesURL, body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "max_output_tokens").Exists())
	require.False(t, gjson.GetBytes(out, "reasoning").Exists())
	require.False(t, gjson.GetBytes(out, "store").Exists())
	require.Equal(t, "k", gjson.GetBytes(out, "prompt_cache_key").String())
}

func TestBuildUpstreamRequestOpenAIPassthroughOpenRouterAutoCacheControlGates(t *testing.T) {
	tests := []struct {
		name        string
		requestPath string
		baseURL     string
		body        []byte
	}{
		{name: "lookalike host", requestPath: "/v1/responses", baseURL: "https://openrouter.ai.evil/api", body: []byte(openRouterCacheTestClaudeBody)},
		{name: "compact subpath", requestPath: "/v1/responses/compact", baseURL: "https://openrouter.ai/api", body: []byte(openRouterCacheTestClaudeBody)},
		{name: "non claude model", requestPath: "/v1/responses", baseURL: "https://openrouter.ai/api", body: []byte(`{"model":"openai/gpt-5.6","input":[]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newOpenRouterCacheTestContext(t, tt.requestPath, tt.body)
			svc := &OpenAIGatewayService{}
			account := newOpenRouterNativeResponsesTestAccount(tt.baseURL)

			req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, tt.body, "or-token")
			require.NoError(t, err)
			upstreamBody := readOpenRouterCacheUpstreamBody(t, req)
			require.Equal(t, tt.body, upstreamBody, "gated upstream request body must stay byte-identical")
		})
	}
}
