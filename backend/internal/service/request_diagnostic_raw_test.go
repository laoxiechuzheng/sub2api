package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
)

func TestRequestDiagnosticServicePreserveRawRoundTrip(t *testing.T) {
	repo := &diagnosticTestRepo{}
	cfg := diagnosticTestConfig()
	cfg.RequestDiagnostics.PreserveRaw = true
	service := NewRequestDiagnosticService(repo, cfg)
	t.Cleanup(service.Stop)
	usage := &UsageLog{APIKeyID: 19, RequestID: "client:raw-round-trip", CreatedAt: time.Now()}
	inbound := "{\n \"token\":\"fixture-original-token\", \"input\":\"fixture-prompt\"\n}\n"
	output := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<summary>fixture-original-token</summary>\"}\r\n\r\n"
	capture := service.Begin(requestdiagnostic.Inbound{Endpoint: "/v1/messages", StartedAt: time.Now()})
	require.NotNil(t, capture)
	capture.SetInbound([]byte(inbound))
	attempt := capture.BeginAttempt(requestdiagnostic.Attempt{Endpoint: "/v1/responses"}, []byte(inbound))
	capture.EndAttempt(attempt, requestdiagnostic.AttemptResult{
		Status: 200, ResponseBody: []byte(output), ResponseBytes: len(output),
		ResponseContentType: "text/event-stream",
	})
	capture.Finish(200)
	require.True(t, capture.BindUsage(usage.APIKeyID, usage.RequestID, usage.CreatedAt))
	require.Eventually(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return len(repo.records) == 1
	}, time.Second, time.Millisecond)
	detail, err := service.Get(context.Background(), usage)
	require.NoError(t, err)
	require.Equal(t, "captured", detail.Status)
	require.Equal(t, inbound, detail.Payload.Meta.Body.Content)
	require.Equal(t, inbound, detail.Payload.Attempts[0].Body.Content)
	require.NotNil(t, detail.Payload.Attempts[0].ResponseBody)
	require.Equal(t, output, detail.Payload.Attempts[0].ResponseBody.Content)
	require.False(t, detail.Payload.Meta.Body.Redacted)
	require.False(t, detail.Payload.Attempts[0].ResponseBody.Redacted)
}
