package requestdiagnostic

import (
	"strings"
	"sync"
	"time"
)

// Capture 支持 gateway、transport 和 usage 记录路径的并发调用。
// 它只持有选定元数据和脱敏 string，不持有调用方字节或 context。
// 应先 EndAttempt 再 Finish，后者会冻结 HTTP 采集数据。
// 未绑定 usage 时 Finish 也会立即调用 Options.OnFinish；
// 确定不会产生 usage 时应调用 Discard，释放剩余的安全字符串。
type Capture struct {
	mu sync.Mutex

	maxBytes    int
	maxAttempts int
	meta        Inbound
	attempts    []attemptState
	nextID      int64
	binding     UsageBinding
	inboundSet  bool
	bound       bool
	finished    bool
	discarded   bool
	published   bool
	dropped     bool
	dropReason  string
	status      int
	capturedAt  time.Time

	droppedAttempts   int
	storedBytes       int
	metadataRedacted  bool
	metadataTruncated bool
	onReady           func(*Snapshot)
	onFinish          func()
}

type attemptState struct {
	Attempt
	begun time.Time
	ended bool
}

// NewCapture 只通过专用采集方法接收正文，忽略构造参数中的 Inbound.Body。
// Finish 和有效 BindUsage 都完成后，无论先后，锁外同步调用交接回调一次。
// Capture 自身不启动 goroutine，调用方的回调应保持轻量。
func NewCapture(options Options, inbound Inbound, onReady func(*Snapshot)) *Capture {
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	maxAttempts := options.MaxAttempts
	if maxAttempts <= 0 || maxAttempts > DefaultMaxAttempts {
		maxAttempts = DefaultMaxAttempts
	}
	meta, redacted, truncated := cleanInbound(inbound)
	c := &Capture{
		maxBytes:          maxBytes,
		maxAttempts:       maxAttempts,
		meta:              meta,
		metadataRedacted:  redacted,
		metadataTruncated: truncated,
		onReady:           onReady,
		onFinish:          options.OnFinish,
	}
	c.enforceBudgetLocked()
	return c
}

// MaxBytes 返回初始化后不变的预算，供调用方先限制正文复制量。
// nil Capture 返回零。
func (c *Capture) MaxBytes() int {
	if c == nil {
		return 0
	}
	return c.maxBytes
}

// SetInbound 记录首次入站正文，不修改或持有原始字节。
// 重复调用以及 Finish 之后的 HTTP 采集修改会被忽略。
func (c *Capture) SetInbound(raw []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.discarded || c.published || c.inboundSet {
		return
	}
	c.inboundSet = true
	if c.dropped {
		c.meta.Body = omittedPayload(len(raw), c.dropReason)
	} else if len(raw) > c.maxBytes {
		c.meta.Body = omittedPayload(len(raw), OmittedTooLarge)
	} else {
		c.meta.Body = sanitizePayload(raw, c.maxBytes)
	}
	c.enforceBudgetLocked()
}

// SetInboundOmitted 记录调用方未读取正文的原始长度和省略原因。
// 原因会归一为固定代码，不会原样保存任意文本。
func (c *Capture) SetInboundOmitted(originalBytes int, reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.discarded || c.published || c.inboundSet {
		return
	}
	c.inboundSet = true
	if c.dropped {
		reason = c.dropReason
	}
	c.meta.Body = omittedPayload(originalBytes, reason)
	c.enforceBudgetLocked()
}

// BeginAttempt 开始一次 transport attempt，返回生成的 ID；Finish/Discard 后返回零。
// 最多保留 MaxAttempts 条，达到上限时移除最旧 attempt，确保最新上游仍被记录。
func (c *Capture) BeginAttempt(metadata Attempt, raw []byte) int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.discarded || c.published {
		return 0
	}
	body := omittedPayload(len(raw), c.dropReason)
	if !c.dropped {
		if len(raw) > c.maxBytes {
			body = omittedPayload(len(raw), OmittedTooLarge)
		} else {
			body = sanitizePayload(raw, c.maxBytes)
		}
	}
	return c.beginAttemptLocked(metadata, body)
}

// BeginAttemptOmitted 记录 attempt 元数据和正文省略信息，无需读取请求正文。
func (c *Capture) BeginAttemptOmitted(metadata Attempt, originalBytes int, reason string) int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.discarded || c.published {
		return 0
	}
	if c.dropped {
		reason = c.dropReason
	}
	return c.beginAttemptLocked(metadata, omittedPayload(originalBytes, reason))
}

func (c *Capture) beginAttemptLocked(metadata Attempt, body Payload) int64 {
	c.nextID++
	started := time.Now()
	attempt := cleanAttempt(metadata, started)
	attempt.ID = c.nextID
	attempt.Body = body
	if c.dropped {
		attempt.ResponseSummary = omittedPayload(0, c.dropReason)
	}
	if len(c.attempts) == c.maxAttempts {
		copy(c.attempts, c.attempts[1:])
		c.attempts = c.attempts[:len(c.attempts)-1]
		c.droppedAttempts++
	}
	c.attempts = append(c.attempts, attemptState{Attempt: attempt, begun: started})
	c.enforceBudgetLocked()
	return attempt.ID
}

// EndAttempt 保存仍保留的 attempt 的首次结果。
// 已移除或未知 ID、重复结果和 Finish 之后的结果会被忽略。
// 响应摘要只接收 JSON，脱敏一次，不会逐 stream frame 重新 Marshal 大正文。
func (c *Capture) EndAttempt(id int64, result AttemptResult) {
	if c == nil || id <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.discarded || c.published {
		return
	}
	for i := range c.attempts {
		state := &c.attempts[i]
		if state.ID != id || state.ended {
			continue
		}
		state.ended = true
		ended := time.Now()
		state.FinishedAt = ended.UTC()
		state.DurationMs = ended.Sub(state.begun).Milliseconds()
		state.Status = result.Status
		state.Error, state.UpstreamRequestID, state.MetadataRedacted, state.MetadataTruncated = cleanResult(result, state.MetadataRedacted, state.MetadataTruncated)
		if c.dropped {
			state.ResponseSummary = omittedPayload(len(result.ResponseSummary), c.dropReason)
		} else if len(result.ResponseSummary) > c.maxBytes {
			state.ResponseSummary = omittedPayload(len(result.ResponseSummary), OmittedTooLarge)
		} else {
			state.ResponseSummary = sanitizePayload(result.ResponseSummary, min(c.maxBytes, MaxResponseSummaryBytes))
		}
		c.enforceBudgetLocked()
		return
	}
}

// BindUsage 绑定精确 canonical usage key，首次有效绑定生效。
// 重复相同绑定返回 true，冲突或无效绑定返回 false。
// 不会回退使用 RequestID、ClientRequestID 或 ops UUID；
// 无法在预算内安全、原样保存的绑定会被拒绝，而不是截断或改写。
func (c *Capture) BindUsage(apiKeyID int64, canonicalID string, usageCreatedAt time.Time) bool {
	if c == nil || apiKeyID <= 0 || strings.TrimSpace(canonicalID) == "" || usageCreatedAt.IsZero() {
		return false
	}
	c.mu.Lock()
	if c.discarded || !validCanonicalID(canonicalID, c.maxBytes) {
		c.mu.Unlock()
		return false
	}
	if c.bound {
		matches := c.binding.APIKeyID == apiKeyID && c.binding.CanonicalID == canonicalID && c.binding.UsageCreatedAt.Equal(usageCreatedAt)
		c.mu.Unlock()
		return matches
	}
	c.binding = UsageBinding{APIKeyID: apiKeyID, CanonicalID: strings.Clone(canonicalID), UsageCreatedAt: usageCreatedAt.UTC()}
	c.bound = true
	c.enforceBudgetLocked()
	callback, snapshot := c.readyLocked()
	c.mu.Unlock()
	if callback != nil {
		callback(snapshot)
	}
	return true
}

// Finish 冻结入站和 attempt 数据，即使没有 usage 也调用 OnFinish 一次。
// onReady 握手可在这里完成，也可在之后的 BindUsage 完成。
func (c *Capture) Finish(status int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finished || c.discarded || c.published {
		c.mu.Unlock()
		return
	}
	c.finished = true
	c.status = status
	c.capturedAt = time.Now().UTC()
	onFinish := c.onFinish
	c.onFinish = nil
	callback, snapshot := c.readyLocked()
	c.mu.Unlock()
	if onFinish != nil {
		onFinish()
	}
	if callback != nil {
		callback(snapshot)
	}
}

// Drop 删除正文和响应摘要，禁止继续采集内容，但保留安全元数据和 Finish/Bind 握手。
// 可用于过载降级；未知原因归一为固定代码 "dropped"。
func (c *Capture) Drop(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discarded || c.published || c.dropped {
		return
	}
	c.dropped = true
	c.dropReason = normalizeReason(reason, OmittedDropped)
	c.meta.Body = droppedPayload(c.meta.Body, c.dropReason)
	for i := range c.attempts {
		c.attempts[i].Body = droppedPayload(c.attempts[i].Body, c.dropReason)
		c.attempts[i].ResponseSummary = droppedPayload(c.attempts[i].ResponseSummary, c.dropReason)
	}
	c.enforceBudgetLocked()
}

// Discard 终止确定没有持久化 usage 的采集，清空内容和回调。
// 若 Finish 尚未触发 OnFinish，则在此调用一次；已经发布的 Snapshot 无法撤回。
func (c *Capture) Discard() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.discarded || c.published {
		c.mu.Unlock()
		return
	}
	c.discarded = true
	c.meta = Inbound{}
	c.attempts = nil
	c.binding = UsageBinding{}
	c.storedBytes = 0
	c.onReady = nil
	onFinish := c.onFinish
	c.onFinish = nil
	c.mu.Unlock()
	if onFinish != nil {
		onFinish()
	}
}

func (c *Capture) readyLocked() (func(*Snapshot), *Snapshot) {
	if !c.finished || !c.bound || c.discarded || c.published {
		return nil, nil
	}
	c.enforceBudgetLocked()
	attempts := make([]Attempt, len(c.attempts))
	for i := range c.attempts {
		attempts[i] = c.attempts[i].Attempt
	}
	snapshot := &Snapshot{
		SchemaVersion:     SchemaVersion,
		Binding:           c.binding,
		Meta:              c.meta,
		Status:            c.status,
		Attempts:          attempts,
		CapturedAt:        c.capturedAt,
		CaptureStatus:     c.captureStatusLocked(),
		DroppedAttempts:   c.droppedAttempts,
		StoredBytes:       c.storedBytes,
		MetadataRedacted:  c.metadataRedacted,
		MetadataTruncated: c.metadataTruncated,
	}
	callback := c.onReady
	c.onReady = nil
	c.published = true
	// 安全字符串移交给回调，不让仍持有 Capture 的请求或 usage worker 保留第二份正文。
	c.meta = Inbound{}
	c.attempts = nil
	return callback, snapshot
}

func (c *Capture) captureStatusLocked() string {
	if c.dropped {
		return CaptureStatusDropped
	}
	present := c.meta.Body.StoredBytes > 0
	partial := c.metadataTruncated || c.droppedAttempts > 0 || incompletePayload(c.meta.Body)
	for i := range c.attempts {
		a := &c.attempts[i]
		present = present || a.Body.StoredBytes > 0 || a.ResponseSummary.StoredBytes > 0
		partial = partial || a.MetadataTruncated || incompletePayload(a.Body)
		if a.ResponseSummary.OmittedReason != OmittedEmptyBody && a.ResponseSummary.OmittedReason != OmittedNotCaptured {
			partial = partial || incompletePayload(a.ResponseSummary)
		}
	}
	if !present {
		return CaptureStatusOmitted
	}
	if partial {
		return CaptureStatusPartial
	}
	return CaptureStatusCaptured
}

func incompletePayload(p Payload) bool {
	return p.Truncated || (p.OmittedReason != "" && p.OmittedReason != OmittedEmptyBody)
}

func (c *Capture) enforceBudgetLocked() {
	// 元数据只占有限份额，避免超长错误或 URL 挤掉优先保留的两份请求正文。
	remaining := c.maxBytes - len(c.binding.CanonicalID)
	metadataBudget := min(64<<10, remaining/8)
	metadataUsed := c.limitMetadataLocked(metadataBudget)
	remaining -= metadataUsed
	c.storedBytes = len(c.binding.CanonicalID) + metadataUsed

	latest := (*Payload)(nil)
	if len(c.attempts) > 0 {
		latest = &c.attempts[len(c.attempts)-1].Body
	}
	inboundAllowance, latestAllowance := priorityAllowances(c.meta.Body.StoredBytes, payloadBytes(latest), remaining)
	limitPayload(&c.meta.Body, inboundAllowance, OmittedBudget)
	if latest != nil {
		limitPayload(latest, latestAllowance, OmittedBudget)
	}
	used := c.meta.Body.StoredBytes + payloadBytes(latest)
	remaining -= used
	c.storedBytes += used

	// 旧正文仅在能完整放入时保留，否则真正释放字符串并明确标记省略。
	for i := len(c.attempts) - 2; i >= 0; i-- {
		body := &c.attempts[i].Body
		if body.StoredBytes > remaining {
			*body = droppedPayload(*body, OmittedEvicted)
		}
		remaining -= body.StoredBytes
		c.storedBytes += body.StoredBytes
	}
	for i := len(c.attempts) - 1; i >= 0; i-- {
		summary := &c.attempts[i].ResponseSummary
		limitPayload(summary, remaining, OmittedBudget)
		remaining -= summary.StoredBytes
		c.storedBytes += summary.StoredBytes
	}
}

func (c *Capture) limitMetadataLocked(maxBytes int) int {
	remaining := max(0, maxBytes)
	limit := func(value *string, truncated *bool) {
		if len(*value) > remaining {
			*value = utf8Prefix(*value, remaining)
			*truncated = true
		}
		remaining -= len(*value)
	}
	for _, field := range []*string{&c.meta.RequestID, &c.meta.ClientRequestID, &c.meta.Endpoint, &c.meta.Method, &c.meta.UserAgent} {
		limit(field, &c.metadataTruncated)
	}
	for i := len(c.attempts) - 1; i >= 0; i-- {
		a := &c.attempts[i]
		for _, field := range []*string{&a.Platform, &a.Endpoint, &a.Method, &a.Model, &a.UpstreamRequestID, &a.Error} {
			limit(field, &a.MetadataTruncated)
		}
	}
	return max(0, maxBytes) - remaining
}

func priorityAllowances(inboundBytes, latestBytes, available int) (int, int) {
	if inboundBytes+latestBytes <= available {
		return inboundBytes, latestBytes
	}
	inbound := (available + 1) / 2
	latest := available / 2
	if inboundBytes < inbound {
		return inboundBytes, available - inboundBytes
	}
	if latestBytes < latest {
		return available - latestBytes, latestBytes
	}
	return inbound, latest
}

func payloadBytes(payload *Payload) int {
	if payload == nil {
		return 0
	}
	return payload.StoredBytes
}

func limitPayload(payload *Payload, maxBytes int, reason string) {
	if payload.StoredBytes <= max(0, maxBytes) {
		return
	}
	payload.Content = utf8Prefix(payload.Content, maxBytes)
	payload.StoredBytes = len(payload.Content)
	payload.Truncated = true
	if payload.OmittedReason == "" {
		payload.OmittedReason = reason
	}
}
