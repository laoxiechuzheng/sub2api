//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV10CacheIdentityRollingThreeTurnsAndIsolation(t *testing.T) {
	makeBody := func(t *testing.T, round int) []byte {
		body := v8CacheBody(t, 1, 1, false, false, false)
		var req map[string]any
		require.NoError(t, json.Unmarshal(body, &req))
		messages := req["messages"].([]any)
		for n := 1; n < round; n++ {
			messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("synthetic-echo-%d", n), "cache_control": map[string]any{"type": "ephemeral"}}}}, v10Message("user", fmt.Sprintf("synthetic-next-%d", n)))
		}
		req["messages"] = messages
		return v10JSON(t, req)
	}
	var key, session string
	for round := 1; round <= 3; round++ {
		// 每轮用冷service，证明稳定key不依赖本地binding。
		b := makeBody(t, round)
		u := &httpUpstreamRecorder{resp: v10Response("synthetic", v10Model, `{"input_tokens":3,"output_tokens":5}`, v10TextOutput("synthetic-answer"))}
		s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: u}
		c, _ := v10Context(b, 10001)
		_, err := s.ForwardAsAnthropic(context.Background(), c, v10Account(), b, "", "")
		require.NoError(t, err)
		got := gjson.GetBytes(u.lastBody, "prompt_cache_key").String()
		sid := u.lastReq.Header.Get("session_id")
		if round == 1 {
			key, session = got, sid
		} else {
			assert.Equal(t, key, got)
			assert.Equal(t, session, sid)
		}
	}
	assert.True(t, strings.HasPrefix(key, "sub2api-pck-"))
	assert.NotEqual(t, isolatedCompatPromptCacheBodyKey(10001, "synthetic"), isolatedCompatPromptCacheBodyKey(10002, "synthetic"))
	assert.Equal(t, isolatedCompatPromptCacheBodyKey(10001, "synthetic"), isolatedCompatPromptCacheBodyKey(10001, "synthetic"), "跨实例确定性，无随机值")
}

func TestV10CacheDigestOnlyStripsKnownBlockControls(t *testing.T) {
	before := json.RawMessage(`[{"type":"tool_use","id":"synthetic","name":"tool","input":{"cache_control":"do-not-remove","nested":{"cache_control":1}},"cache_control":{"type":"ephemeral"}},{"type":"tool_result","tool_use_id":"synthetic","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]}]`)
	stripped := stripAnthropicBlockCacheControl(before)
	assert.False(t, gjson.GetBytes(stripped, "0.cache_control").Exists())
	assert.Equal(t, "do-not-remove", gjson.GetBytes(stripped, "0.input.cache_control").String())
	assert.Equal(t, int64(1), gjson.GetBytes(stripped, "0.input.nested.cache_control").Int())
	assert.False(t, gjson.GetBytes(stripped, "1.content.0.cache_control").Exists())
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(v8CacheBody(t, 1, 1, false, false, false), &req))
	legacyBefore := deriveLegacyAnthropicCacheControlPromptCacheKey(&req)
	stableBefore := deriveAnthropicCacheControlPromptCacheKey(&req)
	req.Messages = append(req.Messages, apicompat.AnthropicMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"synthetic-A","cache_control":{"type":"ephemeral"}}]`)})
	assert.Equal(t, stableBefore, deriveAnthropicCacheControlPromptCacheKey(&req))
	assert.NotEqual(t, legacyBefore, deriveLegacyAnthropicCacheControlPromptCacheKey(&req), "Grok旧算法仍有原行为，caller显式保留")
}
