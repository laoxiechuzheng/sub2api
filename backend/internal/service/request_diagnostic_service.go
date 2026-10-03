package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
)

const (
	maxDiagnosticJSONBytes        = 16 << 20
	requestDiagnosticDrainTimeout = 6 * time.Second
)

// RequestDiagnosticRecord 使用 canonical 用量键关联，不依赖异步插入回填 ID。
type RequestDiagnosticRecord struct {
	APIKeyID       int64
	UsageRequestID string
	UsageCreatedAt time.Time
	CapturedAt     time.Time
	ExpiresAt      time.Time
	PayloadGzip    []byte
	PayloadBytes   int
}

type RequestDiagnosticRepository interface {
	Save(context.Context, *RequestDiagnosticRecord, int64) error
	Get(context.Context, int64, string) (*RequestDiagnosticRecord, error)
	Cleanup(context.Context) error
}

type RequestDiagnosticDetail struct {
	Status     string                      `json:"status"`
	CapturedAt *time.Time                  `json:"captured_at,omitempty"`
	ExpiresAt  *time.Time                  `json:"expires_at,omitempty"`
	Payload    *requestdiagnostic.Snapshot `json:"payload,omitempty"`
}

type diagnosticWrite struct {
	snapshot *requestdiagnostic.Snapshot
	reserved int64
}

// RequestDiagnosticService 的有界后台队列只接收脱敏快照；失败不影响计费。
type RequestDiagnosticService struct {
	repo         RequestDiagnosticRepository
	cfg          config.RequestDiagnosticsConfig
	active       chan struct{}
	queue        chan diagnosticWrite
	pending      atomic.Int64
	dropped      atomic.Uint64
	stop         chan struct{}
	done         chan struct{}
	workerCtx    context.Context
	cancelWorker context.CancelFunc
	once         sync.Once
	queueMu      sync.Mutex
	stopped      bool
}

func NewRequestDiagnosticService(repo RequestDiagnosticRepository, cfg *config.Config) *RequestDiagnosticService {
	options := config.RequestDiagnosticsConfig{}
	if cfg != nil {
		options = cfg.RequestDiagnostics
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	s := &RequestDiagnosticService{
		repo: repo, cfg: options, stop: make(chan struct{}), done: make(chan struct{}),
		workerCtx: workerCtx, cancelWorker: cancelWorker,
	}
	if repo == nil {
		s.cfg.Enabled = false
		cancelWorker()
		close(s.done)
		return s
	}
	if !options.Enabled || options.MaxActive < 1 || options.MaxEntryBytes < 1 {
		s.cfg.Enabled = false
	} else {
		s.active = make(chan struct{}, options.MaxActive)
	}
	s.queue = make(chan diagnosticWrite, 16)
	// 关闭新增采集后仍清理历史正文，TTL 不依赖 enabled 开关。
	go s.run()
	return s
}

func (s *RequestDiagnosticService) Enabled() bool { return s != nil && s.cfg.Enabled }
func (s *RequestDiagnosticService) RetentionHours() int {
	if s == nil {
		return 24
	}
	return s.cfg.RetentionHours
}
func (s *RequestDiagnosticService) MaxEntryBytes() int {
	if s == nil {
		return 0
	}
	return s.cfg.MaxEntryBytes
}

func (s *RequestDiagnosticService) Begin(meta requestdiagnostic.Inbound) *requestdiagnostic.Capture {
	if !s.Enabled() {
		return nil
	}
	select {
	case <-s.stop:
		return nil
	default:
	}
	select {
	case s.active <- struct{}{}:
	default:
		s.dropped.Add(1)
		return nil
	}

	// 等待异步 usage 绑定期间仍占用采集额度，避免 worker 队列持有无限正文。
	var mu sync.Mutex
	var timer *time.Timer
	completed := false
	var capture *requestdiagnostic.Capture
	complete := func() {
		mu.Lock()
		if !completed {
			completed = true
			if timer != nil {
				timer.Stop()
			}
			<-s.active
		}
		mu.Unlock()
	}
	capture = requestdiagnostic.NewCapture(requestdiagnostic.Options{
		MaxBytes: s.cfg.MaxEntryBytes, MaxAttempts: 16,
		OnFinish: func() {
			mu.Lock()
			if !completed {
				timer = time.AfterFunc(30*time.Second, func() { capture.Discard(); complete() })
			}
			mu.Unlock()
		},
	}, meta, func(snapshot *requestdiagnostic.Snapshot) {
		defer complete()
		s.enqueue(snapshot)
	})
	return capture
}

func (s *RequestDiagnosticService) enqueue(snapshot *requestdiagnostic.Snapshot) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.stopped || snapshot == nil {
		return
	}
	// 核心限制的是所有保存字符串字节；按单条上限预留队列额度，不在请求线程编码大 JSON。
	reserved := int64(s.cfg.MaxEntryBytes)
	if s.pending.Add(reserved) > s.cfg.MaxPendingBytes {
		s.pending.Add(-reserved)
		s.dropped.Add(1)
		return
	}
	select {
	case <-s.stop:
		s.pending.Add(-reserved)
	case s.queue <- diagnosticWrite{snapshot: snapshot, reserved: reserved}:
	default:
		s.pending.Add(-reserved)
		s.dropped.Add(1)
	}
}

func (s *RequestDiagnosticService) run() {
	defer close(s.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if s.workerCtx.Err() != nil {
			s.discardQueued()
			return
		}
		select {
		case <-s.workerCtx.Done():
			s.discardQueued()
			return
		case item := <-s.queue:
			s.persist(item)
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.workerCtx, 5*time.Second)
			if ctx.Err() == nil {
				if err := s.repo.Cleanup(ctx); err != nil {
					logger.LegacyPrintf("service.request_diagnostic", "Cleanup failed")
				}
			}
			cancel()
		case <-s.stop:
			for {
				if s.workerCtx.Err() != nil {
					s.discardQueued()
					return
				}
				select {
				case item := <-s.queue:
					s.persist(item)
				default:
					return
				}
			}
		}
	}
}

// Stop 已在原队列锁内禁止入队；超出总预算后只归还剩余额度，不再编码或触库。
func (s *RequestDiagnosticService) discardQueued() {
	for {
		select {
		case item := <-s.queue:
			s.pending.Add(-item.reserved)
			s.dropped.Add(1)
		default:
			return
		}
	}
}

func (s *RequestDiagnosticService) persist(item diagnosticWrite) {
	defer s.pending.Add(-item.reserved)
	if s.workerCtx.Err() != nil {
		s.dropped.Add(1)
		return
	}
	body, err := json.Marshal(item.snapshot)
	if err != nil || len(body) > maxDiagnosticJSONBytes {
		s.dropped.Add(1)
		return
	}
	var compressed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if _, err = writer.Write(body); err != nil {
		_ = writer.Close()
		s.dropped.Add(1)
		return
	}
	if err = writer.Close(); err != nil {
		s.dropped.Add(1)
		return
	}
	now := time.Now().UTC()
	record := &RequestDiagnosticRecord{
		APIKeyID: item.snapshot.Binding.APIKeyID, UsageRequestID: item.snapshot.Binding.CanonicalID,
		UsageCreatedAt: item.snapshot.Binding.UsageCreatedAt,
		CapturedAt:     now, ExpiresAt: now.Add(time.Duration(s.cfg.RetentionHours) * time.Hour),
		PayloadGzip: compressed.Bytes(), PayloadBytes: len(body),
	}
	ctx, cancel := context.WithTimeout(s.workerCtx, 5*time.Second)
	defer cancel()
	// 编码期间也可能耗尽收尾预算，进入 Save 前再检查，避免发起新的 DB 操作。
	if ctx.Err() != nil {
		s.dropped.Add(1)
		return
	}
	if err := s.repo.Save(ctx, record, s.cfg.MaxStorageBytes); err != nil {
		s.dropped.Add(1)
		logger.LegacyPrintf("service.request_diagnostic", "Persist failed")
	}
}

func (s *RequestDiagnosticService) Get(ctx context.Context, usage *UsageLog) (*RequestDiagnosticDetail, error) {
	missing := &RequestDiagnosticDetail{Status: "not_captured"}
	if s == nil || s.repo == nil || usage == nil {
		return missing, nil
	}
	record, err := s.repo.Get(ctx, usage.APIKeyID, usage.RequestID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return missing, nil
	}
	// 重用 client id 的另一次请求不能借旧用量记录读取不属于它的正文。
	if delta := record.UsageCreatedAt.Sub(usage.CreatedAt); delta < -time.Microsecond || delta > time.Microsecond {
		return missing, nil
	}
	result := &RequestDiagnosticDetail{Status: "expired", CapturedAt: &record.CapturedAt, ExpiresAt: &record.ExpiresAt}
	if !time.Now().Before(record.ExpiresAt) {
		return result, nil
	}
	if record.PayloadBytes == 0 {
		result.Status = "evicted"
		return result, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(record.PayloadGzip))
	if err != nil {
		return nil, errors.New("invalid stored request diagnostic")
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(io.LimitReader(reader, maxDiagnosticJSONBytes+1))
	if err != nil || len(body) > maxDiagnosticJSONBytes || len(body) != record.PayloadBytes {
		return nil, errors.New("invalid stored request diagnostic size")
	}
	var snapshot requestdiagnostic.Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, errors.New("invalid stored request diagnostic payload")
	}
	if snapshot.Binding.APIKeyID != usage.APIKeyID || snapshot.Binding.CanonicalID != usage.RequestID || snapshot.SchemaVersion != requestdiagnostic.SchemaVersion {
		return nil, errors.New("invalid stored request diagnostic binding")
	}
	if delta := snapshot.Binding.UsageCreatedAt.Sub(usage.CreatedAt); delta < -time.Microsecond || delta > time.Microsecond {
		return nil, errors.New("invalid stored request diagnostic timestamp")
	}
	result.Status, result.Payload = "captured", &snapshot
	return result, nil
}

func (s *RequestDiagnosticService) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		// 总 drain 预算包含当前 Save/Cleanup；到期取消它们，而不是提前返回
		// 让 Wire 关闭 DB 后 worker 仍访问存储。仓储必须响应 context 取消。
		s.queueMu.Lock()
		s.stopped = true
		close(s.stop)
		// 先禁止入队再启动取消预算，避免取消后又有排队项进入已退出的 worker。
		timer := time.AfterFunc(requestDiagnosticDrainTimeout, s.cancelWorker)
		s.queueMu.Unlock()
		defer timer.Stop()
		defer s.cancelWorker()
		<-s.done
	})
}
