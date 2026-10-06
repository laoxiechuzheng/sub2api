package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 本文件处理 platform=deepseek 原生 Responses（apikey + responses/adaptive）
// 的 thinking mode reasoning 往返：
//
//   - 出站：input 里 reasoning item 缺合法 content[].reasoning_text 时，按
//     APIKey + item id 读 scoped 缓存（content 槽位优先，summary 槽位兜底），
//     仍无则把已有 summary_text 明文原样重编码为 content；无明文不编造。
//     同一 user 轮内缺 reasoning item 的工具子轮沿用最近已知明文补齐；
//     encrypted-only / cache miss / 空 reasoning_text 清空可继承状态。
//   - 入站：流内只收集 reasoning 明文，响应结束时一次 2s detached context
//     写入缓存（content/summary 分槽位，互不覆盖）。
//
// 缓存键 deepseek_responses:<apiKeyID>:<scope>:<itemID>（外层由 repository 加
// reasoning_content: 前缀），按 APIKey 隔离，TTL 7 天，故障 fail-open。

const (
	deepSeekNativeReasoningCachePrefix  = "deepseek_responses:"
	deepSeekResponsesReasoningCacheTTL  = 7 * 24 * time.Hour
	deepSeekResponsesReasoningCacheWait = 2 * time.Second

	deepSeekNativeReasoningScopeContent = "content"
	deepSeekNativeReasoningScopeSummary = "summary"
)

// deepSeekNativeResponsesReasoningScope 报告账号是否走 DeepSeek 原生 Responses
// 的 reasoning 往返处理：只覆盖 platform=deepseek + apikey + 原生 Responses
// 协议（含 adaptive 且支持原生），其余（Chat / mapped OpenAI / 其他平台）不处理。
func deepSeekNativeResponsesReasoningScope(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformDeepseek &&
		account.Type == AccountTypeAPIKey &&
		account.UsesNativeCNResponses()
}

func deepSeekNativeReasoningCacheKey(apiKeyID int64, scope, itemID string) string {
	return fmt.Sprintf("%s%d:%s:%s", deepSeekNativeReasoningCachePrefix, apiKeyID, scope, itemID)
}

// openAIResponsesReasoningNoneRequested 报告请求是否显式要求 effort=none。
// 必须在 Forward 的 none 过滤器之前读取原始报文，过滤后该信号会丢失。
func openAIResponsesReasoningNoneRequested(body []byte) bool {
	if effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()); effort != "" {
		return strings.EqualFold(effort, "none")
	}
	if effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String()); effort != "" {
		return strings.EqualFold(effort, "none")
	}
	return false
}

// restoreDeepSeekNativeResponsesReasoningText 在 Forward 起始（none 过滤之前）
// 调用：effort=none / compact 不改；其余补齐 reasoning content，返回是否修改。
func (s *OpenAIGatewayService) restoreDeepSeekNativeResponsesReasoningText(c *gin.Context, account *Account, body []byte) ([]byte, bool) {
	if s == nil || !deepSeekNativeResponsesReasoningScope(account) {
		return body, false
	}
	if isOpenAIResponsesCompactPath(c) {
		return body, false
	}
	if openAIResponsesReasoningNoneRequested(body) {
		return body, false
	}
	// 快路径：input 里没有 reasoning item 时无需任何补齐，跳过全量 decode。
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false
	}
	hasReasoningItem := false
	for _, item := range input.Array() {
		if item.Get("type").String() == "reasoning" {
			hasReasoningItem = true
			break
		}
	}
	if !hasReasoningItem {
		return body, false
	}
	toolsDeclared := len(gjson.GetBytes(body, "tools").Array()) > 0

	ctx, cancel := context.WithTimeout(context.Background(), deepSeekResponsesReasoningCacheWait)
	defer cancel()
	return s.rewriteDeepSeekResponsesReasoningInput(ctx, getAPIKeyIDFromContext(c), body, toolsDeclared)
}

// rewriteDeepSeekResponsesReasoningInput 只改 input 里的 reasoning 部分，其它
// 字段原样透传（UseNumber 解码避免改写整数/opaque 字段）；缓存 IO 复用同一个
// 2s 总超时 ctx，deadline 后停止读缓存并以已有明文继续。
func (s *OpenAIGatewayService) rewriteDeepSeekResponsesReasoningInput(ctx context.Context, apiKeyID int64, body []byte, allowChain bool) ([]byte, bool) {
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false
	}
	rawInput, ok := requestBody["input"].([]any)
	if !ok || len(rawInput) == 0 {
		return body, false
	}

	changed := false
	out := make([]any, 0, len(rawInput))
	cacheLookup := map[string]string{} // itemID 去重 + 记忆未命中（""）
	lastPlaintext := ""
	subTurnHasReasoning := false

	cacheAvailable := func() bool { return apiKeyID > 0 && ctx.Err() == nil }

	for _, raw := range rawInput {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		switch itemType, _ := item["type"].(string); itemType {
		case "reasoning":
			id, _ := item["id"].(string)
			if text, hasPart := reasoningTextFromContentField(item["content"]); hasPart {
				// 已有合法 reasoning_text（含空的合法 part）：整体原样保留。
				lastPlaintext = text
				subTurnHasReasoning = true
				out = append(out, raw)
				continue
			}
			replaced := false
			if id != "" && cacheAvailable() {
				cached, remembered := cacheLookup[id]
				if !remembered {
					cached = s.deepSeekNativeCachedPlaintext(ctx, apiKeyID, id)
					cacheLookup[id] = cached
				}
				if cached != "" {
					item = withReasoningTextContent(item, cached)
					changed = true
					replaced = true
					lastPlaintext = cached
				}
			}
			if !replaced {
				if summaryText := reasoningSummaryPlaintext(item["summary"]); summaryText != "" {
					// summary 只做兼容重编码（不保证等于原生全文），并写入
					// summary 槽位供后续轮次兜底；绝不覆盖 content 槽位。
					item = withReasoningTextContent(item, summaryText)
					changed = true
					if id != "" && cacheAvailable() {
						s.setDeepSeekNativeReasoningCache(ctx, apiKeyID, deepSeekNativeReasoningScopeSummary, id, summaryText)
					}
					lastPlaintext = summaryText
				} else {
					// encrypted-only / 无明文：原样保留，且不把更早的推理
					// 跨过该 item 继承给后续子轮。
					lastPlaintext = ""
				}
			}
			subTurnHasReasoning = true
			out = append(out, item)
		case "function_call", "custom_tool_call":
			if allowChain && !subTurnHasReasoning && lastPlaintext != "" {
				// 同 user 链式子轮：沿用最近一段真实明文，不编造、不插占位。
				out = append(out, map[string]any{
					"type":    "reasoning",
					"summary": []any{},
					"content": []any{map[string]any{"type": "reasoning_text", "text": lastPlaintext}},
				})
				changed = true
				subTurnHasReasoning = true
			}
			out = append(out, raw)
		case "function_call_output", "custom_tool_call_output":
			subTurnHasReasoning = false
			out = append(out, raw)
		default:
			// input message 可能省略 type（仅 role=user/system/developer）；
			// 新用户轮不借用上一轮的推理。
			if role, _ := item["role"].(string); role == "user" || role == "system" || role == "developer" {
				lastPlaintext = ""
				subTurnHasReasoning = false
			}
			out = append(out, raw)
		}
	}

	if !changed {
		return body, false
	}
	requestBody["input"] = out
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, false
	}
	return rebuilt, true
}

// reasoningTextFromContentField 返回 content 数组里 reasoning_text 的精确文本
// （不 trim、不额外插入分隔符），以及是否存在合法 part（text 为空也算存在）。
func reasoningTextFromContentField(content any) (string, bool) {
	parts, ok := content.([]any)
	if !ok {
		return "", false
	}
	var texts []string
	found := false
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if partType, _ := part["type"].(string); partType != "reasoning_text" {
			continue
		}
		found = true
		if text, _ := part["text"].(string); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, ""), found
}

// reasoningSummaryPlaintext 返回 summary 数组里 summary_text 的非空精确文本
// （不 trim 内容本身，多个 part 直接拼接）。
func reasoningSummaryPlaintext(summary any) string {
	parts, ok := summary.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if partType, _ := part["type"].(string); partType != "summary_text" {
			continue
		}
		if text, _ := part["text"].(string); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "")
}

// withReasoningTextContent 把 reasoning item 的 content 替换为单段
// reasoning_text（仅在该 item 没有合法 reasoning_text part 时调用）。
func withReasoningTextContent(item map[string]any, text string) map[string]any {
	item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
	return item
}

// deepSeekNativeCachedPlaintext 按 content 槽位优先、summary 槽位兜底读取缓存。
func (s *OpenAIGatewayService) deepSeekNativeCachedPlaintext(ctx context.Context, apiKeyID int64, itemID string) string {
	if s == nil || s.cache == nil || apiKeyID <= 0 || strings.TrimSpace(itemID) == "" {
		return ""
	}
	if value, err := s.cache.GetReasoningContent(ctx, deepSeekNativeReasoningCacheKey(apiKeyID, deepSeekNativeReasoningScopeContent, itemID)); err == nil && value != "" {
		return value
	}
	if ctx.Err() != nil {
		return ""
	}
	value, err := s.cache.GetReasoningContent(ctx, deepSeekNativeReasoningCacheKey(apiKeyID, deepSeekNativeReasoningScopeSummary, itemID))
	if err != nil {
		return ""
	}
	return value
}

// setDeepSeekNativeReasoningCache 写入 scoped 缓存，fail-open。
func (s *OpenAIGatewayService) setDeepSeekNativeReasoningCache(ctx context.Context, apiKeyID int64, scope, itemID, content string) {
	if s == nil || s.cache == nil || apiKeyID <= 0 || strings.TrimSpace(itemID) == "" || content == "" {
		return
	}
	if ctx.Err() != nil {
		return
	}
	_ = s.cache.SetReasoningContent(ctx, deepSeekNativeReasoningCacheKey(apiKeyID, scope, itemID), content, deepSeekResponsesReasoningCacheTTL)
}

// deepSeekNativeReasoningCacheCollector 在单个响应处理过程中先把已完成的
// reasoning 精确明文收集到轻量 map（流内不做任何缓存 IO），处理结束（defer）
// 时用一次新的 2s detached context 统一持久化：总 cache IO 预算 2s 且不跨上游
// 生成时间，长流不会因为生成超过 2s 而整段跳过缓存。
type deepSeekNativeReasoningCacheCollector struct {
	apiKeyID int64
	entries  map[string]*deepSeekNativeReasoningCacheEntry
}

type deepSeekNativeReasoningCacheEntry struct {
	fullContent string         // item.done / terminal 的完整 content 明文（最高优先）
	fullSummary string         // item.done / terminal 的 summary 明文（兜底）
	textParts   map[int]string // reasoning_text.done 按 content_index 收集的全文片段
	itemDone    bool           // 收到过该 itemID 的完整 item（item.done / terminal.output）
}

func newDeepSeekNativeReasoningCacheCollector(apiKeyID int64) *deepSeekNativeReasoningCacheCollector {
	if apiKeyID <= 0 {
		return nil
	}
	return &deepSeekNativeReasoningCacheCollector{
		apiKeyID: apiKeyID,
		entries:  make(map[string]*deepSeekNativeReasoningCacheEntry),
	}
}

func (c *deepSeekNativeReasoningCacheCollector) entry(itemID string) *deepSeekNativeReasoningCacheEntry {
	entry, ok := c.entries[itemID]
	if !ok {
		entry = &deepSeekNativeReasoningCacheEntry{}
		c.entries[itemID] = entry
	}
	return entry
}

// collectFullContent 记录 item.done / terminal 给出的完整 reasoning_text 明文
// （不 trim、不额外插入分隔符）；重复来源保留最早的非空值。
func (c *deepSeekNativeReasoningCacheCollector) collectFullContent(itemID, text string) {
	if c == nil || text == "" {
		return
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return
	}
	if entry := c.entry(itemID); entry.fullContent == "" {
		entry.fullContent = text
	}
}

// collectReasoningTextPart 记录 reasoning_text.done 的单个 content_index 全文。
// 它不是完整 content，仅在缺少 item.done/terminal 完整 content 时按序拼接兜底。
func (c *deepSeekNativeReasoningCacheCollector) collectReasoningTextPart(itemID string, contentIndex int, text string) {
	if c == nil || text == "" {
		return
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return
	}
	entry := c.entry(itemID)
	if entry.textParts == nil {
		entry.textParts = make(map[int]string)
	}
	if _, ok := entry.textParts[contentIndex]; !ok {
		entry.textParts[contentIndex] = text
	}
}

// collectSummaryText 记录 item.done / terminal 给出的 summary 明文，仅在没有
// 任何 content 明文时用于兜底；不会覆盖完整 content。
func (c *deepSeekNativeReasoningCacheCollector) collectSummaryText(itemID, text string) {
	if c == nil || text == "" {
		return
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return
	}
	if entry := c.entry(itemID); entry.fullSummary == "" {
		entry.fullSummary = text
	}
}

// collectOutputItem 收集单个完整 reasoning item：content 精确明文优先；没有
// content 时才用 summary 明文兜底（摘要语义，落 summary 槽位）。无论该 item
// 是否带明文，都标记该 itemID 已收到完整 item。
func (c *deepSeekNativeReasoningCacheCollector) collectOutputItem(item gjson.Result) {
	if c == nil {
		return
	}
	itemID := item.Get("id").String()
	if strings.TrimSpace(itemID) == "" {
		return
	}
	c.entry(itemID).itemDone = true
	if text := deepSeekReasoningTextResult(item.Get("content")); text != "" {
		c.collectFullContent(itemID, text)
		return
	}
	if text := deepSeekSummaryTextResult(item.Get("summary")); text != "" {
		c.collectSummaryText(itemID, text)
	}
}

// collectStreamEvent 处理单条 Responses SSE 事件（只收集，不写缓存）：
// output_item.done 里的 reasoning item、reasoning_text.done 的精确 text、
// terminal 事件携带的 response.output。
func (c *deepSeekNativeReasoningCacheCollector) collectStreamEvent(dataBytes []byte, eventType string) {
	if c == nil {
		return
	}
	switch eventType {
	case "response.output_item.done":
		item := gjson.GetBytes(dataBytes, "item")
		if item.Get("type").String() == "reasoning" {
			c.collectOutputItem(item)
		}
	case "response.reasoning_text.done":
		if text := gjson.GetBytes(dataBytes, "text").String(); text != "" {
			c.collectReasoningTextPart(
				gjson.GetBytes(dataBytes, "item_id").String(),
				int(gjson.GetBytes(dataBytes, "content_index").Int()),
				text,
			)
		}
	case "response.completed", "response.done":
		c.collectJSONPayload(dataBytes)
	}
}

// collectJSONPayload 从完整 Responses JSON 载荷（非流式响应、SSE→JSON 重建结果、
// terminal 事件）的 output 里收集 reasoning 明文。
func (c *deepSeekNativeReasoningCacheCollector) collectJSONPayload(payload []byte) {
	if c == nil {
		return
	}
	output := gjson.GetBytes(payload, "output")
	if !output.IsArray() {
		output = gjson.GetBytes(payload, "response.output")
	}
	if !output.IsArray() {
		return
	}
	for _, item := range output.Array() {
		if item.Get("type").String() == "reasoning" {
			c.collectOutputItem(item)
		}
	}
}

// flushDeepSeekNativeResponsesReasoningCache 在响应处理结束时统一持久化：一次
// 新的 detached 2s context，完整 content 优先，其后按 content_index 拼接的
// reasoning_text.done 片段，最后才写 summary 槽位；无明文不写，失败 fail-open。
func (s *OpenAIGatewayService) flushDeepSeekNativeResponsesReasoningCache(c *deepSeekNativeReasoningCacheCollector) {
	if s == nil || s.cache == nil || c == nil || c.apiKeyID <= 0 || len(c.entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), deepSeekResponsesReasoningCacheWait)
	defer cancel()
	for itemID, entry := range c.entries {
		if entry.fullContent != "" {
			s.setDeepSeekNativeReasoningCache(ctx, c.apiKeyID, deepSeekNativeReasoningScopeContent, itemID, entry.fullContent)
			continue
		}
		// 孤立 reasoning_text.done 片段不单独升级为完整 content：只有该 itemID
		// 收到过完整 item（item.done / terminal.output）且 content_index 从 0
		// 连续时才合并兜底；已有完整明文或读缓存失败时不覆盖。
		if entry.itemDone && deepSeekReasoningTextPartsComplete(entry.textParts) {
			if text := joinDeepSeekReasoningTextParts(entry.textParts); text != "" {
				s.trySetDeepSeekNativeReasoningContentFromParts(ctx, c.apiKeyID, itemID, text)
				continue
			}
		}
		if entry.fullSummary != "" {
			s.setDeepSeekNativeReasoningCache(ctx, c.apiKeyID, deepSeekNativeReasoningScopeSummary, itemID, entry.fullSummary)
		}
	}
}

// deepSeekReasoningTextPartsComplete 报告 reasoning_text.done 片段是否覆盖
// content_index 0..n-1 的连续全集；任何缺口都不算完整，不合并升级为完整明文。
func deepSeekReasoningTextPartsComplete(parts map[int]string) bool {
	if len(parts) == 0 {
		return false
	}
	for index := 0; index < len(parts); index++ {
		if _, ok := parts[index]; !ok {
			return false
		}
	}
	return true
}

// trySetDeepSeekNativeReasoningContentFromParts 仅在 scoped content 槽为空（不存在
// 或为空串）时才把拼合的 text.done 片段写入 content 槽；已有完整明文或读缓存
// 失败（含超时）时不覆盖、不提升。
func (s *OpenAIGatewayService) trySetDeepSeekNativeReasoningContentFromParts(ctx context.Context, apiKeyID int64, itemID, text string) {
	if s == nil || s.cache == nil || apiKeyID <= 0 || text == "" || ctx.Err() != nil {
		return
	}
	existing, err := s.cache.GetReasoningContent(ctx, deepSeekNativeReasoningCacheKey(apiKeyID, deepSeekNativeReasoningScopeContent, itemID))
	if err != nil && err != ErrReasoningContentNotFound {
		return
	}
	if existing != "" {
		return
	}
	s.setDeepSeekNativeReasoningCache(ctx, apiKeyID, deepSeekNativeReasoningScopeContent, itemID, text)
}

// joinDeepSeekReasoningTextParts 按 content_index 升序拼接 reasoning_text.done
// 片段（不额外插入分隔符）。
func joinDeepSeekReasoningTextParts(parts map[int]string) string {
	if len(parts) == 0 {
		return ""
	}
	indexes := make([]int, 0, len(parts))
	for index := range parts {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var builder strings.Builder
	for _, index := range indexes {
		builder.WriteString(parts[index])
	}
	return builder.String()
}

// cacheDeepSeekNativeResponsesReasoningPayload 用于一次性拿到完整 JSON 载荷的
// 场景（非流式响应、SSE→JSON 重建结果）：收集后立即以一次 2s detached context
// 持久化。account 不属于 native DeepSeek Responses 或没有 APIKey 时不处理。
func (s *OpenAIGatewayService) cacheDeepSeekNativeResponsesReasoningPayload(account *Account, apiKeyID int64, payload []byte) {
	if s == nil || !deepSeekNativeResponsesReasoningScope(account) || apiKeyID <= 0 || len(payload) == 0 {
		return
	}
	collector := newDeepSeekNativeReasoningCacheCollector(apiKeyID)
	collector.collectJSONPayload(payload)
	s.flushDeepSeekNativeResponsesReasoningCache(collector)
}

// deepSeekReasoningTextResult 取 content 数组里 reasoning_text 的精确文本（不 trim）。
func deepSeekReasoningTextResult(content gjson.Result) string {
	if !content.IsArray() {
		return ""
	}
	var texts []string
	for _, part := range content.Array() {
		if part.Get("type").String() != "reasoning_text" {
			continue
		}
		if text := part.Get("text").String(); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "")
}

// deepSeekSummaryTextResult 取 summary 数组里 summary_text 的精确文本（不 trim）。
func deepSeekSummaryTextResult(summary gjson.Result) string {
	if !summary.IsArray() {
		return ""
	}
	var texts []string
	for _, part := range summary.Array() {
		if part.Get("type").String() != "summary_text" {
			continue
		}
		if text := part.Get("text").String(); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "")
}
