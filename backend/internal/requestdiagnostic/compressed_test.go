package requestdiagnostic_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
)

func TestCaptureCompleteResponseTakesPriorityOverBothLargeRequests(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 1024), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	request := []byte(strings.Repeat("i", 900))
	capture.SetInbound(request)
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, request)
	response := strings.Repeat("r", 700)
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{ResponseBody: []byte(response), ResponseBytes: len(response)})
	require.True(t, capture.BindUsage(1, "u", time.Now()))
	capture.Finish(200)
	require.Equal(t, response, snapshot.Attempts[0].ResponseBody.Content)
	require.False(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.True(t, snapshot.Meta.Body.Truncated)
	require.True(t, snapshot.Attempts[0].Body.Truncated)
	require.LessOrEqual(t, snapshot.StoredBytes, 1024)
}

func TestCaptureCompressedResponseUsesStoredBytesAndDropsAllContent(t *testing.T) {
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	require.NoError(t, err)
	text := strings.Repeat("response-fixture", 1000)
	_, err = writer.Write([]byte(text))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(rawCaptureOptions(t, 1024), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound([]byte(strings.Repeat("i", 900)))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(strings.Repeat("u", 900)))
	encoded, err := json.Marshal(map[string]any{"ResponseBodyGzip": compressed.Bytes(), "ResponseBodyBytes": len(text), "ResponseBytes": len(text)})
	require.NoError(t, err)
	var result requestdiagnostic.AttemptResult
	require.NoError(t, json.Unmarshal(encoded, &result))
	capture.EndAttempt(id, result)
	require.True(t, capture.BindUsage(1, "u", time.Now()))
	capture.Finish(200)
	require.NotNil(t, snapshot.Attempts[0].ResponseBody)
	require.Equal(t, compressed.Len(), snapshot.Attempts[0].ResponseBody.StoredBytes)
	require.False(t, snapshot.Attempts[0].ResponseBody.Truncated)
	require.LessOrEqual(t, snapshot.StoredBytes, 1024)

	var dropped *requestdiagnostic.Snapshot
	capture = requestdiagnostic.NewCapture(rawCaptureOptions(t, 1024), requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { dropped = s })
	id = capture.BeginAttempt(requestdiagnostic.Attempt{}, nil)
	capture.EndAttempt(id, result)
	capture.Drop(requestdiagnostic.OmittedDropped)
	require.True(t, capture.BindUsage(1, "u", time.Now()))
	capture.Finish(200)
	require.NotNil(t, dropped.Attempts[0].ResponseBody)
	encoded, err = json.Marshal(dropped.Attempts[0].ResponseBody)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "content_gzip")
	require.Zero(t, dropped.Attempts[0].ResponseBody.StoredBytes)
	require.Equal(t, requestdiagnostic.OmittedDropped, dropped.Attempts[0].ResponseBody.OmittedReason)
}
