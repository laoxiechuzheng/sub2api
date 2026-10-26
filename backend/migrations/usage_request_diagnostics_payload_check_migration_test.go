package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 248 必须与 service.maxDiagnosticJSONBytes（512 MiB）保持一致，否则大诊断条目会被 CHECK 整条拒绝。
func TestUsageRequestDiagnosticsPayloadCheckMigration(t *testing.T) {
	content, err := FS.ReadFile("248_relax_usage_request_diagnostics_payload_check.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS usage_request_diagnostics_payload_bytes_check")
	require.Contains(t, sql, "ADD CONSTRAINT usage_request_diagnostics_payload_bytes_check CHECK (payload_bytes >= 0 AND payload_bytes <= 536870912)")

	_, err = FS.ReadFile("242_usage_request_diagnostics.sql")
	require.NoError(t, err, "the original table migration must remain embedded")
}
