package config

import "fmt"

// MaxRequestDiagnosticEntryBytes 允许短保留期下完整保存长对话请求与模型输出原文。
const MaxRequestDiagnosticEntryBytes = 128 << 20

// MaxRequestDiagnosticActiveBytes 限制同时采集的最坏内存占用（单条预算 × 并发采集数）。
const MaxRequestDiagnosticActiveBytes = 2 << 30

// RequestDiagnosticsConfig 控制管理员排障正文采集，与用量列表和计费独立。
type RequestDiagnosticsConfig struct {
	Enabled         bool  `mapstructure:"enabled"`
	PreserveRaw     bool  `mapstructure:"preserve_raw"`
	RetentionHours  int   `mapstructure:"retention_hours"`
	MaxEntryBytes   int   `mapstructure:"max_entry_bytes"`
	MaxActive       int   `mapstructure:"max_active"`
	MaxPendingBytes int64 `mapstructure:"max_pending_bytes"`
	MaxStorageBytes int64 `mapstructure:"max_storage_bytes"`
}

func (c RequestDiagnosticsConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if c.RetentionHours < 1 || c.RetentionHours > 168 {
		return fmt.Errorf("request_diagnostics.retention_hours must be between 1 and 168")
	}
	if c.MaxEntryBytes < 1024 || c.MaxEntryBytes > MaxRequestDiagnosticEntryBytes {
		return fmt.Errorf("request_diagnostics.max_entry_bytes must be between 1024 and %d", MaxRequestDiagnosticEntryBytes)
	}
	if c.MaxActive < 1 || c.MaxActive > 32 {
		return fmt.Errorf("request_diagnostics.max_active must be between 1 and 32")
	}
	if int64(c.MaxEntryBytes)*int64(c.MaxActive) > MaxRequestDiagnosticActiveBytes {
		return fmt.Errorf("request_diagnostics.max_entry_bytes * max_active must not exceed %d", int64(MaxRequestDiagnosticActiveBytes))
	}
	if c.MaxPendingBytes < int64(c.MaxEntryBytes) || c.MaxPendingBytes > 1<<30 {
		return fmt.Errorf("request_diagnostics.max_pending_bytes must cover one entry and not exceed 1073741824")
	}
	if c.MaxStorageBytes != 0 && (c.MaxStorageBytes < int64(c.MaxEntryBytes) || c.MaxStorageBytes > 10<<30) {
		return fmt.Errorf("request_diagnostics.max_storage_bytes must be zero (retention only) or cover one entry and not exceed 10737418240")
	}
	return nil
}
