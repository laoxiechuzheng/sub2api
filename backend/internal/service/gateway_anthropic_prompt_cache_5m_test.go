//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAnthropicAPIKeyPromptCache5mOnWire(t *testing.T) {
	const model = "claude-sonnet-5-5"
	body := []byte(`{"model":"claude-sonnet-5-5","cache_control":{"type":"ephemeral","ttl":"1h"},"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"used","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"5m"}},{"name":"deferred","defer_loading":true,"cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"question","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"uncached"}]}]}`)
	service := &GatewayService{cfg: &config.Config{}}
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"anthropic_prompt_cache_5m": true},
	}

	for _, mode := range []string{"ordinary", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var (
				req      *http.Request
				wireBody []byte
				err      error
			)
			if mode == "passthrough" {
				req, wireBody, err = service.buildUpstreamRequestAnthropicAPIKeyPassthrough(ctx, nil, account, body, "test-key")
			} else {
				req, wireBody, err = service.buildUpstreamRequest(ctx, nil, account, body, "test-key", "apikey", model, false, false)
			}
			require.NoError(t, err)
			defer req.Body.Close()
			sent, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, wireBody, sent)
			for _, path := range []string{"cache_control.ttl", "system.0.cache_control.ttl", "tools.0.cache_control.ttl", "messages.0.content.0.cache_control.ttl"} {
				require.Equal(t, "5m", gjson.GetBytes(sent, path).String(), path)
			}
			require.False(t, gjson.GetBytes(sent, "tools.1.cache_control").Exists())
			require.False(t, gjson.GetBytes(sent, "messages.0.content.1.cache_control").Exists())
			require.Equal(t, "question", gjson.GetBytes(sent, "messages.0.content.0.text").String())
			require.Equal(t, "1h", gjson.GetBytes(body, "cache_control.ttl").String(), "must not modify input")
		})
	}
}

func TestAnthropicAPIKeyPromptCache5mRequiresOptInAndExistingControl(t *testing.T) {
	const model = "claude-sonnet-5-5"
	cases := []struct {
		name     string
		platform string
		typeName string
		optIn    any
		token    string
		body     string
	}{
		{"default", PlatformAnthropic, AccountTypeAPIKey, nil, "apikey", `{"cache_control":{"type":"ephemeral","ttl":"1h"}}`},
		{"disabled", PlatformAnthropic, AccountTypeAPIKey, false, "apikey", `{"cache_control":{"type":"ephemeral","ttl":"1h"}}`},
		{"invalid extra", PlatformAnthropic, AccountTypeAPIKey, "true", "apikey", `{"cache_control":{"type":"ephemeral","ttl":"1h"}}`},
		{"OAuth", PlatformAnthropic, AccountTypeOAuth, true, "oauth", `{"cache_control":{"type":"ephemeral","ttl":"1h"}}`},
		{"other platform", PlatformDeepseek, AccountTypeAPIKey, true, "apikey", `{"cache_control":{"type":"ephemeral","ttl":"1h"}}`},
		{"no control", PlatformAnthropic, AccountTypeAPIKey, true, "apikey", `{"messages":[{"role":"user","content":"question"}]}`},
		{"non-ephemeral", PlatformAnthropic, AccountTypeAPIKey, true, "apikey", `{"cache_control":{"type":"other","ttl":"1h"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: tc.platform, Type: tc.typeName}
			if tc.optIn != nil {
				account.Extra = map[string]any{"anthropic_prompt_cache_5m": tc.optIn}
			}
			req, out, err := (&GatewayService{cfg: &config.Config{}}).buildUpstreamRequest(context.Background(), nil, account, []byte(tc.body), "test-key", tc.token, model, false, false)
			require.NoError(t, err)
			defer req.Body.Close()
			require.JSONEq(t, tc.body, string(out))
		})
	}
}
