//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV10CacheFourHandlersSameBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, usage                     string
		input, read, write, short, long int
	}{
		{"aliases", `{"input_tokens":123,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}`, 3, 100, 20, 0, 0},
		{"fieldwise", `{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cache_write_tokens":2},"prompt_tokens_details":{"cached_tokens":5}}`, 3, 5, 2, 0, 0},
		{"negative", `{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":-3,"cache_write_tokens":-2}}`, 10, 0, 0, 0, 0},
		{"ttl", `{"input_tokens":123,"output_tokens":5,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":20,"cache_write_tokens_details":{"ttl_30m_tokens":7,"ttl_12h_tokens":13}}}`, 3, 100, 20, 7, 13},
	} {
		for _, route := range []string{"messages_json", "messages_sse", "chat_json", "chat_sse"} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				repo := &openAIRecordUsageLogRepoStub{inserted: true}
				s := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
				resp := v10Response("synthetic-cache-handlers", v10Model, tc.usage, v10TextOutput("synthetic-ok"))
				r := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(r)
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				var result *OpenAIForwardResult
				var err error
				switch route {
				case "messages_json":
					result, err = s.handleAnthropicBufferedStreamingResponse(resp, c, v10Account(), v10Model, v10Model, v10Model, time.Now())
				case "messages_sse":
					result, err = s.handleAnthropicStreamingResponse(resp, c, v10Account(), v10Model, v10Model, v10Model, time.Now())
				case "chat_json":
					result, err = s.handleChatBufferedStreamingResponse(resp, c, v10Account(), v10Model, v10Model, v10Model, time.Now())
				case "chat_sse":
					result, err = s.handleChatStreamingResponse(resp, c, v10Account(), v10Model, v10Model, v10Model, time.Now(), 0)
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, tc.input+tc.read+tc.write, result.Usage.InputTokens)
				assert.Equal(t, 5, result.Usage.OutputTokens)
				assert.Equal(t, tc.read, result.Usage.CacheReadInputTokens)
				assert.Equal(t, tc.write, result.Usage.CacheCreationInputTokens)
				assert.Equal(t, tc.short, result.Usage.CacheCreation30mInputTokens)
				assert.Equal(t, tc.long, result.Usage.CacheCreation12hInputTokens)
				if strings.HasPrefix(route, "messages") {
					u := v8CacheClientUsage(t, r, strings.HasSuffix(route, "sse"))
					assert.Equal(t, int64(tc.input), u.Get("input_tokens").Int())
					assert.Equal(t, int64(tc.read), u.Get("cache_read_input_tokens").Int())
					assert.Equal(t, int64(tc.write), u.Get("cache_creation_input_tokens").Int())
				} else {
					var u gjson.Result
					if strings.HasSuffix(route, "json") {
						u = gjson.GetBytes(r.Body.Bytes(), "usage")
					} else {
						for _, line := range strings.Split(r.Body.String(), "\n") {
							if strings.HasPrefix(line, "data: ") {
								value := gjson.Parse(strings.TrimPrefix(line, "data: "))
								if value.Get("usage").Exists() {
									u = value.Get("usage")
								}
							}
						}
					}
					require.True(t, u.Exists(), fmt.Sprintf("missing actual Chat usage %s", route))
					assert.Equal(t, int64(tc.input+tc.read+tc.write), u.Get("prompt_tokens").Int())
					assert.Equal(t, int64(tc.read), u.Get("prompt_tokens_details.cached_tokens").Int())
				}
				require.NoError(t, s.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 10001}, User: &User{ID: 10001}, Account: v10Account()}))
				require.NotNil(t, repo.lastLog)
				assert.Equal(t, tc.input, repo.lastLog.InputTokens)
				assert.Equal(t, tc.read, repo.lastLog.CacheReadTokens)
				assert.Equal(t, tc.write, repo.lastLog.CacheCreationTokens)
				assert.Equal(t, tc.short, repo.lastLog.CacheCreation30mTokens)
				assert.Equal(t, tc.long, repo.lastLog.CacheCreation12hTokens)
			})
		}
	}
}
