//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 只观测真实 reader 的资源生命周期；Read 的错误不是 producer 完成信号。
type sseLifecycleObservedBody struct {
	io.ReadCloser
	closed, completed chan struct{}
	active, reads     atomic.Int64
	closeOnce         sync.Once
	budget            *sseQueueBudget
}

func newSSELifecycleObservedBody(reader io.ReadCloser) *sseLifecycleObservedBody {
	return &sseLifecycleObservedBody{ReadCloser: reader, closed: make(chan struct{}), completed: make(chan struct{})}
}

func (b *sseLifecycleObservedBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	b.active.Add(1)
	defer b.active.Add(-1)
	return b.ReadCloser.Read(p)
}

func (b *sseLifecycleObservedBody) Close() error {
	var err error
	b.closeOnce.Do(func() {
		err = b.ReadCloser.Close()
		close(b.closed)
	})
	return err
}

// producer 在 buffer 归还后发布；这里不等待、不读尚未排空的 budget。
func (b *sseLifecycleObservedBody) sseReaderCompleted(budget *sseQueueBudget) {
	b.budget = budget
	close(b.completed)
}

type sseLifecycleExitState struct {
	BodyClosed      bool  `json:"body_closed"`
	ReaderCompleted bool  `json:"reader_completed"`
	ActiveReads     int64 `json:"active_reads"`
	BudgetKnown     bool  `json:"budget_known"`
	QueuedBytes     int64 `json:"queued_bytes"`
}

func sseLifecycleClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// 必须在 handler 返回时立即采样，不在等待 writer 或 producer 善后后重采样。
func (b *sseLifecycleObservedBody) exitState() sseLifecycleExitState {
	state := sseLifecycleExitState{BodyClosed: sseLifecycleClosed(b.closed), ActiveReads: b.active.Load()}
	if sseLifecycleClosed(b.completed) {
		state.ReaderCompleted = true
		// completed 关闭后才访问发布的指针，预算本身使用 atomic。
		if b.budget != nil {
			state.BudgetKnown = true
			state.QueuedBytes = b.budget.used.Load()
		}
	}
	return state
}

func (s sseLifecycleExitState) fullyReleased() bool {
	return s.BodyClosed && s.ReaderCompleted && s.ActiveReads == 0 && s.BudgetKnown && s.QueuedBytes == 0
}

func sseLifecycleAssertReleased(t *testing.T, state sseLifecycleExitState) {
	t.Helper()
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	t.Logf("SSE reader lifecycle snapshot: %s", encoded)
	if !state.fullyReleased() {
		t.Errorf("upstream reader exit contract violated: %s", encoded)
	}
}

// sentinel 只在测试 writer 层触发真实 handler defer，其它 panic 不会被吞掉。
type sseLifecycleInterrupt struct{}

type sseLifecycleGateWriter struct {
	*httptest.ResponseRecorder
	match            string
	entered, release chan struct{}
	enterOnce        sync.Once
	releaseOnce      sync.Once
}

func newSSELifecycleGateWriter(match string) *sseLifecycleGateWriter {
	return &sseLifecycleGateWriter{ResponseRecorder: httptest.NewRecorder(), match: match, entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *sseLifecycleGateWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.enterOnce.Do(func() { close(w.entered) })
		<-w.release
		panic(sseLifecycleInterrupt{})
	}
	return w.ResponseRecorder.Write(p)
}

func (w *sseLifecycleGateWriter) unblock() {
	w.releaseOnce.Do(func() { close(w.release) })
}

type sseLifecycleOutcome struct {
	result      *streamingResult
	err         error
	state       sseLifecycleExitState
	interrupted bool
	unexpected  any
}

func sseLifecycleStart(body *sseLifecycleObservedBody, writer http.ResponseWriter, maxQueue int64) (<-chan sseLifecycleOutcome, <-chan struct{}) {
	out := make(chan sseLifecycleOutcome, 1)
	finished := make(chan struct{})
	c := v0222FastPathContext(writer)
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize, SSEQueueMaxBytes: maxQueue}},
		rateLimitService: &RateLimitService{},
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
	go func() {
		defer close(finished)
		got := sseLifecycleOutcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				if _, ok := recovered.(sseLifecycleInterrupt); ok {
					got.interrupted = true
				} else {
					got.unexpected = recovered
				}
			}
			got.state = body.exitState()
			out <- got
		}()
		got.result, got.err = svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "public-model", "upstream-model", false)
	}()
	return out, finished
}

func sseLifecycleCheckOutcome(t *testing.T, got sseLifecycleOutcome, interrupted bool) {
	t.Helper()
	require.Nil(t, got.unexpected, "unexpected handler panic must fail the fixture")
	require.Equal(t, interrupted, got.interrupted)
	sseLifecycleAssertReleased(t, got.state)
}

func sseLifecycleReclaimed(t *testing.T, body *sseLifecycleObservedBody, finished <-chan struct{}) {
	t.Helper()
	synctest.Wait()
	require.True(t, sseLifecycleClosed(body.completed), "fixture must reclaim the producer after its assertion")
	require.True(t, sseLifecycleClosed(finished), "fixture must reclaim the handler after its assertion")
	require.Zero(t, body.active.Load())
}

// 错误里程碑已发布后仍在 Read 内等待；无 join 负例必须在此时暴露。
type sseLifecycleBlockedReader struct {
	*strings.Reader
	closed, entered, readExited, release        chan struct{}
	closeOnce, enterOnce, exitOnce, releaseOnce sync.Once
}

func newSSELifecycleBlockedReader(payload string) *sseLifecycleBlockedReader {
	return &sseLifecycleBlockedReader{
		Reader: strings.NewReader(payload), closed: make(chan struct{}), entered: make(chan struct{}),
		readExited: make(chan struct{}), release: make(chan struct{}),
	}
}

func (r *sseLifecycleBlockedReader) Read(p []byte) (int, error) {
	if r.Len() > 0 {
		return r.Reader.Read(p)
	}
	r.enterOnce.Do(func() { close(r.entered) })
	<-r.closed
	r.exitOnce.Do(func() { close(r.readExited) })
	<-r.release
	return 0, io.ErrClosedPipe
}

func (r *sseLifecycleBlockedReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	<-r.readExited
	return nil
}

func (r *sseLifecycleBlockedReader) unblock() {
	r.closeOnce.Do(func() { close(r.closed) })
	r.releaseOnce.Do(func() { close(r.release) })
}

type sseLifecycleErrorReader struct {
	*strings.Reader
	err error
}

func (r *sseLifecycleErrorReader) Read(p []byte) (int, error) {
	if r.Len() > 0 {
		return r.Reader.Read(p)
	}
	return 0, r.err
}

func (*sseLifecycleErrorReader) Close() error { return nil }

func sseLifecyclePrefix() string {
	return v0222FastPathEvent(false, "content_block_delta", v0222FastPathText) +
		v0222FastPathEvent(true, "message_delta", v0222FastPathUsage) + "data: [DONE]\n\n"
}

func TestSSEStreamReaderLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Run("blocked_Read_join", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reader := newSSELifecycleBlockedReader(sseLifecyclePrefix())
			body := newSSELifecycleObservedBody(reader)
			out, finished := sseLifecycleStart(body, httptest.NewRecorder(), 0)
			defer func() {
				reader.unblock()
				sseLifecycleReclaimed(t, body, finished)
			}()
			<-reader.entered
			<-reader.readExited
			synctest.Wait()
			require.True(t, sseLifecycleClosed(body.closed))
			require.True(t, sseLifecycleClosed(reader.readExited), "专门 blocked Read 场景保留错误里程碑断言")
			require.Equal(t, int64(1), body.active.Load())
			require.False(t, sseLifecycleClosed(body.completed))
			select {
			case got := <-out:
				// no-join mutation 会在 gate 未释放时返回，快照不能被善后覆盖。
				t.Log("legacy body.closed=true readExited=true while Read remains active")
				sseLifecycleCheckOutcome(t, got, false)
			default:
				reader.unblock()
				got := <-out
				sseLifecycleCheckOutcome(t, got, false)
				require.NoError(t, got.err)
				require.NotNil(t, got.result)
				require.Equal(t, 37, got.result.usage.OutputTokens)
			}
		})
	})

	for _, tc := range []struct {
		name, payload, gate string
		queue               int64
	}{
		{"send_blocked_cancel", sseLifecyclePrefix() + strings.Repeat(v0222FastPathEvent(false, "ping", `{"type":"ping"}`), 8) + "\n", "data: [DONE]", 0},
		{"budget_acquire_cancel", sseLifecyclePrefix() + strings.Repeat(v0222FastPathEvent(false, "ping", `{"type":"ping","padding":"`+strings.Repeat("x", 96)+`"}`), 2), "data: [DONE]", 64},
		{"first_visible_barrier_cancel", v0222FastPathEvent(false, "content_block_delta", v0222FastPathText) + "data: [DONE]\n\n", "answer", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := newSSELifecycleObservedBody(io.NopCloser(strings.NewReader(tc.payload)))
				writer := newSSELifecycleGateWriter(tc.gate)
				out, finished := sseLifecycleStart(body, writer, tc.queue)
				defer func() {
					writer.unblock()
					_ = body.Close()
					sseLifecycleReclaimed(t, body, finished)
				}()
				<-writer.entered
				synctest.Wait()
				require.Equal(t, int64(1), body.reads.Load(), "producer must be blocked after a successful Scan, not in another Read")
				require.Zero(t, body.active.Load())
				require.False(t, sseLifecycleClosed(body.completed))
				writer.unblock()
				got := <-out
				sseLifecycleCheckOutcome(t, got, true)
			})
		})
	}

	t.Run("upstream_error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			upstreamErr := errors.New("synthetic SSE reader error")
			reader := &sseLifecycleErrorReader{Reader: strings.NewReader(v0222FastPathEvent(false, "content_block_delta", v0222FastPathText)), err: upstreamErr}
			body := newSSELifecycleObservedBody(reader)
			rec := httptest.NewRecorder()
			out, finished := sseLifecycleStart(body, rec, 0)
			defer sseLifecycleReclaimed(t, body, finished)
			got := <-out
			sseLifecycleCheckOutcome(t, got, false)
			require.ErrorIs(t, got.err, upstreamErr)
			require.NotNil(t, got.result)
			require.Contains(t, rec.Body.String(), "stream_read_error")
		})
	})

	t.Run("natural_EOF", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			body := newSSELifecycleObservedBody(io.NopCloser(strings.NewReader(sseLifecyclePrefix())))
			out, finished := sseLifecycleStart(body, httptest.NewRecorder(), 0)
			defer sseLifecycleReclaimed(t, body, finished)
			got := <-out
			sseLifecycleCheckOutcome(t, got, false)
			require.NoError(t, got.err)
			require.NotNil(t, got.result)
			require.Equal(t, 37, got.result.usage.OutputTokens)
			require.Equal(t, 15, got.result.usage.CacheReadInputTokens)
		})
	})
}
