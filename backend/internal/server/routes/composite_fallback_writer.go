package routes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// 单次尝试最多暂存的未提交字节；超过后放弃该尝试的备用重放，直接透传。
const compositeFallbackMaxHeldBytes = 4 << 20

// 暂存 SSE 前导事件期间的下游心跳间隔，避免反向代理或客户端空闲超时（测试可调）。
var compositeFallbackKeepaliveInterval = 15 * time.Second

var compositeFallbackKeepaliveFrame = []byte(": keepalive\n\n")

// compositeFallbackOutcome summarizes one handler run for the fallback runner.
type compositeFallbackOutcome struct {
	// Failed reports a failure that reached the client neither as a status nor
	// as visible content, so another target can still serve the request.
	Failed bool
	Status int
	// InBand reports that the failure was an SSE error event on a 2xx stream
	// rather than an HTTP error status.
	InBand           bool
	DownstreamFailed bool
}

// compositeSSEProtocol selects how a synthesized terminal error event is
// framed when a stream was committed before every target failed.
type compositeSSEProtocol int

const (
	compositeSSEProtocolGeneric compositeSSEProtocol = iota
	// Responses clients (Codex) only treat response.failed as a terminal event;
	// a bare error frame makes them reconnect blindly.
	compositeSSEProtocolResponses
	compositeSSEProtocolAnthropic
	compositeSSEProtocolChat
)

// compositeFallbackWriter sits between a gateway handler and the real response
// writer while a composite route still has fallback targets. Each handler run
// is one attempt: error responses and SSE frames that carry no model output
// (comments, lifecycle preamble, stream errors) are held back, and the first
// successful status or visible SSE event commits the response for good. A held
// failure can then be discarded and the request replayed on the next target.
type compositeFallbackWriter struct {
	mu       sync.Mutex
	base     gin.ResponseWriter
	protocol compositeSSEProtocol

	baseline         http.Header
	committed        bool // status and headers were sent through base
	committedSSE     bool // committed as an SSE stream before any visible content
	visible          bool // passthrough mode; nothing is held anymore
	lastClientAt     time.Time
	lastSSEError     []byte
	keepaliveOn      bool
	keepaliveEnd     chan struct{}
	attemptDone      bool
	downstreamFailed error // persistent across attempts, including asynchronous heartbeats

	// Per-attempt state.
	header        http.Header
	status        int
	size          int
	held          bytes.Buffer
	heldErrors    []byte // the attempt's SSE error events only (no preamble)
	pending       []byte
	failed        bool
	failStatus    int
	errorBody     bool // held bytes are a non-SSE error body
	committedHere bool
}

func newCompositeFallbackWriter(base gin.ResponseWriter, protocol compositeSSEProtocol) *compositeFallbackWriter {
	w := &compositeFallbackWriter{base: base, protocol: protocol, baseline: base.Header().Clone()}
	w.beginAttempt()
	return w
}

// beginAttempt resets the per-attempt view so the next handler run observes a
// fresh, unwritten response exactly as it would without the wrapper.
func (w *compositeFallbackWriter) beginAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.header = w.baseline.Clone()
	w.status = http.StatusOK
	w.size = -1
	w.held.Reset()
	w.heldErrors = nil
	w.pending = nil
	w.failed = false
	w.failStatus = 0
	w.errorBody = false
	w.committedHere = false
	w.attemptDone = false
}

// endAttempt freezes the attempt after its handler returned.
func (w *compositeFallbackWriter) endAttempt() compositeFallbackOutcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attemptDone = true
	w.stopKeepaliveLocked()
	if w.downstreamFailed != nil {
		return compositeFallbackOutcome{DownstreamFailed: true}
	}
	if w.visible {
		return compositeFallbackOutcome{}
	}
	if len(w.pending) > 0 && w.sseAttemptLocked() {
		event := w.pending
		w.pending = nil
		if err := w.consumeSSEEventLocked(event); err != nil {
			return compositeFallbackOutcome{DownstreamFailed: true}
		}
		if w.visible {
			return compositeFallbackOutcome{}
		}
	}
	if w.failed {
		return compositeFallbackOutcome{Failed: true, Status: w.failStatus, InBand: !w.errorBody}
	}
	if w.status >= http.StatusBadRequest {
		return compositeFallbackOutcome{Failed: true, Status: w.status}
	}
	return compositeFallbackOutcome{}
}

// discardAttempt drops a failed attempt's held output before a replay, keeping
// its SSE error events (not its preamble) so a committed stream can still end
// with a proper terminal event if no later attempt produces one.
func (w *compositeFallbackWriter) discardAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.heldErrors) > 0 {
		w.lastSSEError = append(w.lastSSEError[:0], w.heldErrors...)
	}
	w.held.Reset()
	w.heldErrors = nil
	w.pending = nil
}

// finish delivers whatever the final attempt held. It must run before the
// original writer is restored on the gin context.
func (w *compositeFallbackWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attemptDone = true
	w.stopKeepaliveLocked()
	if w.visible || w.downstreamFailed != nil {
		return
	}
	held := append(w.held.Bytes(), w.pending...)
	if w.failed && !w.errorBody {
		held = compositeTerminalSSEErrorFrame(w.protocol, w.heldErrors, w.failStatus)
	}
	if w.committed {
		var err error
		switch {
		case w.errorBody && len(held) > 0:
			_, err = w.writeBaseLocked(compositeSSEErrorFrame(w.protocol, held, w.failStatus))
		case len(held) > 0:
			_, err = w.writeBaseLocked(held)
		case len(w.lastSSEError) > 0:
			_, err = w.writeBaseLocked(compositeTerminalSSEErrorFrame(w.protocol, w.lastSSEError, http.StatusBadGateway))
		default:
			_, err = w.writeBaseLocked(compositeSSEErrorFrame(w.protocol, nil, http.StatusBadGateway))
		}
		if err == nil {
			w.base.Flush()
		}
		return
	}
	if w.size < 0 {
		// 最终尝试没写正文：只同步状态码与响应头，交给 gin 收尾写出，与未包装时一致。
		target := w.base.Header()
		for key, values := range w.header {
			target[key] = values
		}
		w.base.WriteHeader(w.status)
		return
	}
	w.commitLocked(w.status)
	if len(held) > 0 {
		_, _ = w.writeBaseLocked(held)
	}
}

func (w *compositeFallbackWriter) sseAttemptLocked() bool {
	if w.errorBody {
		return false
	}
	if w.status >= http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream") || w.committedSSE
}

func (w *compositeFallbackWriter) commitLocked(status int) {
	if w.committed || w.downstreamFailed != nil {
		return
	}
	target := w.base.Header()
	for key, values := range w.header {
		target[key] = values
	}
	w.base.WriteHeader(status)
	w.base.WriteHeaderNow()
	w.committed = true
	w.committedHere = true
}

func (w *compositeFallbackWriter) writeBaseLocked(data []byte) (int, error) {
	if w.downstreamFailed != nil {
		return 0, w.downstreamFailed
	}
	n, err := w.base.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.downstreamFailed = err
		w.stopKeepaliveLocked()
	}
	if n > 0 {
		w.lastClientAt = time.Now()
	}
	return n, err
}

// goVisibleLocked commits (if needed), releases everything held, and switches
// to passthrough. The attempt can no longer be replayed elsewhere.
func (w *compositeFallbackWriter) goVisibleLocked() error {
	if w.downstreamFailed != nil {
		return w.downstreamFailed
	}
	w.commitLocked(w.status)
	w.visible = true
	w.stopKeepaliveLocked()
	held := append(w.held.Bytes(), w.pending...)
	w.held.Reset()
	w.pending = nil
	if len(held) == 0 {
		return nil
	}
	_, err := w.writeBaseLocked(held)
	return err
}

func (w *compositeFallbackWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.visible && w.committedHere {
		return w.base.Header()
	}
	return w.header
}

func (w *compositeFallbackWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.downstreamFailed != nil {
		return
	}
	if w.visible {
		w.base.WriteHeader(code)
		return
	}
	if code > 0 && w.size < 0 {
		w.status = code
	}
}

func (w *compositeFallbackWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.downstreamFailed != nil {
		return
	}
	w.markWrittenLocked()
	if w.visible {
		w.base.WriteHeaderNow()
		return
	}
	w.headerOnlyLocked()
}

func (w *compositeFallbackWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.downstreamFailed != nil {
		return
	}
	w.markWrittenLocked()
	if w.visible {
		w.base.Flush()
		return
	}
	w.headerOnlyLocked()
	if w.committed && w.downstreamFailed == nil {
		w.base.Flush()
	}
}

// headerOnlyLocked handles a header commit without body: plain successes pass
// through, an SSE success waits for content behind keepalives, errors wait.
func (w *compositeFallbackWriter) headerOnlyLocked() {
	if w.status >= http.StatusBadRequest {
		return
	}
	if w.sseAttemptLocked() {
		w.startKeepaliveLocked()
		return
	}
	if !w.committedSSE {
		_ = w.goVisibleLocked()
	}
}

func (w *compositeFallbackWriter) markWrittenLocked() {
	if w.size < 0 {
		w.size = 0
	}
}

func (w *compositeFallbackWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *compositeFallbackWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.downstreamFailed != nil {
		return 0, w.downstreamFailed
	}
	w.markWrittenLocked()
	w.size += len(data)
	if w.visible {
		return w.writeBaseLocked(data)
	}
	switch {
	case w.sseAttemptLocked():
		if err := w.consumeSSELocked(data); err != nil {
			return 0, err
		}
	case w.status < http.StatusBadRequest && !w.committedSSE:
		if err := w.goVisibleLocked(); err != nil {
			return 0, err
		}
		if _, err := w.writeBaseLocked(data); err != nil {
			return 0, err
		}
	default:
		w.failed = true
		w.errorBody = true
		if w.failStatus == 0 {
			w.failStatus = w.status
			if w.failStatus < http.StatusBadRequest {
				w.failStatus = http.StatusBadGateway
			}
		}
		w.held.Write(data)
		if w.held.Len() > compositeFallbackMaxHeldBytes {
			if err := w.goVisibleLocked(); err != nil {
				return 0, err
			}
		}
	}
	return len(data), nil
}

// consumeSSELocked splits complete SSE events and classifies each one.
func (w *compositeFallbackWriter) consumeSSELocked(data []byte) error {
	w.pending = append(w.pending, data...)
	for !w.visible {
		end, sep := compositeSSEEventEnd(w.pending)
		if end < 0 {
			break
		}
		event := w.pending[:end+sep]
		// goVisibleLocked releases the remaining tail together with the held events.
		w.pending = w.pending[end+sep:]
		if err := w.consumeSSEEventLocked(event); err != nil {
			return err
		}
	}
	if !w.visible && w.held.Len()+len(w.pending) > compositeFallbackMaxHeldBytes {
		return w.goVisibleLocked()
	}
	return nil
}

func (w *compositeFallbackWriter) consumeSSEEventLocked(event []byte) error {
	switch class, status := classifyCompositeSSEEvent(event); class {
	case compositeSSEComment:
		if w.failed {
			return nil
		}
		w.commitLocked(http.StatusOK)
		w.committedSSE = true
		if _, err := w.writeBaseLocked(event); err != nil {
			return err
		}
		w.base.Flush()
	case compositeSSEPreamble:
		w.held.Write(event)
		w.startKeepaliveLocked()
	case compositeSSEError:
		if !w.failed {
			w.failed = true
			w.failStatus = status
		}
		w.held.Write(event)
		w.heldErrors = append(w.heldErrors, event...)
	default:
		if w.failed {
			w.held.Write(event)
			return nil
		}
		w.held.Write(event)
		if err := w.goVisibleLocked(); err != nil {
			return err
		}
		w.base.Flush()
	}
	return nil
}

func (w *compositeFallbackWriter) startKeepaliveLocked() {
	if w.keepaliveOn || w.visible || w.attemptDone || w.downstreamFailed != nil {
		return
	}
	w.keepaliveOn = true
	stop := make(chan struct{})
	w.keepaliveEnd = stop
	if w.lastClientAt.IsZero() {
		w.lastClientAt = time.Now()
	}
	go func() {
		ticker := time.NewTicker(compositeFallbackKeepaliveInterval / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			w.mu.Lock()
			if !w.keepaliveOn || w.visible || w.attemptDone || w.downstreamFailed != nil {
				w.mu.Unlock()
				return
			}
			if time.Since(w.lastClientAt) >= compositeFallbackKeepaliveInterval && !w.failed && w.sseAttemptLocked() {
				w.commitLocked(http.StatusOK)
				w.committedSSE = true
				if _, err := w.writeBaseLocked(compositeFallbackKeepaliveFrame); err != nil {
					w.mu.Unlock()
					return
				}
				w.base.Flush()
			}
			w.mu.Unlock()
		}
	}()
}

func (w *compositeFallbackWriter) stopKeepaliveLocked() {
	if !w.keepaliveOn {
		return
	}
	w.keepaliveOn = false
	close(w.keepaliveEnd)
}

func (w *compositeFallbackWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.visible && w.committedHere {
		return w.base.Status()
	}
	return w.status
}

func (w *compositeFallbackWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

func (w *compositeFallbackWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size >= 0
}

func (w *compositeFallbackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed && !w.visible {
		return nil, nil, errors.New("composite fallback: response already committed")
	}
	w.visible = true
	w.committed = true
	w.committedHere = true
	w.stopKeepaliveLocked()
	return w.base.Hijack()
}

func (w *compositeFallbackWriter) CloseNotify() <-chan bool {
	return w.base.CloseNotify()
}

func (w *compositeFallbackWriter) Pusher() http.Pusher {
	return w.base.Pusher()
}

type compositeSSEClass int

const (
	compositeSSEVisible compositeSSEClass = iota
	compositeSSEComment
	compositeSSEPreamble
	compositeSSEError
)

// compositeSSEEventEnd returns the index of the blank line ending the first
// complete event and the separator length, or -1.
func compositeSSEEventEnd(buf []byte) (int, int) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf, 4
	default:
		return lf, 2
	}
}

// classifyCompositeSSEEvent decides whether an SSE event carries model output.
// Comments, lifecycle preambles (response.created/in_progress, message_start,
// ping, role-only chat chunks) and [DONE] are not output; error frames are
// failures; everything else is treated as visible output.
func classifyCompositeSSEEvent(event []byte) (compositeSSEClass, int) {
	var eventName string
	var data []byte
	hasField := false
	for _, rawLine := range bytes.Split(event, []byte("\n")) {
		line := bytes.TrimRight(rawLine, "\r")
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		hasField = true
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(line[len("data:"):])...)
		}
	}
	if !hasField {
		return compositeSSEComment, 0
	}
	switch eventName {
	case "error", "response.failed", "response.error":
		return compositeSSEError, compositeSSEErrorStatus(data)
	case "response.created", "response.in_progress", "response.queued", "message_start", "ping":
		return compositeSSEPreamble, 0
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return compositeSSEPreamble, 0
	}
	if !gjson.ValidBytes(trimmed) {
		return compositeSSEVisible, 0
	}
	root := gjson.ParseBytes(trimmed)
	if errValue := root.Get("error"); errValue.Exists() && errValue.Type != gjson.Null {
		return compositeSSEError, compositeSSEErrorStatus(trimmed)
	}
	switch root.Get("type").String() {
	case "error", "response.failed", "response.error":
		return compositeSSEError, compositeSSEErrorStatus(trimmed)
	case "response.created", "response.in_progress", "response.queued", "message_start", "ping":
		return compositeSSEPreamble, 0
	}
	if compositeChatChunkWithoutOutput(root) {
		return compositeSSEPreamble, 0
	}
	return compositeSSEVisible, 0
}

// compositeChatChunkWithoutOutput matches the role-only opening chunk of a
// Chat Completions stream.
func compositeChatChunkWithoutOutput(root gjson.Result) bool {
	if root.Get("object").String() != "chat.completion.chunk" || root.Get("usage").Type == gjson.JSON {
		return false
	}
	choices := root.Get("choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		return false
	}
	for _, choice := range choices.Array() {
		if finish := choice.Get("finish_reason"); finish.Exists() && finish.Type != gjson.Null {
			return false
		}
		delta := choice.Get("delta")
		for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
			if value := delta.Get(key); value.Exists() && value.String() != "" {
				return false
			}
		}
		for _, key := range []string{"tool_calls", "function_call", "audio"} {
			if value := delta.Get(key); value.Exists() && value.Type != gjson.Null {
				return false
			}
		}
	}
	return true
}

// compositeSSEErrorStatus maps a stream error payload to an HTTP-like status
// used for retry decisions and the usage fallback chain label.
func compositeSSEErrorStatus(data []byte) int {
	if !gjson.ValidBytes(data) {
		return http.StatusBadGateway
	}
	root := gjson.ParseBytes(data)
	for _, path := range []string{"error.status", "error.code", "response.error.code", "status", "code"} {
		if value := root.Get(path); value.Type == gjson.Number && value.Int() >= 400 && value.Int() <= 599 {
			return int(value.Int())
		}
	}
	text := strings.ToLower(root.Get("error.type").String() + " " + root.Get("error.code").String() + " " +
		root.Get("response.error.code").String() + " " + root.Get("error.message").String() + " " + root.Get("response.error.message").String())
	switch {
	case strings.Contains(text, "rate_limit") || strings.Contains(text, "rate limit") || strings.Contains(text, "quota"):
		return http.StatusTooManyRequests
	case strings.Contains(text, "overload"):
		return 529
	case strings.Contains(text, "context_length") || strings.Contains(text, "invalid_request"):
		return http.StatusBadRequest
	case strings.Contains(text, "authentication") || strings.Contains(text, "unauthorized"):
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// compositeSSEErrorFrame turns a held non-SSE error body (or nothing) into the
// terminal error event of the client's stream protocol, for a stream that was
// already committed by keepalives before every target failed.
func compositeSSEErrorFrame(protocol compositeSSEProtocol, body []byte, status int) []byte {
	errType, message := compositeErrorTypeAndMessage(body, status)
	var event string
	var payload any
	switch protocol {
	case compositeSSEProtocolResponses:
		event = "response.failed"
		payload = map[string]any{
			"type":            "response.failed",
			"sequence_number": 0,
			"response": map[string]any{
				"id":         "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
				"object":     "response",
				"created_at": time.Now().Unix(),
				"status":     "failed",
				"output":     []any{},
				"error":      map[string]any{"code": errType, "message": message},
			},
		}
	case compositeSSEProtocolAnthropic:
		event = "error"
		payload = map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}}
	case compositeSSEProtocolChat:
		payload = map[string]any{"error": map[string]any{"type": errType, "message": message, "code": status}}
	default:
		event = "error"
		payload = map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}}
	}
	encoded, _ := json.Marshal(payload)
	frame := make([]byte, 0, len(encoded)+40)
	if event != "" {
		frame = append(frame, "event: "...)
		frame = append(frame, event...)
		frame = append(frame, '\n')
	}
	frame = append(frame, "data: "...)
	frame = append(frame, encoded...)
	frame = append(frame, "\n\n"...)
	return frame
}

// Keep one valid Responses terminal, or normalize the last error payload to
// the ingress protocol. Held lifecycle events never belong to a failed turn.
func compositeTerminalSSEErrorFrame(protocol compositeSSEProtocol, events []byte, status int) []byte {
	var payload []byte
	for len(events) > 0 {
		end, sep := compositeSSEEventEnd(events)
		if end < 0 {
			end, sep = len(events), 0
		}
		event := events[:end+sep]
		events = events[end+sep:]
		var data []byte
		var eventName string
		for _, rawLine := range bytes.Split(event, []byte("\n")) {
			line := bytes.TrimRight(rawLine, "\r")
			if bytes.HasPrefix(line, []byte("event:")) {
				eventName = strings.TrimSpace(string(line[len("event:"):]))
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimSpace(line[len("data:"):])...)
			}
		}
		if len(data) == 0 {
			continue
		}
		root := gjson.ParseBytes(data)
		if protocol == compositeSSEProtocolResponses && gjson.ValidBytes(data) && root.Get("type").String() == "response.failed" &&
			root.Get("response.status").String() == "failed" && root.Get("response.id").String() != "" &&
			root.Get("response.error").IsObject() {
			if eventName != "" && eventName != "response.failed" {
				return []byte("event: response.failed\ndata: " + string(data) + "\n\n")
			}
			frame := append([]byte(nil), event...)
			if sep == 0 {
				frame = append(frame, '\n', '\n')
			}
			return frame
		}
		payload = data
	}
	return compositeSSEErrorFrame(protocol, payload, status)
}

// compositeErrorTypeAndMessage extracts an error type and message from a JSON
// (OpenAI / Anthropic / Google style) or plain-text error body.
func compositeErrorTypeAndMessage(body []byte, status int) (string, string) {
	body = bytes.TrimSpace(body)
	errType, message := "", ""
	if gjson.ValidBytes(body) {
		root := gjson.ParseBytes(body)
		for _, path := range []string{"error.type", "error.code", "error.status", "response.error.code", "response.error.type", "type"} {
			if value := root.Get(path); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" && value.String() != "error" {
				errType = strings.TrimSpace(value.String())
				break
			}
		}
		for _, path := range []string{"error.message", "response.error.message", "message", "detail"} {
			if value := root.Get(path); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				message = strings.TrimSpace(value.String())
				break
			}
		}
	} else {
		message = strings.TrimSpace(string(body))
	}
	if errType == "" {
		errType = "upstream_error"
	}
	if message == "" {
		message = "Upstream request failed with HTTP " + strconv.Itoa(status)
	}
	return errType, message
}
