package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRequestDiagnosticsConfigValidation(t *testing.T) {
	valid := RequestDiagnosticsConfig{Enabled: true, RetentionHours: 24, MaxEntryBytes: 8 << 20, MaxActive: 4, MaxPendingBytes: 32 << 20, MaxStorageBytes: 256 << 20}
	require.NoError(t, valid.validate())
	for _, mutate := range []func(*RequestDiagnosticsConfig){
		func(c *RequestDiagnosticsConfig) { c.RetentionHours = 0 },
		func(c *RequestDiagnosticsConfig) { c.RetentionHours = 169 },
		func(c *RequestDiagnosticsConfig) { c.MaxEntryBytes = 9 << 20 },
		func(c *RequestDiagnosticsConfig) { c.MaxEntryBytes = 0 },
		func(c *RequestDiagnosticsConfig) { c.MaxActive = 33 },
		func(c *RequestDiagnosticsConfig) { c.MaxPendingBytes = 1 },
		func(c *RequestDiagnosticsConfig) { c.MaxPendingBytes = 257 << 20 },
		func(c *RequestDiagnosticsConfig) { c.MaxStorageBytes = 1 },
		func(c *RequestDiagnosticsConfig) { c.MaxStorageBytes = 11 << 30 },
	} {
		c := valid
		mutate(&c)
		require.Error(t, c.validate())
	}
	require.NoError(t, (RequestDiagnosticsConfig{}).validate())
}
