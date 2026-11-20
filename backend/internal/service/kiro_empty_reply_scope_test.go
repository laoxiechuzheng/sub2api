//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKiroEmptyReplyTrackerScopesRepeatedSameRequest(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 50, 0, 0, time.UTC)
	tr := newKiroEmptyReplyTracker(func() time.Time { return now })

	// 生产实测：同一条请求（72795 输入 token）反复空回复 → 始终请求级，不冷却账号。
	for i := 0; i < 8; i++ {
		require.True(t, tr.recordAndIsRequestScoped(69, "claude-opus-5-5", 72795))
		now = now.Add(10 * time.Second)
	}
	// 另一个账号互不影响。
	require.True(t, tr.recordAndIsRequestScoped(70, "claude-opus-5-5", 1000))
}

func TestKiroEmptyReplyTrackerTreatsDifferentRequestsAsAccountFailure(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 50, 0, 0, time.UTC)
	tr := newKiroEmptyReplyTracker(func() time.Time { return now })

	require.True(t, tr.recordAndIsRequestScoped(69, "claude-opus-5-5", 72795))
	now = now.Add(5 * time.Second)
	// 窗口内第二个不同请求也空回复 → 账号级故障，保留冷却。
	require.False(t, tr.recordAndIsRequestScoped(69, "claude-opus-5-5", 1200))
	require.False(t, tr.recordAndIsRequestScoped(69, "claude-sonnet-5-5", 72795))

	// 窗口过后重新按请求级判断。
	now = now.Add(kiroEmptyReplyWindow + time.Second)
	require.True(t, tr.recordAndIsRequestScoped(69, "claude-opus-5-5", 5000))
}

func TestKiroEmptyReplyTrackerUnknownInputIsAccountFailure(t *testing.T) {
	tr := newKiroEmptyReplyTracker(time.Now)
	// 上游空流连 message_start 的输入用量都没有，无法区分请求 → 按账号级处理。
	require.False(t, tr.recordAndIsRequestScoped(69, "claude-opus-5-5", 0))
}

func TestKiroEmptyReplyTrackerPrunesIdleAccounts(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 50, 0, 0, time.UTC)
	tr := newKiroEmptyReplyTracker(func() time.Time { return now })
	tr.recordAndIsRequestScoped(1, "m", 10)
	now = now.Add(kiroEmptyReplyWindow + time.Second)
	tr.recordAndIsRequestScoped(2, "m", 10)
	_, stale := tr.byAccount[1]
	require.False(t, stale)
}
