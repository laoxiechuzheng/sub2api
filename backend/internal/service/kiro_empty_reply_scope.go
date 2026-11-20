package service

import (
	"sync"
	"time"
)

// Kiro 的转换器在上游空流或截断时也会补出 end_turn + message_stop，单看终态事件
// 分不清“这条请求的内容触发了空回复”与“账号对所有请求都返回空流”。这里按账号记录
// 近期情况，满足任一条件即按请求级处理、不冷却账号：
//   - 窗口内该账号有过成功的可见回复：账号本身是好的，空回复只属于这条请求；
//   - 窗口内只有同一个请求指纹（模型 + 提示词大小）在重复空回复。
//
// 否则（窗口内没有成功、且出现了不同指纹的空回复）视为账号级故障，保留冷却。
const kiroEmptyReplyWindow = 2 * time.Minute

type kiroEmptyReplyFingerprint struct {
	model        string
	promptTokens int
}

type kiroEmptyReplyEntry struct {
	fingerprint kiroEmptyReplyFingerprint
	at          time.Time
}

type kiroEmptyReplyAccountState struct {
	empties     []kiroEmptyReplyEntry
	lastSuccess time.Time
}

type kiroEmptyReplyTracker struct {
	mu        sync.Mutex
	byAccount map[int64]*kiroEmptyReplyAccountState
	now       func() time.Time
}

var defaultKiroEmptyReplyTracker = newKiroEmptyReplyTracker(time.Now)

func newKiroEmptyReplyTracker(now func() time.Time) *kiroEmptyReplyTracker {
	return &kiroEmptyReplyTracker{byAccount: make(map[int64]*kiroEmptyReplyAccountState), now: now}
}

// recordSuccess 记录账号的一次成功可见回复。
func (t *kiroEmptyReplyTracker) recordSuccess(accountID int64) {
	if t == nil || accountID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stateLocked(accountID).lastSuccess = t.now()
}

// recordAndIsRequestScoped 记录一次空回复，返回它是否只属于这一条请求。
// promptTokens 是本次请求的提示词大小（同一请求的各次重试应一致）；未知（0）时
// 只能依据近期成功判断。
func (t *kiroEmptyReplyTracker) recordAndIsRequestScoped(accountID int64, model string, promptTokens int) bool {
	if t == nil || accountID <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	cutoff := now.Add(-kiroEmptyReplyWindow)
	t.pruneLocked(accountID, cutoff)
	state := t.stateLocked(accountID)
	kept := state.empties[:0]
	for _, e := range state.empties {
		if e.at.After(cutoff) {
			kept = append(kept, e)
		}
	}
	state.empties = kept

	if state.lastSuccess.After(cutoff) {
		if promptTokens > 0 {
			state.empties = append(state.empties, kiroEmptyReplyEntry{fingerprint: kiroEmptyReplyFingerprint{model, promptTokens}, at: now})
		}
		return true
	}
	if promptTokens <= 0 {
		return false
	}
	fp := kiroEmptyReplyFingerprint{model: model, promptTokens: promptTokens}
	scoped := true
	for _, e := range state.empties {
		if e.fingerprint != fp {
			scoped = false
			break
		}
	}
	state.empties = append(state.empties, kiroEmptyReplyEntry{fingerprint: fp, at: now})
	return scoped
}

func (t *kiroEmptyReplyTracker) stateLocked(accountID int64) *kiroEmptyReplyAccountState {
	state := t.byAccount[accountID]
	if state == nil {
		state = &kiroEmptyReplyAccountState{}
		t.byAccount[accountID] = state
	}
	return state
}

// pruneLocked 清理其他账号已过期的记录，避免长期运行时 map 只增不减。
func (t *kiroEmptyReplyTracker) pruneLocked(currentAccountID int64, cutoff time.Time) {
	for id, state := range t.byAccount {
		if id == currentAccountID {
			continue
		}
		lastEmpty := time.Time{}
		if n := len(state.empties); n > 0 {
			lastEmpty = state.empties[n-1].at
		}
		if !lastEmpty.After(cutoff) && !state.lastSuccess.After(cutoff) {
			delete(t.byAccount, id)
		}
	}
}
