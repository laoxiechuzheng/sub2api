package service

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// openRouterNativeResponsesOutputIncludesReasoning reports whether usage from
// this attempt must treat canonical output_tokens/completion_tokens as already
// reasoning-inclusive. OpenRouter documents reasoning tokens as billed output
// tokens for native Responses requests, and its Responses usage counts cache
// writes inside total_tokens, which makes the shared xAI heuristic
// (openAIUsageFromGJSON) add independent reasoning a second time.
//
// The gate mirrors buildUpstreamRequest's AccountTypeAPIKey branch to rebuild
// the prospective final endpoint and accepts only the bare https OpenRouter
// native Responses endpoint (/api/v1/responses or /v1/responses) with a Claude
// family mapped model. Other destinations, including Grok, keep the default
// parser behavior.
func (s *OpenAIGatewayService) openRouterNativeResponsesOutputIncludesReasoning(c *gin.Context, account *Account, originalModel, mappedModel string) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	if !isClaudeFamilyModelName(openRouterResponsesUsageUpstreamModel(account, originalModel, mappedModel)) {
		return false
	}
	baseURL := account.GetOpenAIBaseURL()
	if account.UsesNativeCNResponses() && account.IsAdaptiveAPIProtocol() {
		baseURL = account.GetCNProtocolBaseURL(APIProtocolResponses)
	}
	if strings.TrimSpace(baseURL) == "" {
		return false
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return false
	}
	targetURL := buildOpenAIResponsesURLForPlatform(account.Platform, validatedURL)
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))
	parsed, err := url.Parse(targetURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	return isOpenRouterNativeResponsesTarget(targetURL)
}

// openRouterResponsesUsageUpstreamModel mirrors the model the forwarders put on
// the wire: the scheduler-admitted mapped model when known, otherwise the
// account's exact request-resolution chain. Passthrough accounts keep the
// request model and never apply account model_mapping; normal accounts apply
// the same mapping chain as the scheduler.
func openRouterResponsesUsageUpstreamModel(account *Account, originalModel, mappedModel string) string {
	if model := strings.TrimSpace(mappedModel); model != "" {
		return model
	}
	originalModel = strings.TrimSpace(originalModel)
	if model := strings.TrimSpace(resolveOpenAIAccountUpstreamModelForRequest(account, originalModel, false)); model != "" {
		return model
	}
	return originalModel
}
