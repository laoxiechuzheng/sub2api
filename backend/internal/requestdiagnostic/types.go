// Package requestdiagnostic provides bounded HTTP request and response capture.
// 本包不读取 HTTP body，不持有 context，不处理读取权限、队列、存储和 TTL；
// 这些职责由调用方的 middleware、transport 和 service 实现。
package requestdiagnostic

import "time"

const (
	SchemaVersion           = 1
	CompressedSchemaVersion = 2
	DefaultMaxBytes         = 8 << 20
	DefaultMaxAttempts      = 16
	MaxResponseSummaryBytes = 64 << 10
	// Expansion resource guard; compressed storage has no 64 MiB plaintext cap.
	MaxExpandedBodyBytes = 512 << 20
)

const (
	CaptureStatusCaptured = "captured"
	CaptureStatusPartial  = "partial"
	CaptureStatusOmitted  = "omitted"
	CaptureStatusDropped  = "dropped"
)

// 固定省略原因供管理员 UI 使用，无需解析正文或保留任意错误文本。
const (
	OmittedNotCaptured        = "not_captured"
	OmittedEmptyBody          = "empty_body"
	OmittedInvalidJSON        = "invalid_json"
	OmittedInvalidUTF8        = "invalid_utf8"
	OmittedDepthLimit         = "depth_limit"
	OmittedNodeLimit          = "node_limit"
	OmittedBinaryData         = "binary_data"
	OmittedBudget             = "budget_exceeded"
	OmittedEvicted            = "evicted_for_latest_attempt"
	OmittedDropped            = "dropped"
	OmittedTooLarge           = "body_too_large"
	OmittedDecompressionLimit = "decompression_limit"
)

// Options 只控制单条采集，不控制 service 的全局并发或队列大小。
// 非正数采用默认值，MaxAttempts 始终不超过 16。
// OnFinish 在第一次 Finish 或 Discard 后锁外调用一次，即使没有 usage 绑定。
// 它适合触发轻量生命周期管理；onReady 是单独的同步交接回调。
type Options struct {
	MaxBytes    int
	MaxAttempts int
	PreserveRaw bool
	OnFinish    func()
}

// Payload.Content 是标准 JSON string 字段，不是 json.RawMessage。
// Content preserves the original text when PreserveRaw is enabled.
// Truncated content is a UTF-8-safe prefix and can be incomplete.
// 前缀自身可能不是有效 JSON，但外层 Snapshot 始终可正常序列化。
// StoredBytes counts ContentGzip when present, otherwise len(Content). V2
// compressed payloads declare UncompressedBytes; Get restores the V1 text API.
// 即使保留了其他内容，也会明确标记二进制内容或嵌套限制导致的局部省略。
type Payload struct {
	Content           string `json:"content"`
	ContentGzip       []byte `json:"content_gzip,omitempty"`
	UncompressedBytes int    `json:"uncompressed_bytes,omitempty"`
	OriginalBytes     int    `json:"original_bytes"`
	StoredBytes       int    `json:"stored_bytes"`
	Truncated         bool   `json:"truncated"`
	OmittedReason     string `json:"omitted_reason,omitempty"`
	Redacted          bool   `json:"redacted"`
}

// Inbound 只包含选定元数据，不包含原始 headers 或 cookies。
// RequestID 是 server/ops 关联 ID，ClientRequestID 是独立的客户端 ID；
// 两者都不能用于推断 canonical usage 绑定。
// Body 只作输出，必须通过 SetInbound/SetInboundOmitted 填充，构造参数中的 Body 会忽略。
// Endpoint 可以是路径或 URL，URL 凭据、query 和 fragment 会被移除。
type Inbound struct {
	RequestID       string    `json:"request_id"`
	ClientRequestID string    `json:"client_request_id"`
	Endpoint        string    `json:"endpoint"`
	Method          string    `json:"method"`
	StartedAt       time.Time `json:"started_at"`
	UserAgent       string    `json:"user_agent,omitempty"`
	Body            Payload   `json:"body"`
}

// Attempt 同时作为输入元数据和输出记录。BeginAttempt 只接收 AccountID、
// Platform、Endpoint、Method、Model 和 StartedAt，其余字段由 Capture 生成。
// ID 是单次采集内递增的 attempt 序号，不是 usage ID。
type Attempt struct {
	ID                  int64     `json:"id"`
	AccountID           int64     `json:"account_id"`
	Platform            string    `json:"platform"`
	Endpoint            string    `json:"endpoint"`
	Method              string    `json:"method"`
	Model               string    `json:"model,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at,omitempty"`
	DurationMs          int64     `json:"duration_ms"`
	Status              int       `json:"status"`
	Error               string    `json:"error,omitempty"`
	UpstreamRequestID   string    `json:"upstream_request_id,omitempty"`
	Body                Payload   `json:"body"`
	ResponseSummary     Payload   `json:"response_summary"`
	ResponseBody        *Payload  `json:"response_body,omitempty"`
	ResponseContentType string    `json:"response_content_type,omitempty"`
	MetadataRedacted    bool      `json:"metadata_redacted"`
	MetadataTruncated   bool      `json:"metadata_truncated"`
}

// AttemptResult 描述实际的上游 HTTP 结果。
// ResponseSummary contains bounded JSON metadata. ResponseBody contains the
// observed response bytes when PreserveRaw is enabled; it may be a prefix.
// EndAttempt 采用首次结果并以内置单调时钟计算 DurationMs。
type AttemptResult struct {
	Status                int
	Error                 string
	UpstreamRequestID     string
	ResponseSummary       []byte
	ResponseBody          []byte
	ResponseBodyGzip      []byte
	ResponseBodyBytes     int
	ResponseOmittedReason string
	ResponseBytes         int
	ResponseContentType   string
	ResponseIncomplete    bool
}

// UsageBinding 必须来自权威 usage 创建或计费路径。
// CanonicalID 对应 usage_logs.request_id，不是 ops UUID，也不能猜测或回填。
type UsageBinding struct {
	APIKeyID       int64     `json:"api_key_id"`
	CanonicalID    string    `json:"usage_request_id"`
	UsageCreatedAt time.Time `json:"usage_created_at"`
}

// Snapshot 是仅交给 onReady 一次的独立值，发布后 Capture 不再修改它。
// Callers must treat exported fields as read-only; payloads own their strings.
// StoredBytes 包含已保存正文和元数据字符串的字节数，以及精确 canonical 绑定；
// 固定 JSON 字段名、wire 转义、数字和时间字段不计入该预算。
// CapturedAt 是 Finish 时间，TTL/过期时间由 service 管理。
type Snapshot struct {
	SchemaVersion     int          `json:"schema_version"`
	Binding           UsageBinding `json:"binding"`
	Meta              Inbound      `json:"inbound"`
	Status            int          `json:"status"`
	Attempts          []Attempt    `json:"attempts"`
	CapturedAt        time.Time    `json:"captured_at"`
	CaptureStatus     string       `json:"capture_status"`
	DroppedAttempts   int          `json:"dropped_attempts"`
	StoredBytes       int          `json:"stored_bytes"`
	MetadataRedacted  bool         `json:"metadata_redacted"`
	MetadataTruncated bool         `json:"metadata_truncated"`
}
