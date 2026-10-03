package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type usageDiagnosticRouteUsageRepo struct {
	service.UsageLogRepository
	usage *service.UsageLog
	calls int
}

func (r *usageDiagnosticRouteUsageRepo) GetByID(_ context.Context, id int64) (*service.UsageLog, error) {
	r.calls++
	if r.usage == nil || id != r.usage.ID {
		return nil, service.ErrUsageLogNotFound
	}
	return r.usage, nil
}

type usageDiagnosticRouteUserRepo struct {
	service.UserRepository
	users map[int64]*service.User
}

func (r *usageDiagnosticRouteUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	user, ok := r.users[id]
	if !ok {
		return nil, service.ErrUserNotFound
	}
	copy := *user
	return &copy, nil
}

// UserService 在真实认证读取后会补头像，测试无需实际头像仓库。
func (r *usageDiagnosticRouteUserRepo) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

func newUsageDiagnosticRouteHandler(repo *usageDiagnosticRouteUsageRepo) *handler.Handlers {
	usageHandler := adminhandler.NewUsageHandler(service.NewUsageService(repo, nil, nil, nil), nil, nil, nil)
	usageHandler.SetRequestDiagnosticService(service.NewRequestDiagnosticService(nil, &config.Config{
		RequestDiagnostics: config.RequestDiagnosticsConfig{Enabled: false, RetentionHours: 24},
	}))
	return &handler.Handlers{Admin: &handler.AdminHandlers{Usage: usageHandler}}
}

func TestUsageDiagnosticRouteRetainsAdminGroupMiddlewareGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usageRepo := &usageDiagnosticRouteUsageRepo{}
	router := gin.New()
	admin := router.Group("/api/v1/admin")
	gateCalls := 0
	// 此假 gate 只证明注册路径沿用父组中间件，不等同真实认证验证。
	admin.Use(func(c *gin.Context) {
		gateCalls++
		middleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "route test gate")
	})
	registerUsageRoutes(admin, newUsageDiagnosticRouteHandler(usageRepo))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/42/diagnostic", nil))
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Equal(t, 1, gateCalls)
	require.Zero(t, usageRepo.calls)
	require.Contains(t, recorder.Body.String(), "route test gate")
}

func TestUsageDiagnosticRouteRealAdminAuthJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "usage-diagnostic-route-test-signing-key", ExpireHour: 1}}
	authService := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	users := map[int64]*service.User{
		1: {ID: 1, Email: "admin@example.com", Role: service.RoleAdmin, Status: service.StatusActive, TokenVersion: 3, TokenVersionResolved: true},
		2: {ID: 2, Email: "user@example.com", Role: service.RoleUser, Status: service.StatusActive, TokenVersion: 3, TokenVersionResolved: true},
		3: {ID: 3, Email: "disabled-admin@example.com", Role: service.RoleAdmin, Status: service.StatusDisabled, TokenVersion: 3, TokenVersionResolved: true},
		4: {ID: 4, Email: "demoted@example.com", Role: service.RoleUser, Status: service.StatusActive, TokenVersion: 3, TokenVersionResolved: true},
	}
	userService := service.NewUserService(&usageDiagnosticRouteUserRepo{users: users}, nil, nil, nil)
	mint := func(user *service.User) string {
		t.Helper()
		token, err := authService.GenerateToken(context.Background(), user)
		require.NoError(t, err)
		return token
	}
	adminToken := mint(users[1])
	userToken := mint(users[2])
	disabledToken := mint(users[3])
	// 声明仍为 admin，但数据库角色已变为 user，不能只相信 token 声明。
	demotedClaim := *users[4]
	demotedClaim.Role = service.RoleAdmin
	demotedToken := mint(&demotedClaim)
	revokedClaim := *users[1]
	revokedClaim.TokenVersion--
	revokedToken := mint(&revokedClaim)

	for _, tc := range []struct {
		name       string
		auth       string
		wantStatus int
		wantReason string
		wantCalls  int
	}{
		{name: "unauthenticated", wantStatus: http.StatusUnauthorized, wantReason: "UNAUTHORIZED"},
		{name: "invalid_jwt", auth: "Bearer invalid-token", wantStatus: http.StatusUnauthorized, wantReason: "INVALID_TOKEN"},
		{name: "ordinary_user", auth: "Bearer " + userToken, wantStatus: http.StatusForbidden, wantReason: "FORBIDDEN"},
		{name: "demoted_admin", auth: "Bearer " + demotedToken, wantStatus: http.StatusForbidden, wantReason: "FORBIDDEN"},
		{name: "disabled_admin", auth: "Bearer " + disabledToken, wantStatus: http.StatusUnauthorized, wantReason: "USER_INACTIVE"},
		{name: "revoked_admin", auth: "Bearer " + revokedToken, wantStatus: http.StatusUnauthorized, wantReason: "TOKEN_REVOKED"},
		{name: "active_admin", auth: "Bearer " + adminToken, wantStatus: http.StatusOK, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &usageDiagnosticRouteUsageRepo{usage: &service.UsageLog{
				ID: 42, UserID: 7, APIKeyID: 19, RequestID: "canonical-route-usage", Model: "test-model",
				CreatedAt: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC),
			}}
			router := gin.New()
			admin := router.Group("/api/v1/admin")
			// 使用真实 JWT/数据库角色校验；范围仅为认证与 usage 路由，不涵盖完整审计/合规链。
			admin.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(authService, userService, nil, nil)))
			registerUsageRoutes(admin, newUsageDiagnosticRouteHandler(usageRepo))
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/42/diagnostic", nil)
			if tc.auth != "" {
				request.Header.Set("Authorization", tc.auth)
			}
			router.ServeHTTP(recorder, request)
			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			require.Equal(t, tc.wantCalls, usageRepo.calls)
			if tc.wantReason != "" {
				require.Contains(t, recorder.Body.String(), tc.wantReason)
				return
			}
			var response struct {
				Code int `json:"code"`
				Data struct {
					CaptureEnabled bool `json:"capture_enabled"`
					Diagnostic     struct {
						Status  string          `json:"status"`
						Payload json.RawMessage `json:"payload"`
					} `json:"diagnostic"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.Zero(t, response.Code)
			require.False(t, response.Data.CaptureEnabled)
			require.Equal(t, "not_captured", response.Data.Diagnostic.Status)
			require.Empty(t, response.Data.Diagnostic.Payload)
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		})
	}
}
