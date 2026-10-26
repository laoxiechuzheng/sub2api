package requestdiagnostic

import (
	"bytes"
	"compress/gzip"
	"errors"
	"unicode/utf8"
)

var errCompressedBudget = errors.New("compressed diagnostic budget exceeded")

type compressedBodyBuffer struct {
	bytes.Buffer
	limit int
}

func (b *compressedBodyBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errCompressedBudget
	}
	return b.Buffer.Write(p)
}

// ResponseBodyCompressor observes caller-read bytes without retaining plaintext.
// The transport serializes Observe, Finish and Discard with its body mutex.
type ResponseBodyCompressor struct {
	buffer   compressedBodyBuffer
	writer   *gzip.Writer
	pending  [utf8.UTFMax]byte
	pendingN int
	original int
	written  int
	reason   string
}

func NewResponseBodyCompressor(maxBytes int) *ResponseBodyCompressor {
	return &ResponseBodyCompressor{buffer: compressedBodyBuffer{limit: max(0, maxBytes)}}
}

func (b *ResponseBodyCompressor) Observe(p []byte) {
	b.original += len(p)
	if b.reason != "" || len(p) == 0 {
		return
	}
	if b.original > MaxExpandedBodyBytes {
		b.omit(OmittedDecompressionLimit)
		return
	}
	if b.pendingN > 0 {
		for len(p) > 0 && !utf8.FullRune(b.pending[:b.pendingN]) {
			b.pending[b.pendingN] = p[0]
			b.pendingN++
			p = p[1:]
		}
		if !utf8.FullRune(b.pending[:b.pendingN]) {
			return
		}
		if !utf8.Valid(b.pending[:b.pendingN]) {
			b.omit(OmittedInvalidUTF8)
			return
		}
		b.write(b.pending[:b.pendingN])
		b.pendingN = 0
	}
	if utf8.Valid(p) {
		b.write(p)
		return
	}
	// Only an unfinished final rune may span Read calls. Invalid bytes anywhere
	// else must never become valid-looking text through JSON replacement.
	for size := len(p) - 1; size >= max(0, len(p)-utf8.UTFMax+1); size-- {
		if utf8.Valid(p[:size]) && !utf8.FullRune(p[size:]) {
			b.write(p[:size])
			b.pendingN = copy(b.pending[:], p[size:])
			return
		}
	}
	b.omit(OmittedInvalidUTF8)
}

func (b *ResponseBodyCompressor) write(p []byte) {
	if b.reason != "" || len(p) == 0 {
		return
	}
	if b.writer == nil {
		b.writer, _ = gzip.NewWriterLevel(&b.buffer, gzip.BestSpeed)
	}
	if _, err := b.writer.Write(p); err != nil {
		b.omit(OmittedBudget)
		return
	}
	b.written += len(p)
}

func (b *ResponseBodyCompressor) omit(reason string) {
	b.reason = reason
	b.Discard()
}

func (b *ResponseBodyCompressor) Discard() {
	b.writer = nil
	b.buffer.Buffer = bytes.Buffer{}
	b.pending = [utf8.UTFMax]byte{}
	b.pendingN = 0
}

func (b *ResponseBodyCompressor) Finish(incomplete bool) Payload {
	if b.pendingN > 0 && !incomplete {
		b.omit(OmittedInvalidUTF8)
	}
	if b.reason == "" && b.writer != nil {
		if err := b.writer.Close(); err != nil {
			b.omit(OmittedBudget)
		}
	}
	if b.reason != "" {
		payload := omittedPayload(b.original, b.reason)
		payload.Truncated = true
		return payload
	}
	if b.original == 0 {
		payload := omittedPayload(0, OmittedEmptyBody)
		payload.Truncated = incomplete
		return payload
	}
	if b.written == 0 {
		return Payload{OriginalBytes: b.original, Truncated: true, OmittedReason: OmittedInvalidUTF8}
	}
	payload := Payload{ContentGzip: b.buffer.Bytes(), OriginalBytes: b.original,
		UncompressedBytes: b.written, StoredBytes: b.buffer.Len(),
		Truncated: incomplete || b.written != b.original}
	b.Discard()
	return payload
}
