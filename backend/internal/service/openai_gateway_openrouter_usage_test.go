//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	openRouterUsageTestModel   = "anthropic/claude-sonnet-4.5"
	openRouterUsageTestBaseURL = "https://openrouter.ai/api"
)

// Synthetic OpenRouter native Responses usage: total_tokens counts the 100000
// cache write tokens, while reasoning tokens are already part of the canonical
// output_tokens. The xAI heuristic would add min(9000, 110004-4-10000) and
// inflate output to 19000.
const openRouterUsageTestUsageJSON = `"usage":{"input_tokens":4,"output_tokens":10000,"total_tokens":110004,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":100000},"output_tokens_details":{"reasoning_tokens":9000}}`

func openRouterUsageTestJSONBody() string {
	return `{"id":"resp_or_usage","object":"response","model":"` + openRouterUsageTestModel + `","output":[],` + openRouterUsageTestUsageJSON + `}`
}

func openRouterUsageTestSSEBody() string {
	completed := `{"type":"response.completed","response":{"id":"resp_or_usage","status":"completed","model":"` + openRouterUsageTestModel + `","output":[],` + openRouterUsageTestUsageJSON + `}}`
	return "data: " + completed + "\n\ndata: [DONE]\n\n"
}

func newOpenRouterUsageTestAccount(baseURL, platform, accountType string) *Account {
	return &Account{
		ID:       7717,
		Platform: platform,
		Type:     accountType,
		Credentials: map[string]any{
			"base_url": baseURL,
			"api_key":  "sk-or-v1-usage-test",
		},
	}
}

func newOpenRouterUsageTestContext(t *testing.T, requestPath string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, requestPath, nil)
	return c
}

func newOpenRouterUsageTestResponse(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func requireOpenRouterUsageCanonical(t *testing.T, usage *OpenAIUsage) {
	t.Helper()
	require.NotNil(t, usage)
	require.Equal(t, 4, usage.InputTokens)
	require.Equal(t, 10000, usage.OutputTokens, "reasoning tokens are already inside canonical OpenRouter output_tokens")
	require.Equal(t, 100000, usage.CacheCreationInputTokens, "cache write tokens stay untouched")
	require.Zero(t, usage.CacheReadInputTokens)
}

func TestOpenRouterNativeResponsesUsageKeepsCanonicalOutputForStandardHandlers(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		resp := newOpenRouterUsageTestResponse("application/json", openRouterUsageTestJSONBody())

		result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, openRouterUsageTestModel, openRouterUsageTestModel)
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})

	t.Run("sse_to_json", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		resp := newOpenRouterUsageTestResponse("text/event-stream", openRouterUsageTestSSEBody())

		result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, openRouterUsageTestModel, openRouterUsageTestModel)
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})

	t.Run("stream", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		resp := newOpenRouterUsageTestResponse("text/event-stream", openRouterUsageTestSSEBody())

		result, err := svc.handleStreamingResponseWithReasoning(
			context.Background(), resp, c, account, time.Now(), openRouterUsageTestModel, openRouterUsageTestModel, "",
		)
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})
}

func TestOpenRouterNativeResponsesUsageKeepsCanonicalOutputForPassthroughHandlers(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		resp := newOpenRouterUsageTestResponse("application/json", openRouterUsageTestJSONBody())

		result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, openRouterUsageTestModel, "")
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})

	t.Run("sse_to_json", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		resp := newOpenRouterUsageTestResponse("text/event-stream", openRouterUsageTestSSEBody())

		result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, openRouterUsageTestModel, "")
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})

	t.Run("stream", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		resp := newOpenRouterUsageTestResponse("text/event-stream", openRouterUsageTestSSEBody())

		result, err := svc.handleStreamingResponsePassthrough(
			context.Background(), resp, c, account, time.Now(), openRouterUsageTestModel, "",
		)
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})
}

func TestOpenRouterNativeResponsesUsageKeepsCanonicalOutputForOrdinaryInputInclusiveCache(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	c := newOpenRouterUsageTestContext(t, "/v1/responses")
	account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
	body := `{"id":"resp_or_cache","object":"response","model":"` + openRouterUsageTestModel + `","output":[],"usage":{"input_tokens":1200,"output_tokens":12,"total_tokens":1212,"input_tokens_details":{"cached_tokens":1024,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":7}}}`
	resp := newOpenRouterUsageTestResponse("application/json", body)

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, openRouterUsageTestModel, openRouterUsageTestModel)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1200, result.usage.InputTokens)
	require.Equal(t, 12, result.usage.OutputTokens)
	require.Equal(t, 1024, result.usage.CacheReadInputTokens)
	require.Zero(t, result.usage.CacheCreationInputTokens)
}

func TestOpenRouterNativeResponsesUsageGateLeavesOtherDestinationsUntouched(t *testing.T) {
	tests := []struct {
		name          string
		requestPath   string
		baseURL       string
		platform      string
		accountType   string
		originalModel string
		mappedModel   string
	}{
		{name: "lookalike host", requestPath: "/v1/responses", baseURL: "https://openrouter.ai.evil/api", platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
		{name: "subdomain host", requestPath: "/v1/responses", baseURL: "https://proxy.openrouter.ai/api", platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
		{name: "compact subpath", requestPath: "/v1/responses/compact", baseURL: openRouterUsageTestBaseURL, platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
		{name: "input tokens subpath", requestPath: "/v1/responses/input_tokens", baseURL: openRouterUsageTestBaseURL, platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
		{name: "rawCC subpath", requestPath: "/v1/responses/rawCC", baseURL: openRouterUsageTestBaseURL, platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
		{name: "non claude model", requestPath: "/v1/responses", baseURL: openRouterUsageTestBaseURL, platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: "openai/gpt-5.6", mappedModel: "openai/gpt-5.6"},
		{name: "oauth account", requestPath: "/v1/responses", baseURL: openRouterUsageTestBaseURL, platform: PlatformOpenAI, accountType: AccountTypeOAuth, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
		{name: "non openrouter endpoint", requestPath: "/v1/responses", baseURL: "https://api.openai.com/v1", platform: PlatformOpenAI, accountType: AccountTypeAPIKey, originalModel: openRouterUsageTestModel, mappedModel: openRouterUsageTestModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			c := newOpenRouterUsageTestContext(t, tt.requestPath)
			account := newOpenRouterUsageTestAccount(tt.baseURL, tt.platform, tt.accountType)
			resp := newOpenRouterUsageTestResponse("application/json", openRouterUsageTestJSONBody())

			result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, tt.originalModel, tt.mappedModel)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 19000, result.usage.OutputTokens, "non native OpenRouter targets keep the existing xAI reasoning heuristic")
			require.Equal(t, 100000, result.usage.CacheCreationInputTokens)
		})
	}
}

func TestXAIReasoningHeuristicStillAppliesToGrokAndDefaultParser(t *testing.T) {
	// The default parser keeps the xAI shape: independent reasoning_tokens are
	// added to billed output when total_tokens proves they are outside.
	usage, ok := extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"prompt_tokens":32,"completion_tokens":9,"total_tokens":135,"completion_tokens_details":{"reasoning_tokens":94}}}`))
	require.True(t, ok)
	require.Equal(t, 103, usage.OutputTokens)

	usage, ok = extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"input_tokens":4,"output_tokens":10000,"total_tokens":110004,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":100000},"output_tokens_details":{"reasoning_tokens":9000}}}`))
	require.True(t, ok)
	require.Equal(t, 19000, usage.OutputTokens, "default parser behavior is unchanged without the OpenRouter gate")

	// Grok accounts never hit the OpenRouter override even for the synthetic
	// fixture shape.
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	c := newOpenRouterUsageTestContext(t, "/v1/responses")
	account := newOpenRouterUsageTestAccount("https://api.x.ai/v1", PlatformGrok, AccountTypeAPIKey)
	resp := newOpenRouterUsageTestResponse("application/json", `{"id":"resp_xai","object":"response","model":"grok-4","output":[],"usage":{"input_tokens":32,"output_tokens":9,"total_tokens":135,"output_tokens_details":{"reasoning_tokens":94}}}`)

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "grok-4", "grok-4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 103, result.usage.OutputTokens, "Grok keeps independent reasoning accounting")
}

// 透传账号的线上模型就是请求 model 本身，账号 model_mapping 不参与出站改写；
// usage 覆盖门控必须按真实线上模型判断，避免映射方向相反时误开关。
func TestOpenRouterNativeResponsesUsagePassthroughIgnoresAccountMapping(t *testing.T) {
	t.Run("claude request mapped to gpt keeps canonical usage", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		account.Extra = map[string]any{"openai_passthrough": true}
		account.Credentials["model_mapping"] = map[string]any{openRouterUsageTestModel: "openai/gpt-5.6"}
		resp := newOpenRouterUsageTestResponse("application/json", openRouterUsageTestJSONBody())

		result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, openRouterUsageTestModel, "")
		require.NoError(t, err)
		require.NotNil(t, result)
		requireOpenRouterUsageCanonical(t, result.usage)
	})

	t.Run("gpt request mapped to claude keeps legacy heuristic", func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		c := newOpenRouterUsageTestContext(t, "/v1/responses")
		account := newOpenRouterUsageTestAccount(openRouterUsageTestBaseURL, PlatformOpenAI, AccountTypeAPIKey)
		account.Extra = map[string]any{"openai_passthrough": true}
		account.Credentials["model_mapping"] = map[string]any{"openai/gpt-5.6": openRouterUsageTestModel}
		resp := newOpenRouterUsageTestResponse("application/json", openRouterUsageTestJSONBody())

		result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "openai/gpt-5.6", "")
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 19000, result.usage.OutputTokens, "non Claude passthrough keeps the existing xAI reasoning heuristic")
		require.Equal(t, 100000, result.usage.CacheCreationInputTokens)
	})
}
