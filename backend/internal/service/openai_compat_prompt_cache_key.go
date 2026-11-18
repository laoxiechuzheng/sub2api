package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const compatPromptCacheKeyPrefix = "compat_cc_"

func shouldAutoInjectPromptCacheKeyForCompat(model string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(model))
	canonical := canonicalizeOpenAIModelAliasSpelling(trimmed)
	// GPT-6 Sol/Luna and GPT-6.1 Sol/Terra share the supported Responses cache identity contract.
	if canonical == "gpt-6" || canonical == "gpt-6-astra" || openai.IsGPT6SolOrLunaModelSpelling(canonical) || openai.IsGPT61SolModelSpelling(canonical) || openai.IsGPT61TerraModelSpelling(canonical) {
		return true
	}
	// 仅对 Responses 兼容路径支持的 GPT-5 族开启自动注入，避免 normalizeCodexModel
	// 的默认兜底把任意模型（如 gpt-4o、claude-*）误判为 gpt-5.4。
	if !strings.Contains(trimmed, "gpt-5") && !strings.Contains(trimmed, "codex") {
		return false
	}
	normalized := strings.TrimSpace(strings.ToLower(normalizeCodexModel(trimmed)))
	return strings.HasPrefix(normalized, "gpt-5") || strings.Contains(normalized, "codex")
}
func deriveCompatPromptCacheKey(req *apicompat.ChatCompletionsRequest, mappedModel string) string {
	if req == nil {
		return ""
	}

	normalizedModel := normalizeCodexModel(strings.TrimSpace(mappedModel))
	if normalizedModel == "" {
		normalizedModel = normalizeCodexModel(strings.TrimSpace(req.Model))
	}
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(req.Model)
	}

	seedParts := []string{"model=" + normalizedModel}
	if req.ReasoningEffort != "" {
		seedParts = append(seedParts, "reasoning_effort="+strings.TrimSpace(req.ReasoningEffort))
	}
	if len(req.ToolChoice) > 0 {
		seedParts = append(seedParts, "tool_choice="+normalizeCompatSeedJSON(req.ToolChoice))
	}
	if len(req.Tools) > 0 {
		if raw, err := json.Marshal(req.Tools); err == nil {
			seedParts = append(seedParts, "tools="+normalizeCompatSeedJSON(raw))
		}
	}
	if len(req.Functions) > 0 {
		if raw, err := json.Marshal(req.Functions); err == nil {
			seedParts = append(seedParts, "functions="+normalizeCompatSeedJSON(raw))
		}
	}

	firstUserCaptured := false
	for _, msg := range req.Messages {
		switch strings.TrimSpace(msg.Role) {
		case "system":
			seedParts = append(seedParts, "system="+normalizeCompatSeedJSON(msg.Content))
		case "user":
			if !firstUserCaptured {
				seedParts = append(seedParts, "first_user="+normalizeCompatSeedJSON(msg.Content))
				firstUserCaptured = true
			}
		}
	}

	return compatPromptCacheKeyPrefix + hashSensitiveValueForLog(strings.Join(seedParts, "|"))
}

func deriveAnthropicCompatPromptCacheKey(req *apicompat.AnthropicRequest, mappedModel string) string {
	if req == nil {
		return ""
	}
	if anchorKey := deriveAnthropicCacheControlPromptCacheKey(req); anchorKey != "" {
		return anchorKey
	}

	normalizedModel := normalizeCodexModel(strings.TrimSpace(mappedModel))
	if normalizedModel == "" {
		normalizedModel = normalizeCodexModel(strings.TrimSpace(req.Model))
	}
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(req.Model)
	}

	seedParts := []string{"model=" + normalizedModel}
	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		seedParts = append(seedParts, "effort="+strings.TrimSpace(req.OutputConfig.Effort))
	}
	if len(req.ToolChoice) > 0 {
		seedParts = append(seedParts, "tool_choice="+normalizeCompatSeedJSON(req.ToolChoice))
	}
	if len(req.Tools) > 0 {
		if raw, err := json.Marshal(req.Tools); err == nil {
			seedParts = append(seedParts, "tools="+normalizeCompatSeedJSON(raw))
		}
	}
	if len(req.System) > 0 {
		seedParts = append(seedParts, "system="+normalizeCompatSeedJSON(req.System))
	}

	firstUserCaptured := false
	for _, msg := range req.Messages {
		if strings.TrimSpace(msg.Role) != "user" || firstUserCaptured {
			continue
		}
		seedParts = append(seedParts, "first_user="+normalizeCompatSeedJSON(msg.Content))
		firstUserCaptured = true
	}

	return compatPromptCacheKeyPrefix + hashSensitiveValueForLog(strings.Join(seedParts, "|"))
}

// 原 anchor 算法仅用于保持非本次目标的 Grok identity 不变。
func deriveLegacyAnthropicCacheControlPromptCacheKey(req *apicompat.AnthropicRequest) string {
	if req == nil {
		return ""
	}

	var parts []string
	var systemBlocks []apicompat.AnthropicContentBlock
	if len(req.System) > 0 && json.Unmarshal(req.System, &systemBlocks) == nil {
		for _, block := range systemBlocks {
			if block.Type == "text" &&
				block.CacheControl != nil &&
				strings.TrimSpace(block.CacheControl.Type) == "ephemeral" &&
				strings.TrimSpace(block.Text) != "" {
				parts = append(parts, "system:"+strings.TrimSpace(block.Text))
			}
		}
	}

	firstUserAnchor := ""
	for _, msg := range req.Messages {
		var blocks []apicompat.AnthropicContentBlock
		if len(msg.Content) == 0 || json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		for _, block := range blocks {
			if block.Type != "text" ||
				block.CacheControl == nil ||
				strings.TrimSpace(block.CacheControl.Type) != "ephemeral" ||
				strings.TrimSpace(block.Text) == "" {
				continue
			}
			switch role {
			case "user":
				if firstUserAnchor == "" {
					firstUserAnchor = strings.TrimSpace(block.Text)
				}
			case "assistant":
				parts = append(parts, "assistant:"+strings.TrimSpace(block.Text))
			}
		}
	}
	if firstUserAnchor != "" {
		parts = append(parts, "user_anchor:"+firstUserAnchor)
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("anthropic-cache:" + strings.Join(parts, "\n")))
	return fmt.Sprintf("anthropic-cache-%x", sum[:16])
}

func isolatedCompatPromptCacheBodyKey(apiKeyID int64, raw string) string {
	seed := fmt.Sprintf("v1|apikey:%d|key:%s", apiKeyID, strings.TrimSpace(raw))
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("sub2api-pck-%x", sum[:16])
}

func deriveAnthropicCacheControlPromptCacheKey(req *apicompat.AnthropicRequest) string {
	if req == nil || deriveLegacyAnthropicCacheControlPromptCacheKey(req) == "" {
		return ""
	}
	parts := make([]string, 0, 2)
	var systemBlocks []apicompat.AnthropicContentBlock
	if json.Unmarshal(req.System, &systemBlocks) == nil {
		for _, block := range systemBlocks {
			if block.Type == "text" && block.CacheControl != nil && strings.TrimSpace(block.CacheControl.Type) == "ephemeral" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, "system:"+block.Text)
			}
		}
	}
	for _, message := range req.Messages {
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) != nil {
			var blocks []apicompat.AnthropicContentBlock
			if json.Unmarshal(message.Content, &blocks) == nil {
				for _, block := range blocks {
					if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
						text = block.Text
						break
					}
				}
			}
		}
		if strings.TrimSpace(text) != "" {
			parts = append(parts, "first_user:"+text)
		}
		break
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("anthropic-cache-v2-%x", sum[:16])
}

func normalizeCompatSeedJSON(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var tmp any
	if err := json.Unmarshal(v, &tmp); err != nil {
		return string(v)
	}
	out, err := json.Marshal(tmp)
	if err != nil {
		return string(v)
	}
	return string(out)
}
