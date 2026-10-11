//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestV0215MergeCCPingWriteFailureDrainsTerminalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &gatewayForwardDrainFailWriter{
		ResponseRecorder: httptest.NewRecorder(),
		failAt:           1,
		failedCh:         make(chan struct{}),
		cancel:           cancel,
	}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	const ping = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	body := &gatewayForwardDrainGatedBody{
		prefix: strings.NewReader(ping),
		// The tail is readable only after the ping write fails; usage after message_stop still counts.
		tail:   strings.NewReader(gatewayForwardDrainStart + ping + gatewayForwardDrainStop + gatewayForwardDrainDelta + ping),
		gate:   writer.failedCh,
		closed: make(chan struct{}),
	}
	defer body.Close()
	resp := &http.Response{Body: body}

	result, err := runGatewayForwardDrainHandler(t, gatewayForwardDrainHandlers()["chat_completions"], &GatewayService{}, resp, c)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, writer.failed)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 27, result.Usage.OutputTokens)
	require.Equal(t, 15, result.Usage.CacheReadInputTokens)
	require.Equal(t, 6, result.Usage.CacheCreationInputTokens)
	require.Zero(t, writer.writesAfterFailure)
	require.Zero(t, writer.flushAfterFailure)
	require.Empty(t, writer.Body.String())
}
