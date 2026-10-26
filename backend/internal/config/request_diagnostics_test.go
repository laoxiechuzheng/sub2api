package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRequestDiagnosticsConfigValidation(t *testing.T) {
	valid := RequestDiagnosticsConfig{Enabled: true, RetentionHours: 24, MaxEntryBytes: 8 << 20, MaxActive: 4, MaxPendingBytes: 32 << 20, MaxStorageBytes: 256 << 20}
	require.NoError(t, valid.validate())
	// 短保留期允许完整保存长对话：单条 128 MiB、队列 1 GiB 为上限。
	large := RequestDiagnosticsConfig{Enabled: true, RetentionHours: 6, MaxEntryBytes: 128 << 20, MaxActive: 16, MaxPendingBytes: 1 << 30, MaxStorageBytes: 8 << 30}
	require.NoError(t, large.validate())
	// 单条预算 × 并发采集数限制最坏内存占用（2 GiB）。
	tooWide := large
	tooWide.MaxActive = 17
	require.Error(t, tooWide.validate())
	for _, mutate := range []func(*RequestDiagnosticsConfig){
		func(c *RequestDiagnosticsConfig) { c.RetentionHours = 0 },
		func(c *RequestDiagnosticsConfig) { c.RetentionHours = 169 },
		func(c *RequestDiagnosticsConfig) { c.MaxEntryBytes = 128<<20 + 1 },
		func(c *RequestDiagnosticsConfig) { c.MaxEntryBytes = 0 },
		func(c *RequestDiagnosticsConfig) { c.MaxActive = 33 },
		func(c *RequestDiagnosticsConfig) { c.MaxPendingBytes = 1 },
		func(c *RequestDiagnosticsConfig) { c.MaxPendingBytes = 1<<30 + 1 },
		func(c *RequestDiagnosticsConfig) { c.MaxStorageBytes = 1 },
		func(c *RequestDiagnosticsConfig) { c.MaxStorageBytes = 11 << 30 },
	} {
		c := valid
		mutate(&c)
		require.Error(t, c.validate())
	}
	require.NoError(t, (RequestDiagnosticsConfig{}).validate())
}

func TestRequestDiagnosticsConfigRetentionOnly(t *testing.T) {
	cfg := RequestDiagnosticsConfig{Enabled: true, RetentionHours: 6, MaxEntryBytes: 8 << 20, MaxActive: 4, MaxPendingBytes: 32 << 20, MaxStorageBytes: 0}
	require.NoError(t, cfg.validate(), "zero storage budget selects retention-only persistence")
	cfg.MaxStorageBytes = -1
	require.Error(t, cfg.validate())
}
