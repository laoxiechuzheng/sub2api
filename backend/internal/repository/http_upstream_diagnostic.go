package repository

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

// 包装最内层 RoundTripper，使 Grok 兼容重试和 HTTP 重定向也分别记录真正发出的正文。
func httpClientWithRequestDiagnostic(client *http.Client, req *http.Request, accountID int64) *http.Client {
	if client == nil || req == nil || requestdiagnostic.FromContext(req.Context()) == nil {
		return client
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &diagnosticTransport{base: base, accountID: accountID}
	return &clone
}

type diagnosticTransport struct {
	base      http.RoundTripper
	accountID int64
}

func (t *diagnosticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	capture := requestdiagnostic.FromContext(req.Context())
	if capture == nil {
		return t.base.RoundTrip(req)
	}
	platform, _ := service.ResolvedTargetPlatformFromContext(req.Context())
	if forced, _ := req.Context().Value(ctxkey.ForcePlatform).(string); platform == "" {
		platform = forced
	}
	if platform == "" {
		platform = service.RequestDiagnosticPlatformFromContext(req.Context())
	}
	meta := requestdiagnostic.Attempt{
		AccountID: t.accountID, Platform: platform,
		Endpoint: req.URL.String(), Method: req.Method, StartedAt: time.Now().UTC(),
	}
	var attempt int64
	switch {
	case req.Body == nil || req.Body == http.NoBody:
		attempt = capture.BeginAttempt(meta, nil)
	case req.GetBody == nil:
		// 绝不为了诊断消费原请求体，multipart/不可重读请求只保留元数据。
		attempt = capture.BeginAttemptOmitted(meta, diagnosticRequestLength(req.ContentLength), requestdiagnostic.OmittedNotCaptured)
	case req.ContentLength > int64(capture.MaxBytes()):
		attempt = capture.BeginAttemptOmitted(meta, diagnosticRequestLength(req.ContentLength), requestdiagnostic.OmittedTooLarge)
	default:
		body, err := req.GetBody()
		if err != nil {
			attempt = capture.BeginAttemptOmitted(meta, diagnosticRequestLength(req.ContentLength), requestdiagnostic.OmittedNotCaptured)
		} else {
			copied, readErr := io.ReadAll(io.LimitReader(body, int64(capture.MaxBytes())+1))
			_ = body.Close()
			switch {
			case readErr != nil:
				attempt = capture.BeginAttemptOmitted(meta, diagnosticRequestLength(req.ContentLength), requestdiagnostic.OmittedNotCaptured)
			case len(copied) > capture.MaxBytes():
				attempt = capture.BeginAttemptOmitted(meta, len(copied), requestdiagnostic.OmittedTooLarge)
			default:
				meta.Model = gjson.GetBytes(copied, "model").String()
				attempt = capture.BeginAttempt(meta, copied)
			}
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		capture.EndAttempt(attempt, requestdiagnostic.AttemptResult{Error: "upstream_transport_error"})
		return resp, err
	}
	if resp == nil {
		capture.EndAttempt(attempt, requestdiagnostic.AttemptResult{Error: "empty_upstream_response"})
		return resp, nil
	}
	requestID := resp.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = resp.Header.Get("Request-ID")
	}
	if requestID == "" {
		requestID = resp.Header.Get("X-Goog-Request-ID")
	}
	result := requestdiagnostic.AttemptResult{Status: resp.StatusCode, UpstreamRequestID: requestID}
	if resp.Body == nil {
		capture.EndAttempt(attempt, result)
		return resp, nil
	}
	resp.Body = &diagnosticResponseBody{
		ReadCloser: resp.Body, capture: capture, attempt: attempt, result: result,
		stream:  strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"),
		encoded: resp.Header.Get("Content-Encoding") != "", usage: make(map[string]int64),
	}
	return resp, nil
}

func diagnosticRequestLength(length int64) int {
	if length < 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if length > maxInt {
		return int(maxInt)
	}
	return int(length)
}

// 只旁观调用方已读取的字节，不自行读取、预取、等待或保存完整 SSE。
// Close 先关闭原 body，保留取消阻塞 Read 的既有行为，再完成一次诊断结果。
type diagnosticResponseBody struct {
	io.ReadCloser
	capture       *requestdiagnostic.Capture
	attempt       int64
	result        requestdiagnostic.AttemptResult
	mu            sync.Mutex
	done          bool
	stream        bool
	encoded       bool
	buffer        []byte
	tooLarge      bool
	lineOverflow  bool
	bytesRead     int64
	events        int64
	complete      bool
	terminal      bool
	usage         map[string]int64
	stopReason    string
	upstreamError json.RawMessage
}

func (b *diagnosticResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return n, err
	}
	b.bytesRead += int64(n)
	if !b.encoded {
		if b.stream {
			b.observeSSE(p[:n])
		} else if !b.tooLarge {
			if len(b.buffer)+n > requestdiagnostic.MaxResponseSummaryBytes {
				b.tooLarge, b.buffer = true, nil
			} else {
				b.buffer = append(b.buffer, p[:n]...)
			}
		}
	}
	if err != nil {
		b.complete = err == io.EOF
		if err != io.EOF {
			b.result.Error = "upstream_body_read_error"
		}
		b.finishLocked()
	}
	return n, err
}

func (b *diagnosticResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil && b.result.Error == "" {
		b.result.Error = "upstream_body_close_error"
	}
	b.finishLocked()
	return err
}

func (b *diagnosticResponseBody) observeSSE(chunk []byte) {
	for len(chunk) > 0 {
		newline := bytes.IndexByte(chunk, '\n')
		end := len(chunk)
		if newline >= 0 {
			end = newline
		}
		part := chunk[:end]
		if !b.lineOverflow {
			if len(b.buffer)+len(part) > requestdiagnostic.MaxResponseSummaryBytes {
				b.lineOverflow, b.buffer = true, nil
			} else {
				b.buffer = append(b.buffer, part...)
			}
		}
		if newline < 0 {
			break
		}
		if !b.lineOverflow {
			b.observeDataLine(bytes.TrimSpace(b.buffer))
		}
		b.buffer = b.buffer[:0]
		b.lineOverflow = false
		chunk = chunk[newline+1:]
	}
}

func (b *diagnosticResponseBody) observeDataLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[5:])
	b.events++
	if bytes.Equal(data, []byte("[DONE]")) {
		b.terminal = true
		return
	}
	if !gjson.ValidBytes(data) {
		return
	}
	kind := gjson.GetBytes(data, "type").String()
	if kind == "message_stop" || kind == "response.completed" || kind == "response.failed" {
		b.terminal = true
	}
	for _, path := range []string{"usage", "message.usage", "response.usage"} {
		usage := gjson.GetBytes(data, path)
		for _, name := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "prompt_tokens", "completion_tokens", "total_tokens"} {
			value := usage.Get(name)
			if value.Type == gjson.Number {
				b.usage[name] = value.Int()
			}
		}
		for _, name := range []string{"input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens", "completion_tokens_details.reasoning_tokens"} {
			value := usage.Get(name)
			if value.Type == gjson.Number {
				b.usage[name] = value.Int()
			}
		}
	}
	for _, path := range []string{"delta.stop_reason", "stop_reason", "choices.0.finish_reason", "response.status"} {
		value := gjson.GetBytes(data, path)
		if value.Type == gjson.String && len(value.Str) <= 128 {
			b.stopReason = value.Str
		}
	}
	for _, path := range []string{"error", "response.error"} {
		value := gjson.GetBytes(data, path)
		if value.Exists() && value.Type != gjson.Null && len(value.Raw) <= 16<<10 {
			b.upstreamError = append(b.upstreamError[:0], value.Raw...)
		}
	}
}

func (b *diagnosticResponseBody) finishLocked() {
	if b.done {
		return
	}
	b.done = true
	if b.stream && !b.encoded {
		if !b.lineOverflow && len(b.buffer) > 0 {
			b.observeDataLine(bytes.TrimSpace(b.buffer))
		}
		b.result.ResponseSummary, _ = json.Marshal(map[string]any{
			"summary_only": true, "stream": true, "bytes_read": b.bytesRead,
			"events_observed": b.events, "reached_eof": b.complete, "terminal_observed": b.terminal,
			"usage": b.usage, "stop_reason": b.stopReason, "error": b.upstreamError,
		})
	} else if !b.encoded && !b.tooLarge && b.complete && json.Valid(b.buffer) {
		b.result.ResponseSummary = b.buffer
	} else {
		b.result.ResponseSummary, _ = json.Marshal(map[string]any{
			"summary_only": true, "bytes_read": b.bytesRead, "reached_eof": b.complete,
			"body_omitted": true, "content_encoded": b.encoded, "over_summary_limit": b.tooLarge,
		})
	}
	b.capture.EndAttempt(b.attempt, b.result)
	b.buffer, b.upstreamError, b.usage = nil, nil, nil
}
