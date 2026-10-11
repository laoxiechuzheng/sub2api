package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0215MergeResponsesCompatibilityOptions(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5","store":false,"parallel_tool_calls":false,
		"input":[
			{"type":"compaction_trigger"},
			{"type":"message","id":"invalid-message-id","role":"user","content":"continue"},
			{"type":"web_search_call","id":"ws_merge","status":"completed","action":{"type":"search","query":"test"}},
			{"type":"reasoning","id":"rs_merge","encrypted_content":null,"summary":[],"content":[{"type":"reasoning_text","text":"keep replay"}],"opaque":9007199254740993}
		],
		"tools":[{"type":"function","name":"lookup","parameters":{"type":null,"properties":{}}}],
		"text":{"format":{"type":"json_schema","name":"reply","schema":{"type":null,"properties":{"answer":{"type":"string"}},"minProperties":1}}}
	}`)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, compact := range []bool{false, true} {
			for _, lite := range []bool{false, true} {
				for _, preserve := range []bool{false, true} {
					name := fmt.Sprintf("%s/compact=%t/lite=%t/preserve=%t", accountType, compact, lite, preserve)
					t.Run(name, func(t *testing.T) {
						account := &Account{Platform: PlatformOpenAI, Type: accountType}
						out, changed, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, account, openAIResponsesCompatibilityOptions{
							ResponsesLite:           lite,
							Compact:                 compact,
							PreserveReasoningReplay: preserve,
						})
						require.NoError(t, err)
						require.True(t, changed)

						reasoning := gjson.GetBytes(out, `input.#(type=="reasoning")`)
						if preserve {
							require.Equal(t, "keep replay", reasoning.Get("content.0.text").String())
							require.Equal(t, "rs_merge", reasoning.Get("id").String())
							require.Equal(t, "null", reasoning.Get("encrypted_content").Raw)
							require.Equal(t, "9007199254740993", reasoning.Get("opaque").Raw)
						} else {
							require.False(t, reasoning.Get("content").Exists())
							require.Equal(t, accountType == AccountTypeOAuth, reasoning.Exists())
						}

						// Replay preservation must not bypass ID, schema, tool, or trigger normalization.
						require.False(t, gjson.GetBytes(out, "input.0.id").Exists())
						require.Equal(t, "message", gjson.GetBytes(out, "input.0.type").String())
						require.Equal(t, "object", gjson.GetBytes(out, "tools.0.parameters.type").String())
						require.Equal(t, "object", gjson.GetBytes(out, "text.format.schema.type").String())
						require.False(t, gjson.GetBytes(out, "text.format.schema.minProperties").Exists())
						require.Equal(t, gjson.False, gjson.GetBytes(out, "parallel_tool_calls").Type)
						items := gjson.GetBytes(out, "input").Array()
						require.Equal(t, "compaction_trigger", items[len(items)-1].Get("type").String())

						injectWebSearch := accountType == AccountTypeOAuth && !compact
						require.Equal(t, injectWebSearch && !lite, gjsonToolsContainWebSearch(gjson.GetBytes(out, "tools")))
						additionalTools := gjson.GetBytes(out, `input.#(type=="additional_tools").tools`)
						require.Equal(t, injectWebSearch && lite, gjsonToolsContainWebSearch(additionalTools))
						require.Equal(t, "ws_merge", gjson.GetBytes(out, `input.#(type=="web_search_call").id`).String())
					})
				}
			}
		}
	}
}
