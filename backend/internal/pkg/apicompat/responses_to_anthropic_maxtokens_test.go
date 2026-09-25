package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 未指定 max_output_tokens 时的兜底值：Claude 系列要够大，避免 Opus 5.5 的
// adaptive thinking 把 8192 的预算吃光后触发 max_output_tokens 截断；非 Claude
// 的 Anthropic 兼容端点输出上限更小，保持原来的保守默认值。
func TestResponsesToAnthropicRequest_DefaultMaxTokens(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  int
	}{
		{name: "opus 5.5", model: "claude-opus-5-5", want: defaultClaudeMaxTokens},
		{name: "provider prefixed claude", model: "anthropic/claude-opus-5-5", want: defaultClaudeMaxTokens},
		{name: "other claude", model: "claude-sonnet-4-6", want: defaultClaudeMaxTokens},
		{name: "deepseek", model: "deepseek-v4-flash", want: defaultMaxTokens},
		{name: "kimi", model: "kimi-k2.6", want: defaultMaxTokens},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
				Model: tt.model,
				Input: json.RawMessage(`"hi"`),
			})
			require.NoError(t, err)
			require.Equal(t, tt.want, out.MaxTokens)
		})
	}
}

func TestResponsesToAnthropicRequest_ExplicitMaxOutputTokensWins(t *testing.T) {
	limit := 4096
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:           "claude-opus-5-5",
		Input:           json.RawMessage(`"hi"`),
		MaxOutputTokens: &limit,
	})
	require.NoError(t, err)
	require.Equal(t, limit, out.MaxTokens)
}
