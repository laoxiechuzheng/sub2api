//go:build unit

package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCompositeForwardMappingRecomputedPerFailoverAccount(t *testing.T) {
	routingModel := "deepseek-v4.1-flash"
	accounts := []Account{
		{ID: 34, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{routingModel: routingModel}}},
		{ID: 54, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{routingModel: "deepseek-flash"}}},
	}
	for i := range accounts {
		billing, upstream := resolveOpenAIForwardMappedModels(&accounts[i], routingModel, false)
		expected := routingModel
		if accounts[i].ID == 54 {
			expected = "deepseek-flash"
		}
		require.Equal(t, expected, billing)
		require.Equal(t, expected, upstream)
		require.Equal(t, upstream, ResolveOpenAIAccountUpstreamModelForRequest(&accounts[i], routingModel, false))
	}
	require.Equal(t, "deepseek-v4.1-flash", routingModel, "account forwarding must not mutate the next account's routing model")
}
