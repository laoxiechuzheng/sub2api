//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func eligibilityFixture(kind string) *http.Response {
	response := v10Response("synthetic-eligibility", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-visible"))
	body, _ := io.ReadAll(response.Body)
	at := strings.Index(string(body), "\n\n") + 2
	var extra string
	switch kind {
	case "unknown_delta":
		extra = `data: {"type":"response.future_unknown.delta","delta":"synthetic-unknown"}` + "\n\n"
	case "malformed_ignored":
		extra = "data: {synthetic-malformed}\n\n"
	case "unknown_finish":
		// created 帧保持正常，只把无末分隔符的 terminal 留给 parser.Finish。
		terminal := strings.Replace(string(body[at:]), `"output":[`, `"output":[{"type":"future_unknown_output"},`, 1)
		body = append(append([]byte(nil), body[:at]...), []byte(strings.TrimRight(terminal, "\n"))...)
	case "known_lifecycle":
		extra = `data: {"type":"response.in_progress","response":{"id":"synthetic-eligibility","status":"in_progress","output":[]}}` + "\n\n"
		extra += `data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}` + "\n\n"
	}
	if extra != "" {
		body = append(append(append([]byte(nil), body[:at]...), []byte(extra)...), body[at:]...)
	}
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	return response
}

func TestV10CacheEligibilityUnknownDeltaFinishAndRecovery(t *testing.T) {
	for _, kind := range []string{"unknown_delta", "malformed_ignored", "unknown_finish", "known_lifecycle"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, stream), func(t *testing.T) {
				u := &httpUpstreamRecorder{responses: []*http.Response{
					eligibilityFixture(kind),
					v10Response("synthetic-valid-after-veto", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-valid")),
					v10Response("synthetic-third", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done")),
				}}
				s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
				root := []any{v10Message("user", "synthetic-root")}
				b := proofRequest(t, root, stream)
				c, recorder := v10Context(b, 10001)
				result, err := s.ForwardAsAnthropic(context.Background(), c, v10Account(), b, "synthetic-proof-session", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, 3, result.Usage.InputTokens)
				assert.Equal(t, 5, result.Usage.OutputTokens)
				binding := s.loadOpenAICompatSessionBinding(c, v10Account(), "synthetic-proof-session")
				if kind == "known_lifecycle" {
					assert.Equal(t, "synthetic-eligibility", binding.ResponseID)
				} else {
					assert.Empty(t, binding.ResponseID, "unknown/malformed上游不能被静默丢弃后绑定")
				}
				// 对照相同response handler但无observer：veto只能改变proof，不改变body/header/usage/error。
				baselineContext, baselineRecorder := v10Context(b, 10001)
				baseline := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
				var baselineResult *OpenAIForwardResult
				if stream {
					baselineResult, err = baseline.handleAnthropicStreamingResponse(eligibilityFixture(kind), baselineContext, v10Account(), v10Model, v10Model, v10Model, time.Now())
				} else {
					baselineResult, err = baseline.handleAnthropicBufferedStreamingResponse(eligibilityFixture(kind), baselineContext, v10Account(), v10Model, v10Model, v10Model, time.Now())
				}
				require.NoError(t, err)
				assert.Equal(t, recorder.Body.String(), baselineRecorder.Body.String())
				assert.Equal(t, recorder.Header(), baselineRecorder.Header())
				assert.Equal(t, result.Usage, baselineResult.Usage)
				if kind != "known_lifecycle" {
					_, echo := proofSend(t, s, root, stream)
					assert.False(t, gjson.GetBytes(u.lastBody, "previous_response_id").Exists())
					history := append(root, map[string]any{"role": "assistant", "content": echo}, v10Message("user", "synthetic-next"))
					proofSend(t, s, history, stream)
					assert.Equal(t, "synthetic-valid-after-veto", gjson.GetBytes(u.lastBody, "previous_response_id").String())
				}
			})
		}
	}
}

func TestV10CacheEligibilityReasoningAndTextRemainUsable(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			output := []any{map[string]any{"type": "reasoning", "encrypted_content": "synthetic-provider-signature", "summary": []any{map[string]any{"type": "summary_text", "text": "synthetic-reasoning"}}}}
			output = append(output, v10TextOutput("synthetic-text")...)
			response := v10Response("synthetic-reasoning-proof", v10Model, `{"input_tokens":3,"output_tokens":5}`, output)
			if stream {
				body, _ := io.ReadAll(response.Body)
				at := strings.Index(string(body), "\n\n") + 2
				var frames strings.Builder
				for _, typ := range []string{"response.output_item.added", "response.output_item.done"} {
					item := output[0]
					fmt.Fprintf(&frames, "data: %s\n\n", v10JSON(t, map[string]any{"type": typ, "output_index": 0, "item": item}))
					if typ == "response.output_item.added" {
						fmt.Fprintf(&frames, "data: %s\n\n", v10JSON(t, map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": "synthetic-reasoning"}))
					}
				}
				response.Body = io.NopCloser(strings.NewReader(string(body[:at]) + frames.String() + string(body[at:])))
			}
			u := &httpUpstreamRecorder{responses: []*http.Response{response, v10Response("synthetic-next", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done"))}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
			root := []any{v10Message("user", "synthetic-root")}
			_, echo := proofSend(t, s, root, stream)
			for _, block := range echo {
				if block.Type == "thinking" {
					assert.Equal(t, "synthetic-provider-signature", block.Signature)
				}
			}
			history := append(root, map[string]any{"role": "assistant", "content": echo}, v10Message("user", "synthetic-next"))
			proofSend(t, s, history, stream)
			assert.Equal(t, "synthetic-reasoning-proof", gjson.GetBytes(u.lastBody, "previous_response_id").String())
		})
	}
}
