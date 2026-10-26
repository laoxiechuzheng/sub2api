package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newRawUpstreamDiagnosticCapture(t *testing.T, limit int) (*requestdiagnostic.Capture, <-chan *requestdiagnostic.Snapshot) {
	t.Helper()
	options := requestdiagnostic.Options{MaxBytes: limit}
	require.NoError(t, json.Unmarshal([]byte(`{"PreserveRaw":true}`), &options))
	ready := make(chan *requestdiagnostic.Snapshot, 1)
	capture := requestdiagnostic.NewCapture(options, requestdiagnostic.Inbound{Endpoint: "/v1/messages", Method: http.MethodPost}, func(s *requestdiagnostic.Snapshot) { ready <- s })
	return capture, ready
}

func TestHTTPUpstreamDiagnosticPreserveRawCapturesSSEOutputWithoutPrefetch(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 1<<20)
	request := "{\n \"model\":\"gemini-compression-model\", \"input\":\"token=fixture-original-input\"\n}\n"
	payload := "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"<summary>token=fixture-original-output</summary>\"}\r\n\r\n" +
		"event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"<summary>token=fixture-original-output</summary>\"}]}],\"usage\":{\"input_tokens\":200520,\"output_tokens\":7984}}}\r\n\r\n"
	original := &upstreamDiagnosticCountingBody{reader: strings.NewReader(payload)}
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(request))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(actual *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(actual.Body)
		require.NoError(t, readErr)
		require.Equal(t, request, string(body))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: original}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Zero(t, original.reads)
	var forwarded bytes.Buffer
	_, err = io.CopyBuffer(struct{ io.Writer }{&forwarded}, struct{ io.Reader }{resp.Body}, make([]byte, 31))
	require.NoError(t, err)
	require.Equal(t, payload, forwarded.String())
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Equal(t, request, snapshot.Attempts[0].Body.Content)
	require.Equal(t, payload, diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody))
	require.Equal(t, len(payload), snapshot.Attempts[0].ResponseBody.OriginalBytes)
	require.False(t, snapshot.Attempts[0].ResponseBody.Redacted)
	require.False(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.Equal(t, int64(7984), gjson.Get(snapshot.Attempts[0].ResponseSummary.Content, "usage.output_tokens").Int())
}

func TestHTTPUpstreamDiagnosticPreserveRawKeepsJSONBeyondSummaryLimit(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 1<<20)
	payload := "{\n \"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"" + strings.Repeat("summary-fixture-", 6000) + "\"}]}]\n}\n"
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"fixture"}`))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) { return upstreamDiagnosticResponse(200, payload), nil })}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.Equal(t, payload, diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody))
	require.True(t, gjson.Get(snapshot.Attempts[0].ResponseSummary.Content, "over_summary_limit").Bool())
}

func TestHTTPUpstreamDiagnosticRawResponseIsBoundedAndUTF8Safe(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 4096)
	payload := "data: {\"delta\":\"" + strings.Repeat("\u4f60", 1600) + "\"}\n\n"
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"fixture"}`))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	forwarded, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, payload, string(forwarded))
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	content := diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody)
	require.NotEmpty(t, content)
	require.Equal(t, payload, content, "the compressed response fits the storage budget in full")
	require.True(t, utf8.ValidString(content))
	require.False(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.Equal(t, len(payload), snapshot.Attempts[0].ResponseBody.OriginalBytes)
	require.LessOrEqual(t, snapshot.StoredBytes, 4096)
}

func TestHTTPUpstreamDiagnosticRawResponseMarksEarlyCloseIncomplete(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 4096)
	prefix := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture-partial-output\"}\n\n"
	payload := prefix + "data: {\"type\":\"response.completed\"}\n\n"
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{\"model\":\"fixture\"}"))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	forwarded := make([]byte, len(prefix))
	_, err = io.ReadFull(resp.Body, forwarded)
	require.NoError(t, err)
	require.Equal(t, prefix, string(forwarded))
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.NotNil(t, snapshot.Attempts[0].ResponseBody)
	require.Equal(t, prefix, diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody))
	require.True(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.Equal(t, requestdiagnostic.CaptureStatusPartial, snapshot.CaptureStatus)
}

func TestHTTPUpstreamDiagnosticRawTerminalFrameCompleteness(t *testing.T) {
	for _, fixture := range []struct {
		name, payload string
		incomplete    bool
	}{
		{"unfinished_terminal_frame", "data: {\"type\":\"response.completed\"}", true},
		{"space_line_does_not_close_frame", "data: {\"type\":\"response.completed\"}\n \n", true},
		{"tab_line_does_not_close_frame", "data: {\"type\":\"response.completed\"}\n\t\n", true},
		{"crlf_closes_frame", "data: {\"type\":\"response.completed\"}\r\n\r\n", false},
		{"response_done", "data: {\"type\":\"response.done\"}\n\n", false},
		{"large_terminal_frame", "data: {\"type\":\"response.completed\",\"text\":\"" + strings.Repeat("x", 70<<10) + "\"}\n\n", false},
		{"large_terminal_type_at_end", "data: {\"text\":\"" + strings.Repeat("x", 70<<10) + "\",\"type\":\"response.completed\"}\n\n", false},
		{"nested_type_is_not_terminal", "data: {\"response\":{\"type\":\"response.completed\"}}\n\n", true},
		{"quoted_type_is_not_terminal", "data: {\"text\":\"\\\"type\\\":\\\"response.completed\\\"\"}\n\n", true},
		{"invalid_json_is_not_terminal", "data: {\"type\":\"response.completed\",}\n\n", true},
		{"invalid_number_is_not_terminal", "data: {\"type\":\"response.completed\",\"n\":01}\n\n", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			capture, ready := newRawUpstreamDiagnosticCapture(t, 1<<20)
			req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
			require.NoError(t, err)
			transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fixture.payload))}, nil
			})}
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			received := make([]byte, len(fixture.payload))
			_, err = io.ReadFull(resp.Body, received)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
			require.NotNil(t, snapshot.Attempts[0].ResponseBody)
			require.Equal(t, fixture.payload, diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody))
			require.Equal(t, fixture.incomplete, snapshot.Attempts[0].ResponseBody.Truncated)
		})
	}
}

func TestHTTPUpstreamDiagnosticRawInterruptedUTF8KeepsValidPrefix(t *testing.T) {
	capture, ready := newRawUpstreamDiagnosticCapture(t, 4096)
	prefix := "data: {\"delta\":\"fixture-valid-prefix"
	payload := prefix + string([]byte{0xe4})
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.ReadFull(resp.Body, make([]byte, len(payload)))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	snapshot := finishUpstreamDiagnosticCapture(t, capture, ready)
	require.NotNil(t, snapshot.Attempts[0].ResponseBody)
	require.Equal(t, prefix, diagnosticResponseText(t, snapshot.Attempts[0].ResponseBody))
	require.True(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.Equal(t, len(payload), snapshot.Attempts[0].ResponseBody.OriginalBytes)
}

func TestHTTPUpstreamDiagnosticRawBuffersReleasedAfterFinish(t *testing.T) {
	capture, _ := newRawUpstreamDiagnosticCapture(t, 4096)
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return upstreamDiagnosticResponse(200, "{\"output_text\":\"fixture-original-output\"}"), nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	observed, ok := resp.Body.(*diagnosticResponseBody)
	require.True(t, ok, "expected diagnostic response body")
	require.Nil(t, observed.responseCompressor)
	require.Nil(t, observed.result.ResponseBody)
	require.Nil(t, observed.result.ResponseBodyGzip)
	require.Nil(t, observed.result.ResponseSummary)
	capture.Discard()
}

func TestHTTPUpstreamDiagnosticDropStopsRawBuffering(t *testing.T) {
	capture, _ := newRawUpstreamDiagnosticCapture(t, 4096)
	req, err := http.NewRequestWithContext(requestdiagnostic.WithCapture(context.Background(), capture), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	transport := &diagnosticTransport{base: upstreamDiagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return upstreamDiagnosticResponse(200, strings.Repeat("x", 3000)), nil
	})}
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.ReadFull(resp.Body, make([]byte, 128))
	require.NoError(t, err)
	capture.Drop(requestdiagnostic.OmittedDropped)
	_, err = io.ReadFull(resp.Body, make([]byte, 128))
	require.NoError(t, err)
	observed, ok := resp.Body.(*diagnosticResponseBody)
	require.True(t, ok, "expected diagnostic response body")
	require.Nil(t, observed.responseCompressor)
	require.NoError(t, resp.Body.Close())
	capture.Discard()
}
