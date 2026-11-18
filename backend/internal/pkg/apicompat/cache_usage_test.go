package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV10CacheUsageCanonicalFieldsOnly(t *testing.T) {
	for _, tc := range []struct {
		name, raw                string
		read, write, short, long int
	}{
		{"alias", `{"input_tokens":123,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}`, 100, 20, 0, 0},
		{"fieldwise", `{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cache_write_tokens":2},"prompt_tokens_details":{"cached_tokens":5}}`, 5, 2, 0, 0},
		{"zero", `{"input_tokens":10,"cache_read_input_tokens":9,"cache_creation_input_tokens":9,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, 0, 0, 0, 0},
		{"null", `{"input_tokens":10,"cache_read_input_tokens":9,"cache_creation_input_tokens":9,"input_tokens_details":{"cached_tokens":null,"cache_write_tokens":null}}`, 0, 0, 0, 0},
		{"negative", `{"input_tokens":10,"cache_read_input_tokens":9,"cache_creation_input_tokens":9,"input_tokens_details":{"cached_tokens":-3,"cache_write_tokens":-2}}`, 0, 0, 0, 0},
		{"alias_priority", `{"input_tokens":20,"cache_read_input_tokens":-1,"cache_read_tokens":4,"cached_tokens":8,"cache_write_tokens":3,"cache_creation_input_tokens":9}`, 4, 3, 0, 0},
		{"ttl", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":7}}}`, 0, 10, 3, 7},
		{"ttl_partial", `{"input_tokens":20,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_12h_tokens":10}}}`, 0, 10, 0, 0},
		{"duplicate_unsupported_cache_first", `{"input_tokens":10,"input_tokens":11,"input_tokens_details":{"cached_tokens":1,"cached_tokens":7,"cache_write_tokens":3,"cache_write_tokens":9}}`, 1, 3, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := CacheUsageFromGJSON(gjson.Parse(tc.raw))
			assert.Equal(t, CacheUsage{Read: tc.read, Write: tc.write, TTL30m: tc.short, TTL12h: tc.long}, cache)
			var typed ResponsesUsage
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &typed))
			read := 0
			if typed.InputTokensDetails != nil {
				read = typed.InputTokensDetails.CachedTokens
			}
			assert.Equal(t, tc.read, read)
			assert.Equal(t, tc.write, typed.CacheCreationInputTokens)
			assert.Equal(t, tc.short, typed.CacheCreation30mInputTokens)
			assert.Equal(t, tc.long, typed.CacheCreation12hInputTokens)
			if tc.name == "duplicate_unsupported_cache_first" {
				assert.Equal(t, 11, typed.InputTokens, "不改非cache字段原typed末值行为")
			}
			encoded, err := json.Marshal(typed)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "CacheCreation30mInputTokens")
			assert.NotContains(t, string(encoded), "CacheCreation12hInputTokens")
		})
	}
	for _, bad := range []string{`{"input_tokens":10,"input_tokens_details":{"cached_tokens":2.5}}`, `{"input_tokens":10,"input_tokens_details":{"cached_tokens":"2"}}`, `{"input_tokens":"10"}`} {
		var typed ResponsesUsage
		assert.Error(t, json.Unmarshal([]byte(bad), &typed), "原typed非法数值拒绝不放宽")
	}
}
