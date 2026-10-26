package repository

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
)

func diagnosticResponseText(t *testing.T, payload *requestdiagnostic.Payload) string {
	t.Helper()
	require.NotNil(t, payload)
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	var stored struct {
		ContentGzip []byte `json:"content_gzip"`
	}
	require.NoError(t, json.Unmarshal(encoded, &stored))
	if len(stored.ContentGzip) == 0 {
		return payload.Content
	}
	require.Empty(t, payload.Content, "transport must retain compressed bytes instead of a second plaintext copy")
	require.Equal(t, len(stored.ContentGzip), payload.StoredBytes)
	reader, err := gzip.NewReader(bytes.NewReader(stored.ContentGzip))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	return string(body)
}

func TestHTTPUpstreamDiagnosticZeroByteResponseCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reader     io.Reader
		readErr    error
		incomplete bool
	}{
		{name: "unread_close", reader: strings.NewReader("fixture-unread-response"), incomplete: true},
		{name: "first_read_error", reader: iotest.ErrReader(io.ErrUnexpectedEOF), readErr: io.ErrUnexpectedEOF, incomplete: true},
		{name: "empty_eof", reader: strings.NewReader(""), readErr: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture, ready := newRawUpstreamDiagnosticCapture(t, 4096)
			capture.SetInbound([]byte("{}"))
			original := &upstreamDiagnosticCountingBody{reader: tc.reader}
			req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
			require.NoError(t, err)
			transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: original}, nil
			})}
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			require.Zero(t, original.reads)
			if tc.readErr != nil {
				n, readErr := resp.Body.Read(make([]byte, 1))
				require.Zero(t, n)
				require.ErrorIs(t, readErr, tc.readErr)
				require.Equal(t, 1, original.reads)
			}
			require.NoError(t, resp.Body.Close())
			require.True(t, original.closed)
			if tc.readErr == nil {
				require.Zero(t, original.reads, "Close must not read the unobserved response")
			}
			snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
			status := requestdiagnostic.CaptureStatusCaptured
			if tc.incomplete {
				status = requestdiagnostic.CaptureStatusPartial
			}
			require.Equal(t, status, snapshot.CaptureStatus)
			payload := snapshot.Attempts[0].ResponseBody
			require.NotNil(t, payload)
			require.Equal(t, tc.incomplete, payload.Truncated)
			require.Equal(t, requestdiagnostic.OmittedEmptyBody, payload.OmittedReason)
			require.Zero(t, payload.OriginalBytes)
			require.Zero(t, payload.StoredBytes)
			require.Empty(t, payload.Content)
			require.Nil(t, payload.ContentGzip)
			if tc.readErr == io.ErrUnexpectedEOF {
				require.Equal(t, "upstream_body_read_error", snapshot.Attempts[0].Error)
			} else {
				require.Empty(t, snapshot.Attempts[0].Error)
			}
		})
	}
}

func TestHTTPUpstreamDiagnosticCompressedBeyond64MiBRetainsHashAndTail(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 96<<20)
	capture.SetInbound([]byte("{}"))
	prefix := "data: {\"type\":\"response.completed\",\"text\":\"" + strings.Repeat("x", 65<<20)
	tail := "fixture-tail-\u4f60\u597d\"}\n\n"
	want := sha256.New()
	_, err := io.WriteString(want, prefix+tail)
	require.NoError(t, err)
	original := &upstreamDiagnosticCountingBody{reader: io.MultiReader(strings.NewReader(prefix), strings.NewReader(tail))}
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: original}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Zero(t, original.reads)
	forwarded := sha256.New()
	n, err := io.CopyN(forwarded, resp.Body, int64(len(prefix)+len(tail)))
	require.NoError(t, err)
	require.EqualValues(t, len(prefix)+len(tail), n)
	require.Equal(t, want.Sum(nil), forwarded.Sum(nil))
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	body := snapshot.Attempts[0].ResponseBody
	text := diagnosticResponseText(t, body)
	require.Equal(t, len(prefix)+len(tail), len(text), "the old 64 MiB plaintext cap must not truncate diagnostics")
	require.True(t, strings.HasSuffix(text, tail))
	got := sha256.Sum256([]byte(text))
	require.Equal(t, want.Sum(nil), got[:])
	require.False(t, body.Truncated)
	require.Empty(t, body.OmittedReason)
	require.Less(t, body.StoredBytes, 1<<20)
	require.Equal(t, requestdiagnostic.CaptureStatusCaptured, snapshot.CaptureStatus)
	t.Logf("response original=%d gzip=%d sha256=%x terminal_without_eof=true", len(text), body.StoredBytes, got)
}

func TestHTTPUpstreamDiagnosticCompressedUTF8AcrossSingleByteReads(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 4096)
	payload := "data: {\"delta\":\"" + strings.Repeat("\u4f60\u597d\U0001f642", 1000) + "\"}\n\ndata: [DONE]\n\n"
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", nil)
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{resp.Body}, make([]byte, 1))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	text := diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody)
	require.Equal(t, len(payload), len(text))
	require.Equal(t, sha256.Sum256([]byte(payload)), sha256.Sum256([]byte(text)))
	require.False(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.LessOrEqual(t, snapshot.StoredBytes, 4096)
}

type diagnosticPrefixBlockingBody struct {
	prefix  *strings.Reader
	blocked *upstreamDiagnosticBlockingBody
}

func (b *diagnosticPrefixBlockingBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	return b.blocked.Read(p)
}

func (b *diagnosticPrefixBlockingBody) Close() error { return b.blocked.Close() }

func TestHTTPUpstreamDiagnosticCompressedCloseAndCancelReleaseBuffers(t *testing.T) {
	for _, mode := range []string{"close", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			capture, ready := newRawUpstreamDiagnosticCapture(t, 4096)
			ctx, cancel := context.WithCancel(requestdiagnostic.WithCapture(context.Background(), capture))
			defer cancel()
			prefix := "data: {\"delta\":\"fixture-before-cancel\"}\n\n"
			blocked := &upstreamDiagnosticBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
			body := &diagnosticPrefixBlockingBody{prefix: strings.NewReader(prefix), blocked: blocked}
			defer func() { require.NoError(t, body.Close()) }()
			stopCancel := context.AfterFunc(ctx, func() { _ = body.Close() })
			defer stopCancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", nil)
			require.NoError(t, err)
			transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
			})}
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			_, err = io.ReadFull(resp.Body, make([]byte, len(prefix)))
			require.NoError(t, err)
			readDone := make(chan error, 1)
			go func() { _, err := resp.Body.Read(make([]byte, 128)); readDone <- err }()
			select {
			case <-blocked.started:
			case <-time.After(time.Second):
				t.Fatal("fixture did not enter blocking Read")
			}
			if mode == "cancel" {
				cancel()
			} else {
				require.NoError(t, resp.Body.Close())
			}
			select {
			case err := <-readDone:
				require.ErrorIs(t, err, io.ErrClosedPipe)
			case <-time.After(time.Second):
				t.Fatal("Close/cancel blocked behind diagnostic compression")
			}
			require.NoError(t, resp.Body.Close())
			observed, ok := resp.Body.(*diagnosticResponseBody)
			require.True(t, ok)
			require.Nil(t, observed.responseCompressor)
			require.Nil(t, observed.result.ResponseBodyGzip)
			snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
			require.Equal(t, prefix, diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody))
			require.True(t, snapshot.Attempts[0].ResponseBody.Truncated)
		})
	}
}
