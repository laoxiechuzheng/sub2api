package middleware

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RequestDiagnostic 在鉴权后、模型/正文改写前安装被动采集，不提前读取请求。
func RequestDiagnostic(svc *service.RequestDiagnosticService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !svc.Enabled() || c.Request == nil || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodDelete {
			c.Next()
			return
		}
		key, ok := GetAPIKeyFromContext(c)
		if !ok || key == nil {
			c.Next()
			return
		}
		ctx := c.Request.Context()
		if key.Group != nil && key.Group.Platform != service.PlatformComposite {
			ctx = service.WithRequestDiagnosticPlatform(ctx, key.Group.Platform)
		}
		requestID, _ := ctx.Value(ctxkey.RequestID).(string)
		clientID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
		capture := svc.Begin(requestdiagnostic.Inbound{
			RequestID: requestID, ClientRequestID: clientID,
			Endpoint: c.Request.URL.Path, Method: c.Request.Method,
			UserAgent: c.Request.UserAgent(), StartedAt: time.Now().UTC(),
		})
		if capture == nil {
			c.Next()
			return
		}
		c.Request = c.Request.WithContext(requestdiagnostic.WithCapture(ctx, capture))
		defer func() { capture.Finish(c.Writer.Status()) }()

		contentType := strings.ToLower(c.Request.Header.Get("Content-Type"))
		encoding := strings.ToLower(strings.TrimSpace(c.Request.Header.Get("Content-Encoding")))
		switch {
		case contentType != "" && !strings.Contains(contentType, "json"):
			capture.SetInboundOmitted(diagnosticContentLength(c.Request.ContentLength), requestdiagnostic.OmittedBinaryData)
		case c.Request.Body == nil || c.Request.Body == http.NoBody:
			capture.SetInbound(nil)
		case encoding == "" || encoding == "identity":
			// 原有 handler 读取到 EOF 时才发布原文；不改变 JSON/限流/取消语义。
			body := &diagnosticInboundBody{ReadCloser: c.Request.Body, capture: capture, limit: svc.MaxEntryBytes()}
			c.Request.Body = body
			defer body.finish()
			// 压缩正文由共享 JSON body reader 解压后采集，不把压缩字节伪装成 JSON。
		}
		c.Next()
	}
}

type diagnosticInboundBody struct {
	io.ReadCloser
	capture  *requestdiagnostic.Capture
	limit    int
	buffer   []byte
	total    int
	done     bool
	tooLarge bool
}

func (b *diagnosticInboundBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.done {
		return n, err
	}
	b.total += n
	if !b.tooLarge && n > 0 {
		if b.total > b.limit {
			b.tooLarge, b.buffer = true, nil
		} else {
			b.buffer = append(b.buffer, p[:n]...)
		}
	}
	if err != nil {
		b.done = true
		switch {
		case b.tooLarge:
			b.capture.SetInboundOmitted(b.total, requestdiagnostic.OmittedTooLarge)
		case err != io.EOF:
			b.capture.SetInboundOmitted(b.total, requestdiagnostic.OmittedNotCaptured)
		default:
			b.capture.SetInbound(b.buffer)
		}
		b.buffer = nil
	}
	return n, err
}

func (b *diagnosticInboundBody) finish() {
	if !b.done {
		b.capture.SetInboundOmitted(b.total, requestdiagnostic.OmittedNotCaptured)
		b.done, b.buffer = true, nil
	}
}

func diagnosticContentLength(length int64) int {
	if length < 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if length > maxInt {
		return int(maxInt)
	}
	return int(length)
}
