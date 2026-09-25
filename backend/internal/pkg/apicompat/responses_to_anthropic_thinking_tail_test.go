package apicompat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func responsesToAnthropicMessagesForModel(t *testing.T, model, input string) []AnthropicMessage {
	t.Helper()

	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model: model,
		Input: json.RawMessage(input),
	})
	require.NoError(t, err)
	return out.Messages
}

func anthropicThinkingEnvelopeForTest(t *testing.T, block AnthropicContentBlock) string {
	t.Helper()

	payload, err := json.Marshal(block)
	require.NoError(t, err)
	return anthropicThinkingEnvelopePrefix + base64.RawStdEncoding.EncodeToString(payload)
}

func requireAlternatingAnthropicRoles(t *testing.T, messages []AnthropicMessage) {
	t.Helper()

	for i := 1; i < len(messages); i++ {
		require.NotEqualf(t, messages[i-1].Role, messages[i].Role,
			"messages[%d] and messages[%d] have the same role %q", i-1, i, messages[i].Role)
	}
}

func requireNoAssistantEndsWithThinking(t *testing.T, messages []AnthropicMessage) {
	t.Helper()

	for i, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		blocks := parseContentBlocks(m.Content)
		require.NotEmptyf(t, blocks, "assistant messages[%d] has empty content", i)
		last := blocks[len(blocks)-1].Type
		require.NotContainsf(t, []string{"thinking", "redacted_thinking"}, last,
			"assistant messages[%d] ends with %q", i, last)
	}
}

func TestResponsesToAnthropic_DropsTruncatedAssistantThinkingTail(t *testing.T) {
	envelope := anthropicThinkingEnvelopeForTest(t, AnthropicContentBlock{
		Type:      "thinking",
		Thinking:  "reasoning only",
		Signature: "sig-thinking",
	})
	input := fmt.Sprintf(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":%q},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	]`, envelope)

	messages := responsesToAnthropicMessagesForModel(t, "claude-opus-5-5", input)

	require.Len(t, messages, 1)
	require.Equal(t, "user", messages[0].Role)
	requireAlternatingAnthropicRoles(t, messages)
	requireNoAssistantEndsWithThinking(t, messages)
}

func TestResponsesToAnthropic_KeepsThinkingBeforeAssistantText(t *testing.T) {
	envelope := anthropicThinkingEnvelopeForTest(t, AnthropicContentBlock{
		Type:      "thinking",
		Thinking:  "reasoning before text",
		Signature: "sig-thinking",
	})
	input := fmt.Sprintf(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":%q},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
	]`, envelope)

	messages := responsesToAnthropicMessagesForModel(t, "claude-opus-5-5", input)

	require.Len(t, messages, 2)
	require.Equal(t, "assistant", messages[1].Role)
	blocks := parseContentBlocks(messages[1].Content)
	require.Len(t, blocks, 2)
	require.Equal(t, "thinking", blocks[0].Type)
	require.Equal(t, "reasoning before text", blocks[0].Thinking)
	require.Equal(t, "text", blocks[1].Type)
	requireAlternatingAnthropicRoles(t, messages)
	requireNoAssistantEndsWithThinking(t, messages)
}

func TestResponsesToAnthropic_KeepsThinkingBeforeToolUse(t *testing.T) {
	envelope := anthropicThinkingEnvelopeForTest(t, AnthropicContentBlock{
		Type:      "thinking",
		Thinking:  "planning a tool call",
		Signature: "sig-thinking",
	})
	input := fmt.Sprintf(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run it"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":%q},
		{"type":"function_call","id":"fc_1","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":"ok"}
	]`, envelope)

	messages := responsesToAnthropicMessagesForModel(t, "claude-opus-5-5", input)

	require.Len(t, messages, 3)
	require.Equal(t, "assistant", messages[1].Role)
	blocks := parseContentBlocks(messages[1].Content)
	require.Len(t, blocks, 2)
	require.Equal(t, "thinking", blocks[0].Type)
	require.Equal(t, "tool_use", blocks[1].Type)
	assertAnthropicPairing(t, messages)
	requireAlternatingAnthropicRoles(t, messages)
	requireNoAssistantEndsWithThinking(t, messages)
}

func TestResponsesToAnthropic_DropsTruncatedRedactedThinkingTail(t *testing.T) {
	envelope := anthropicThinkingEnvelopeForTest(t, AnthropicContentBlock{
		Type: "redacted_thinking",
		Data: "redacted-payload",
	})
	input := fmt.Sprintf(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":%q},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	]`, envelope)

	messages := responsesToAnthropicMessagesForModel(t, "claude-opus-5-5", input)

	require.Len(t, messages, 1)
	require.Equal(t, "user", messages[0].Role)
	requireAlternatingAnthropicRoles(t, messages)
	requireNoAssistantEndsWithThinking(t, messages)
}

func TestStripTrailingAssistantThinking(t *testing.T) {
	tests := []struct {
		name     string
		messages []AnthropicMessage
		want     []string
	}{
		{
			name: "drops assistant containing only thinking",
			messages: []AnthropicMessage{
				{Role: "user", Content: mustMarshalAnthropicBlocksForTest(t, AnthropicContentBlock{Type: "text", Text: "hi"})},
				{Role: "assistant", Content: mustMarshalAnthropicBlocksForTest(t, AnthropicContentBlock{Type: "thinking", Thinking: "only", Signature: "sig"})},
			},
			want: []string{"user"},
		},
		{
			name: "drops all trailing thinking blocks",
			messages: []AnthropicMessage{
				{Role: "assistant", Content: mustMarshalAnthropicBlocksForTest(t,
					AnthropicContentBlock{Type: "thinking", Thinking: "first", Signature: "sig-1"},
					AnthropicContentBlock{Type: "redacted_thinking", Data: "data"},
				)},
			},
			want: []string{},
		},
		{
			name: "keeps thinking before text",
			messages: []AnthropicMessage{
				{Role: "assistant", Content: mustMarshalAnthropicBlocksForTest(t,
					AnthropicContentBlock{Type: "thinking", Thinking: "plan", Signature: "sig"},
					AnthropicContentBlock{Type: "text", Text: "answer"},
				)},
			},
			want: []string{"assistant"},
		},
		{
			name: "ignores non-assistant messages",
			messages: []AnthropicMessage{
				{Role: "user", Content: mustMarshalAnthropicBlocksForTest(t, AnthropicContentBlock{Type: "thinking", Thinking: "not stripped", Signature: "sig"})},
			},
			want: []string{"user"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripTrailingAssistantThinking(tt.messages)
			require.Len(t, got, len(tt.want))
			for i, role := range tt.want {
				require.Equal(t, role, got[i].Role)
			}
		})
	}
}

func mustMarshalAnthropicBlocksForTest(t *testing.T, blocks ...AnthropicContentBlock) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	return raw
}
