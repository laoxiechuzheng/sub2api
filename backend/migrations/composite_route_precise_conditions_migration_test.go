package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompositeRoutePreciseConditionsMigration(t *testing.T) {
	content, err := FS.ReadFile("242_composite_route_precise_conditions.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS request_kind VARCHAR(20) NOT NULL DEFAULT 'any'")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS body_match_scope VARCHAR(32) NOT NULL DEFAULT 'full_body'")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS body_match_mode VARCHAR(20) NOT NULL DEFAULT 'any'")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS body_not_contains TEXT NOT NULL DEFAULT ''")
	require.Contains(t, sql, "CHECK (request_kind IN ('any', 'compaction', 'conversation'))")
	require.Contains(t, sql, "CHECK (body_match_scope IN ('full_body', 'instructions', 'last_message', 'current_turn'))")
	require.Contains(t, sql, "CHECK (body_match_mode IN ('any', 'all', 'prefix'))")
	require.Contains(t, sql, "CHECK (octet_length(body_not_contains) <= 8192)")
	require.Contains(t, sql, "DROP INDEX IF EXISTS idx_composite_model_routes_unique_active")
	require.Contains(t, sql, "ON composite_model_routes ( group_id, endpoint, match_type, public_model, request_kind, body_match_scope, body_match_mode, md5(user_agent_contains), md5(body_contains), md5(body_not_contains) ) WHERE deleted_at IS NULL")

	_, err = FS.ReadFile("242_usage_request_diagnostics.sql")
	require.NoError(t, err, "the existing migration must remain embedded alongside the new filename")
}
