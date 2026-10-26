package service

// HasObservedUsage reports whether a forward result contains upstream consumption.
func (r *OpenAIForwardResult) HasObservedUsage() bool {
	return r != nil && hasOpenAIPartialForwardUsage(&r.Usage, r.ImageCount)
}

// An observed billable result must reach the handler together with the error,
// including failures before visible output that would otherwise be replayed.
func hasOpenAIPartialForwardUsage(usage *OpenAIUsage, imageCount int) bool {
	return imageCount > 0 || openAIUsageHasTokens(usage) || (usage != nil && usage.ImageCacheReadTokens > 0)
}
