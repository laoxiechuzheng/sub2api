package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/stretchr/testify/require"
)

type diagnosticTestRepo struct {
	mu        sync.Mutex
	records   map[string]*RequestDiagnosticRecord
	getError  error
	saveError error
	started   chan struct{}
	release   chan struct{}
}

func (r *diagnosticTestRepo) Save(ctx context.Context, record *RequestDiagnosticRecord, _ int64) error {
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveError != nil {
		return r.saveError
	}
	if r.records == nil {
		r.records = make(map[string]*RequestDiagnosticRecord)
	}
	copied := *record
	copied.PayloadGzip = append([]byte(nil), record.PayloadGzip...)
	r.records[fmt.Sprintf("%d:%s", record.APIKeyID, record.UsageRequestID)] = &copied
	return nil
}
func (r *diagnosticTestRepo) Get(_ context.Context, key int64, id string) (*RequestDiagnosticRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getError != nil {
		return nil, r.getError
	}
	record := r.records[fmt.Sprintf("%d:%s", key, id)]
	if record == nil {
		return nil, nil
	}
	copied := *record
	return &copied, nil
}
func (r *diagnosticTestRepo) Cleanup(context.Context) error { return nil }
func diagnosticTestConfig() *config.Config {
	return &config.Config{RequestDiagnostics: config.RequestDiagnosticsConfig{Enabled: true, RetentionHours: 24, MaxEntryBytes: 8 << 20, MaxActive: 4, MaxPendingBytes: 32 << 20, MaxStorageBytes: 256 << 20}}
}
func diagnosticTestFinish(s *RequestDiagnosticService, usage *UsageLog, bindFirst bool) *requestdiagnostic.Capture {
	capture := s.Begin(requestdiagnostic.Inbound{Endpoint: "/v1/messages", StartedAt: time.Now()})
	if capture == nil {
		return nil
	}
	capture.SetInbound([]byte(`{"model":"client-model","messages":[{"role":"user","content":"完整中文内容"}],"api_key":"secret-field"}`))
	if bindFirst {
		capture.BindUsage(usage.APIKeyID, usage.RequestID, usage.CreatedAt)
		capture.Finish(200)
	} else {
		capture.Finish(200)
		capture.BindUsage(usage.APIKeyID, usage.RequestID, usage.CreatedAt)
	}
	return capture
}

func TestRequestDiagnosticServiceHandshakeAndGzipRoundTrip(t *testing.T) {
	for _, bindFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(bindFirst), func(t *testing.T) {
			repo := &diagnosticTestRepo{}
			s := NewRequestDiagnosticService(repo, diagnosticTestConfig())
			t.Cleanup(s.Stop)
			usage := &UsageLog{APIKeyID: 19, RequestID: "client:exact-canonical", CreatedAt: time.Now()}
			capture := diagnosticTestFinish(s, usage, bindFirst)
			require.NotNil(t, capture)
			require.Eventually(t, func() bool { repo.mu.Lock(); defer repo.mu.Unlock(); return len(repo.records) == 1 }, time.Second, time.Millisecond)
			detail, err := s.Get(context.Background(), usage)
			require.NoError(t, err)
			require.Equal(t, "captured", detail.Status)
			require.Equal(t, usage.RequestID, detail.Payload.Binding.CanonicalID)
			require.Contains(t, detail.Payload.Meta.Body.Content, "完整中文内容")
			require.NotContains(t, detail.Payload.Meta.Body.Content, "secret-field")
			require.True(t, detail.Payload.Meta.Body.Redacted)
			require.Equal(t, 0, len(s.active))
			record, _ := repo.Get(context.Background(), usage.APIKeyID, usage.RequestID)
			require.InDelta(t, 24, record.ExpiresAt.Sub(record.CapturedAt).Hours(), 0.001)
			reader, err := gzip.NewReader(bytes.NewReader(record.PayloadGzip))
			require.NoError(t, err)
			body, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.True(t, json.Valid(body))
			wrong := *usage
			wrong.CreatedAt = usage.CreatedAt.Add(time.Second)
			detail, err = s.Get(context.Background(), &wrong)
			require.NoError(t, err)
			require.Equal(t, "not_captured", detail.Status)
			wrong = *usage
			wrong.APIKeyID++
			detail, err = s.Get(context.Background(), &wrong)
			require.NoError(t, err)
			require.Equal(t, "not_captured", detail.Status)
		})
	}
}

func TestRequestDiagnosticServiceBoundsAndStop(t *testing.T) {
	repo := &diagnosticTestRepo{started: make(chan struct{}, 1), release: make(chan struct{})}
	cfg := diagnosticTestConfig()
	cfg.RequestDiagnostics.MaxActive = 1
	cfg.RequestDiagnostics.MaxPendingBytes = int64(cfg.RequestDiagnostics.MaxEntryBytes)
	s := NewRequestDiagnosticService(repo, cfg)
	capture := s.Begin(requestdiagnostic.Inbound{})
	require.NotNil(t, capture)
	require.Nil(t, s.Begin(requestdiagnostic.Inbound{}))
	usage := &UsageLog{APIKeyID: 1, RequestID: "client:queued", CreatedAt: time.Now()}
	capture.Finish(200)
	require.Equal(t, 1, len(s.active))
	require.True(t, capture.BindUsage(usage.APIKeyID, usage.RequestID, usage.CreatedAt))
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("write not started")
	}
	second := diagnosticTestFinish(s, &UsageLog{APIKeyID: 1, RequestID: "client:dropped", CreatedAt: time.Now()}, true)
	require.NotNil(t, second)
	require.Equal(t, int64(cfg.RequestDiagnostics.MaxEntryBytes), s.pending.Load())
	require.GreaterOrEqual(t, s.dropped.Load(), uint64(2))
	close(repo.release)
	s.Stop()
	require.Equal(t, int64(0), s.pending.Load())
	require.Nil(t, s.Begin(requestdiagnostic.Inbound{}))
	s.Stop()
}

// 首条写入立即成功，后续写入只等 context；release 仅用于红灯失败时回收旧 worker。
type diagnosticStopBudgetRepo struct {
	calls       atomic.Int32
	afterStop   atomic.Int32
	returned    atomic.Bool
	firstSaved  chan struct{}
	saveStarted chan context.Context
	release     chan struct{}
	releaseOnce sync.Once
}

func (r *diagnosticStopBudgetRepo) Save(ctx context.Context, _ *RequestDiagnosticRecord, _ int64) error {
	if r.returned.Load() {
		r.afterStop.Add(1)
	}
	call := r.calls.Add(1)
	if call == 1 {
		close(r.firstSaved)
		return nil
	}
	select {
	case r.saveStarted <- ctx:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

func (r *diagnosticStopBudgetRepo) Get(context.Context, int64, string) (*RequestDiagnosticRecord, error) {
	return nil, nil
}

func (r *diagnosticStopBudgetRepo) Cleanup(ctx context.Context) error { return ctx.Err() }

func TestRequestDiagnosticServiceStopCancelsAndJoinsWorkerWithinDrainBudget(t *testing.T) {
	repo := &diagnosticStopBudgetRepo{
		firstSaved: make(chan struct{}), saveStarted: make(chan context.Context, 4), release: make(chan struct{}),
	}
	cfg := diagnosticTestConfig()
	cfg.RequestDiagnostics.MaxEntryBytes = 1024
	cfg.RequestDiagnostics.MaxPendingBytes = 16 * 1024
	s := NewRequestDiagnosticService(repo, cfg)
	t.Cleanup(func() {
		repo.releaseOnce.Do(func() { close(repo.release) })
		s.Stop()
		select {
		case <-s.done:
		case <-time.After(time.Second):
			t.Error("fixture cleanup did not reclaim the diagnostic worker")
		}
	})

	publish := func(id string) {
		t.Helper()
		require.NotNil(t, diagnosticTestFinish(s, &UsageLog{APIKeyID: 1, RequestID: id, CreatedAt: time.Now()}, true))
	}
	publish("client:stop-first")
	select {
	case <-repo.firstSaved:
	case <-time.After(time.Second):
		t.Fatal("first diagnostic Save did not complete")
	}
	publish("client:stop-blocked")
	select {
	case <-repo.saveStarted:
	case <-time.After(time.Second):
		t.Fatal("second diagnostic Save did not enter its context wait")
	}
	publish("client:stop-queued-1")
	publish("client:stop-queued-2")

	started := time.Now()
	s.Stop()
	repo.returned.Store(true)
	require.Less(t, time.Since(started), 8*time.Second, "Stop must not sum the per-Save timeout over the queue")
	workerJoined := false
	select {
	case <-s.done:
		workerJoined = true
	default:
		t.Error("Stop returned before the diagnostic worker exited")
	}
	require.Zero(t, s.pending.Load(), "all active and queued reservations must be returned before Stop returns")
	if workerJoined {
		// 首 Save 后最多允许消耗一次 5s 超时，再执行一条被总预算取消的 Save。
		require.LessOrEqual(t, repo.calls.Load(), int32(3), "expired shutdown budget must discard remaining diagnostics, not start more Save calls")
		callsAtReturn := repo.calls.Load()
		s.Stop()
		s.enqueue(&requestdiagnostic.Snapshot{})
		require.Equal(t, callsAtReturn, repo.calls.Load())
		require.Zero(t, repo.afterStop.Load(), "no repository Save may run after Stop returns")
		require.Equal(t, uint64(3), s.dropped.Load(), "two canceled saves and one queued discard must each release exactly one reservation")
		require.Nil(t, s.Begin(requestdiagnostic.Inbound{}))
	}
}

func TestRequestDiagnosticServiceMissingExpiredEvictedAndInvalid(t *testing.T) {
	repo := &diagnosticTestRepo{records: make(map[string]*RequestDiagnosticRecord)}
	s := NewRequestDiagnosticService(repo, diagnosticTestConfig())
	t.Cleanup(s.Stop)
	usage := &UsageLog{APIKeyID: 1, RequestID: "client:x", CreatedAt: time.Now()}
	detail, err := s.Get(context.Background(), usage)
	require.NoError(t, err)
	require.Equal(t, "not_captured", detail.Status)
	record := &RequestDiagnosticRecord{APIKeyID: 1, UsageRequestID: usage.RequestID, UsageCreatedAt: usage.CreatedAt, CapturedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute)}
	repo.records["1:client:x"] = record
	detail, err = s.Get(context.Background(), usage)
	require.NoError(t, err)
	require.Equal(t, "expired", detail.Status)
	require.Nil(t, detail.Payload)
	record.ExpiresAt = time.Now().Add(time.Hour)
	detail, err = s.Get(context.Background(), usage)
	require.NoError(t, err)
	require.Equal(t, "evicted", detail.Status)
	record.PayloadBytes = 20
	record.PayloadGzip = []byte("invalid")
	_, err = s.Get(context.Background(), usage)
	require.Error(t, err)
	repo.getError = errors.New("db unavailable")
	_, err = s.Get(context.Background(), usage)
	require.Error(t, err)
}

func TestRequestDiagnosticServicePersistFailureDoesNotAffectCapture(t *testing.T) {
	repo := &diagnosticTestRepo{saveError: errors.New("failed")}
	s := NewRequestDiagnosticService(repo, diagnosticTestConfig())
	t.Cleanup(s.Stop)
	require.NotNil(t, diagnosticTestFinish(s, &UsageLog{APIKeyID: 1, RequestID: "client:no-db", CreatedAt: time.Now()}, false))
	require.Eventually(t, func() bool { return s.dropped.Load() > 0 }, time.Second, time.Millisecond)
	require.Equal(t, 0, len(s.active))
}

func TestRequestDiagnosticUsageBindingOnlyAfterSuccessfulWrite(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			var snapshot *requestdiagnostic.Snapshot
			capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
			capture.Finish(200)
			ctx := requestdiagnostic.WithCapture(context.Background(), capture)
			usage := &UsageLog{APIKeyID: 41, RequestID: "local:authoritative", CreatedAt: time.Now()}
			repo := &diagnosticUsageWriteRepo{fail: failure}
			writeUsageLogBestEffort(ctx, repo, usage, "test")
			if failure {
				require.Nil(t, snapshot)
			} else {
				require.NotNil(t, snapshot)
				require.Equal(t, usage.RequestID, snapshot.Binding.CanonicalID)
			}
		})
	}
}

func TestRequestDiagnosticUsageBindingKeepsCurrentTimestampAfterDuplicateWrite(t *testing.T) {
	for _, bestEffort := range []bool{false, true} {
		t.Run(fmt.Sprint(bestEffort), func(t *testing.T) {
			var snapshot *requestdiagnostic.Snapshot
			capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
			capture.Finish(200)
			ctx := requestdiagnostic.WithCapture(context.Background(), capture)
			original := time.Now().UTC()
			usage := &UsageLog{APIKeyID: 41, RequestID: "client:reused-id", CreatedAt: original}
			historical := original.Add(-time.Hour)
			base := &diagnosticDuplicateUsageRepo{historical: historical}
			var repo UsageLogRepository = base
			if bestEffort {
				repo = &diagnosticDuplicateBestEffortRepo{diagnosticDuplicateUsageRepo: base}
			}
			writeUsageLogBestEffort(ctx, repo, usage, "test")
			require.Equal(t, historical, usage.CreatedAt)
			require.NotNil(t, snapshot)
			// 即使 Create 回填历史行，数据库也只能用本次时间验证，不能把当前正文挂到历史记录。
			require.Equal(t, original, snapshot.Binding.UsageCreatedAt)
			require.Equal(t, usage.RequestID, snapshot.Binding.CanonicalID)
		})
	}
}

type diagnosticDuplicateUsageRepo struct {
	UsageLogRepository
	historical time.Time
}

func (r *diagnosticDuplicateUsageRepo) Create(_ context.Context, usage *UsageLog) (bool, error) {
	usage.ID = 99
	usage.CreatedAt = r.historical
	return false, nil
}

type diagnosticDuplicateBestEffortRepo struct {
	*diagnosticDuplicateUsageRepo
}

func (r *diagnosticDuplicateBestEffortRepo) CreateBestEffort(context.Context, *UsageLog) error {
	return errors.New("best effort unavailable")
}

type diagnosticUsageWriteRepo struct {
	UsageLogRepository
	fail bool
}

func (r *diagnosticUsageWriteRepo) Create(context.Context, *UsageLog) (bool, error) {
	if r.fail {
		return false, errors.New("write failed")
	}
	return true, nil
}
