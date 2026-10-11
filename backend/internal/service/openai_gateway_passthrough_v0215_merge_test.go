//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestV0215MergeOAuthPassthroughCompactDoesNotInjectWebSearch(t *testing.T) {
	for _, lite := range []bool{false, true} {
		t.Run(fmt.Sprintf("lite=%t", lite), func(t *testing.T) {
			setup := newAstraOAuthSetup(t, true)
			setup.c.Request.URL.Path = "/v1/responses/compact"
			if lite {
				setup.c.Request.Header.Set(responsesLiteHeader, "true")
			}
			setup.upstream.resp = &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"cmp_merge","usage":{"input_tokens":3,"output_tokens":4}}`)),
			}
			body, err := sjson.SetRawBytes([]byte(openAIWebSearchHistoryCompactionBody), "input.-1", []byte(`{"type":"reasoning","id":"rs_merge","summary":[],"content":[{"type":"reasoning_text","text":"remove replay"}]}`))
			require.NoError(t, err)

			result, err := setup.svc.Forward(context.Background(), setup.c, setup.account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.Stream)
			require.NotNil(t, setup.upstream.lastReq)
			require.Equal(t, "/backend-api/codex/responses/compact", setup.upstream.lastReq.URL.Path)
			out := setup.upstream.lastBody
			require.False(t, gjson.GetBytes(out, "stream").Exists())
			require.False(t, gjson.GetBytes(out, "store").Exists())
			require.False(t, gjsonToolsContainWebSearch(gjson.GetBytes(out, "tools")))
			for _, item := range gjson.GetBytes(out, "input").Array() {
				require.False(t, gjsonToolsContainWebSearch(item.Get("tools")))
			}
			require.Equal(t, "ws_123", gjson.GetBytes(out, `input.#(type=="web_search_call").id`).String())
			require.False(t, gjson.GetBytes(out, `input.#(type=="reasoning").content`).Exists())
		})
	}
}

func TestV0215MergeOpenRouterPassthroughPreservesReplayAndExplicitTTL(t *testing.T) {
	svc, upstream := newOpenRouterReasoningReplayTestService()
	account := newOpenRouterReasoningReplayTestAccount("https://openrouter.ai/api", map[string]any{"openai_passthrough": true})
	body, err := sjson.SetRawBytes([]byte(openRouterReasoningReplayTestBody), "cache_control", []byte(`{"type":"ephemeral","ttl":"1h"}`))
	require.NoError(t, err)
	body, err = sjson.SetRawBytes(body, "tools.0.parameters.type", []byte("null"))
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), newOpenRouterReasoningReplayTestContext(t), account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/api/v1/responses", upstream.lastReq.URL.Path)
	require.Equal(t, "snapshot reasoning", gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String())
	require.Equal(t, "null", gjson.GetBytes(upstream.lastBody, "input.1.encrypted_content").Raw)
	require.Equal(t, "rs_or_1", gjson.GetBytes(upstream.lastBody, "input.1.id").String())
	require.Equal(t, "object", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.type").String())
	require.Equal(t, "1h", gjson.GetBytes(upstream.lastBody, "cache_control.ttl").String())
}
