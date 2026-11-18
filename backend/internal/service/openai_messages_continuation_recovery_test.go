//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV10CacheProofInvalidResponseRecoversNextValid(t *testing.T) {
	for _, kind := range []string{"duplicate_args", "server_block", "oversize", "max_tokens"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, stream), func(t *testing.T) {
				var output []any
				switch kind {
				case "duplicate_args":
					output = []any{map[string]any{"type": "function_call", "name": "synthetic_tool", "call_id": "synthetic-duplicate", "arguments": `{"x":1,"x":2}`, "status": "completed"}}
				case "server_block":
					output = []any{map[string]any{"type": "web_search_call", "id": "synthetic-search", "status": "completed", "action": map[string]any{"type": "search", "query": "synthetic"}}}
				case "oversize":
					output = v10TextOutput(strings.Repeat("x", 4<<20))
				default:
					output = v10TextOutput("synthetic-incomplete")
				}
				first := v10Response("synthetic-unprovable", v10Model, `{"input_tokens":3,"output_tokens":5}`, output)
				if kind == "max_tokens" {
					b, _ := io.ReadAll(first.Body)
					b = []byte(strings.ReplaceAll(string(b), `"type":"response.completed"`, `"type":"response.incomplete"`))
					b = []byte(strings.ReplaceAll(string(b), `"status":"completed"`, `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}`))
					first.Body = io.NopCloser(strings.NewReader(string(b)))
				}
				upstream := &httpUpstreamRecorder{responses: []*http.Response{first, v10Response("synthetic-recovered", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-good")), v10Response("synthetic-third", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done"))}}
				s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				if kind == "oversize" {
					s.cfg.Gateway.MaxLineSize = 16 << 20
				}
				history := []any{v10Message("user", "synthetic-root")}
				body := proofRequest(t, history, stream)
				c, _ := v10Context(body, 10001)
				_, _ = s.ForwardAsAnthropic(context.Background(), c, v10Account(), body, "synthetic-proof-session", "")
				assert.Empty(t, s.loadOpenAICompatSessionBinding(c, v10Account(), "synthetic-proof-session").ResponseID)
				// 该响应不可证明不禁会话；后续完整有效响应可重新建立proof。
				_, blocks := proofSend(t, s, history, stream)
				assert.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
				history = append(history, map[string]any{"role": "assistant", "content": blocks}, v10Message("user", "synthetic-next"))
				proofSend(t, s, history, stream)
				assert.Equal(t, "synthetic-recovered", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
			})
		}
	}
}

func TestV10CacheProofThinkingSignatureUsesSameConverter(t *testing.T) {
	for _, signature := range []string{"synthetic-provider-signature", "gAAAA-synthetic-foreign", "ccswitch-openai-reasoning-v1:synthetic"} {
		t.Run(signature, func(t *testing.T) {
			request := proofRequest(t, []any{v10Message("user", "synthetic-root")}, false)
			var req apicompat.AnthropicRequest
			require.NoError(t, json.Unmarshal(request, &req))
			converted, err := apicompat.AnthropicToResponses(&req)
			require.NoError(t, err)
			observer, _ := newContinuationObservation(converted)
			blocks := []apicompat.AnthropicContentBlock{{Type: "thinking", Thinking: "synthetic-thinking", Signature: signature}, {Type: "text", Text: "synthetic-answer"}}
			observer.observeResponse(&apicompat.AnthropicResponse{Content: blocks, StopReason: apicompat.AnthropicStopReasonPtr("end_turn")})
			proof, ok := observer.completedProof()
			require.True(t, ok)
			next := proofRequest(t, []any{v10Message("user", "synthetic-root"), map[string]any{"role": "assistant", "content": blocks}, v10Message("user", "synthetic-next")}, false)
			require.NoError(t, json.Unmarshal(next, &req))
			converted, err = apicompat.AnthropicToResponses(&req)
			require.NoError(t, err)
			current, items := newContinuationObservation(converted)
			_, ok = selectContinuationSuffix(openAICompatSessionResponseBinding{ResponseID: "synthetic", Proof: proof}, current, items)
			assert.True(t, ok)
		})
	}
}

func TestV10CacheProofStreamObserverSequenceAndLimits(t *testing.T) {
	request := &apicompat.ResponsesRequest{Model: v10Model, Input: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic"}]}]`)}
	for _, kind := range []string{"unknown_delta", "missing_stop", "duplicate_args", "limit"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := newContinuationObservation(request)
			o.observeEvent(apicompat.AnthropicStreamEvent{Type: "message_start", Message: &apicompat.AnthropicResponse{}})
			index := 0
			block := &apicompat.AnthropicContentBlock{Type: "text"}
			if kind == "duplicate_args" {
				block = &apicompat.AnthropicContentBlock{Type: "tool_use", ID: "synthetic", Name: "synthetic_tool", Input: json.RawMessage(`{}`)}
			}
			o.observeEvent(apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &index, ContentBlock: block})
			if kind == "limit" {
				o.spend(4 << 20)
			} else if kind == "unknown_delta" {
				o.observeEvent(apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &index, Delta: &apicompat.AnthropicDelta{Type: "future_unknown", Text: "synthetic"}})
			} else if kind == "duplicate_args" {
				o.observeEvent(apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &index, Delta: &apicompat.AnthropicDelta{Type: "input_json_delta", PartialJSON: `{"x":1,"x":2}`}})
			}
			if kind != "missing_stop" {
				o.observeEvent(apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &index})
			}
			o.observeEvent(apicompat.AnthropicStreamEvent{Type: "message_delta", Delta: &apicompat.AnthropicDelta{StopReason: "end_turn"}})
			o.observeEvent(apicompat.AnthropicStreamEvent{Type: "message_stop"})
			_, ok := o.completedProof()
			assert.False(t, ok)
		})
	}
}
