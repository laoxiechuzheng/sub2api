package config

import "fmt"

// RequestDiagnosticsConfig 控制管理员排障正文采集，与用量列表和计费独立。
type RequestDiagnosticsConfig struct {
	Enabled         bool  `mapstructure:"enabled"`
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
	if c.MaxEntryBytes < 1024 || c.MaxEntryBytes > 8<<20 {
		return fmt.Errorf("request_diagnostics.max_entry_bytes must be between 1024 and 8388608")
	}
	if c.MaxActive < 1 || c.MaxActive > 32 {
		return fmt.Errorf("request_diagnostics.max_active must be between 1 and 32")
	}
	if c.MaxPendingBytes < int64(c.MaxEntryBytes) || c.MaxPendingBytes > 256<<20 {
		return fmt.Errorf("request_diagnostics.max_pending_bytes must cover one entry and not exceed 268435456")
	}
	if c.MaxStorageBytes < int64(c.MaxEntryBytes) || c.MaxStorageBytes > 10<<30 {
		return fmt.Errorf("request_diagnostics.max_storage_bytes must cover one entry and not exceed 10737418240")
	}
	return nil
}
