package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 未指定 max_output_tokens 时的兜底值：统一用 81920，避免 thinking 把 8192
// 的预算吃光后触发 max_output_tokens 截断。
func TestResponsesToAnthropicRequest_DefaultMaxTokens(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  int
	}{
		{name: "opus 5.5", model: "claude-opus-5-5", want: defaultMaxTokens},
		{name: "provider prefixed claude", model: "anthropic/claude-opus-5-5", want: defaultMaxTokens},
		{name: "other claude", model: "claude-sonnet-4-6", want: defaultMaxTokens},
		{name: "deepseek", model: "deepseek-v4-flash", want: defaultMaxTokens},
		{name: "kimi", model: "kimi-k2.6", want: defaultMaxTokens},
		{name: "glm", model: "glm-5.3", want: defaultMaxTokens},
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
