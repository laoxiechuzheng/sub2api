package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestApplyClaudeCodeModeToolOutputHint_OnlyTargetsClaudeExec(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "claude-opus-5",
		Tools: []apicompat.ChatTool{{
			Type: "function",
			Function: &apicompat.ChatFunction{
				Name:        "functions__exec",
				Description: "Execute code",
			},
		}},
	}

	applyClaudeCodeModeToolOutputHint(req)
	require.Contains(t, req.Tools[0].Function.Description, "notify(result.output)")

	other := &apicompat.ChatCompletionsRequest{
		Model: "glm-5.2",
		Tools: []apicompat.ChatTool{{
			Type: "function",
			Function: &apicompat.ChatFunction{Name: "functions__exec", Description: "Execute code"},
		}},
	}
	applyClaudeCodeModeToolOutputHint(other)
	require.NotContains(t, other.Tools[0].Function.Description, "notify(result.output)")
}
