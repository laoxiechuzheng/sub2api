package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 后台计费 worker 只拿得到 usageRecordContext 复制的值，Composite 路由归属必须随之传递。
func TestUsageRecordContextCopiesCompositeRouteTrace(t *testing.T) {
	trace := &service.CompositeRouteTrace{RouteID: 42, RequestKind: service.CompositeRouteRequestKindCompaction}
	parent := service.WithCompositeRouteTrace(context.Background(), trace)

	ctx := usageRecordContext(parent, context.Background())

	require.Same(t, trace, service.CompositeRouteTraceFromContext(ctx))
	require.Nil(t, service.CompositeRouteTraceFromContext(usageRecordContext(context.Background(), context.Background())))
}

// The fallback runner decides right after the handler returns, before the
// async worker runs: handing a usage task to billing must mark the attempt
// synchronously so a failed-but-billed attempt is never replayed.
func TestWrapUsageRecordTaskMarksCompositeUsageSubmittedSynchronously(t *testing.T) {
	trace := &service.CompositeRouteTrace{RouteID: 7}
	parent := service.WithCompositeRouteTrace(context.Background(), trace)

	ran := false
	task, _ := wrapUsageRecordTaskContext(parent, func(context.Context) { ran = true })

	require.False(t, ran)
	require.True(t, trace.UsageCommitted(), "submission alone must mark the attempt")
	require.False(t, trace.UsageRecorded())
	task(context.Background())
	require.True(t, ran)

	// Requests outside a composite route are unaffected.
	_, abandonPlain := wrapUsageRecordTaskContext(context.Background(), func(context.Context) {})
	abandonPlain()
}
