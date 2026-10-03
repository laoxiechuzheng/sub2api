package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 仅覆盖详情读取需要的方法，避免复制无关仓库实现。
type usageDiagnosticHandlerUsageRepo struct {
	service.UsageLogRepository
	getByID func(context.Context, int64) (*service.UsageLog, error)
	calls   int
}

func (r *usageDiagnosticHandlerUsageRepo) GetByID(ctx context.Context, id int64) (*service.UsageLog, error) {
	r.calls++
	if r.getByID == nil {
		panic("unexpected GetByID call")
	}
	return r.getByID(ctx, id)
}

type usageDiagnosticHandlerCaptureRepo struct {
	service.RequestDiagnosticRepository
	get   func(context.Context, int64, string) (*service.RequestDiagnosticRecord, error)
	calls int
}

func (r *usageDiagnosticHandlerCaptureRepo) Get(ctx context.Context, apiKeyID int64, requestID string) (*service.RequestDiagnosticRecord, error) {
	r.calls++
	if r.get == nil {
		panic("unexpected diagnostic Get call")
	}
	return r.get(ctx, apiKeyID, requestID)
}

func (r *usageDiagnosticHandlerCaptureRepo) Cleanup(context.Context) error { return nil }

type usageDiagnosticHandlerEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Usage          dto.AdminUsageLog                `json:"usage"`
		CaptureEnabled bool                             `json:"capture_enabled"`
		RetentionHours int                              `json:"retention_hours"`
		Diagnostic     *service.RequestDiagnosticDetail `json:"diagnostic"`
	} `json:"data"`
}

func newUsageDiagnosticHandlerForTest(t *testing.T, usageRepo *usageDiagnosticHandlerUsageRepo, captureRepo *usageDiagnosticHandlerCaptureRepo, enabled bool) *UsageHandler {
	t.Helper()
	h := NewUsageHandler(service.NewUsageService(usageRepo, nil, nil, nil), nil, nil, nil)
	var repo service.RequestDiagnosticRepository
	if captureRepo != nil {
		repo = captureRepo
	}
	svc := service.NewRequestDiagnosticService(repo, &config.Config{RequestDiagnostics: config.RequestDiagnosticsConfig{
		Enabled: enabled, RetentionHours: 24, MaxActive: 1, MaxEntryBytes: 4096,
		MaxPendingBytes: 4096, MaxStorageBytes: 8192,
	}})
	t.Cleanup(svc.Stop)
	h.SetRequestDiagnosticService(svc)
	return h
}

func serveUsageDiagnosticHandler(t *testing.T, h *UsageHandler, id string, role any, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// 这里只验证 handler 自身的第二层权限，不冒充真实认证中间件。
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		if role != nil {
			c.Set(string(middleware.ContextKeyUserRole), role)
		}
		c.Next()
	})
	router.GET("/api/v1/admin/usage/:id/diagnostic", h.Diagnostic)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/"+id+"/diagnostic"+query, nil)
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeUsageDiagnosticHandlerResponse(t *testing.T, recorder *httptest.ResponseRecorder) usageDiagnosticHandlerEnvelope {
	t.Helper()
	var result usageDiagnosticHandlerEnvelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	return result
}

func requireUsageDiagnosticNoStore(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", recorder.Header().Get("Pragma"))
}

func usageDiagnosticHandlerUsage() *service.UsageLog {
	forwarded := "forwarded-model"
	return &service.UsageLog{
		ID: 42, UserID: 7, APIKeyID: 19, AccountID: 23,
		RequestID: "canonical-usage-request", Model: "billing-model", RequestedModel: "client-model", UpstreamModel: &forwarded,
		CreatedAt: time.Date(2026, 10, 3, 9, 0, 0, 123000000, time.UTC),
	}
}

func usageDiagnosticHandlerRecord(t *testing.T, usage *service.UsageLog, mutate func(*requestdiagnostic.Snapshot)) *service.RequestDiagnosticRecord {
	t.Helper()
	inboundText := `{"messages":[{"role":"user","content":"diagnostic fixture prompt"}]}`
	upstreamText := `{"model":"forwarded-model","input":"final upstream fixture"}`
	summaryText := `{"model":"response-model"}`
	snapshot := requestdiagnostic.Snapshot{
		SchemaVersion: requestdiagnostic.SchemaVersion,
		Binding: requestdiagnostic.UsageBinding{
			APIKeyID: usage.APIKeyID, CanonicalID: usage.RequestID, UsageCreatedAt: usage.CreatedAt,
		},
		Meta: requestdiagnostic.Inbound{
			RequestID: "ops-correlation-id", ClientRequestID: "client-id-not-canonical",
			Endpoint: "/v1/responses", Method: http.MethodPost, StartedAt: usage.CreatedAt,
			Body: requestdiagnostic.Payload{Content: inboundText, OriginalBytes: len(inboundText), StoredBytes: len(inboundText)},
		},
		Status: http.StatusOK, CaptureStatus: requestdiagnostic.CaptureStatusCaptured, CapturedAt: usage.CreatedAt,
		Attempts: []requestdiagnostic.Attempt{{
			ID: 1, AccountID: usage.AccountID, Platform: "openai", Model: "forwarded-model",
			Endpoint: "/v1/responses", Method: http.MethodPost, StartedAt: usage.CreatedAt,
			Status: http.StatusOK, UpstreamRequestID: "upstream-fixture-request",
			Body:            requestdiagnostic.Payload{Content: upstreamText, OriginalBytes: len(upstreamText), StoredBytes: len(upstreamText)},
			ResponseSummary: requestdiagnostic.Payload{Content: summaryText, OriginalBytes: len(summaryText), StoredBytes: len(summaryText)},
		}},
		StoredBytes: len(inboundText) + len(upstreamText) + len(summaryText),
	}
	if mutate != nil {
		mutate(&snapshot)
	}
	body, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err = writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return &service.RequestDiagnosticRecord{
		APIKeyID: usage.APIKeyID, UsageRequestID: usage.RequestID, UsageCreatedAt: usage.CreatedAt,
		CapturedAt: usage.CreatedAt, ExpiresAt: time.Now().Add(time.Hour),
		PayloadGzip: compressed.Bytes(), PayloadBytes: len(body),
	}
}

func TestUsageDiagnosticHandlerRejectsNonAdminBeforeRepositoryAccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		role any
	}{
		{name: "no_role"},
		{name: "user", role: service.RoleUser},
		{name: "empty_role", role: ""},
		{name: "wrong_type", role: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &usageDiagnosticHandlerUsageRepo{}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			recorder := serveUsageDiagnosticHandler(t, h, "42", tc.role, "")
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Equal(t, "Admin access required", decodeUsageDiagnosticHandlerResponse(t, recorder).Message)
			require.Zero(t, usageRepo.calls)
			require.Zero(t, captureRepo.calls)
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerRejectsInvalidIDBeforeRepositoryAccess(t *testing.T) {
	for _, id := range []string{"abc", "0", "-1", "1.5", "9223372036854775808"} {
		t.Run(id, func(t *testing.T) {
			usageRepo := &usageDiagnosticHandlerUsageRepo{}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			recorder := serveUsageDiagnosticHandler(t, h, id, service.RoleAdmin, "")
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "Invalid id", decodeUsageDiagnosticHandlerResponse(t, recorder).Message)
			require.Zero(t, usageRepo.calls)
			require.Zero(t, captureRepo.calls)
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerReturnsNotFoundBeforeCaptureLookup(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "nil_usage"},
		{name: "repository_not_found", err: service.ErrUsageLogNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(context.Context, int64) (*service.UsageLog, error) { return nil, tc.err }}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			recorder := serveUsageDiagnosticHandler(t, h, "404", service.RoleAdmin, "")
			require.Equal(t, http.StatusNotFound, recorder.Code)
			require.Equal(t, 1, usageRepo.calls)
			require.Zero(t, captureRepo.calls)
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerLoadsUsageThenExactCanonicalSnapshot(t *testing.T) {
	usage := usageDiagnosticHandlerUsage()
	record := usageDiagnosticHandlerRecord(t, usage, nil)
	order := []string{}
	var requestContext context.Context
	usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(ctx context.Context, id int64) (*service.UsageLog, error) {
		require.Equal(t, usage.ID, id)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Positive(t, time.Until(deadline))
		require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
		requestContext = ctx
		order = append(order, "usage")
		return usage, nil
	}}
	captureRepo := &usageDiagnosticHandlerCaptureRepo{get: func(ctx context.Context, apiKeyID int64, canonicalID string) (*service.RequestDiagnosticRecord, error) {
		require.Equal(t, requestContext, ctx)
		require.Equal(t, usage.APIKeyID, apiKeyID)
		require.Equal(t, usage.RequestID, canonicalID)
		order = append(order, "capture")
		return record, nil
	}}
	h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
	// 查询参数不能改变权威 usage 行提供的关联键。
	recorder := serveUsageDiagnosticHandler(t, h, "42", service.RoleAdmin, "?api_key_id=999&request_id=client-id-not-canonical")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"usage", "capture"}, order)
	requireUsageDiagnosticNoStore(t, recorder)
	result := decodeUsageDiagnosticHandlerResponse(t, recorder)
	require.Zero(t, result.Code)
	require.True(t, result.Data.CaptureEnabled)
	require.Equal(t, 24, result.Data.RetentionHours)
	require.Equal(t, usage.ID, result.Data.Usage.ID)
	require.Equal(t, usage.RequestID, result.Data.Usage.RequestID)
	require.Equal(t, "client-model", result.Data.Usage.Model)
	require.Equal(t, "captured", result.Data.Diagnostic.Status)
	require.NotNil(t, result.Data.Diagnostic.Payload)
	require.Equal(t, usage.APIKeyID, result.Data.Diagnostic.Payload.Binding.APIKeyID)
	require.Equal(t, usage.RequestID, result.Data.Diagnostic.Payload.Binding.CanonicalID)
	require.Equal(t, usage.CreatedAt, result.Data.Diagnostic.Payload.Binding.UsageCreatedAt)
	require.Equal(t, "ops-correlation-id", result.Data.Diagnostic.Payload.Meta.RequestID)
	require.Contains(t, result.Data.Diagnostic.Payload.Meta.Body.Content, "diagnostic fixture prompt")
	require.Len(t, result.Data.Diagnostic.Payload.Attempts, 1)
	require.Contains(t, result.Data.Diagnostic.Payload.Attempts[0].Body.Content, "final upstream fixture")
	require.Contains(t, result.Data.Diagnostic.Payload.Attempts[0].ResponseSummary.Content, "response-model")
}

func TestUsageDiagnosticHandlerMissingDisabledAndUnconfigured(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		configured bool
		withRepo   bool
	}{
		{name: "old_record", enabled: true, configured: true, withRepo: true},
		{name: "capture_disabled", configured: true, withRepo: true},
		{name: "repository_unconfigured", enabled: true, configured: true},
		{name: "service_unconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := usageDiagnosticHandlerUsage()
			usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(context.Context, int64) (*service.UsageLog, error) { return usage, nil }}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{get: func(context.Context, int64, string) (*service.RequestDiagnosticRecord, error) { return nil, nil }}
			h := NewUsageHandler(service.NewUsageService(usageRepo, nil, nil, nil), nil, nil, nil)
			if tc.configured {
				var repo *usageDiagnosticHandlerCaptureRepo
				if tc.withRepo {
					repo = captureRepo
				}
				h = newUsageDiagnosticHandlerForTest(t, usageRepo, repo, tc.enabled)
			}
			recorder := serveUsageDiagnosticHandler(t, h, "42", service.RoleAdmin, "")
			require.Equal(t, http.StatusOK, recorder.Code)
			result := decodeUsageDiagnosticHandlerResponse(t, recorder)
			require.Equal(t, usage.ID, result.Data.Usage.ID)
			require.Equal(t, "not_captured", result.Data.Diagnostic.Status)
			require.Nil(t, result.Data.Diagnostic.Payload)
			require.Equal(t, tc.enabled && tc.withRepo, result.Data.CaptureEnabled)
			require.Equal(t, 24, result.Data.RetentionHours)
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerDoesNotBindReusedCanonicalIDToAnotherUsageTime(t *testing.T) {
	for _, delta := range []time.Duration{time.Millisecond, time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			usage := usageDiagnosticHandlerUsage()
			record := usageDiagnosticHandlerRecord(t, usage, nil)
			// 外层存储记录属于另一条同 client id 请求，毫秒差也不能跨绑正文。
			record.UsageCreatedAt = usage.CreatedAt.Add(delta)
			usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(context.Context, int64) (*service.UsageLog, error) { return usage, nil }}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{get: func(context.Context, int64, string) (*service.RequestDiagnosticRecord, error) { return record, nil }}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			recorder := serveUsageDiagnosticHandler(t, h, "42", service.RoleAdmin, "")
			require.Equal(t, http.StatusOK, recorder.Code)
			result := decodeUsageDiagnosticHandlerResponse(t, recorder)
			require.Equal(t, "not_captured", result.Data.Diagnostic.Status)
			require.Nil(t, result.Data.Diagnostic.Payload)
			require.NotContains(t, recorder.Body.String(), "diagnostic fixture prompt")
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerRejectsMismatchedPayloadBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*requestdiagnostic.Snapshot)
	}{
		{name: "api_key", mutate: func(s *requestdiagnostic.Snapshot) { s.Binding.APIKeyID++ }},
		{name: "canonical_id", mutate: func(s *requestdiagnostic.Snapshot) { s.Binding.CanonicalID = "another-request" }},
		{name: "created_at", mutate: func(s *requestdiagnostic.Snapshot) {
			s.Binding.UsageCreatedAt = s.Binding.UsageCreatedAt.Add(time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := usageDiagnosticHandlerUsage()
			record := usageDiagnosticHandlerRecord(t, usage, tc.mutate)
			usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(context.Context, int64) (*service.UsageLog, error) { return usage, nil }}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{get: func(context.Context, int64, string) (*service.RequestDiagnosticRecord, error) { return record, nil }}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			recorder := serveUsageDiagnosticHandler(t, h, "42", service.RoleAdmin, "")
			require.Equal(t, http.StatusInternalServerError, recorder.Code)
			require.Equal(t, "Failed to load request diagnostics", decodeUsageDiagnosticHandlerResponse(t, recorder).Message)
			require.NotContains(t, recorder.Body.String(), "diagnostic fixture prompt")
			require.NotContains(t, recorder.Body.String(), "invalid stored")
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerRepositoryErrorsReturnGenericResponses(t *testing.T) {
	for _, source := range []string{"usage", "capture"} {
		t.Run(source, func(t *testing.T) {
			usage := usageDiagnosticHandlerUsage()
			secretError := errors.New("database failure token=diagnostic-test-secret")
			usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(context.Context, int64) (*service.UsageLog, error) {
				if source == "usage" {
					return nil, secretError
				}
				return usage, nil
			}}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{get: func(context.Context, int64, string) (*service.RequestDiagnosticRecord, error) {
				return nil, secretError
			}}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			var logged bytes.Buffer
			previousLogOutput := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(previousLogOutput) })
			recorder := serveUsageDiagnosticHandler(t, h, "42", service.RoleAdmin, "")
			require.NotContains(t, logged.String(), "diagnostic-test-secret")
			require.NotContains(t, logged.String(), "database failure")
			require.Equal(t, http.StatusInternalServerError, recorder.Code)
			result := decodeUsageDiagnosticHandlerResponse(t, recorder)
			require.Equal(t, http.StatusInternalServerError, result.Code)
			if source == "usage" {
				require.Equal(t, "internal error", result.Message)
				require.Zero(t, captureRepo.calls)
			} else {
				require.Equal(t, "Failed to load request diagnostics", result.Message)
			}
			require.NotContains(t, recorder.Body.String(), "diagnostic-test-secret")
			require.NotContains(t, recorder.Body.String(), "database failure")
			require.Nil(t, result.Data.Diagnostic)
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}

func TestUsageDiagnosticHandlerExpiredAndEvictedDoNotReturnBody(t *testing.T) {
	for _, status := range []string{"expired", "evicted"} {
		t.Run(status, func(t *testing.T) {
			usage := usageDiagnosticHandlerUsage()
			record := usageDiagnosticHandlerRecord(t, usage, nil)
			if status == "expired" {
				record.ExpiresAt = time.Now().Add(-time.Minute)
			} else {
				record.PayloadBytes = 0
				record.PayloadGzip = nil
			}
			usageRepo := &usageDiagnosticHandlerUsageRepo{getByID: func(context.Context, int64) (*service.UsageLog, error) { return usage, nil }}
			captureRepo := &usageDiagnosticHandlerCaptureRepo{get: func(context.Context, int64, string) (*service.RequestDiagnosticRecord, error) { return record, nil }}
			h := newUsageDiagnosticHandlerForTest(t, usageRepo, captureRepo, true)
			recorder := serveUsageDiagnosticHandler(t, h, "42", service.RoleAdmin, "")
			require.Equal(t, http.StatusOK, recorder.Code)
			result := decodeUsageDiagnosticHandlerResponse(t, recorder)
			require.Equal(t, status, result.Data.Diagnostic.Status)
			require.NotNil(t, result.Data.Diagnostic.CapturedAt)
			require.NotNil(t, result.Data.Diagnostic.ExpiresAt)
			require.Nil(t, result.Data.Diagnostic.Payload)
			require.NotContains(t, recorder.Body.String(), "diagnostic fixture prompt")
			requireUsageDiagnosticNoStore(t, recorder)
		})
	}
}
