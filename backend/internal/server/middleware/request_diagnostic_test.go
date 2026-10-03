//go:build unit

package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 保存成功用通道通知，测试不轮询、不依赖 sleep 或后台 worker 的调度速度。
type requestDiagnosticMiddlewareRepo struct {
	saved chan *service.RequestDiagnosticRecord
}

func (r *requestDiagnosticMiddlewareRepo) Save(ctx context.Context, record *service.RequestDiagnosticRecord, _ int64) error {
	copied := *record
	copied.PayloadGzip = append([]byte(nil), record.PayloadGzip...)
	select {
	case r.saved <- &copied:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *requestDiagnosticMiddlewareRepo) Get(context.Context, int64, string) (*service.RequestDiagnosticRecord, error) {
	return nil, nil
}

func (r *requestDiagnosticMiddlewareRepo) Cleanup(context.Context) error { return nil }

func newRequestDiagnosticMiddlewareService(t *testing.T, enabled bool, limit int) (*service.RequestDiagnosticService, *requestDiagnosticMiddlewareRepo) {
	t.Helper()
	repo := &requestDiagnosticMiddlewareRepo{saved: make(chan *service.RequestDiagnosticRecord, 4)}
	cfg := &config.Config{RequestDiagnostics: config.RequestDiagnosticsConfig{
		Enabled: enabled, RetentionHours: 24, MaxEntryBytes: limit, MaxActive: 4,
		MaxPendingBytes: int64(limit) * 8, MaxStorageBytes: 8 << 20,
	}}
	svc := service.NewRequestDiagnosticService(repo, cfg)
	t.Cleanup(svc.Stop)
	return svc, repo
}

func awaitRequestDiagnosticMiddlewareSnapshot(t *testing.T, repo *requestDiagnosticMiddlewareRepo) *requestdiagnostic.Snapshot {
	t.Helper()
	var record *service.RequestDiagnosticRecord
	select {
	case record = <-repo.saved:
	case <-time.After(time.Second):
		t.Fatal("diagnostic record was not saved after Finish and BindUsage")
	}
	reader, err := gzip.NewReader(bytes.NewReader(record.PayloadGzip))
	require.NoError(t, err)
	payload, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, len(payload), record.PayloadBytes)
	var snapshot requestdiagnostic.Snapshot
	require.NoError(t, json.Unmarshal(payload, &snapshot))
	require.Equal(t, record.APIKeyID, snapshot.Binding.APIKeyID)
	require.Equal(t, record.UsageRequestID, snapshot.Binding.CanonicalID)
	return &snapshot
}

type requestDiagnosticMiddlewareBody struct {
	reader io.Reader
	reads  int
	closed bool
}

func (b *requestDiagnosticMiddlewareBody) Read(p []byte) (int, error) {
	b.reads++
	return b.reader.Read(p)
}

func (b *requestDiagnosticMiddlewareBody) Close() error {
	b.closed = true
	return nil
}

func requestDiagnosticMiddlewareAuth(c *gin.Context) {
	c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 17, Group: &service.Group{Platform: service.PlatformAnthropic}})
	c.Next()
}

func TestRequestDiagnosticMiddlewareCapturesOriginalJSONAndFinishBindHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, bindFirst := range []bool{false, true} {
		name := "finish_before_bind"
		if bindFirst {
			name = "bind_before_finish"
		}
		t.Run(name, func(t *testing.T) {
			svc, repo := newRequestDiagnosticMiddlewareService(t, true, 1<<20)
			originalJSON := `{"model":"client-original-model","input":"原始请求内容","api_key":"fixture-inbound-secret"}`
			body := &requestDiagnosticMiddlewareBody{reader: strings.NewReader(originalJSON)}
			var capture *requestdiagnostic.Capture
			createdAt := time.Now().UTC()
			router := gin.New()
			router.Use(func(c *gin.Context) {
				require.Nil(t, requestdiagnostic.FromContext(c.Request.Context()), "capture must not exist before authentication")
				require.Zero(t, body.reads)
				requestDiagnosticMiddlewareAuth(c)
			})
			router.Use(RequestDiagnostic(svc))
			router.POST("/v1/responses", func(c *gin.Context) {
				require.Zero(t, body.reads, "diagnostic middleware must not preread the original body")
				require.NotSame(t, body, c.Request.Body)
				capture = requestdiagnostic.FromContext(c.Request.Context())
				require.NotNil(t, capture)
				require.Equal(t, service.PlatformAnthropic, service.RequestDiagnosticPlatformFromContext(c.Request.Context()))
				// 首次 Read 的原始字节与 EOF 都保持不变，再由正常 handler 顺序消费剩余正文。
				first := make([]byte, 1)
				n, err := c.Request.Body.Read(first)
				require.NoError(t, err)
				require.Equal(t, 1, n)
				rest, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, originalJSON, string(append(first, rest...)))
				// 模拟后续模型改写及共享 reader 再次读取；第一次原文必须仍优先。
				c.Request.Body = pkghttputil.NewPrereadBody([]byte(`{"model":"rewritten-upstream-model"}`))
				_, err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
				require.NoError(t, err)
				capture.SetInbound([]byte(`{"model":"rewritten-upstream-model"}`))
				if bindFirst {
					require.True(t, capture.BindUsage(17, "client:middleware-canonical", createdAt))
				}
				c.Status(http.StatusAccepted)
			})
			ctx := context.WithValue(context.Background(), ctxkey.RequestID, "ops-request-fixture")
			ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client-request-fixture")
			req := httptest.NewRequest(http.MethodPost, "/v1/responses?ignored=secret", body).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			req.Header.Set("User-Agent", "fixture-client/1")
			req.ContentLength = int64(len(originalJSON))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusAccepted, rec.Code)
			require.NotNil(t, capture)
			if !bindFirst {
				select {
				case <-repo.saved:
					t.Fatal("Finish without canonical usage binding must not save a record")
				default:
				}
				require.True(t, capture.BindUsage(17, "client:middleware-canonical", createdAt))
			}
			snapshot := awaitRequestDiagnosticMiddlewareSnapshot(t, repo)
			require.Equal(t, http.StatusAccepted, snapshot.Status)
			require.Equal(t, "client-original-model", gjson.Get(snapshot.Meta.Body.Content, "model").String())
			require.Equal(t, "原始请求内容", gjson.Get(snapshot.Meta.Body.Content, "input").String())
			require.NotContains(t, snapshot.Meta.Body.Content, "fixture-inbound-secret")
			require.True(t, snapshot.Meta.Body.Redacted)
			require.Equal(t, "ops-request-fixture", snapshot.Meta.RequestID)
			require.Equal(t, "client-request-fixture", snapshot.Meta.ClientRequestID)
			require.Equal(t, "client:middleware-canonical", snapshot.Binding.CanonicalID)
			require.Equal(t, "/v1/responses", snapshot.Meta.Endpoint)
			require.Equal(t, "fixture-client/1", snapshot.Meta.UserAgent)
			require.False(t, body.closed, "middleware must not change request body ownership")
			capture.Finish(http.StatusInternalServerError)
			require.True(t, capture.BindUsage(17, "client:middleware-canonical", createdAt))
			svc.Stop() // 等 worker 排空后，确定重复 Finish/Bind 没有重复保存。
			select {
			case <-repo.saved:
				t.Fatal("Finish/Bind handshake must save exactly once")
			default:
			}
		})
	}
}

func TestRequestDiagnosticMiddlewareOverBudgetOmitsBodyWithoutTruncatingHandlerRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = 1024
	svc, repo := newRequestDiagnosticMiddlewareService(t, true, limit)
	payload := `{"model":"client-large-model","input":"` + strings.Repeat("完整内容", 1024) + `"}`
	body := &requestDiagnosticMiddlewareBody{reader: strings.NewReader(payload)}
	router := gin.New()
	router.Use(requestDiagnosticMiddlewareAuth, RequestDiagnostic(svc))
	router.POST("/v1/responses", func(c *gin.Context) {
		require.Zero(t, body.reads)
		read, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.Equal(t, payload, string(read), "diagnostic limits must not impose a new ingress body limit")
		capture := requestdiagnostic.FromContext(c.Request.Context())
		require.NotNil(t, capture)
		require.True(t, capture.BindUsage(17, "client:oversized", time.Now()))
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(payload))
	router.ServeHTTP(httptest.NewRecorder(), req)
	snapshot := awaitRequestDiagnosticMiddlewareSnapshot(t, repo)
	require.Equal(t, requestdiagnostic.OmittedTooLarge, snapshot.Meta.Body.OmittedReason)
	require.Empty(t, snapshot.Meta.Body.Content)
	require.Equal(t, len(payload), snapshot.Meta.Body.OriginalBytes)
	require.LessOrEqual(t, snapshot.StoredBytes, limit)
	require.False(t, body.closed)
}

func TestRequestDiagnosticMiddlewareGzipCapturesDecodedOriginalJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, repo := newRequestDiagnosticMiddlewareService(t, true, 1<<20)
	payload := `{"model":"gzip-client-model","input":"解压前后保留原文","api_key":"fixture-gzip-secret"}`
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, err := io.WriteString(zip, payload)
	require.NoError(t, err)
	require.NoError(t, zip.Close())
	body := &requestDiagnosticMiddlewareBody{reader: bytes.NewReader(compressed.Bytes())}
	router := gin.New()
	router.Use(requestDiagnosticMiddlewareAuth, RequestDiagnostic(svc))
	router.POST("/v1/responses", func(c *gin.Context) {
		require.Zero(t, body.reads)
		require.Same(t, body, c.Request.Body, "middleware must not wrap compressed bytes as inbound JSON")
		decoded, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		require.Equal(t, payload, string(decoded))
		require.Empty(t, c.Request.Header.Get("Content-Encoding"))
		require.Equal(t, int64(len(payload)), c.Request.ContentLength)
		capture := requestdiagnostic.FromContext(c.Request.Context())
		require.NotNil(t, capture)
		capture.SetInbound([]byte(`{"model":"rewritten-after-gzip"}`))
		require.True(t, capture.BindUsage(17, "client:gzip", time.Now()))
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.ContentLength = int64(compressed.Len())
	router.ServeHTTP(httptest.NewRecorder(), req)
	snapshot := awaitRequestDiagnosticMiddlewareSnapshot(t, repo)
	require.Equal(t, "gzip-client-model", gjson.Get(snapshot.Meta.Body.Content, "model").String())
	require.Equal(t, "解压前后保留原文", gjson.Get(snapshot.Meta.Body.Content, "input").String())
	require.Equal(t, len(payload), snapshot.Meta.Body.OriginalBytes)
	require.NotContains(t, snapshot.Meta.Body.Content, "fixture-gzip-secret")
	require.True(t, snapshot.Meta.Body.Redacted)
}

func TestRequestDiagnosticMiddlewareNonJSONDoesNotWrapOrReadBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, contentType := range []string{"application/octet-stream", "multipart/form-data; boundary=fixture", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			svc, repo := newRequestDiagnosticMiddlewareService(t, true, 1<<20)
			body := &requestDiagnosticMiddlewareBody{reader: strings.NewReader("fixture-binary-body")}
			router := gin.New()
			router.Use(requestDiagnosticMiddlewareAuth, RequestDiagnostic(svc))
			router.POST("/v1/responses", func(c *gin.Context) {
				require.Same(t, body, c.Request.Body)
				require.Zero(t, body.reads)
				capture := requestdiagnostic.FromContext(c.Request.Context())
				require.NotNil(t, capture)
				require.True(t, capture.BindUsage(17, "client:binary", time.Now()))
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
			req.Header.Set("Content-Type", contentType)
			req.ContentLength = 19
			router.ServeHTTP(httptest.NewRecorder(), req)
			snapshot := awaitRequestDiagnosticMiddlewareSnapshot(t, repo)
			require.Equal(t, requestdiagnostic.OmittedBinaryData, snapshot.Meta.Body.OmittedReason)
			require.Empty(t, snapshot.Meta.Body.Content)
			require.Equal(t, 19, snapshot.Meta.Body.OriginalBytes)
			require.Zero(t, body.reads)
			require.False(t, body.closed)
		})
	}
}

func TestRequestDiagnosticMiddlewareDisabledOrUnauthenticatedIsPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"disabled", "no_auth", "ops_fallback_only", "get", "delete"} {
		t.Run(mode, func(t *testing.T) {
			svc, repo := newRequestDiagnosticMiddlewareService(t, mode != "disabled", 1<<20)
			body := &requestDiagnosticMiddlewareBody{reader: strings.NewReader(`{"model":"untouched"}`)}
			router := gin.New()
			if mode != "no_auth" && mode != "ops_fallback_only" {
				router.Use(requestDiagnosticMiddlewareAuth)
			}
			if mode == "ops_fallback_only" {
				router.Use(func(c *gin.Context) { SetOpsFallbackAPIKey(c, &service.APIKey{ID: 17}); c.Next() })
			}
			router.Use(RequestDiagnostic(svc))
			method := http.MethodPost
			if mode == "get" {
				method = http.MethodGet
			} else if mode == "delete" {
				method = http.MethodDelete
			}
			router.Handle(method, "/v1/responses", func(c *gin.Context) {
				require.Nil(t, requestdiagnostic.FromContext(c.Request.Context()))
				require.Same(t, body, c.Request.Body)
				require.Zero(t, body.reads)
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(method, "/v1/responses", body)
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(httptest.NewRecorder(), req)
			svc.Stop() // 等后台 worker 收尾后再断言确实没有创建可保存的采集。
			select {
			case <-repo.saved:
				t.Fatal("disabled or unauthenticated requests must not persist diagnostics")
			default:
			}
			require.Zero(t, body.reads)
			require.False(t, body.closed)
		})
	}
}
