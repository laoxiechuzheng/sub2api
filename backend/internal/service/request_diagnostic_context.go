package service

import "context"

type requestDiagnosticPlatformKey struct{}

// 仅保留鉴权分组明确配置的平台，Composite 在实际解析后覆盖；不按模型名猜分组。
func WithRequestDiagnosticPlatform(ctx context.Context, platform string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestDiagnosticPlatformKey{}, platform)
}

func RequestDiagnosticPlatformFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	platform, _ := ctx.Value(requestDiagnosticPlatformKey{}).(string)
	return platform
}
