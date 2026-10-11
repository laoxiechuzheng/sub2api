//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0215MergeCompactionMappedModelProtocols(t *testing.T) {
	body := []byte(`{"model":"compact-alias","input":[{"type":"compaction_trigger"}]}`)
	for _, platform := range []string{PlatformOpenCodeGo, PlatformCommandCode} {
		for _, protocol := range []string{APIProtocolAnthropic, APIProtocolChatCompletions, APIProtocolResponses} {
			t.Run(platform+"/"+protocol, func(t *testing.T) {
				account := routingTestAccount(platform, AccountTypeAPIKey, map[string]any{
					"api_protocol":  APIProtocolAdaptive,
					"model_mapping": map[string]any{"compact-alias": "mapped-compact-model"},
					"protocol_rules": []any{
						map[string]any{"pattern": "mapped-compact-model", "protocol": protocol},
					},
				}, nil)
				require.Equal(t, "mapped-compact-model", upstreamRoutingModel(account, body, ""))
				require.Equal(t, protocol == APIProtocolResponses, CanForwardOpenAICompaction(account, body))
			})
		}
	}
}

func TestV0215MergeCompactionProviderBoundaries(t *testing.T) {
	body := []byte(`{"model":"compact-alias","input":[{"type":"compaction_trigger"}]}`)
	mapped := func(platform, model string) *Account {
		return routingTestAccount(platform, AccountTypeAPIKey, map[string]any{
			"api_protocol":  APIProtocolAdaptive,
			"model_mapping": map[string]any{"compact-alias": model},
		}, nil)
	}
	for _, tc := range []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil", nil, false},
		{"grok dedicated bridge", &Account{Platform: PlatformGrok}, true},
		{"opencode mapped anthropic", mapped(PlatformOpenCodeGo, "minimax-m3"), false},
		{"opencode mapped responses", mapped(PlatformOpenCodeGo, "gpt-5.5"), true},
		{"commandcode mapped anthropic", mapped(PlatformCommandCode, "claude-sonnet-4-6"), false},
		{"commandcode mapped responses", mapped(PlatformCommandCode, "gpt-5.5"), true},
		{"cline adaptive chat only", mapped(PlatformCline, "gpt-5.5"), false},
		{"kimi adaptive responses", mapped(PlatformKimi, "kimi-k2"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, CanForwardOpenAICompaction(tc.account, body))
		})
	}
}

func TestV0215MergeCompactionRejectsCatalogConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{APIProtocolAnthropic, APIProtocolChatCompletions} {
		for _, tc := range []struct {
			name   string
			path   string
			body   string
			marked bool
		}{
			{"legacy compact", "/v1/responses/compact", `{"model":"compact-alias","input":"summarize","stream":false}`, false},
			{"native trigger", "/v1/responses", `{"model":"compact-alias","stream":true,"input":[{"type":"message","role":"user","content":"summarize"},{"type":"compaction_trigger"}]}`, false},
			{"native marker", "/v1/responses", `{"model":"compact-alias","input":"summarize","stream":true}`, true},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				body := []byte(tc.body)
				account := commandCodeTestAccount(time.Now().UnixNano())
				account.Credentials["model_mapping"] = map[string]any{"compact-alias": "gpt-5.5"}
				require.True(t, CanForwardOpenAICompaction(account, body), "the configured GPT rule initially permits Responses")

				catalogURL := buildOpenAIModelsURL(account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
				catalogKey := modelProtocolCatalogKey(account, catalogURL)
				upstreamModelProtocols.store(catalogKey, map[string][]string{"gpt-5.5": {protocol}}, nil, time.Now())
				t.Cleanup(func() {
					upstreamModelProtocols.mu.Lock()
					defer upstreamModelProtocols.mu.Unlock()
					delete(upstreamModelProtocols.entries, catalogKey)
				})

				upstream := &httpUpstreamRecorder{err: errors.New("unexpected protocol conversion request")}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				require.Equal(t, protocol, svc.resolveUpstreamProtocolFor(context.Background(), account, APIProtocolResponses, upstreamRoutingModel(account, body, "")))
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
				if tc.marked {
					MarkOpenAINativeCompactionV2(c)
				}

				result, err := svc.Forward(context.Background(), c, account, body)
				require.ErrorContains(t, err, "compaction cannot be converted")
				require.Nil(t, result)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Equal(t, "compact_not_supported", gjson.Get(recorder.Body.String(), "error.type").String())
				require.Empty(t, upstream.requests, "catalog routing must not send compaction to a conversion endpoint")
			})
		}
	}
}
