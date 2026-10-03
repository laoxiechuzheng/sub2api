package requestdiagnostic_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
)

func TestCaptureFinishesAfterCancellationInEitherBindingOrder(t *testing.T) {
	for _, bindFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish_then_bind", true: "bind_then_finish"}[bindFirst], func(t *testing.T) {
			var ready, finished atomic.Int64
			capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{OnFinish: func() { finished.Add(1) }}, requestdiagnostic.Inbound{RequestID: "ops-only"}, func(s *requestdiagnostic.Snapshot) {
				ready.Add(1)
				if s.Binding.CanonicalID != "canonical" || s.Meta.Body.Content != `{"text":"saved"}` {
					t.Error("callback lost safe content or canonical binding")
				}
				// 回调必须在锁外执行，允许调用方重入。
				if _, err := json.Marshal(s); err != nil {
					t.Errorf("snapshot marshal: %v", err)
				}
			})
			ctx, cancel := context.WithCancel(requestdiagnostic.WithCapture(context.Background(), capture))
			capture.SetInbound([]byte(`{"text":"saved"}`))
			cancel()
			if ctx.Err() != context.Canceled || requestdiagnostic.FromContext(ctx) != capture {
				t.Fatal("cancellation must not detach capture")
			}
			created := time.Now()
			if bindFirst {
				capture.BindUsage(1, "canonical", created)
				if ready.Load() != 0 {
					t.Fatal("must not hand off before finish")
				}
				capture.Finish(200)
			} else {
				capture.Finish(200)
				if ready.Load() != 0 || finished.Load() != 1 {
					t.Fatal("finish must release permit without prematurely handing off")
				}
				capture.BindUsage(1, "canonical", created)
			}
			capture.Finish(500)
			capture.Discard()
			capture.BindUsage(1, "canonical", created)
			if ready.Load() != 1 || finished.Load() != 1 {
				t.Fatalf("callbacks not once-only: ready=%d finished=%d", ready.Load(), finished.Load())
			}
		})
	}
}

func TestCaptureCallbacksAreLockFreeAndDiscardWithoutFinishReleasesOnce(t *testing.T) {
	var capture *requestdiagnostic.Capture
	var ready, finished int
	capture = requestdiagnostic.NewCapture(requestdiagnostic.Options{OnFinish: func() {
		finished++
		capture.Finish(500)
	}}, requestdiagnostic.Inbound{}, func(*requestdiagnostic.Snapshot) {
		ready++
		capture.Finish(500)
		capture.Discard()
	})
	capture.SetInbound([]byte(`{"ok":true}`))
	capture.BindUsage(1, "usage", time.Now())
	capture.Finish(200)
	if ready != 1 || finished != 1 {
		t.Fatal("callbacks must run outside lock and exactly once")
	}
	capture = requestdiagnostic.NewCapture(requestdiagnostic.Options{OnFinish: func() {
		finished++
		capture.Discard()
	}}, requestdiagnostic.Inbound{}, func(*requestdiagnostic.Snapshot) { ready++ })
	capture.SetInbound([]byte(`{"ok":true}`))
	capture.Discard()
	capture.Finish(500)
	if capture.BindUsage(1, "usage", time.Now()) || ready != 1 || finished != 2 {
		t.Fatal("discard-before-finish must release without publishing")
	}
}

func TestCopyContextCopiesOnlyCaptureAndKeepsBaseCancellation(t *testing.T) {
	type testKey struct{}
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, nil)
	parent, cancelParent := context.WithCancel(requestdiagnostic.WithCapture(context.WithValue(context.Background(), testKey{}, "parent"), capture))
	base, cancelBase := context.WithCancel(context.WithValue(context.Background(), testKey{}, "base"))
	defer cancelBase()
	cancelParent()
	copied := requestdiagnostic.CopyContext(parent, base)
	if requestdiagnostic.FromContext(copied) != capture || copied.Value(testKey{}) != "base" || copied.Err() != nil {
		t.Fatal("copy imported parent cancellation or unrelated values")
	}
	cancelBase()
	if copied.Err() != context.Canceled {
		t.Fatal("copy lost base cancellation")
	}
	if requestdiagnostic.FromContext(nil) != nil || requestdiagnostic.CopyContext(context.Background(), base) != base {
		t.Fatal("nil/absent capture must be harmless")
	}
}

func TestConcurrentCaptureCallbacksPublishOnlyOnceAndSnapshotNeverChanges(t *testing.T) {
	var ready, finished atomic.Int64
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 2048, OnFinish: func() { finished.Add(1) }}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) {
		snapshot = s
		ready.Add(1)
	})
	capture.SetInbound([]byte(`{"text":"inbound"}`))
	created := time.Now()
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(`{"text":"upstream"}`))
			capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 200, ResponseSummary: []byte(`{"ok":true}`)})
			capture.BindUsage(1, "canonical", created)
			capture.Finish(200)
		}()
	}
	group.Wait()
	if ready.Load() != 1 || finished.Load() != 1 || snapshot == nil {
		t.Fatalf("concurrent callback count: ready=%d finished=%d", ready.Load(), finished.Load())
	}
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			capture.SetInbound([]byte(`{"changed":true}`))
			capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(`{"changed":true}`))
			capture.EndAttempt(1, requestdiagnostic.AttemptResult{Status: 500})
			capture.Drop("dropped")
			capture.Discard()
			capture.Finish(500)
			capture.BindUsage(2, "other", created)
			if _, err := json.Marshal(snapshot); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	after, err := json.Marshal(snapshot)
	if err != nil || string(before) != string(after) {
		t.Fatal("completed snapshot changed after handoff")
	}
}
