package service

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenRouter native Responses supports rolling automatic caching via the
// top-level cache_control directive. The gateway injects one at request-build
// time when the client sent none, without converting protocol or touching
// reasoning / output-limit / tooling semantics.
const (
	openRouterNativeResponsesAutoCacheTTL        = "5m"
	openRouterNativeResponsesAutoCacheControlRaw = `{"type":"ephemeral","ttl":"` + openRouterNativeResponsesAutoCacheTTL + `"}`
)

// isOpenRouterNativeResponsesTarget reports whether the final upstream URL is
// exactly the OpenRouter native Responses endpoint. Only the bare endpoint
// qualifies: lookalike hosts (openrouter.ai.evil) and subpaths
// (/compact, /input_tokens, ...) stay untouched.
func isOpenRouterNativeResponsesTarget(targetURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil {
		return false
	}
	if !strings.EqualFold(parsed.Hostname(), "openrouter.ai") {
		return false
	}
	switch parsed.Path {
	case "/api/v1/responses", "/v1/responses":
		return true
	default:
		return false
	}
}

// openRouterAutoCacheExplicitMode reports whether the client already controls
// prompt caching itself (prompt_cache_options.mode=explicit).
func openRouterAutoCacheExplicitMode(body []byte) bool {
	mode := gjson.GetBytes(body, "prompt_cache_options.mode")
	if mode.Type != gjson.String {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(mode.String()), "explicit")
}

// injectOpenRouterClaudeAutoCacheControl injects the top-level automatic cache
// breakpoint for OpenRouter native Responses requests whose final mapped model
// is Claude-family:
//
//   - missing cache_control: {type:ephemeral, ttl:5m}
//   - existing exact {"type":"ephemeral"} without ttl: fill ttl=5m only
//   - explicit ttl, client null/false, or any other type: untouched
//
// sjson raw edits preserve every other byte of the native Responses payload
// (reasoning, max_output_tokens, tools, input, prompt_cache_key, ...).
func injectOpenRouterClaudeAutoCacheControl(targetURL string, body []byte) ([]byte, error) {
	if !isOpenRouterNativeResponsesTarget(targetURL) {
		return body, nil
	}
	model := gjson.GetBytes(body, "model")
	if model.Type != gjson.String || !isClaudeFamilyModelName(model.String()) {
		return body, nil
	}
	if openRouterAutoCacheExplicitMode(body) {
		return body, nil
	}

	cacheControl := gjson.GetBytes(body, "cache_control")
	if !cacheControl.Exists() {
		return sjson.SetRawBytes(body, "cache_control", []byte(openRouterNativeResponsesAutoCacheControlRaw))
	}
	if !cacheControl.IsObject() {
		return body, nil
	}
	// Only the exact JSON enum value counts as a client-supported ephemeral
	// breakpoint; casing variants are left untouched on purpose.
	if cacheControl.Get("type").String() != "ephemeral" {
		return body, nil
	}
	if cacheControl.Get("ttl").Exists() {
		return body, nil
	}
	return sjson.SetBytes(body, "cache_control.ttl", openRouterNativeResponsesAutoCacheTTL)
}

// shouldPreserveOpenRouterClaudeResponsesReasoningReplay reports whether the
// request must keep its native OpenRouter Responses reasoning history untouched
// by the OpenAI-only replay cleanup: an APIKey account that natively forwards
// the plain Responses endpoint to the exact OpenRouter host/path whose actual
// upstream model is Claude-family. Passthrough accounts keep the request model
// (account model_mapping is not applied), mapped accounts use the account map.
func shouldPreserveOpenRouterClaudeResponsesReasoningReplay(s *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) bool {
	if s == nil || account == nil || !account.IsOpenAIApiKey() {
		return false
	}
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return false
	}
	if !isPlainOpenAIResponsesIngressPath(c) {
		return false
	}
	baseURL := account.GetOpenAIBaseURL()
	if account.UsesNativeCNResponses() && account.IsAdaptiveAPIProtocol() {
		baseURL = account.GetCNProtocolBaseURL(APIProtocolResponses)
	}
	targetURL := openaiPlatformAPIURL
	if strings.TrimSpace(baseURL) != "" {
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return false
		}
		targetURL = buildOpenAIResponsesURLForPlatform(account.Platform, validatedURL)
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))
	if !isOpenRouterNativeResponsesTarget(targetURL) {
		return false
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return false
	}
	return isClaudeFamilyModelName(resolveOpenAIAccountUpstreamModelForRequest(account, model, false))
}

// isPlainOpenAIResponsesIngressPath rejects /compact, /input_tokens and
// trailing-slash variants that the upstream path builder would absorb silently.
func isPlainOpenAIResponsesIngressPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := strings.TrimSpace(c.Request.URL.Path)
	if path == "" || strings.HasSuffix(path, "/") {
		return false
	}
	return strings.HasSuffix(path, "/responses")
}
