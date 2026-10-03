//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestCompositeRoutePreciseConditionsRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := testEntTx(t).Client()
	group := mustCreateGroup(t, client, &service.Group{
		Name: "precise-conditions-repository", Platform: service.PlatformComposite, RateMultiplier: 1,
	})
	repo := NewCompositeModelRouteRepository(client)
	route := &service.CompositeModelRoute{
		GroupID:           group.ID,
		PublicModel:       "roundtrip",
		MatchType:         service.CompositeRouteMatchExact,
		TargetPlatform:    service.PlatformOpenAI,
		UpstreamModel:     "upstream-model",
		Endpoint:          service.CompositeRouteEndpointResponses,
		UserAgentContains: "Codex",
		BodyContains:      "first signature\nsecond signature",
		RequestKind:       "compaction",
		BodyMatchScope:    "last_message",
		BodyMatchMode:     "all",
		BodyNotContains:   "quoted example\nnot a compaction",
		Priority:          20,
		Enabled:           true,
	}
	require.NoError(t, repo.Create(ctx, route))
	require.Positive(t, route.ID)

	routes, err := repo.ListByGroup(ctx, group.ID, true)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, route.ID, routes[0].ID)
	require.Equal(t, "compaction", routes[0].RequestKind)
	require.Equal(t, "last_message", routes[0].BodyMatchScope)
	require.Equal(t, "all", routes[0].BodyMatchMode)
	require.Equal(t, "quoted example\nnot a compaction", routes[0].BodyNotContains)

	updated := routes[0]
	updated.RequestKind = "conversation"
	updated.BodyMatchScope = "current_turn"
	updated.BodyMatchMode = "prefix"
	updated.BodyNotContains = "new exclusion\nsecond exclusion"
	require.NoError(t, repo.Update(ctx, &updated))
	routes, err = repo.ListByGroup(ctx, group.ID, true)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, route.ID, routes[0].ID)
	require.Equal(t, "conversation", routes[0].RequestKind)
	require.Equal(t, "current_turn", routes[0].BodyMatchScope)
	require.Equal(t, "prefix", routes[0].BodyMatchMode)
	require.Equal(t, "new exclusion\nsecond exclusion", routes[0].BodyNotContains)

	legacy := &service.CompositeModelRoute{
		GroupID:        group.ID,
		PublicModel:    "legacy-roundtrip",
		MatchType:      service.CompositeRouteMatchExact,
		TargetPlatform: service.PlatformOpenAI,
		Endpoint:       service.CompositeRouteEndpointResponses,
		Priority:       100,
		Enabled:        true,
	}
	require.NoError(t, repo.Create(ctx, legacy))
	routes, err = repo.ListByGroup(ctx, group.ID, true)
	require.NoError(t, err)
	require.Len(t, routes, 2)
	require.Equal(t, legacy.ID, routes[1].ID)
	require.Equal(t, "any", routes[1].RequestKind)
	require.Equal(t, "full_body", routes[1].BodyMatchScope)
	require.Equal(t, "any", routes[1].BodyMatchMode)
	require.Empty(t, routes[1].BodyNotContains)
}

func TestCompositeRoutePreciseConditionsMigrationPreservesLegacyWrites(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	_, err := tx.ExecContext(ctx, `
ALTER TABLE composite_model_routes
    DROP COLUMN request_kind CASCADE,
    DROP COLUMN body_match_scope CASCADE,
    DROP COLUMN body_match_mode CASCADE,
    DROP COLUMN body_not_contains CASCADE;
CREATE UNIQUE INDEX idx_composite_model_routes_unique_active
    ON composite_model_routes (group_id, endpoint, match_type, public_model, md5(user_agent_contains), md5(body_contains))
    WHERE deleted_at IS NULL;
`)
	require.NoError(t, err)
	groupID := preciseConditionsMigrationGroup(t, tx)
	var legacyID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO composite_model_routes (group_id, public_model, target_platform, body_contains)
VALUES ($1, 'legacy', 'openai', 'old signature') RETURNING id
`, groupID).Scan(&legacyID))

	applyPreciseConditionsMigration(t, tx)
	applyPreciseConditionsMigration(t, tx)

	var kind, scope, mode, exclusions, contains string
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT request_kind, body_match_scope, body_match_mode, body_not_contains, body_contains
FROM composite_model_routes WHERE id = $1
`, legacyID).Scan(&kind, &scope, &mode, &exclusions, &contains))
	require.Equal(t, "any", kind)
	require.Equal(t, "full_body", scope)
	require.Equal(t, "any", mode)
	require.Empty(t, exclusions)
	require.Equal(t, "old signature", contains)

	var newID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO composite_model_routes (group_id, public_model, target_platform)
VALUES ($1, 'old-binary-write', 'openai') RETURNING id
`, groupID).Scan(&newID))
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT request_kind, body_match_scope, body_match_mode, body_not_contains
FROM composite_model_routes WHERE id = $1
`, newID).Scan(&kind, &scope, &mode, &exclusions))
	require.Equal(t, "any", kind)
	require.Equal(t, "full_body", scope)
	require.Equal(t, "any", mode)
	require.Empty(t, exclusions)
}

func TestCompositeRoutePreciseConditionsMigrationUniqueKeyIncludesEveryCondition(t *testing.T) {
	tx := testTx(t)
	groupID := preciseConditionsMigrationGroup(t, tx)
	ctx := context.Background()
	insert := `
INSERT INTO composite_model_routes
    (group_id, public_model, target_platform, request_kind, body_match_scope, body_match_mode, user_agent_contains, body_contains, body_not_contains)
VALUES ($1, 'same-model', 'openai', $2, $3, $4, $5, $6, $7)
`
	conditions := [][]string{
		{"any", "full_body", "any", "Codex", "signature", ""},
		{"compaction", "full_body", "any", "Codex", "signature", ""},
		{"conversation", "full_body", "any", "Codex", "signature", ""},
		{"any", "instructions", "any", "Codex", "signature", ""},
		{"any", "last_message", "any", "Codex", "signature", ""},
		{"any", "current_turn", "any", "Codex", "signature", ""},
		{"any", "full_body", "all", "Codex", "signature", ""},
		{"any", "full_body", "prefix", "Codex", "signature", ""},
		{"any", "full_body", "any", "Other", "signature", ""},
		{"any", "full_body", "any", "Codex", "other signature", ""},
		{"any", "full_body", "any", "Codex", "signature", "excluded"},
	}
	for _, c := range conditions {
		_, err := tx.ExecContext(ctx, insert, groupID, c[0], c[1], c[2], c[3], c[4], c[5])
		require.NoError(t, err, "distinct conditions must coexist: %v", c)
	}

	_, err := tx.ExecContext(ctx, "SAVEPOINT duplicate_route")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, insert, groupID, "any", "full_body", "any", "Codex", "signature", "")
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23505", string(pgErr.Code))
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT duplicate_route")
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `UPDATE composite_model_routes SET deleted_at = NOW() WHERE group_id = $1 AND request_kind = 'any' AND body_match_scope = 'full_body' AND body_match_mode = 'any' AND user_agent_contains = 'Codex' AND body_contains = 'signature' AND body_not_contains = ''`, groupID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, insert, groupID, "any", "full_body", "any", "Codex", "signature", "")
	require.NoError(t, err, "soft-deleted routes must not block replacement")
}

func TestCompositeRoutePreciseConditionsMigrationRejectsUnknownEnumsAndOversizedExclusions(t *testing.T) {
	tx := testTx(t)
	groupID := preciseConditionsMigrationGroup(t, tx)
	ctx := context.Background()
	tests := []struct {
		name, kind, scope, mode string
		length                  int
	}{
		{name: "request_kind", kind: "unsupported", scope: "full_body", mode: "any"},
		{name: "body_match_scope", kind: "any", scope: "unsupported", mode: "any"},
		{name: "body_match_mode", kind: "any", scope: "full_body", mode: "unsupported"},
		{name: "body_not_contains", kind: "any", scope: "full_body", mode: "any", length: 8193},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tx.ExecContext(ctx, "SAVEPOINT invalid_route")
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `
INSERT INTO composite_model_routes (group_id, public_model, target_platform, request_kind, body_match_scope, body_match_mode, body_not_contains)
VALUES ($1, 'invalid', 'openai', $2, $3, $4, repeat('a', $5))
`, groupID, tt.kind, tt.scope, tt.mode, tt.length)
			var pgErr *pq.Error
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, "23514", string(pgErr.Code))
			_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT invalid_route")
			require.NoError(t, err)
		})
	}

	_, err := tx.ExecContext(ctx, `
INSERT INTO composite_model_routes (group_id, public_model, target_platform, body_not_contains)
VALUES ($1, 'maximum', 'openai', repeat('a', 8192))
`, groupID)
	require.NoError(t, err)
}

func preciseConditionsMigrationGroup(t *testing.T, tx *sql.Tx) int64 {
	t.Helper()
	var id int64
	require.NoError(t, tx.QueryRowContext(context.Background(), `
INSERT INTO groups (name, platform, rate_multiplier, status)
VALUES ('precise-conditions-migration', 'composite', 1, 'active') RETURNING id
`).Scan(&id))
	return id
}

func applyPreciseConditionsMigration(t *testing.T, tx *sql.Tx) {
	t.Helper()
	content, err := dbmigrations.FS.ReadFile("242_composite_route_precise_conditions.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), string(content))
	require.NoError(t, err)
}
