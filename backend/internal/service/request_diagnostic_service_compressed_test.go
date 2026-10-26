package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
)

func diagnosticCompressedFixture(t *testing.T, body []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	require.NoError(t, err)
	_, err = writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return compressed.Bytes()
}

func TestRequestDiagnosticServiceCompressedPersistedRoundTrip(t *testing.T) {
	repo := &diagnosticTestRepo{}
	cfg := diagnosticTestConfig()
	cfg.RequestDiagnostics.PreserveRaw = true
	cfg.RequestDiagnostics.RetentionHours = 6
	cfg.RequestDiagnostics.MaxEntryBytes = 1 << 20
	cfg.RequestDiagnostics.MaxStorageBytes = 0
	svc := NewRequestDiagnosticService(repo, cfg)
	t.Cleanup(svc.Stop)
	usage := &UsageLog{APIKeyID: 19, RequestID: "client:compressed-roundtrip", CreatedAt: time.Now()}
	text := strings.Repeat("data: {\"delta\":\"\u4f60\u597d\"}\n\n", 50000) + "data: [DONE]\n\nfixture-tail"
	compressed := diagnosticCompressedFixture(t, []byte(text))
	capture := svc.Begin(requestdiagnostic.Inbound{})
	require.NotNil(t, capture)
	capture.SetInbound([]byte(strings.Repeat("i", 900<<10)))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(strings.Repeat("u", 900<<10)))
	encoded, err := json.Marshal(map[string]any{"ResponseBodyGzip": compressed, "ResponseBodyBytes": len(text), "ResponseBytes": len(text)})
	require.NoError(t, err)
	var result requestdiagnostic.AttemptResult
	require.NoError(t, json.Unmarshal(encoded, &result))
	capture.EndAttempt(id, result)
	capture.Finish(200)
	require.True(t, capture.BindUsage(usage.APIKeyID, usage.RequestID, usage.CreatedAt))
	svc.Stop()
	record, err := repo.Get(context.Background(), usage.APIKeyID, usage.RequestID)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, 6*time.Hour, record.ExpiresAt.Sub(record.CapturedAt))
	detail, err := svc.Get(context.Background(), usage)
	require.NoError(t, err)
	require.NotNil(t, detail.Payload.Attempts[0].ResponseBody)
	body := detail.Payload.Attempts[0].ResponseBody
	require.Equal(t, sha256.Sum256([]byte(text)), sha256.Sum256([]byte(body.Content)))
	require.Equal(t, len(text), body.StoredBytes)
	require.False(t, body.Truncated)
	encoded, err = json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "content_gzip")
	require.NotContains(t, string(encoded), "uncompressed_bytes")
}

func TestRequestDiagnosticServiceCompressedQueueUsesStoredBytes(t *testing.T) {
	repo := &diagnosticTestRepo{started: make(chan struct{}, 1), release: make(chan struct{})}
	cfg := diagnosticTestConfig()
	cfg.RequestDiagnostics.PreserveRaw = true
	cfg.RequestDiagnostics.MaxPendingBytes = minDiagnosticQueueReservation
	svc := NewRequestDiagnosticService(repo, cfg)
	t.Cleanup(svc.Stop)
	defer close(repo.release)
	text := strings.Repeat("queue-compressed-fixture", 100000)
	capture := svc.Begin(requestdiagnostic.Inbound{})
	require.NotNil(t, capture)
	capture.SetInbound([]byte("{}"))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte("{}"))
	encoded, err := json.Marshal(map[string]any{"ResponseBodyGzip": diagnosticCompressedFixture(t, []byte(text)), "ResponseBodyBytes": len(text), "ResponseBytes": len(text)})
	require.NoError(t, err)
	var result requestdiagnostic.AttemptResult
	require.NoError(t, json.Unmarshal(encoded, &result))
	capture.EndAttempt(id, result)
	capture.Finish(200)
	require.True(t, capture.BindUsage(1, "client:compressed-queue", time.Now()))
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("compressed snapshot was dropped before persistence")
	}
	require.EqualValues(t, minDiagnosticQueueReservation, svc.pending.Load())
	require.Zero(t, svc.dropped.Load())
}

func TestRequestDiagnosticServiceReadsLegacyAndValidatesCompressedSnapshots(t *testing.T) {
	for _, name := range []string{"legacy_v1", "compressed_v2", "bad_gzip", "bad_checksum", "wrong_length", "invalid_utf8", "mixed_content", "wrong_stored_size", "legacy_with_gzip", "unknown_schema", "expansion_guard"} {
		t.Run(name, func(t *testing.T) {
			usage := &UsageLog{APIKeyID: 7, RequestID: "client:stored-fixture", CreatedAt: time.Now().UTC()}
			text := "fixture-response-\u4f60\u597d-tail"
			compressed := diagnosticCompressedFixture(t, []byte(text))
			payload := map[string]any{"content": "", "content_gzip": compressed, "uncompressed_bytes": len(text), "original_bytes": len(text), "stored_bytes": len(compressed)}
			version := 2
			wantError := false
			switch name {
			case "legacy_v1":
				version = 1
				payload = map[string]any{"content": text, "original_bytes": len(text), "stored_bytes": len(text)}
			case "bad_gzip":
				payload["content_gzip"], payload["stored_bytes"] = []byte("not-gzip"), 8
				wantError = true
			case "bad_checksum":
				compressed[len(compressed)-8] ^= 1
				wantError = true
			case "wrong_length":
				payload["uncompressed_bytes"] = len(text) - 1
				wantError = true
			case "invalid_utf8":
				invalid := diagnosticCompressedFixture(t, []byte{0xff})
				payload["content_gzip"], payload["stored_bytes"], payload["uncompressed_bytes"], payload["original_bytes"] = invalid, len(invalid), 1, 1
				wantError = true
			case "mixed_content":
				payload["content"] = "unexpected-plaintext"
				wantError = true
			case "wrong_stored_size":
				payload["stored_bytes"] = len(compressed) - 1
				wantError = true
			case "legacy_with_gzip":
				version, wantError = 1, true
			case "unknown_schema":
				version, wantError = 99, true
			case "expansion_guard":
				payload["uncompressed_bytes"], payload["original_bytes"] = (512<<20)+1, (512<<20)+1
			}
			snapshot := map[string]any{
				"schema_version": version,
				"binding":        requestdiagnostic.UsageBinding{APIKeyID: usage.APIKeyID, CanonicalID: usage.RequestID, UsageCreatedAt: usage.CreatedAt},
				"capture_status": "captured", "attempts": []any{map[string]any{"response_body": payload}},
			}
			body, err := json.Marshal(snapshot)
			require.NoError(t, err)
			record := &RequestDiagnosticRecord{APIKeyID: usage.APIKeyID, UsageRequestID: usage.RequestID, UsageCreatedAt: usage.CreatedAt, CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), PayloadGzip: diagnosticCompressedFixture(t, body), PayloadBytes: len(body)}
			repo := &diagnosticTestRepo{records: map[string]*RequestDiagnosticRecord{fmt.Sprintf("%d:%s", usage.APIKeyID, usage.RequestID): record}}
			svc := NewRequestDiagnosticService(repo, diagnosticTestConfig())
			t.Cleanup(svc.Stop)
			detail, err := svc.Get(context.Background(), usage)
			if wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "captured", detail.Status)
			response := detail.Payload.Attempts[0].ResponseBody
			require.NotNil(t, response)
			if name == "expansion_guard" {
				require.Empty(t, response.Content)
				require.True(t, response.Truncated)
				require.Equal(t, "decompression_limit", response.OmittedReason)
				require.Equal(t, requestdiagnostic.CaptureStatusPartial, detail.Payload.CaptureStatus)
			} else {
				require.Equal(t, text, response.Content)
				require.Equal(t, len(text), response.StoredBytes)
			}
			encoded, err := json.Marshal(detail)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "content_gzip")
		})
	}
}
