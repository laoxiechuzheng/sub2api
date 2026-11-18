//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV10CacheAnchorOnlyStableMaterial(t *testing.T) {
	var first, second apicompat.AnthropicRequest
	for i, target := range []*apicompat.AnthropicRequest{&first, &second} {
		body := fmt.Sprintf(`{"system":[{"type":"text","text":"synthetic-stable","cache_control":{"type":"ephemeral"}},{"type":"text","text":"synthetic-changing-%d"}],"messages":[{"role":"user","content":"synthetic-root"}]}`, i)
		require.NoError(t, json.Unmarshal([]byte(body), target))
	}
	assert.Equal(t, deriveAnthropicCacheControlPromptCacheKey(&first), deriveAnthropicCacheControlPromptCacheKey(&second), "无标记system动态信息不应变stable anchor")
}

func TestV10CacheProofUnknownProviderOutputCannotProve(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			output := append(v10TextOutput("synthetic-deliverable"), map[string]any{"type": "future_unknown_output", "id": "synthetic-unknown"})
			upstream := &httpUpstreamRecorder{resp: v10Response("synthetic-unknown-response", v10Model, `{"input_tokens":3,"output_tokens":5}`, output)}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			body := proofRequest(t, []any{v10Message("user", "synthetic-root")}, stream)
			c, _ := v10Context(body, 10001)
			_, err := s.ForwardAsAnthropic(context.Background(), c, v10Account(), body, "synthetic-proof-session", "")
			require.NoError(t, err)
			assert.Empty(t, s.loadOpenAICompatSessionBinding(c, v10Account(), "synthetic-proof-session").ResponseID, "unknown provider块不能仅凭其余text视为可证明")
		})
	}
}

func TestV10CacheProofClientWriteBytesUnchanged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := proofRequest(t, []any{v10Message("user", "synthetic-root")}, stream)
			var got []string
			for _, key := range []string{"synthetic-enable-proof", ""} {
				// 同一真实转换/写出，空key显式禁本次proof，不改变响应内容。
				u := &httpUpstreamRecorder{resp: v10Response("synthetic-identical", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-identical"))}
				s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
				c, r := v10Context(body, 10001)
				if key == "" {
					var req map[string]any
					require.NoError(t, json.Unmarshal(body, &req))
					delete(req, "metadata")
					req["model"] = "gpt-4o"
					body = v10JSON(t, req)
				}
				_, err := s.ForwardAsAnthropic(context.Background(), c, v10Account(), body, key, "")
				require.NoError(t, err)
				text := r.Body.String()
				text = strings.ReplaceAll(text, "gpt-4o", v10Model)
				got = append(got, text)
			}
			assert.Equal(t, got[0], got[1], "只读proof观测不能改变client bytes")
		})
	}
}

func TestV10CacheGrokIdentityLegacyUnchanged(t *testing.T) {
	body := v8CacheBody(t, 2, 1, false, true, false)
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(body, &req))
	legacy := deriveLegacyAnthropicCacheControlPromptCacheKey(&req)
	a := v10Account()
	a.Platform = PlatformGrok
	u := &httpUpstreamRecorder{resp: v10Response("synthetic-grok", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-ok"))}
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
	c, _ := v10Context(body, 10001)
	_, err := s.ForwardAsAnthropic(context.Background(), c, a, body, "", "")
	require.NoError(t, err)
	expected := generateSessionUUID(fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", 10001, v10Model, legacy))
	assert.Equal(t, expected, gjson.GetBytes(u.lastBody, "prompt_cache_key").String())
	assert.Empty(t, u.lastReq.Header.Get("session_id"))
}

func TestV10CacheProofInterruptedStreamDoesNotBind(t *testing.T) {
	body := proofRequest(t, []any{v10Message("user", "synthetic-root")}, true)
	resp := v10Response("synthetic-cut", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-partial"))
	b, _ := io.ReadAll(resp.Body)
	at := strings.Index(string(b), `data: {"type":"response.completed"`)
	resp.Body = io.NopCloser(strings.NewReader(string(b[:at]) + `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"synthetic-partial"}` + "\n\n"))
	u := &httpUpstreamRecorder{resp: resp}
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
	c, _ := v10Context(body, 10001)
	_, err := s.ForwardAsAnthropic(context.Background(), c, v10Account(), body, "synthetic-proof-session", "")
	require.Error(t, err)
	assert.Empty(t, s.loadOpenAICompatSessionBinding(c, v10Account(), "synthetic-proof-session").ResponseID)
}
