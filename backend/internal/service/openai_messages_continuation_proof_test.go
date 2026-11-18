//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func proofRequest(t *testing.T, messages []any, stream bool) []byte {
	t.Helper()
	b := v10Request(t, messages, "synthetic-system", "synthetic_tool")
	var r map[string]any
	require.NoError(t, json.Unmarshal(b, &r))
	r["stream"] = stream
	return v10JSON(t, r)
}

func proofClientBlocks(t *testing.T, body []byte, stream bool) []apicompat.AnthropicContentBlock {
	t.Helper()
	if !stream {
		var r apicompat.AnthropicResponse
		require.NoError(t, json.Unmarshal(body, &r))
		return r.Content
	}
	var blocks []apicompat.AnthropicContentBlock
	var active apicompat.AnthropicContentBlock
	var args string
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event apicompat.AnthropicStreamEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		switch event.Type {
		case "content_block_start":
			active = *event.ContentBlock
			args = ""
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				active.Text += event.Delta.Text
			case "thinking_delta":
				active.Thinking += event.Delta.Thinking
			case "signature_delta":
				active.Signature += event.Delta.Signature
			case "input_json_delta":
				args += event.Delta.PartialJSON
			}
		case "content_block_stop":
			if active.Type == "tool_use" && args != "" {
				active.Input = json.RawMessage(args)
			}
			blocks = append(blocks, active)
		}
	}
	return blocks
}

func proofSend(t *testing.T, s *OpenAIGatewayService, messages []any, stream bool) (*OpenAIForwardResult, []apicompat.AnthropicContentBlock) {
	t.Helper()
	b := proofRequest(t, messages, stream)
	c, rec := v10Context(b, 10001)
	result, err := s.ForwardAsAnthropic(context.Background(), c, v10Account(), b, "synthetic-proof-session", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, proofClientBlocks(t, rec.Body.Bytes(), stream)
}

func proofDeltaResponse(id string, completeType string, status string, output []any, text string) *http.Response {
	resp := v10Response(id, v10Model, `{"input_tokens":3,"output_tokens":5}`, output)
	b, _ := io.ReadAll(resp.Body)
	if text != "" {
		prelude := `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":` + fmt.Sprintf("%q", text) + "}\n\n"
		at := bytes.Index(b, []byte("\n\n")) + 2
		b = append(append(append([]byte(nil), b[:at]...), []byte(prelude)...), b[at:]...)
	}
	if completeType != "" {
		b = bytes.ReplaceAll(b, []byte(`"type":"response.completed"`), []byte(`"type":"`+completeType+`"`))
	}
	if status != "" {
		b = bytes.ReplaceAll(b, []byte(`"status":"completed"`), []byte(`"status":"`+status+`"`))
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp
}

func TestV10CacheProofMultiTurnActualEcho(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				v10Response("synthetic-proof-1", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-answer-1")),
				v10Response("synthetic-proof-2", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-answer-2")),
				v10Response("synthetic-proof-3", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-answer-3")),
			}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			history := []any{v10Message("user", "synthetic-root")}
			for round := 0; round < 3; round++ {
				_, blocks := proofSend(t, s, history, stream)
				if round == 0 {
					assert.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
				} else {
					assert.Equal(t, fmt.Sprintf("synthetic-proof-%d", round), gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
					assert.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "input.#").Int(), "仅guard+新user")
				}
				history = append(history, map[string]any{"role": "assistant", "content": blocks}, v10Message("user", fmt.Sprintf("synthetic-next-%d", round)))
			}
		})
	}
}

func TestV10CacheProofDeltaOnlyAndSplitText(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				proofDeltaResponse("synthetic-delta", "", "", []any{}, "synthetic-delta-only"),
				v10Response("synthetic-next", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done")),
			}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			history := []any{v10Message("user", "synthetic-root")}
			_, blocks := proofSend(t, s, history, stream)
			require.NotEmpty(t, blocks)
			history = append(history, map[string]any{"role": "assistant", "content": blocks}, v10Message("user", "synthetic-next"))
			proofSend(t, s, history, stream)
			assert.Equal(t, "synthetic-delta", gjson.GetBytes(upstream.lastBody, "previous_response_id").String(), "实际已发delta完整且normal terminal可证明")
		})
	}
	// 多text块必须整体通过同converter，不能每block独立生成不同item。
	first := proofRequest(t, []any{v10Message("user", "synthetic-root")}, false)
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(first, &req))
	converted, err := apicompat.AnthropicToResponses(&req)
	require.NoError(t, err)
	obs, _ := newContinuationObservation(converted)
	obs.observeResponse(&apicompat.AnthropicResponse{Content: []apicompat.AnthropicContentBlock{{Type: "text", Text: "first"}, {Type: "text", Text: "second"}}, StopReason: apicompat.AnthropicStopReasonPtr("end_turn")})
	proof, ok := obs.completedProof()
	require.True(t, ok)
	next := proofRequest(t, []any{v10Message("user", "synthetic-root"), map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "first"}, map[string]any{"type": "text", "text": "second"}}}, v10Message("user", "synthetic-next")}, false)
	require.NoError(t, json.Unmarshal(next, &req))
	converted, err = apicompat.AnthropicToResponses(&req)
	require.NoError(t, err)
	current, items := newContinuationObservation(converted)
	_, ok = selectContinuationSuffix(openAICompatSessionResponseBinding{ResponseID: "synthetic", Proof: proof}, current, items)
	assert.True(t, ok)
}

func TestV10CacheProofParallelToolSuffixAndCanonicalArgs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := []any{
				map[string]any{"type": "function_call", "id": "fc_one", "call_id": "call_one", "name": "synthetic_tool", "arguments": `{"a":1,"b":2}`, "status": "completed"},
				map[string]any{"type": "function_call", "id": "fc_two", "call_id": "call_two", "name": "synthetic_tool", "arguments": `{"x":3}`, "status": "completed"},
			}
			first := v10Response("synthetic-tool-proof", v10Model, `{"input_tokens":3,"output_tokens":5}`, calls)
			if stream {
				b, _ := io.ReadAll(first.Body)
				var events strings.Builder
				for i, call := range calls {
					item := call.(map[string]any)
					fmt.Fprintf(&events, "data: %s\n\n", v10JSON(t, map[string]any{"type": "response.output_item.added", "output_index": i, "item": item}))
					fmt.Fprintf(&events, "data: %s\n\n", v10JSON(t, map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "arguments": item["arguments"]}))
					fmt.Fprintf(&events, "data: %s\n\n", v10JSON(t, map[string]any{"type": "response.output_item.done", "output_index": i, "item": item}))
				}
				at := bytes.Index(b, []byte("\n\n")) + 2
				first.Body = io.NopCloser(strings.NewReader(string(b[:at]) + events.String() + string(b[at:])))
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{first, v10Response("synthetic-after-tools", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done"))}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			history := []any{v10Message("user", "synthetic-root")}
			_, blocks := proofSend(t, s, history, stream)
			require.Len(t, blocks, 2)
			blocks[0].Input = json.RawMessage(`{ "b": 2, "a": 1 }`)
			history = append(history, map[string]any{"role": "assistant", "content": blocks}, map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_two", "content": "synthetic-result-two"},
				map[string]any{"type": "tool_result", "tool_use_id": "call_one", "content": "synthetic-result-one"},
				map[string]any{"type": "text", "text": "synthetic-continue"},
			}})
			proofSend(t, s, history, stream)
			assert.Equal(t, "synthetic-tool-proof", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
			assert.False(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call")`).Exists(), "provider已有function_call不得重发")
			assert.Len(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call_output")#`).Array(), 2)
		})
	}
}

type proofFailWriter struct {
	header http.Header
	mode   string
}

func (w *proofFailWriter) Header() http.Header { return w.header }
func (w *proofFailWriter) WriteHeader(int)     {}
func (w *proofFailWriter) Write(p []byte) (int, error) {
	if w.mode == "short" {
		return len(p) / 2, nil
	}
	return 0, errors.New("synthetic-write-failure")
}
func (w *proofFailWriter) Flush() {}

func TestV10CacheProofBufferedWriteFailureDoesNotBind(t *testing.T) {
	for _, mode := range []string{"error", "short", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: v10Response("synthetic-unwritten", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-answer"))}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			b := proofRequest(t, []any{v10Message("user", "synthetic-root")}, false)
			writer := &proofFailWriter{header: make(http.Header), mode: mode}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(b))
			c.Set("api_key", &APIKey{ID: 10001})
			if mode == "canceled" {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			_, _ = s.ForwardAsAnthropic(context.Background(), c, v10Account(), b, "synthetic-proof-session", "")
			assert.Empty(t, s.loadOpenAICompatSessionBinding(c, v10Account(), "synthetic-proof-session").ResponseID, "实际JSON未完整发出不能绑定")
		})
	}
}

func TestV10CacheProofUnknownAndNonNormalTerminalNoBinding(t *testing.T) {
	for _, tc := range []struct {
		name, typ, status string
		output            []any
	}{
		{"incomplete_other", "response.incomplete", "incomplete", v10TextOutput("synthetic-partial")},
		{"unknown_block", "", "", []any{map[string]any{"type": "web_search_call", "id": "synthetic-search", "status": "completed", "action": map[string]any{"type": "search", "query": "synthetic"}}}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", tc.name, stream), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: proofDeltaResponse("synthetic-no-proof", tc.typ, tc.status, tc.output, "")}
				s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				b := proofRequest(t, []any{v10Message("user", "synthetic-root")}, stream)
				c, _ := v10Context(b, 10001)
				_, _ = s.ForwardAsAnthropic(context.Background(), c, v10Account(), b, "synthetic-proof-session", "")
				assert.Empty(t, s.loadOpenAICompatSessionBinding(c, v10Account(), "synthetic-proof-session").ResponseID)
			})
		}
	}
}

func TestV10CacheProofBindingSnapshotCASAndBudget(t *testing.T) {
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	a := v10Account()
	proof := compatContinuationProof{ReqCount: 1, Count: 2, ChainHash: [32]byte{1}}
	s.bindOpenAICompatSessionResponseProof(context.Background(), nil, a, "synthetic", "old", proof)
	old := s.loadOpenAICompatSessionBinding(nil, a, "synthetic")
	newProof := proof
	newProof.ChainHash[0] = 2
	s.bindOpenAICompatSessionResponseProof(context.Background(), nil, a, "synthetic", "new", newProof)
	s.deleteOpenAICompatSessionResponseIDIf(context.Background(), nil, a, "synthetic", old)
	assert.Equal(t, "new", s.loadOpenAICompatSessionBinding(nil, a, "synthetic").ResponseID)
	assert.Equal(t, "old", old.ResponseID, "快照不可变")
	assert.Equal(t, byte(1), old.Proof.ChainHash[0])
	assert.Equal(t, 80+len("old"), old.sessionCost())
	value := reflect.TypeOf(compatContinuationProof{})
	for i := 0; i < value.NumField(); i++ {
		assert.NotEqual(t, reflect.String, value.Field(i).Type.Kind())
		assert.NotEqual(t, reflect.Slice, value.Field(i).Type.Kind())
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			for n := 0; n < 50; n++ {
				s.bindOpenAICompatSessionTurnState(context.Background(), nil, a, "synthetic", fmt.Sprintf("synthetic-%d-%d", i, n))
				s.loadOpenAICompatSessionBinding(nil, a, "synthetic")
			}
		}(i)
	}
	group.Wait()
	assert.Equal(t, "new", s.loadOpenAICompatSessionBinding(nil, a, "synthetic").ResponseID)
	obs := &continuationObservation{valid: true, blocks: []apicompat.AnthropicContentBlock{{Type: "text", Text: "synthetic-raw"}}, activeIndex: -1}
	assert.False(t, obs.spend(compatContinuationObservationMaxBytes+1))
	assert.Nil(t, obs.blocks)
	assert.False(t, obs.valid)
	assert.Equal(t, 16384, sessionMemoryCacheMaxEntries)
	assert.Equal(t, 32<<20, sessionMemoryCacheMaxBytes)
	s.openaiCompatSessionResponses.Close()
}

func TestV10CacheStableAnchorRequiresRealMaterial(t *testing.T) {
	for _, system := range []string{`""`, `[]`, `null`} {
		var req apicompat.AnthropicRequest
		require.NoError(t, json.Unmarshal([]byte(`{"system":`+system+`,"messages":[{"role":"assistant","content":[{"type":"text","text":"synthetic-changing","cache_control":{"type":"ephemeral"}}]}]}`), &req))
		assert.Empty(t, deriveAnthropicCacheControlPromptCacheKey(&req), "不能用空system生成跨会话常量key")
	}
}

func TestV10CacheProofCASRetryUsesFullReplay(t *testing.T) {
	// 专项模拟旧snapshot失效时另一个同history新response已写入；本次重试仍必须全量。
	body := proofRequest(t, []any{v10Message("user", "synthetic-root"), v10Message("assistant", "synthetic-answer"), v10Message("user", "synthetic-next")}, false)
	a := v10Account()
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	c, _ := v10Context(body, 10001)
	bindCompatProofForNextRequest(t, s, c, a, "synthetic-proof-session", "synthetic-old", body, v10Model)
	old := s.loadOpenAICompatSessionBinding(c, a, "synthetic-proof-session")
	u := &proofConcurrentRetryUpstream{s: s, account: a, c: c, proof: old.Proof}
	s.httpUpstream = u
	_, err := s.ForwardAsAnthropic(context.Background(), c, a, body, "synthetic-proof-session", "")
	require.NoError(t, err)
	require.Len(t, u.bodies, 2)
	assert.Equal(t, "synthetic-old", gjson.GetBytes(u.bodies[0], "previous_response_id").String())
	assert.False(t, gjson.GetBytes(u.bodies[1], "previous_response_id").Exists(), "not-found retry不能另挑并发新previous")
	assert.Equal(t, int64(5), gjson.GetBytes(u.bodies[1], "input.#").Int())
}

type proofConcurrentRetryUpstream struct {
	s       *OpenAIGatewayService
	account *Account
	c       *gin.Context
	proof   compatContinuationProof
	bodies  [][]byte
}

func (u *proofConcurrentRetryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.bodies = append(u.bodies, b)
	if len(u.bodies) == 1 {
		u.s.bindOpenAICompatSessionResponseProof(context.Background(), u.c, u.account, "synthetic-proof-session", "synthetic-new", u.proof)
		return v10HTTPError("Previous response with id synthetic-old not found", "previous_response_not_found"), nil
	}
	return v10Response("synthetic-final", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-done")), nil
}
func (u *proofConcurrentRetryUpstream) DoWithTLS(req *http.Request, p string, a int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, a, n)
}
