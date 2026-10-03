package requestdiagnostic

import "context"

type captureContextKey struct{}

// WithCapture 将采集对象关联到 context，但 Capture 自身不持有 context。
// nil context 按 context.Background() 处理。
func WithCapture(ctx context.Context, capture *Capture) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, captureContextKey{}, capture)
}

// FromContext 返回关联的采集对象，没有关联时返回 nil。
func FromContext(ctx context.Context) *Capture {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(captureContextKey{}).(*Capture)
	return capture
}

// CopyContext 仅把 parent 中的诊断采集对象复制到 base。
// deadline、取消和其他值都保留 base 的语义，便于脱离 HTTP 取消的 usage
// worker 绑定采集对象，同时不持有 Gin 或整个原始请求 context。
func CopyContext(parent, base context.Context) context.Context {
	if base == nil {
		base = context.Background()
	}
	if capture := FromContext(parent); capture != nil {
		return WithCapture(base, capture)
	}
	return base
}
