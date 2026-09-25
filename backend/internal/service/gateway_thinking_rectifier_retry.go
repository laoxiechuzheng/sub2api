package service

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// retryAnthropicBodyAfterThinkingError 用 thinking 整流后的请求体在同一账号上重发一次，
// 供转换路径（Responses→Anthropic / Chat Completions→Anthropic）在收到 thinking
// 结构类或签名类 400 时兜底。
//
// 背景：/v1/messages 主路径早就有「检测 400 → FilterThinkingBlocksForRetry → 重发」的
// 整流链（见 gateway_forward.go），但两条转换路径把上游 400 直接交给客户端。转换后的
// 历史一旦出现 Anthropic 拒收的 thinking 结构（assistant 末块是 thinking / 签名失效 /
// 空 content），整轮请求就会失败，用户侧只看到 max_output_tokens 之后的重连报错。
//
// 返回值：
//   - resp：整流后重发的响应，body 已重置为可读；StatusCode < 400 表示整流成功
//   - wireBody：本次实际发给上游的 body（调用方据此重算 reasoning effort / 计费）
//   - 两者均为 nil 表示没有可用的整流（请求体无变化，或构建/发送请求失败）
func (s *GatewayService) retryAnthropicBodyAfterThinkingError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	tokenType string,
	modelID string,
	reqStream bool,
	mimicClaudeCode bool,
	proxyURL string,
) (*http.Response, []byte) {
	filteredBody := FilterThinkingBlocksForRetry(body, modelID)
	if bytes.Equal(filteredBody, body) {
		// 请求体没有任何可整流的内容：不发重复请求，交给调用方原有错误处理。
		return nil, nil
	}

	logger.LegacyPrintf("service.gateway", "[warn] Account %d: upstream rejected thinking blocks, retrying with rectified body", account.ID)

	retryCtx, releaseRetryCtx := detachStreamUpstreamContext(ctx, reqStream)
	retryReq, retryWireBody, err := s.buildUpstreamRequest(retryCtx, c, account, filteredBody, token, tokenType, modelID, reqStream, mimicClaudeCode)
	releaseRetryCtx()
	if err != nil {
		logger.LegacyPrintf("service.gateway", "Account %d: thinking rectifier retry build request failed: %v", account.ID, err)
		return nil, nil
	}

	retryResp, err := s.httpUpstream.DoWithTLS(retryReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		if retryResp != nil && retryResp.Body != nil {
			_ = retryResp.Body.Close()
		}
		logger.LegacyPrintf("service.gateway", "Account %d: thinking rectifier retry failed: %v", account.ID, err)
		return nil, nil
	}
	if retryResp.StatusCode < http.StatusBadRequest {
		logger.LegacyPrintf("service.gateway", "Account %d: thinking rectifier retry succeeded (blocks rectified)", account.ID)
		return retryResp, retryWireBody
	}

	// 整流后仍失败：把 body 读回来重建，交给调用方原有的错误处理 / failover 逻辑，
	// 避免调用方再读一次已经消费掉的网络流。
	retryRespBody, _ := s.readUpstreamErrorBody(retryResp)
	_ = retryResp.Body.Close()
	retryResp.Body = io.NopCloser(bytes.NewReader(retryRespBody))
	logger.LegacyPrintf("service.gateway", "Account %d: thinking rectifier retry still failed (%d)", account.ID, retryResp.StatusCode)
	return retryResp, retryWireBody
}
