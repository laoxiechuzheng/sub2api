package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type upstreamDiagnosticRoundTripFunc func(*http.Request) (*http.Response, error)

func (f upstreamDiagnosticRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// 直接记录原流的消费量，避免诊断副本预读时误把原流被消费当成正常转发。
type upstreamDiagnosticCountingBody struct {
	reader io.Reader
	reads  int
	bytes  int
	closed bool
}

func (b *upstreamDiagnosticCountingBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.reader.Read(p)
	b.bytes += n
	return n, err
}

func (b *upstreamDiagnosticCountingBody) Close() error {
	b.closed = true
	return nil
}

func newUpstreamDiagnosticCapture(limit int) (*requestdiagnostic.Capture, <-chan *requestdiagnostic.Snapshot) {
	ready := make(chan *requestdiagnostic.Snapshot, 1)
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: limit}, requestdiagnostic.Inbound{
		Endpoint: "/v1/responses", Method: http.MethodPost,
	}, func(snapshot *requestdiagnostic.Snapshot) { ready <- snapshot })
	return capture, ready
}

func finishUpstreamDiagnosticCapture(t *testing.T, capture *requestdiagnostic.Capture, ready <-chan *requestdiagnostic.Snapshot) *requestdiagnostic.Snapshot {
	t.Helper()
	require.True(t, capture.BindUsage(31, "client:transport-fixture", time.Now()))
	capture.Finish(http.StatusOK)
	select {
	case snapshot := <-ready:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("diagnostic snapshot was not published after Finish and BindUsage")
		return nil
	}
}

func upstreamDiagnosticResponse(status int, payload string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"rid-upstream"}}, Body: io.NopCloser(strings.NewReader(payload))}
}

func TestHTTPUpstreamDiagnosticCapturesFinalBodyWithoutConsumingOriginal(t *testing.T) {
	capture, ready := newUpstreamDiagnosticCapture(1 << 20)
	capture.SetInbound([]byte(`{"model":"raw-client-model","input":"before rewrite"}`))
	final := `{"model":"mapped-upstream-model","input":"rewritten payload","api_key":"fixture-body-secret"}`
	original := &upstreamDiagnosticCountingBody{reader: strings.NewReader(final)}
	ctx := service.WithResolvedTargetPlatform(requestdiagnostic.WithCapture(context.Background(), capture), service.PlatformAnthropic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://user:fixture-url-secret@fixture.invalid/v1/messages?api_key=fixture-query-secret", original)
	require.NoError(t, err)
	req.ContentLength = int64(len(final))
	copies := 0
	req.GetBody = func() (io.ReadCloser, error) {
		copies++
		return io.NopCloser(strings.NewReader(final)), nil
	}
	var sent string
	base := &http.Client{Timeout: 7 * time.Second, Transport: upstreamDiagnosticRoundTripFunc(func(actual *http.Request) (*http.Response, error) {
		require.Same(t, req, actual)
		require.Zero(t, original.reads, "diagnostics must not read the request's live Body")
		body, readErr := io.ReadAll(actual.Body)
		require.NoError(t, readErr)
		sent = string(body)
		return upstreamDiagnosticResponse(http.StatusOK, `{"usage":{"output_tokens":3}}`), nil
	})}
	client := httpClientWithRequestDiagnostic(base, req, 81)
	require.NotSame(t, base, client)
	require.Equal(t, base.Timeout, client.Timeout)
	require.IsType(t, upstreamDiagnosticRoundTripFunc(nil), base.Transport, "shared client transport must not be replaced")
	resp, err := client.Transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, final, sent)
	require.Equal(t, 1, copies)
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Len(t, snapshot.Attempts, 1)
	attempt := snapshot.Attempts[0]
	require.Equal(t, "mapped-upstream-model", attempt.Model)
	require.Equal(t, "raw-client-model", gjson.Get(snapshot.Meta.Body.Content, "model").String())
	require.Equal(t, "rewritten payload", gjson.Get(attempt.Body.Content, "input").String())
	require.Equal(t, int64(81), attempt.AccountID)
	require.Equal(t, service.PlatformAnthropic, attempt.Platform)
	require.Equal(t, "https://fixture.invalid/v1/messages", attempt.Endpoint)
	require.Equal(t, http.StatusOK, attempt.Status)
	require.Equal(t, "rid-upstream", attempt.UpstreamRequestID)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, secret := range []string{"fixture-body-secret", "fixture-url-secret", "fixture-query-secret"} {
		require.NotContains(t, string(encoded), secret)
	}
}

func TestHTTPUpstreamDiagnosticRecordsEveryGrokFallbackAttempt(t *testing.T) {
	capture, ready := newUpstreamDiagnosticCapture(1 << 20)
	body := `{"model":"grok-mapped-model","input":"fixture"}`
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://"+grokCLIProxyHost+"/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("Authorization", "Bearer fixture-oauth-secret")
	var hosts []string
	base := &http.Client{Transport: upstreamDiagnosticRoundTripFunc(func(actual *http.Request) (*http.Response, error) {
		hosts = append(hosts, actual.URL.Hostname())
		sent, readErr := io.ReadAll(actual.Body)
		require.NoError(t, readErr)
		require.Equal(t, body, string(sent))
		if len(hosts) == 1 {
			return upstreamDiagnosticResponse(http.StatusForbidden, `{"error":"Access denied"}`), nil
		}
		return upstreamDiagnosticResponse(http.StatusOK, `{"usage":{"output_tokens":4}}`), nil
	})}
	client := httpClientWithGrokAccessDeniedFallback(httpClientWithRequestDiagnostic(base, req, 82))
	resp, err := client.Transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Equal(t, []string{grokCLIProxyHost, grokOfficialAPIHost}, hosts)
	require.Len(t, snapshot.Attempts, 2)
	require.Equal(t, []int{http.StatusForbidden, http.StatusOK}, []int{snapshot.Attempts[0].Status, snapshot.Attempts[1].Status})
	for i, attempt := range snapshot.Attempts {
		require.Equal(t, int64(i+1), attempt.ID)
		require.Equal(t, "grok-mapped-model", attempt.Model)
		require.Equal(t, "https://"+hosts[i]+"/v1/responses", attempt.Endpoint)
		require.JSONEq(t, body, attempt.Body.Content)
	}
}

func TestHTTPUpstreamDiagnosticRecordsRedirectAttempt(t *testing.T) {
	capture, ready := newUpstreamDiagnosticCapture(1 << 20)
	body := `{"model":"redirect-final-model","input":"fixture"}`
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/first", strings.NewReader(body))
	require.NoError(t, err)
	var paths []string
	base := &http.Client{Transport: upstreamDiagnosticRoundTripFunc(func(actual *http.Request) (*http.Response, error) {
		paths = append(paths, actual.URL.Path)
		sent, readErr := io.ReadAll(actual.Body)
		require.NoError(t, readErr)
		require.Equal(t, body, string(sent))
		resp := upstreamDiagnosticResponse(http.StatusOK, `{}`)
		if actual.URL.Path == "/first" {
			resp.StatusCode = http.StatusTemporaryRedirect
			resp.Header.Set("Location", "https://fixture.invalid/second")
		}
		return resp, nil
	})}
	resp, err := httpClientWithRequestDiagnostic(base, req, 83).Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Equal(t, []string{"/first", "/second"}, paths)
	require.Len(t, snapshot.Attempts, 2)
	require.Equal(t, http.StatusTemporaryRedirect, snapshot.Attempts[0].Status)
	require.Equal(t, http.StatusOK, snapshot.Attempts[1].Status)
	for i, attempt := range snapshot.Attempts {
		require.Equal(t, "https://fixture.invalid"+paths[i], attempt.Endpoint)
		require.Equal(t, "redirect-final-model", attempt.Model)
	}
}

func TestHTTPUpstreamDiagnosticRequestCopyIsBoundedAndMetadataOnly(t *testing.T) {
	const limit = 1024
	for _, mode := range []string{"known_large", "unknown_large", "not_replayable", "get_body_error"} {
		t.Run(mode, func(t *testing.T) {
			capture, ready := newUpstreamDiagnosticCapture(limit)
			payload := `{"model":"oversized-model","input":"` + strings.Repeat("x", limit*4) + `"}`
			original := &upstreamDiagnosticCountingBody{reader: strings.NewReader(payload)}
			copyBody := &upstreamDiagnosticCountingBody{reader: strings.NewReader(payload)}
			req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", original)
			require.NoError(t, err)
			req.ContentLength = -1
			copies := 0
			req.GetBody = func() (io.ReadCloser, error) {
				copies++
				if mode == "get_body_error" {
					return nil, errors.New("fixture copy unavailable")
				}
				return copyBody, nil
			}
			if mode == "known_large" {
				req.ContentLength = int64(len(payload))
			}
			if mode == "not_replayable" {
				req.GetBody = nil
			}
			transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(actual *http.Request) (*http.Response, error) {
				require.Same(t, original, actual.Body)
				require.Zero(t, original.reads)
				return &http.Response{StatusCode: http.StatusOK}, nil
			})}
			_, err = transport.RoundTrip(req)
			require.NoError(t, err)
			require.Zero(t, original.reads)
			require.False(t, original.closed)
			if mode == "unknown_large" {
				require.Equal(t, 1, copies)
				require.Equal(t, limit+1, copyBody.bytes, "unknown length copy must stop at MaxBytes+1")
				require.True(t, copyBody.closed)
			} else {
				require.Zero(t, copyBody.bytes)
				if mode != "get_body_error" {
					require.Zero(t, copies)
				}
			}
			snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
			require.Len(t, snapshot.Attempts, 1)
			attempt := snapshot.Attempts[0]
			require.Empty(t, attempt.Body.Content)
			require.Empty(t, attempt.Model)
			if mode == "known_large" || mode == "unknown_large" {
				require.Equal(t, requestdiagnostic.OmittedTooLarge, attempt.Body.OmittedReason)
			} else {
				require.Equal(t, requestdiagnostic.OmittedNotCaptured, attempt.Body.OmittedReason)
			}
		})
	}
}

func TestHTTPUpstreamDiagnosticResponseIsPassiveAndSSETailUsageIsSummarized(t *testing.T) {
	capture, ready := newUpstreamDiagnosticCapture(1 << 20)
	largeOutput := strings.Repeat("private-output-fixture-", 8000)
	payload := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":9}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + largeOutput + "\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":27,\"cache_creation_input_tokens\":6}}\n\n"
	body := &upstreamDiagnosticCountingBody{reader: strings.NewReader(payload)}
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/messages", strings.NewReader(`{"model":"final-model"}`))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Zero(t, body.reads, "RoundTrip must not prefetch SSE")
	var output bytes.Buffer
	_, err = io.CopyBuffer(&output, resp.Body, make([]byte, 113))
	require.NoError(t, err)
	require.Equal(t, payload, output.String(), "the caller must receive every original byte")
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Len(t, snapshot.Attempts, 1)
	summary := snapshot.Attempts[0].ResponseSummary
	require.LessOrEqual(t, summary.StoredBytes, requestdiagnostic.MaxResponseSummaryBytes)
	require.NotContains(t, summary.Content, "private-output-fixture")
	require.True(t, gjson.Get(summary.Content, "summary_only").Bool())
	require.True(t, gjson.Get(summary.Content, "terminal_observed").Bool())
	require.True(t, gjson.Get(summary.Content, "reached_eof").Bool())
	require.Equal(t, int64(len(payload)), gjson.Get(summary.Content, "bytes_read").Int())
	require.Equal(t, int64(12), gjson.Get(summary.Content, "usage.input_tokens").Int())
	require.Equal(t, int64(27), gjson.Get(summary.Content, "usage.output_tokens").Int())
	require.Equal(t, int64(9), gjson.Get(summary.Content, "usage.cache_read_input_tokens").Int())
	require.Equal(t, int64(6), gjson.Get(summary.Content, "usage.cache_creation_input_tokens").Int())
	require.Equal(t, "end_turn", gjson.Get(summary.Content, "stop_reason").String())
}

// Close 必须先取消底层阻塞 Read；用通道同步，不靠等待时长猜测调度顺序。
type upstreamDiagnosticBlockingBody struct {
	started chan struct{}
	closed  chan struct{}
	reads   atomic.Int32
	once    sync.Once
}

func (b *upstreamDiagnosticBlockingBody) Read([]byte) (int, error) {
	if b.reads.Add(1) == 1 {
		close(b.started)
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *upstreamDiagnosticBlockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestHTTPUpstreamDiagnosticCloseUnblocksReadBeforeCompletingCapture(t *testing.T) {
	capture, ready := newUpstreamDiagnosticCapture(1 << 20)
	body := &upstreamDiagnosticBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	defer body.Close()
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/messages", nil)
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Zero(t, body.reads.Load())
	readDone := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 32)); readDone <- err }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("fixture read did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- resp.Body.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("diagnostic Close blocked behind the original Read")
	}
	select {
	case err := <-readDone:
		require.ErrorIs(t, err, io.ErrClosedPipe)
	case <-time.After(time.Second):
		t.Fatal("original Read was not canceled by Close")
	}
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Len(t, snapshot.Attempts, 1)
	require.Equal(t, http.StatusOK, snapshot.Attempts[0].Status)
	require.False(t, gjson.Get(snapshot.Attempts[0].ResponseSummary.Content, "reached_eof").Bool())
}

func TestHTTPUpstreamDiagnosticTransportErrorNeverStoresRawCredentials(t *testing.T) {
	capture, ready := newUpstreamDiagnosticCapture(1 << 20)
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost,
		"https://user:fixture-password@fixture.invalid/v1/responses?api_key=fixture-query-secret", strings.NewReader(`{"model":"final-model"}`))
	require.NoError(t, err)
	rawError := errors.New("Authorization: Bearer fixture-bearer-secret; proxy https://user:fixture-password@proxy.invalid")
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, rawError })}
	resp, err := transport.RoundTrip(req)
	require.Nil(t, resp)
	require.ErrorIs(t, err, rawError, "caller error semantics must remain unchanged")
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Len(t, snapshot.Attempts, 1)
	require.Equal(t, "upstream_transport_error", snapshot.Attempts[0].Error)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, secret := range []string{"fixture-bearer-secret", "fixture-password", "fixture-query-secret"} {
		require.NotContains(t, string(encoded), secret)
	}
}

func TestHTTPUpstreamDiagnosticWithoutCaptureIsExactPassThrough(t *testing.T) {
	body := &upstreamDiagnosticCountingBody{reader: strings.NewReader(`{"model":"untouched"}`)}
	req, err := http.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", body)
	require.NoError(t, err)
	getBodyCalls := 0
	req.GetBody = func() (io.ReadCloser, error) { getBodyCalls++; return nil, fmt.Errorf("must not run") }
	expected := upstreamDiagnosticResponse(http.StatusAccepted, `{"untouched":true}`)
	responseBody := &upstreamDiagnosticCountingBody{reader: strings.NewReader(`{"untouched":true}`)}
	expected.Body = responseBody
	base := &http.Client{Transport: upstreamDiagnosticRoundTripFunc(func(actual *http.Request) (*http.Response, error) {
		require.Same(t, req, actual)
		return expected, nil
	})}
	require.Same(t, base, httpClientWithRequestDiagnostic(base, req, 84))
	resp, err := (&diagnosticTransport{base: base.Transport}).RoundTrip(req)
	require.NoError(t, err)
	require.Same(t, expected, resp)
	require.Same(t, responseBody, resp.Body)
	require.Zero(t, responseBody.reads)
	require.Zero(t, getBodyCalls)
	require.Zero(t, body.reads)
	require.NoError(t, resp.Body.Close())
}
