package kiro

import "testing"

func TestApplyKiroUpstreamCacheUsage(t *testing.T) {
	ctx := KiroRequestContext{CacheFollowsUpstream: true, PromptCache: KiroPromptCacheHint{Resolved: true, CachedTokens: 150000}}

	// credits 校验过的命中量优先于推断值。
	got := applyKiroUpstreamCacheUsage(Usage{OutputTokens: 500, CacheCreationInputTokens: 9}, ctx,
		KiroOutputEstimate{Method: KiroOutputMethodCredits, CachedPromptTokens: 140000}, 177000)
	if got.CacheReadInputTokens != 140000 || got.InputTokens != 37000 || got.CacheCreationInputTokens != 0 || got.TotalTokens != 177500 {
		t.Fatalf("credits path: %+v", got)
	}

	// 没有 credits 结果时用推断值；命中量不超过提示词。
	got = applyKiroUpstreamCacheUsage(Usage{}, ctx, KiroOutputEstimate{}, 100000)
	if got.CacheReadInputTokens != 100000 || got.InputTokens != 0 {
		t.Fatalf("hint clamp: %+v", got)
	}

	// 未解析：全部按普通输入。
	got = applyKiroUpstreamCacheUsage(Usage{}, KiroRequestContext{CacheFollowsUpstream: true}, KiroOutputEstimate{}, 5000)
	if got.CacheReadInputTokens != 0 || got.InputTokens != 5000 {
		t.Fatalf("unresolved: %+v", got)
	}
}
