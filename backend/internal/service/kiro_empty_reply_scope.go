package service

import (
	"sync"
	"time"
)

// Kiro 的转换器在上游空流或截断时也会补出 end_turn + message_stop，单看终态事件
// 分不清“这条请求的内容触发了空回复”与“账号对所有请求都返回空流”。这里按请求
// 指纹（模型 + 上游计入的输入 token）记录近期空回复：窗口内只有同一个指纹在重复
// 空回复时按请求级处理、不冷却账号；出现第二个不同指纹，视为账号级故障，保留冷却。
const kiroEmptyReplyWindow = 2 * time.Minute

type kiroEmptyReplyFingerprint struct {
	model       string
	inputTokens int
}

type kiroEmptyReplyEntry struct {
	fingerprint kiroEmptyReplyFingerprint
	at          time.Time
}

type kiroEmptyReplyTracker struct {
	mu        sync.Mutex
	byAccount map[int64][]kiroEmptyReplyEntry
	now       func() time.Time
}

var defaultKiroEmptyReplyTracker = newKiroEmptyReplyTracker(time.Now)

func newKiroEmptyReplyTracker(now func() time.Time) *kiroEmptyReplyTracker {
	return &kiroEmptyReplyTracker{byAccount: make(map[int64][]kiroEmptyReplyEntry), now: now}
}

// recordAndIsRequestScoped 记录一次空回复，返回它是否只来自这一条请求。
// 输入 token 未知（0）时无法区分请求，按账号级处理。
func (t *kiroEmptyReplyTracker) recordAndIsRequestScoped(accountID int64, model string, inputTokens int) bool {
	if inputTokens <= 0 {
		return false
	}
	fp := kiroEmptyReplyFingerprint{model: model, inputTokens: inputTokens}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	cutoff := now.Add(-kiroEmptyReplyWindow)
	// 顺带清理其他账号的过期记录，避免长期运行时 map 只增不减。
	for id, entries := range t.byAccount {
		if id != accountID && (len(entries) == 0 || !entries[len(entries)-1].at.After(cutoff)) {
			delete(t.byAccount, id)
		}
	}
	kept := t.byAccount[accountID][:0]
	scoped := true
	for _, e := range t.byAccount[accountID] {
		if !e.at.After(cutoff) {
			continue
		}
		kept = append(kept, e)
		if e.fingerprint != fp {
			scoped = false
		}
	}
	t.byAccount[accountID] = append(kept, kiroEmptyReplyEntry{fingerprint: fp, at: now})
	return scoped
}
