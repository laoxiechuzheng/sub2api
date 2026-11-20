package kiro

import (
	"encoding/json"
	"strings"
)

// KiroUpstreamException 是 Kiro 在事件流中下发的失败：:message-type 为 exception
// （类型在 :exception-type）或 error（类型在 :error-code）的帧。这类帧不带
// :event-type，以前被当成无关事件丢掉，客户端拿到的是 HTTP 200 的空回复。
type KiroUpstreamException struct {
	Type    string // 如 ValidationException、ThrottlingException；error 帧为 error code
	Message string
	// Payload 是帧载荷原文（通常是 {"message":...,"reason":...}），调用方据此按 HTTP 错误
	// 体复用现有分类；超过 kiroUpstreamExceptionPayloadLimit 时截断。
	Payload []byte
	// PartialUsage 只在流式转换已向客户端输出后才有：截至异常的用量（输入估算与缓存模拟、
	// 已输出内容的本地计数、已收到的 credits）。此时不能换号，调用方按流中断记录这部分用量。
	PartialUsage *Usage
}

func (e *KiroUpstreamException) Error() string {
	if e.Message == "" {
		return "kiro upstream exception: " + e.Type
	}
	return "kiro upstream exception: " + e.Type + ": " + e.Message
}

// KiroContentLengthExceededException 表示输出达到上限：按 stop_reason max_tokens 正常收尾，不当失败。
const KiroContentLengthExceededException = "ContentLengthExceededException"

// KiroContentFilteredException 表示上游以正常事件帧下发的"内容审核拒绝"：帧里带
// stopReason=CONTENT_FILTERED 与 stopDetails.refusal.explanation，且没有任何正文。
const KiroContentFilteredException = "ContentFilteredException"

const kiroUpstreamExceptionPayloadLimit = 4096

// upstreamExceptionFromFrame 返回帧携带的上游失败；普通事件帧返回 nil。
func upstreamExceptionFromFrame(msg *eventStreamMessage) *KiroUpstreamException {
	if msg == nil {
		return nil
	}
	var typ string
	switch strings.ToLower(strings.TrimSpace(msg.MessageType)) {
	case "exception":
		typ = msg.ExceptionType
	case "error":
		typ = msg.ErrorCode
	default:
		return contentFilteredExceptionFromFrame(msg)
	}
	payload := msg.Payload
	if len(payload) > kiroUpstreamExceptionPayloadLimit {
		payload = payload[:kiroUpstreamExceptionPayloadLimit]
	}
	message := strings.TrimSpace(msg.ErrorMessage)
	var body map[string]any
	if json.Unmarshal(msg.Payload, &body) == nil {
		if message == "" {
			message = exceptionPayloadString(body, "message", "Message", "errorMessage")
		}
		if typ == "" {
			typ = exceptionPayloadString(body, "__type", "type", "errorCode")
			// AWS 的 __type 形如 "com.amazon.aws.codewhisperer#ValidationException"
			if i := strings.LastIndexByte(typ, '#'); i >= 0 {
				typ = typ[i+1:]
			}
		}
	} else if message == "" {
		message = strings.TrimSpace(string(payload))
	}
	if typ = strings.TrimSpace(typ); typ == "" {
		typ = "UnknownUpstreamError"
	}
	return &KiroUpstreamException{
		Type:    typ,
		Message: message,
		Payload: append([]byte(nil), payload...),
	}
}

// contentFilteredExceptionFromFrame 识别上游在正常事件帧里下发的"内容审核拒绝"：
// {"stopReason":"CONTENT_FILTERED","stopDetails":{"refusal":{"explanation":"..."}}}。
// 这类帧不带 :message-type=exception，以前被当作 metadataEvent 忽略，整条流只剩用量帧，
// 网关按"上游无可见输出"重试三次并临时停调度账号，客户端最终只拿到 502。
// 只认没有正文的元数据/结束帧：assistantResponseEvent 里的 refusal 是正常回复内容。
func contentFilteredExceptionFromFrame(msg *eventStreamMessage) *KiroUpstreamException {
	switch msg.EventType {
	case "metadataEvent", "messageMetadataEvent", "usageEvent", "messageStopEvent", "meteringEvent":
	default:
		return nil
	}
	var body struct {
		StopReason  string `json:"stopReason"`
		StopDetails struct {
			Refusal struct {
				Explanation string `json:"explanation"`
			} `json:"refusal"`
		} `json:"stopDetails"`
	}
	if err := json.Unmarshal(msg.Payload, &body); err != nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(body.StopReason), "CONTENT_FILTERED") {
		return nil
	}
	message := strings.TrimSpace(body.StopDetails.Refusal.Explanation)
	if message == "" {
		message = "Kiro refused this request (content filter)"
	}
	payload := msg.Payload
	if len(payload) > kiroUpstreamExceptionPayloadLimit {
		payload = payload[:kiroUpstreamExceptionPayloadLimit]
	}
	return &KiroUpstreamException{
		Type:    KiroContentFilteredException,
		Message: message,
		Payload: append([]byte(nil), payload...),
	}
}

func exceptionPayloadString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
