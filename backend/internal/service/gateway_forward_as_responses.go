package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ForwardAsResponses accepts an OpenAI Responses API request body, converts it
// to Anthropic Messages format, forwards to the Anthropic upstream, and converts
// the response back to Responses format. This enables OpenAI Responses API
// clients to access Anthropic models through Anthropic platform groups.
//
// The method follows the same pattern as OpenAIGatewayService.ForwardAsAnthropic
// but in reverse direction: Responses → Anthropic upstream → Responses.
func (s *GatewayService) ForwardAsResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	parsed *ParsedRequest,
) (*ForwardResult, error) {
	startTime := time.Now()

	normalizedBody, normalized, err := normalizeOpenAIResponsesLegacyIngressWithConfig(s.cfg, body)
	if err != nil {
		return nil, err
	}
	if normalized {
		body = normalizedBody
	}

	// 1. Lower Codex client-side tools to function tools understood by Anthropic.
	adaptedBody, clientToolMapping, err := adaptResponsesClientToolsForAnthropic(body, account.Platform == PlatformKiro)
	if err != nil {
		return nil, fmt.Errorf("adapt responses client tools: %w", err)
	}

	// 2. Parse Responses request
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(adaptedBody, &responsesReq); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	originalModel := responsesReq.Model
	clientStream := responsesReq.Stream

	// 3. Convert Responses → Anthropic
	// Resolve the final upstream model before model-specific conversion.
	mappedModel := originalModel
	if account.Platform == PlatformKiro {
		if next := account.GetMappedModel(originalModel); next != "" {
			mappedModel = next
		}
	} else if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mappedModel = account.GetMappedModel(originalModel)
	}
	if mappedModel == originalModel && account.Platform == PlatformAnthropic && account.Type == AccountTypeServiceAccount {
		normalized := normalizeVertexAnthropicModelID(claude.NormalizeModelID(originalModel))
		if normalized != originalModel {
			mappedModel = normalized
		}
	} else if mappedModel == originalModel && account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey {
		normalized := claude.NormalizeModelID(originalModel)
		if normalized != originalModel {
			mappedModel = normalized
		}
	}
	if err := validateClaude55RequestForAccount(body, account, mappedModel); err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	responsesReq.Model = mappedModel
	responsesReq.KiroCompat = account.Platform == PlatformKiro
	markKiroResponsesCompat(c, responsesReq.KiroCompat)
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(&responsesReq)
	if err != nil {
		if isClaude55SignedThinkingModel(mappedModel) {
			writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return nil, fmt.Errorf("convert responses to anthropic: %w", err)
	}

	// 3. Force upstream streaming (Anthropic works best with streaming)
	anthropicReq.Stream = true
	reqStream := true

	reasoningEffort := ExtractResponsesReasoningEffortFromBody(body, mappedModel, originalModel)
	// 国产模型默认 effort 补充：需要 mappedModel 判定，推迟到 mapping 完成之后。
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, mappedModel)

	// 4b. Codex remote compaction v2：input 里带 compaction_trigger 的请求不是普通
	// 轮次，而是"把前文压缩成摘要"。Anthropic 协议族没有原生 compact 端点，转换器
	// 已把触发器降级成摘要指令（见 apicompat.CompactionSummaryPrompt），这里只需把
	// 请求参数调成适合产出摘要的形态。
	// compaction 仅对 Kiro 账号启用；其他账号保持基线行为（触发器项被转换器忽略）。
	isCompact := account.Platform == PlatformKiro && apicompat.HasCompactionTrigger(&responsesReq)
	if isCompact {
		// 压缩专用模型映射：账号未配 compact_model_mapping 时沿用普通映射结果。
		if next, matched := account.ResolveCompactMappedModel(originalModel); matched {
			if trimmed := strings.TrimSpace(next); trimmed != "" {
				mappedModel = trimmed
			}
		}
		// 摘要轮次不允许调用工具。tools 必须保留：历史里的 tool_use 块引用了工具
		// 定义，删掉会让上游校验失败；tool_choice=none 已足够抑制调用。
		anthropicReq.ToolChoice = json.RawMessage(`{"type":"none"}`)
		// 摘要不需要思考块，省 token 也避免 thinking 与 tool_choice 的组合限制。
		anthropicReq.Thinking = nil
		// Codex 的 compact 请求不带 max_output_tokens，会落到转换器的 8192 默认值；
		// 对覆盖数十万 token 前文的结构化摘要偏紧，容易被截断。
		if anthropicReq.MaxTokens < compactionMinMaxTokens {
			anthropicReq.MaxTokens = compactionMinMaxTokens
		}
	}
	anthropicReq.Model = mappedModel

	logger.L().Debug("gateway forward_as_responses: model mapping applied",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("mapped_model", mappedModel),
		zap.Bool("client_stream", clientStream),
		zap.Bool("compaction", isCompact),
	)

	// 5. Marshal Anthropic request body
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	// 6. Apply Claude Code mimicry for OAuth accounts (non-Claude-Code endpoints).
	// OpenAI Responses 协议进来的请求永远不是 Claude Code 客户端，所以对 OAuth 账号
	// 必须完整执行 /v1/messages 主路径上的伪装链路（system 重写 + normalize + metadata 注入），
	// 否则会被 Anthropic 判为第三方应用并扣 extra usage。
	// 见 applyClaudeCodeOAuthMimicryToBody 与 shouldMimicClaudeCodeForAccount 的 godoc
	// （后者说明了 Kiro 为何必须排除）。
	isClaudeCode := false
	shouldMimicClaudeCode := shouldMimicClaudeCodeForAccount(account, isClaudeCode)

	if shouldMimicClaudeCode {
		anthropicBody = s.applyClaudeCodeOAuthMimicryToBody(ctx, c, account, anthropicBody, anthropicReq.System, mappedModel)
	}

	// 7. 注入随对话前进的缓存断点并强制执行 cache_control 块数量限制。
	// Responses 客户端（Codex 等）本身不带 cache_control，不注入断点的话
	// 只有上游自己打在 tools 上的固定前缀会被缓存写一次，后续轮次永远只读不写。
	anthropicBody = applyResponsesAnthropicCacheBreakpoints(anthropicBody, mappedModel)
	anthropicBody = enforceCacheControlLimit(anthropicBody)

	var (
		token         string
		tokenType     string
		proxyURL      string
		upstreamReq   *http.Request
		forwardedBody []byte
		resp          *http.Response
	)
	kiroDirect := isKiroDirectModeAccount(account)
	// compactionRetry 只在 Kiro 压缩请求上设置：上游确认超限后截断工具输出重试一次。
	var compactionRetry func() (*http.Response, error)
	if kiroDirect {
		var group *Group
		if parsed != nil {
			group = parsed.Group
		}
		cachePlan := s.prepareKiroResponsesCacheEmulationUsage(ctx, account, group, body, mappedModel, estimateKiroInputTokens(ctx, s.kiroMeteringAlgorithm(ctx), anthropicBody))
		resp, _, err = s.openKiroAnthropicStreamResponse(ctx, account, parsed, anthropicBody, mappedModel, originalModel, c.Request.Header, group, cachePlan)
		if err == nil && isCompact {
			compactionRetry = newKiroCompactionRetry(anthropicBody, mappedModel, func(retryBody []byte) (*http.Response, error) {
				retryResp, _, retryErr := s.openKiroAnthropicStreamResponse(ctx, account, parsed, retryBody, mappedModel, originalModel, c.Request.Header, group, cachePlan)
				return retryResp, retryErr
			})
			resp, err = retryKiroCompactionOnContextLength(resp, compactionRetry)
		}
		if err != nil {
			if failoverErr := kiroFailoverFromOpenErr(c, account, err); failoverErr != nil {
				return nil, failoverErr
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				Kind:               "request_error",
				Message:            safeErr,
			})
			writeResponsesError(c, http.StatusBadGateway, "server_error", "Upstream request failed")
			return nil, fmt.Errorf("upstream request failed: %s", safeErr)
		}
	} else {
		// 8. Get access token
		token, tokenType, err = s.GetAccessToken(ctx, account)
		if err != nil {
			return nil, fmt.Errorf("get access token: %w", err)
		}

		// 9. Get proxy URL
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}

		// 10. Build and send the request, recovering a concurrent token rotation once.
		for retriedRotatedToken := false; ; {
			upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, reqStream)
			upstreamReq, forwardedBody, err = s.buildUpstreamRequest(upstreamCtx, c, account, anthropicBody, token, tokenType, mappedModel, reqStream, shouldMimicClaudeCode)
			releaseUpstreamCtx()
			if err != nil {
				return nil, fmt.Errorf("build upstream request: %w", err)
			}
			resp, err = s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
			if err != nil {
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, s.handleUpstreamTransportError(ctx, c, account, err, OpsUpstreamErrorEvent{
					UpstreamURL: safeUpstreamURL(upstreamReq.URL.String()),
				})
			}
			if resp.StatusCode == http.StatusUnauthorized && !retriedRotatedToken {
				if rotatedToken := s.rotatedClaudeAccessToken(ctx, account, token); rotatedToken != "" {
					_ = resp.Body.Close()
					token = rotatedToken
					retriedRotatedToken = true
					continue
				}
			}
			break
		}

		// Bill the final Anthropic effort after conversion and account normalization.
		// For example, OpenAI xhigh is forwarded as output_config.effort=max.
		reasoningEffort = NormalizeClaudeOutputEffort(gjson.GetBytes(forwardedBody, "output_config.effort").String())
		reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, forwardedBody, mappedModel)
	}
	defer func() { _ = resp.Body.Close() }()

	// 12. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody, _ := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()

		// thinking 整流重试：转换路径原先没有 /v1/messages 主路径上的整流链，上游因历史
		// thinking 结构（assistant 末块是 thinking、签名失效等）返回 400 时整轮直接失败。
		// 这里用同一策略清洗一次请求体重发，仍失败才走下面的错误处理 / failover。
		if !kiroDirect && resp.StatusCode == http.StatusBadRequest &&
			(s.shouldRectifyThinkingTailError(respBody, mappedModel) ||
				s.shouldRectifySignatureError(ctx, account, respBody, mappedModel)) {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
				Kind:               "signature_error",
				Message:            extractUpstreamErrorMessage(respBody),
			})
			if retryResp, retryWireBody := s.retryAnthropicBodyAfterThinkingError(
				ctx, c, account, anthropicBody, token, tokenType, mappedModel, reqStream, shouldMimicClaudeCode, proxyURL,
			); retryResp != nil {
				resp = retryResp
				if resp.StatusCode < 400 {
					forwardedBody = retryWireBody
					reasoningEffort = NormalizeClaudeOutputEffort(gjson.GetBytes(forwardedBody, "output_config.effort").String())
					reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, forwardedBody, mappedModel)
				} else {
					respBody, _ = s.readUpstreamErrorBody(resp)
					_ = resp.Body.Close()
				}
			}
		}

		if resp.StatusCode >= 400 {
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
			upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

			// Apply account policies to the final response after the one-shot rectification.
			shouldDisable := false
			if s.rateLimitService != nil {
				shouldDisable = s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, mappedModel)
			}
			if shouldDisable || s.shouldFailoverUpstreamError(resp.StatusCode) {
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					ProxyID:            opsUpstreamProxyID(account),
					ProxyName:          opsUpstreamProxyName(account),
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "failover",
					Message:            upstreamMsg,
				})
				return nil, newGatewayHTTPFailoverError(account, resp.StatusCode, respBody, shouldDisable)
			}

			// Non-failover error: return Responses-formatted error to client
			if account.Platform == PlatformKiro {
				if fixed, ok := kiroContextLengthClientMessage(resp.StatusCode, respBody); ok {
					if isCompact {
						return nil, s.failCompactionWithCode(c, clientStream, http.StatusBadRequest,
							"context_length_exceeded", kiroCompactionOverflowMessage, resp.Header.Get("x-request-id"))
					}
					writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", fixed)
					return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
				}
			}
			writeResponsesError(c, mapUpstreamStatusCode(resp.StatusCode), "server_error", upstreamMsg)
			return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
		}
	}

	// 13. Handle normal response (convert Anthropic → Responses)
	var result *ForwardResult
	var handleErr error
	if isCompact {
		// compact 请求必须缓冲：要拿到完整摘要文本才能合成 Codex 要求的单个
		// compaction item，逐事件透传做不到。
		result, handleErr = s.handleResponsesCompactionResponseWithRetry(resp, c, originalModel, mappedModel, reasoningEffort, startTime, clientStream, compactionRetry)
		if exc := kiroUpstreamExceptionFromError(handleErr); exc != nil {
			// 压缩处理只在客户端还没收到任何字节时把 Kiro 异常交回来：按 Kiro HTTP 错误处理，
			// 不换号时以压缩失败的协议格式回写。
			requestID := resp.Header.Get("x-request-id")
			writeCompactionError := func(c *gin.Context, upstreamStatus int, message string) {
				code := ""
				if upstreamStatus == http.StatusBadRequest {
					code = "invalid_request_error"
				}
				_ = s.failCompactionWithCode(c, clientStream, mapUpstreamStatusCode(upstreamStatus), code, message, requestID)
			}
			return nil, s.handleKiroUpstreamException(ctx, c, account, mappedModel, anthropicBody, exc, resp.Header, writeCompactionError)
		}
	} else if clientStream {
		writerSizeBeforeStream := c.Writer.Size()
		result, handleErr = s.handleResponsesStreamingResponse(resp, c, originalModel, mappedModel, reasoningEffort, startTime, clientToolMapping)
		var sseErr *sseStreamErrorEventError
		switch {
		case errors.As(handleErr, &sseErr):
			outputStarted := c.Writer.Size() != writerSizeBeforeStream
			if kiroResult, handled, kiroErr := s.handleKiroStreamErrorEvent(ctx, c, account, resp, anthropicBody, originalModel, mappedModel, mappedModel, startTime, sseErr.RawData, outputStarted, writeKiroResponsesError); handled {
				return kiroResult, kiroErr
			}
			return nil, s.sseStreamErrorEventFailover(ctx, c, account, resp, sseErr.RawData, mappedModel, outputStarted, sseErr.SafeToFailoverAfterWrite)
		case errors.Is(handleErr, errAnthropicNativeStreamIdle) && s.rateLimitService != nil:
			s.rateLimitService.HandleStreamTimeout(ctx, account, originalModel)
		}
	} else {
		result, handleErr = s.handleResponsesBufferedStreamingResponse(resp, c, originalModel, mappedModel, reasoningEffort, startTime, clientToolMapping)
		if exc := kiroUpstreamExceptionFromError(handleErr); exc != nil {
			// 非流式客户端还没收到任何字节：与 /v1/messages 非流式一致，按 Kiro HTTP 错误处理。
			return nil, s.handleKiroUpstreamException(ctx, c, account, mappedModel, anthropicBody, exc, resp.Header, writeKiroResponsesError)
		}
	}

	return result, handleErr
}

const kiroResponsesCompatContextKey = "kiro_responses_compat"

// markKiroResponsesCompat 在请求上下文中标记目标账号为 Kiro，供 Anthropic→Responses
// 响应转换（stream/buffered）启用 Kiro 专属扩展；其他账号不置位，保持基线行为。
func markKiroResponsesCompat(c *gin.Context, enabled bool) {
	if c != nil && enabled {
		c.Set(kiroResponsesCompatContextKey, true)
	}
}

func isKiroResponsesCompat(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(kiroResponsesCompatContextKey)
	enabled, _ := v.(bool)
	return ok && enabled
}

// kiroCompat 为 true 时启用 Kiro 移植的 namespace custom 子工具摊平；其他账号保持基线行为。
func adaptResponsesClientToolsForAnthropic(body []byte, kiroCompat bool) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var requestBody map[string]any
	if err := decoder.Decode(&requestBody); err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	additionalToolsChanged, err := liftResponsesAdditionalTools(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}

	adaptTools := apicompat.AdaptResponsesClientTools
	if kiroCompat {
		adaptTools = apicompat.AdaptResponsesClientToolsKiroCompat
	}
	mapping, changed, err := adaptTools(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	changed = changed || additionalToolsChanged
	if !changed {
		return body, mapping, nil
	}
	rebuilt, err := json.Marshal(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	return rebuilt, mapping, nil
}

func liftResponsesAdditionalTools(requestBody map[string]any) (bool, error) {
	input, ok := requestBody["input"].([]any)
	if !ok {
		return false, nil
	}

	tools, _ := requestBody["tools"].([]any)
	kept := make([]any, 0, len(input))
	changed := false
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(item["type"])) != "additional_tools" {
			kept = append(kept, raw)
			continue
		}
		additional, ok := item["tools"].([]any)
		if !ok {
			return false, fmt.Errorf("additional_tools.tools must be an array")
		}
		tools = append(tools, additional...)
		changed = true
	}
	if !changed {
		return false, nil
	}
	requestBody["tools"] = tools
	requestBody["input"] = kept
	return true, nil
}

// ExtractResponsesReasoningEffortFromBody reads Responses API reasoning.effort
// and normalizes it for usage logging.
func ExtractResponsesReasoningEffortFromBody(body []byte, modelCandidates ...string) *string {
	raw := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if raw == "" {
		return nil
	}
	model := firstNonEmpty(modelCandidates...)
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	normalized := normalizeOpenAIReasoningEffortForModel(raw, model)
	if normalized == "" {
		return nil
	}
	return &normalized
}

func mergeAnthropicUsage(dst *ClaudeUsage, src apicompat.AnthropicUsage) {
	if dst == nil {
		return
	}

	cacheReadTokens := max(src.CacheReadInputTokens, 0)
	hasCacheReadTokens := src.CacheReadInputTokensPresent || src.CacheReadInputTokens != 0
	if cacheReadTokens == 0 && (src.CachedTokensPresent || src.CachedTokens != 0) {
		cacheReadTokens = max(src.CachedTokens, 0)
		hasCacheReadTokens = true
	}
	if cacheReadTokens == 0 && src.PromptTokensDetails != nil && (src.PromptTokensDetails.CachedTokensPresent || src.PromptTokensDetails.CachedTokens != 0) {
		cacheReadTokens = max(src.PromptTokensDetails.CachedTokens, 0)
		hasCacheReadTokens = true
	}
	if cacheReadTokens == 0 && src.PromptCacheHitTokens != nil {
		cacheReadTokens = max(*src.PromptCacheHitTokens, 0)
		hasCacheReadTokens = true
	}
	mergeAnthropicCacheCreationUsage(dst, src)

	// Some Anthropic-compatible providers retain OpenAI-style prompt/cache
	// fields. Prefer those authoritative totals or hit/miss buckets over the
	// overloaded input_tokens field. This covers Kimi's changing stream
	// semantics as well as GLM/DeepSeek cache aliases.
	if src.PromptTokens <= 0 && src.PromptCacheHitTokens == nil && src.PromptCacheMissTokens == nil && !dst.promptTotalPresent {
		// Without an authoritative prompt total or miss bucket, input_tokens is
		// provider-specific: it may already be the uncached bucket, or it may be
		// a total from an earlier event. Do not infer a subtraction merely because
		// a later event contains cache buckets; that would corrupt providers whose
		// stream uses independent input and cache fields.
		if src.InputTokens > 0 {
			dst.InputTokens = src.InputTokens
		}
	}
	if hasCacheReadTokens && (cacheReadTokens > 0 || dst.CacheReadInputTokens <= 0) {
		dst.CacheReadInputTokens = cacheReadTokens
	}
	normalizeAnthropicPromptTotal(dst, src.PromptTokens, src.PromptCacheMissTokens)
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
}

func mergeKiroCreditsFromAnthropicPayload(dst *ClaudeUsage, payload string) {
	if dst == nil || payload == "" || !gjson.Valid(payload) {
		return
	}
	if credits := kiroCreditsFromUsageGJSON(gjson.Get(payload, "usage")); credits > 0 {
		dst.KiroCredits = credits
		return
	}
	if credits := kiroCreditsFromUsageGJSON(gjson.Get(payload, "message.usage")); credits > 0 {
		dst.KiroCredits = credits
	}
}

func normalizeAnthropicPromptTotal(usage *ClaudeUsage, promptTokens int, promptCacheMissTokens *int) {
	if promptCacheMissTokens != nil {
		usage.promptTotalPresent = false
		usage.InputTokens = max(*promptCacheMissTokens, 0)
		return
	}
	if promptTokens > 0 {
		usage.promptTotalTokens = promptTokens
		usage.promptTotalPresent = true
	}
	if usage.promptTotalPresent {
		usage.InputTokens = max(usage.promptTotalTokens-usage.CacheReadInputTokens-usage.CacheCreationInputTokens, 0)
	}
}

func mergeAnthropicCacheCreationUsage(dst *ClaudeUsage, src apicompat.AnthropicUsage) {
	if src.HasCacheCreationInputTokens() && (src.CacheCreationInputTokens > 0 || dst.CacheCreationInputTokens <= 0) {
		dst.CacheCreationInputTokens = max(src.CacheCreationInputTokens, 0)
		dst.cacheCreationInputTokensPresent = true
		dst.cacheCreationInputTokensFromDetails = false
	}
	if src.CacheCreation != nil {
		if src.CacheCreation.Ephemeral5mInputTokens != nil {
			dst.CacheCreation5mTokens = max(*src.CacheCreation.Ephemeral5mInputTokens, 0)
		}
		if src.CacheCreation.Ephemeral1hInputTokens != nil {
			dst.CacheCreation1hTokens = max(*src.CacheCreation.Ephemeral1hInputTokens, 0)
		}
		if (!dst.cacheCreationInputTokensPresent || dst.cacheCreationInputTokensFromDetails || dst.CacheCreationInputTokens <= 0) && (src.CacheCreation.Ephemeral5mInputTokens != nil || src.CacheCreation.Ephemeral1hInputTokens != nil) {
			dst.CacheCreationInputTokens = dst.CacheCreation5mTokens + dst.CacheCreation1hTokens
			dst.cacheCreationInputTokensFromDetails = true
		}
	}
}

func mergeRawAnthropicCacheCreationUsage(node gjson.Result, dst *ClaudeUsage) {
	aggregate := node.Get("cache_creation_input_tokens")
	usage := apicompat.AnthropicUsage{
		CacheCreationInputTokens:        int(aggregate.Int()),
		CacheCreationInputTokensPresent: aggregate.Exists() && aggregate.Type != gjson.Null,
		CacheCreation:                   &apicompat.AnthropicCacheCreation{},
	}
	if tokens := node.Get("cache_creation.ephemeral_5m_input_tokens"); tokens.Exists() && tokens.Type != gjson.Null {
		value := int(tokens.Int())
		usage.CacheCreation.Ephemeral5mInputTokens = &value
	}
	if tokens := node.Get("cache_creation.ephemeral_1h_input_tokens"); tokens.Exists() && tokens.Type != gjson.Null {
		value := int(tokens.Int())
		usage.CacheCreation.Ephemeral1hInputTokens = &value
	}
	mergeAnthropicCacheCreationUsage(dst, usage)
}

func syncAnthropicResponsesUsage(state *apicompat.AnthropicEventToResponsesState, usage ClaudeUsage) {
	state.InputTokens = usage.InputTokens
	state.OutputTokens = usage.OutputTokens
	state.CacheReadInputTokens = usage.CacheReadInputTokens
	state.CacheCreationInputTokens = usage.CacheCreationInputTokens
}

func normalizeAnthropicEventUsageForResponses(event *apicompat.AnthropicStreamEvent, usage ClaudeUsage) {
	normalize := func(dst *apicompat.AnthropicUsage) {
		if dst == nil {
			return
		}
		dst.InputTokens = usage.InputTokens
		dst.OutputTokens = usage.OutputTokens
		dst.CacheReadInputTokens = usage.CacheReadInputTokens
		dst.CacheCreationInputTokens = usage.CacheCreationInputTokens
		dst.CacheCreationInputTokensPresent = true
		dst.CacheCreation = nil
	}
	normalize(event.Usage)
	if event.Message != nil {
		normalize(&event.Message.Usage)
	}
}

// parseAnthropicSSEField parses an SSE field line in the form "field:value" or "field: value".
// According to the SSE spec (https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation),
// the space after the colon is optional. This function handles both formats.
func parseAnthropicSSEField(line, field string) (string, bool) {
	prefix := field + ":"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
}

// handleResponsesBufferedStreamingResponse reads all Anthropic SSE events from
// the upstream streaming response, assembles them into a complete Anthropic
// response, converts to Responses API JSON format, and writes it to the client.
func (s *GatewayService) handleResponsesBufferedStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
	reasoningEffort *string,
	startTime time.Time,
	clientToolMapping apicompat.ResponsesClientToolMapping,
) (*ForwardResult, error) {
	observer := beginUpstreamResponseModelObservation(c)
	requestID := resp.Header.Get("x-request-id")
	finalResp, usage, streamStatus := s.collectAnthropicResponseFromSSE(resp.Body, requestID, nil, observer)

	if failure := streamStatus.failure(); failure != nil {
		_ = resp.Body.Close()
		if kiroUpstreamExceptionFromError(failure.err) != nil {
			// Kiro 流内异常：不写响应、不计费，由调用方按 Kiro HTTP 错误处理（换号或回 4xx）。
			return nil, failure.err
		}
		writeResponsesError(c, http.StatusBadGateway, "server_error", failure.message)
		return &ForwardResult{
			UpstreamResponseModel:         observer.Model(),
			UpstreamResponseModelConflict: observer.Conflict(),
			UpstreamResponseServiceTier:   observer.ServiceTier(),
			RequestID:                     requestID, UpstreamHeaders: resp.Header, Usage: usage,
			Model: originalModel, UpstreamModel: mappedModel, ReasoningEffort: reasoningEffort,
			Duration: time.Since(startTime),
		}, failure.err
	}

	if finalResp == nil {
		writeResponsesError(c, http.StatusBadGateway, "server_error", "Upstream stream ended without a response")
		return nil, fmt.Errorf("upstream stream ended without response")
	}
	return s.writeResponsesBufferedResult(
		c, resp, finalResp, usage, requestID,
		originalModel, mappedModel, reasoningEffort, startTime, clientToolMapping,
	)
}

// collectAnthropicResponseFromSSE 读完上游 Anthropic SSE 流并聚合成一个完整的
// Anthropic 响应。onEvent 在每个事件解析成功后被调用（可为 nil），供调用方挂载
// 心跳一类的副作用——compact 请求会借此在漫长的摘要生成期间向下游发送保活字节。
func (s *GatewayService) collectAnthropicResponseFromSSE(
	body io.ReadCloser,
	requestID string,
	onEvent func(),
	observer *upstreamResponseModelObserver,
) (*apicompat.AnthropicResponse, ClaudeUsage, *anthropicCompatStream) {
	scanner := bufio.NewScanner(body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(newBoundedSSESplit(s.cfg, maxLineSize, nil))

	// Accumulate the final Anthropic response from streaming events
	var finalResp *apicompat.AnthropicResponse
	var usage ClaudeUsage
	streamStatus := newAnthropicCompatStreamStatus(body, nil)

	for scanner.Scan() {
		line := scanner.Text()
		eventType, ok := parseAnthropicSSEField(line, "event")
		if !ok {
			continue
		}

		// Read the data line
		if !scanner.Scan() {
			break
		}
		dataLine := scanner.Text()
		payload, ok := parseAnthropicSSEField(dataLine, "data")
		if !ok {
			continue
		}

		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("forward_as_responses buffered: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
				zap.String("event_type", eventType),
			)
			continue
		}
		if onEvent != nil {
			onEvent()
		}

		if observer != nil {
			observer.ObserveAnthropic([]byte(payload))
		}
		streamStatus.observe(&event, payload)
		if streamStatus.upstreamErrType != "" {
			break
		}

		// message_start carries the initial response structure
		if event.Type == "message_start" && event.Message != nil {
			finalResp = event.Message
			mergeAnthropicUsage(&usage, event.Message.Usage)
		}

		// message_delta carries final usage and stop_reason
		if event.Type == "message_delta" {
			if event.Usage != nil {
				mergeAnthropicUsage(&usage, *event.Usage)
			}
			mergeKiroCreditsFromAnthropicPayload(&usage, payload)
			if event.Delta != nil && event.Delta.StopReason != "" && finalResp != nil {
				finalResp.StopReason = apicompat.AnthropicStopReasonPtr(event.Delta.StopReason)
			}
		}

		// Accumulate content blocks
		if event.Type == "content_block_start" && event.ContentBlock != nil && finalResp != nil {
			finalResp.Content = append(finalResp.Content, *event.ContentBlock)
		}
		if event.Type == "content_block_delta" && event.Delta != nil && finalResp != nil && event.Index != nil {
			idx := *event.Index
			if idx >= 0 && idx < len(finalResp.Content) {
				switch event.Delta.Type {
				case "text_delta":
					finalResp.Content[idx].Text += event.Delta.Text
				case "thinking_delta":
					finalResp.Content[idx].Thinking += event.Delta.Thinking
				case "signature_delta":
					finalResp.Content[idx].Signature += event.Delta.Signature
				case "input_json_delta":
					finalResp.Content[idx].Input = appendRawJSON(finalResp.Content[idx].Input, event.Delta.PartialJSON)
				}
			}
		}
		if anthropicStreamEventIsTerminal("", payload) {
			break
		}
	}

	streamStatus.readErr = scanner.Err()
	if err := scanner.Err(); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("forward_as_responses buffered: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}
	return finalResp, usage, streamStatus
}

// writeResponsesBufferedResult 把聚合好的 Anthropic 响应转成 Responses JSON 写回。
func (s *GatewayService) writeResponsesBufferedResult(
	c *gin.Context,
	resp *http.Response,
	finalResp *apicompat.AnthropicResponse,
	usage ClaudeUsage,
	requestID string,
	originalModel string,
	mappedModel string,
	reasoningEffort *string,
	startTime time.Time,
	clientToolMapping apicompat.ResponsesClientToolMapping,
) (*ForwardResult, error) {
	observer := upstreamResponseModelObserverFromContext(c)
	// Update usage from accumulated delta. 无条件赋值：纯缓存命中的响应
	// （input/output 均为 0 但 cache read/write 非 0）不能被整体丢弃。
	normalizeAnthropicEventUsageForResponses(&apicompat.AnthropicStreamEvent{Message: finalResp}, usage)

	// Convert to Responses format
	if isClaude55SignedThinkingModel(mappedModel) {
		finalResp.Model = mappedModel
	}
	responsesResp := apicompat.AnthropicToResponsesResponse(finalResp)
	if isKiroResponsesCompat(c) {
		responsesResp = apicompat.AnthropicToResponsesResponseWithCustomTools(finalResp, nil)
	}
	responsesResp.Model = originalModel // Use original model name

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	// 非流式响应必须是 application/json。上游被强制流式后会返回
	// Content-Type: text/event-stream，经 WriteFilteredHeaders 透传后会污染
	// 响应头；而 c.Data/c.JSON 走 Gin 的 writeContentType（仅当头不存在时才设置），
	// 无法覆盖已存在的 SSE 头。这里显式 Set 强制改回 JSON，避免下游中间层
	// （如 new-api）按 Content-Type 误判为流式。
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	if respBytes, err := json.Marshal(responsesResp); err == nil {
		respBytes = reverseToolNamesIfPresent(c, respBytes)
		respBytes, _, err = apicompat.RestoreResponsesClientToolPayload(respBytes, clientToolMapping)
		if err != nil {
			return nil, fmt.Errorf("restore responses client tools: %w", err)
		}
		respBytes = withResponsesUpstreamCredits(c, respBytes, "usage", usage.KiroCredits)
		c.Data(http.StatusOK, "application/json; charset=utf-8", respBytes)
	} else {
		c.JSON(http.StatusOK, responsesResp)
	}

	return &ForwardResult{
		UpstreamResponseModel:         observer.Model(),
		UpstreamResponseModelConflict: observer.Conflict(),
		UpstreamResponseServiceTier:   observer.ServiceTier(),
		RequestID:                     requestID,
		UpstreamHeaders:               resp.Header,
		Usage:                         usage,
		Model:                         originalModel,
		UpstreamModel:                 mappedModel,
		ReasoningEffort:               reasoningEffort,
		Stream:                        false,
		Duration:                      time.Since(startTime),
	}, nil
}

// handleResponsesStreamingResponse reads Anthropic SSE events from upstream,
// converts each to Responses SSE events, and writes them to the client.
func (s *GatewayService) handleResponsesStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
	reasoningEffort *string,
	startTime time.Time,
	clientToolMapping apicompat.ResponsesClientToolMapping,
) (*ForwardResult, error) {
	observer := beginUpstreamResponseModelObservation(c)
	requestID := resp.Header.Get("x-request-id")

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	state := apicompat.NewAnthropicEventToResponsesState()
	state.KiroCompat = isKiroResponsesCompat(c)
	state.Model = originalModel
	state.PreserveThinkingSignatures = isClaude55SignedThinkingModel(mappedModel)
	clientToolRestorer := apicompat.NewResponsesClientToolStreamRestorer(clientToolMapping)
	var usage ClaudeUsage
	var firstTokenMs *int
	firstChunk := true
	writerSizeBeforeStream := c.Writer.Size()

	stream := newAnthropicCompatStream(s.cfg, c, resp, "forward_as_responses", requestID)
	defer stream.stop()

	resultWithUsage := func() *ForwardResult {
		return &ForwardResult{
			UpstreamResponseModel:         observer.Model(),
			UpstreamResponseModelConflict: observer.Conflict(),
			UpstreamResponseServiceTier:   observer.ServiceTier(),
			RequestID:                     requestID,
			UpstreamHeaders:               resp.Header,
			Usage:                         usage,
			Model:                         originalModel,
			UpstreamModel:                 mappedModel,
			ReasoningEffort:               reasoningEffort,
			Stream:                        true,
			Duration:                      time.Since(startTime),
			FirstTokenMs:                  firstTokenMs,
			ClientDisconnect:              stream.clientDisconnected,
		}
	}

	// 所有输出（包括补发的最终事件）共用此写失败保护。
	writeEvents := func(events []apicompat.ResponsesStreamEvent) {
		if stream.disconnected() {
			return
		}
		for _, evt := range events {
			payload, err := json.Marshal(evt)
			if err != nil {
				logger.L().Warn("forward_as_responses stream: failed to marshal event",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			payload = reverseToolNamesIfPresent(c, payload)
			payloads, _, err := clientToolRestorer.RestoreEvent(payload)
			if err != nil {
				logger.L().Warn("forward_as_responses stream: failed to restore client tools",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			for _, restored := range payloads {
				if stream.disconnected() {
					return
				}
				eventType := gjson.GetBytes(restored, "type").String()
				if isResponsesTerminalEvent(eventType) {
					restored = withResponsesUpstreamCredits(c, restored, "response.usage", usage.KiroCredits)
				}
				if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, restored); err != nil {
					stream.markClientDisconnected()
					logger.L().Info("forward_as_responses stream: client disconnected",
						zap.String("request_id", requestID),
					)
					return
				}
				stream.markDownstreamWrote()
			}
		}
		if len(events) > 0 && !stream.disconnected() {
			c.Writer.Flush()
		}
	}

	// 下游写失败后和 terminal 尾窗口内仍继续累计 usage。
	processEvent := func(event *apicompat.AnthropicStreamEvent) {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
		if event.Type == "message_start" && event.Message != nil {
			mergeAnthropicUsage(&usage, event.Message.Usage)
		}
		if event.Usage != nil {
			mergeAnthropicUsage(&usage, *event.Usage)
		}

		// Keep the terminal Responses usage aligned with the normalized billing
		// buckets. Normalize the converter input too, so message handlers cannot
		// restore the provider's overlapping raw input total.
		syncAnthropicResponsesUsage(state, usage)
		normalizeAnthropicEventUsageForResponses(event, usage)
		if stream.disconnected() {
			return
		}
		// 等固定 usage 尾窗口排水结束后，再转换最终事件。
		if stream.drain.terminalSeen() && event.Type != "message_delta" {
			return
		}
		writeEvents(apicompat.AnthropicEventToResponsesEvents(event, state))
	}

	finalizeStream := func() (*ForwardResult, error) {
		if failure := stream.failure(); failure != nil {
			return finishAnthropicCompatStreamFailure(c, writerSizeBeforeStream, failure, resultWithUsage(), func() {
				writeAnthropicCompatResponsesStreamFailure(c, state, clientToolRestorer, failure)
			})
		}
		if stream.disconnected() {
			return resultWithUsage(), nil
		}
		writeEvents(apicompat.FinalizeAnthropicResponsesStream(state))
		return resultWithUsage(), nil
	}

	// 读取不受下游请求取消影响：上游请求已有 WithoutCancel，脱离取消后的读取
	// 由闲置超时和 terminal 尾窗口限制。
	for {
		line, ok := stream.nextLine()
		if !ok {
			break
		}
		eventType, ok := parseAnthropicSSEField(line, "event")
		if !ok {
			continue
		}

		dataLine, ok := stream.nextLine()
		if !ok {
			break
		}
		payload, ok := parseAnthropicSSEField(dataLine, "data")
		if !ok {
			continue
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			continue
		}

		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("forward_as_responses stream: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
				zap.String("event_type", eventType),
			)
			continue
		}
		if event.Type == "" {
			event.Type = eventType
		}

		mergeKiroCreditsFromAnthropicPayload(&usage, payload)
		observer.ObserveAnthropic([]byte(payload))
		stream.observe(&event, payload)
		if stream.upstreamErrType != "" {
			break
		}
		processEvent(&event)
	}

	return finalizeStream()
}

// appendRawJSON appends a JSON fragment string to existing raw JSON.
func appendRawJSON(existing json.RawMessage, fragment string) json.RawMessage {
	// Anthropic initializes tool_use.input to {} in content_block_start, then
	// streams the actual input through input_json_delta events. Treat that empty
	// object as a placeholder instead of prefixing it to the streamed JSON.
	var existingObject map[string]json.RawMessage
	isEmptyObject := json.Unmarshal(existing, &existingObject) == nil && existingObject != nil && len(existingObject) == 0
	if len(existing) == 0 || isEmptyObject {
		return json.RawMessage(fragment)
	}
	return json.RawMessage(string(existing) + fragment)
}

// writeResponsesError writes an error response in OpenAI Responses API format.
func writeResponsesError(c *gin.Context, statusCode int, code, message string) {
	MarkResponseCommitted(c)
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	})
}

// mapUpstreamStatusCode maps upstream HTTP status codes to appropriate client-facing codes.
func mapUpstreamStatusCode(code int) int {
	if code >= 500 {
		return http.StatusBadGateway
	}
	return code
}
