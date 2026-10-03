package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *UsageHandler) SetRequestDiagnosticService(svc *service.RequestDiagnosticService) {
	h.requestDiagnostics = svc
}

// Diagnostic 只由管理员路由注册；列表和普通用户接口不返回正文。
func (h *UsageHandler) Diagnostic(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	// 双重校验，避免未来误挂到普通用户路由时暴露正文。
	if role, ok := middleware.GetUserRoleFromContext(c); !ok || role != service.RoleAdmin {
		response.Forbidden(c, "Admin access required")
		return
	}
	id, ok := parsePositiveIDParam(c, "id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	usage, err := h.usageService.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, service.ErrUsageLogNotFound) {
			response.NotFound(c, "Usage log not found")
		} else {
			// 底层错误可能含请求片段；不交给会记录原始异常的通用处理器。
			response.Error(c, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if usage == nil {
		response.NotFound(c, "Usage log not found")
		return
	}
	detail, err := h.requestDiagnostics.Get(ctx, usage)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "Failed to load request diagnostics")
		return
	}
	response.Success(c, gin.H{
		"usage":           dto.UsageLogFromServiceAdmin(usage),
		"capture_enabled": h.requestDiagnostics.Enabled(),
		"retention_hours": h.requestDiagnostics.RetentionHours(),
		"diagnostic":      detail,
	})
}
