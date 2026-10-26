package requestdiagnostic_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func rawCaptureOptions(t *testing.T, limit int) requestdiagnostic.Options {
	t.Helper()
	options := requestdiagnostic.Options{MaxBytes: limit}
	require.NoError(t, json.Unmarshal([]byte(`{"PreserveRaw":true}`), &options))
	return options
}

func TestCapturePreserveRawKeepsRequestBytesAndEmbeddedValues(t *testing.T) {
	raw := "{\n  \"token\": \"fixture-original-token\", \"url\": \"https://example.invalid/?key=fixture-query\",\n  \"input\": \"{\\\"password\\\":\\\"fixture-password\\\"}\", \"file_data\": \"data:application/pdf;base64,YQ==\"\n}\n"
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 4096), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound([]byte(raw))
	capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(raw))
	require.True(t, capture.BindUsage(1, "client:raw-request", time.Now()))
	capture.Finish(200)
	require.NotNil(t, snapshot)
	for _, payload := range []requestdiagnostic.Payload{snapshot.Meta.Body, snapshot.Attempts[0].Body} {
		require.Equal(t, raw, payload.Content)
		require.Equal(t, len(raw), payload.StoredBytes)
		require.False(t, payload.Redacted)
		require.Empty(t, payload.OmittedReason)
	}
}

func TestCaptureRawResponseGetsSpaceBeforeOlderRequests(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 1024), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	request := []byte(`{"input":"` + strings.Repeat("x", 600) + `"}`)
	capture.SetInbound(request)
	capture.BeginAttempt(requestdiagnostic.Attempt{}, request)
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, request)
	response := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"<summary>fixture-password</summary>\"}\n\n")
	encoded, err := json.Marshal(map[string]any{"Status": 200, "ResponseBody": response, "ResponseBytes": len(response), "ResponseContentType": "text/event-stream"})
	require.NoError(t, err)
	var result requestdiagnostic.AttemptResult
	require.NoError(t, json.Unmarshal(encoded, &result))
	capture.EndAttempt(id, result)
	require.True(t, capture.BindUsage(1, "client:raw-budget", time.Now()))
	capture.Finish(200)
	body, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Equal(t, string(response), gjson.GetBytes(body, "attempts.1.response_body.content").String())
	require.Equal(t, "text/event-stream", gjson.GetBytes(body, "attempts.1.response_content_type").String())
	require.LessOrEqual(t, snapshot.StoredBytes, 1024)
	require.Empty(t, snapshot.Attempts[0].Body.Content)
	require.Equal(t, requestdiagnostic.OmittedEvicted, snapshot.Attempts[0].Body.OmittedReason)
}

func TestCaptureDropClearsRawResponse(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 4096), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(`{"input":"fixture-original-request"}`))
	encoded, err := json.Marshal(map[string]any{"Status": 200, "ResponseBody": []byte("fixture-original-response"), "ResponseBytes": 25})
	require.NoError(t, err)
	var result requestdiagnostic.AttemptResult
	require.NoError(t, json.Unmarshal(encoded, &result))
	capture.EndAttempt(id, result)
	capture.Drop(requestdiagnostic.OmittedDropped)
	capture.BindUsage(1, "client:raw-drop", time.Now())
	capture.Finish(200)
	body, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(body, "attempts.0.response_body").Exists())
	require.Equal(t, requestdiagnostic.OmittedDropped, gjson.GetBytes(body, "attempts.0.response_body.omitted_reason").String())
	require.NotContains(t, string(body), "fixture-original")
}

func TestCaptureRawResponseKeepsCompleteTextWhenBudgetFits(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 1024, PreserveRaw: true}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound([]byte("{}"))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte("{}"))
	response := strings.Repeat("x", 600)
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 200, ResponseBody: []byte(response), ResponseBytes: len(response)})
	require.True(t, capture.BindUsage(1, "u", time.Now()))
	capture.Finish(200)
	require.NotNil(t, snapshot.Attempts[0].ResponseBody)
	require.Equal(t, response, snapshot.Attempts[0].ResponseBody.Content)
	require.False(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.Equal(t, 605, snapshot.StoredBytes)
}

// 复现 #146873：416 KB 入站 + 438 KB 上游请求 + 3.5 MB SSE 输出，旧 1 MiB 上限会截断模型输出。
func TestCaptureRawResponseKeepsMultiMegabyteOutputWhenEntryBudgetFits(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 48<<20), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	inbound := []byte(`{"input":"` + strings.Repeat("i", 416<<10) + `"}`)
	upstream := []byte(`{"input":"` + strings.Repeat("u", 438<<10) + `"}`)
	capture.SetInbound(inbound)
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, upstream)
	var sse strings.Builder
	for sse.Len() < 3500<<10 {
		_, _ = sse.WriteString("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"摘要片段\"}\n\n")
	}
	_, _ = sse.WriteString("data: {\"type\":\"response.completed\"}\n\n")
	response := sse.String()
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 200, ResponseBody: []byte(response), ResponseBytes: len(response), ResponseContentType: "text/event-stream"})
	require.True(t, capture.BindUsage(1, "client:large-output", time.Now()))
	capture.Finish(200)
	require.NotNil(t, snapshot)
	require.Equal(t, requestdiagnostic.CaptureStatusCaptured, snapshot.CaptureStatus)
	got := snapshot.Attempts[0].ResponseBody
	require.NotNil(t, got)
	require.False(t, got.Truncated)
	require.Empty(t, got.OmittedReason)
	require.Equal(t, response, got.Content)
	require.Equal(t, string(inbound), snapshot.Meta.Body.Content)
	require.Equal(t, string(upstream), snapshot.Attempts[0].Body.Content)
}

func TestCaptureRawIncompleteResponseRejectsInvalidUTF8Suffix(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		suffix byte
	}{{"invalid_lead", 0xff}, {"isolated_continuation", 0x80}} {
		t.Run(fixture.name, func(t *testing.T) {
			var snapshot *requestdiagnostic.Snapshot
			capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 4096), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
			id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte("{}"))
			response := append([]byte("ok"), fixture.suffix)
			capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 200, ResponseBody: response, ResponseBytes: len(response), ResponseIncomplete: true})
			require.True(t, capture.BindUsage(1, "client:invalid-utf8-suffix", time.Now()))
			capture.Finish(200)
			require.NotNil(t, snapshot.Attempts[0].ResponseBody)
			require.Empty(t, snapshot.Attempts[0].ResponseBody.Content)
			require.Equal(t, requestdiagnostic.OmittedInvalidUTF8, snapshot.Attempts[0].ResponseBody.OmittedReason)
			require.Equal(t, len(response), snapshot.Attempts[0].ResponseBody.OriginalBytes)
		})
	}
}
