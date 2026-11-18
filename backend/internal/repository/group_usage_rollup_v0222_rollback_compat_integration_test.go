//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appTimezone "github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestCompositeUsageV0222Rollback is deliberately inside the existing personal
// publish selector (^TestCompositeUsage), as well as make test-integration.
// TestMain must run with CI=true: without Docker the existing harness otherwise
// exits zero BEFORE any test executes. No DSN override or production connection
// is accepted here. All records are synthetic; concurrent cases own a schema.
//
// The d78 fixtures at the end are frozen from the actual Git objects, not
// reconstructed from the v0.2.22 implementation. New code is exercised through
// real unexported repository phase functions and SyncGroupUsageRollups.

func TestCompositeUsageV0222RollbackOldSQLAndAttributionRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NotNil(t, integrationDB, "the real integration harness must execute")
	var applied int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE filename IN (
		'249_add_usage_logs_api_key_cost_index_notx.sql', '250_openai_cache_write_ttl.sql',
		'251_group_force_openai_ultrafast.sql', '251_group_usage_rollup_nonblocking_build.sql'
	)`).Scan(&applied))
	require.Equal(t, 4, applied, "all four real v0.2.22 migrations must have executed")
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE filename='252_custom_group_usage_rollup_rollback_floor_guard.sql'`).Scan(&applied))
	require.Equal(t, 1, applied, "the custom compatibility migration must execute in the real harness")
	f := newCompositeUsageFixture(t, true)
	old := f.log("rollback-d78-"+uuid.NewString(), "public-a", "token")
	old.CacheCreation5mTokens, old.CacheCreation1hTokens = 17, 19
	old.CacheCreationTokens = 36
	prepared := rollbackCompatD78PrepareUsageLogInsert(old)
	require.Len(t, prepared.args, 65, "frozen d78 argument contract")
	require.NoError(t, scanSingleRow(ctx, integrationDB, rollbackCompatD78InsertSQL, prepared.args, &old.ID, &old.CreatedAt))
	// The old ON CONFLICT contract must still deduplicate against the new schema.
	var duplicateID int64
	var duplicateAt time.Time
	err := scanSingleRow(ctx, integrationDB, rollbackCompatD78InsertSQL, prepared.args, &duplicateID, &duplicateAt)
	require.ErrorIs(t, err, sql.ErrNoRows)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs WHERE api_key_id=$1 AND request_id=$2`, old.APIKeyID, old.RequestID).Scan(&count))
	require.Equal(t, 1, count)
	got, err := f.repo.GetByID(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, old.CompositeRouteID, got.CompositeRouteID)
	require.Equal(t, old.CompositeRouteKind, got.CompositeRouteKind)
	require.Equal(t, old.CompositeFallbackChain, got.CompositeFallbackChain)
	require.Equal(t, old.CacheCreation5mTokens, got.CacheCreation5mTokens)
	require.Equal(t, old.CacheCreation1hTokens, got.CacheCreation1hTokens)
	require.Zero(t, got.CacheCreation30mTokens)
	require.Zero(t, got.CacheCreation12hTokens)
	rollbackCompatReadD78Projection(t, ctx, old)

	newLog := f.log("rollback-new-"+uuid.NewString(), "public-a", "token")
	newLog.CacheCreation30mTokens, newLog.CacheCreation12hTokens = 23, 29
	newLog.CacheCreationTokens = 52
	inserted, err := f.repo.Create(ctx, newLog)
	require.NoError(t, err)
	require.True(t, inserted)
	rollbackCompatReadD78Projection(t, ctx, newLog)
	got, err = f.repo.GetByID(ctx, newLog.ID)
	require.NoError(t, err)
	require.Equal(t, 23, got.CacheCreation30mTokens)
	require.Equal(t, 29, got.CacheCreation12hTokens)

	// Use the actual compressed diagnostic repository and canonical binding.
	// maxStorageBytes=0 avoids global eviction/locking; cleanup is exact-ID only.
	diagnostics := NewRequestDiagnosticRepository(integrationDB)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, cleanupErr := integrationDB.ExecContext(cleanupCtx, `DELETE FROM usage_request_diagnostics WHERE api_key_id=$1 AND usage_request_id=$2`, old.APIKeyID, old.RequestID)
		require.NoError(t, cleanupErr)
	})
	var now time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT now()`).Scan(&now))
	record := requestDiagnosticIntegrationRecord(t, old, old.CreatedAt, now, now.Add(time.Hour), "synthetic rollback diagnostic")
	require.NoError(t, diagnostics.Save(ctx, record, 0))
	stored, err := diagnostics.Get(ctx, old.APIKeyID, old.RequestID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, record.PayloadGzip, stored.PayloadGzip)
	require.Equal(t, record.PayloadBytes, stored.PayloadBytes)
	require.True(t, old.CreatedAt.Equal(stored.UsageCreatedAt))
	rollbackCompatReadD78Projection(t, ctx, old)
}

func rollbackCompatReadD78Projection(t *testing.T, ctx context.Context, usage *service.UsageLog) {
	t.Helper()
	rows, err := integrationDB.QueryContext(ctx, "SELECT "+rollbackCompatD78SelectColumns+" FROM usage_logs WHERE id=$1", usage.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	require.NoError(t, err)
	require.Len(t, columns, 66)
	require.True(t, rows.Next())
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	require.NoError(t, rows.Scan(pointers...))
	byName := make(map[string]any, len(columns))
	for i, column := range columns {
		byName[column] = values[i]
	}
	text := func(value any) string {
		if bytes, ok := value.([]byte); ok {
			return string(bytes)
		}
		return fmt.Sprint(value)
	}
	require.Equal(t, usage.ID, byName["id"])
	require.Equal(t, usage.RequestID, text(byName["request_id"]))
	require.Equal(t, usage.RequestedModel, text(byName["requested_model"]))
	require.Equal(t, *usage.CompositeRouteID, byName["composite_route_id"])
	require.Equal(t, *usage.CompositeRouteKind, text(byName["composite_route_kind"]))
	require.Equal(t, *usage.CompositeFallbackChain, text(byName["composite_fallback_chain"]))
	require.False(t, rows.Next())
	require.NoError(t, rows.Err())
}

type rollbackCompatFixture struct {
	ctx   context.Context
	db    *sql.DB
	today time.Time
}

func newRollbackCompatFixture(t *testing.T) rollbackCompatFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	require.NotNil(t, integrationDB, "the real integration harness must execute")
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")
	schema := createGroupUsageRollupTriggerTestSchema(t, ctx, false)
	db := openGroupUsageRollupSchemaDB(t, schema, "Asia/Shanghai")
	db.SetMaxOpenConns(8)
	// Negative-control execution uses the exact same fixture/assertions while
	// deliberately omitting only 252 in this isolated schema. It never changes
	// the harness's public schema or an existing production migration.
	if os.Getenv("SUB2API_TEST_ROLLUP_DISABLE_COMPAT_GUARD") != "1" {
		migrationSQL, err := migrations.FS.ReadFile("252_custom_group_usage_rollup_rollback_floor_guard.sql")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		for range 2 {
			_, err = tx.ExecContext(ctx, string(migrationSQL))
			require.NoError(t, err, "custom migration must also be re-applicable")
		}
		require.NoError(t, tx.Commit())
	} else {
		t.Log("NEGATIVE CONTROL: isolated schema uses official 251 invalidation without 252")
	}
	rollbackCompatRequireLocalFunctions(t, ctx, db, schema, os.Getenv("SUB2API_TEST_ROLLUP_DISABLE_COMPAT_GUARD") != "1")
	execGroupUsageRollupSQL(t, ctx, db, `
		INSERT INTO groups(id) VALUES(10),(20);
		INSERT INTO users(id) VALUES(1);
		INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES
		(1,1,10,1,'2026-08-11 09:00:00+08'),
		(2,1,10,2,'2026-08-12 09:00:00+08'),
		(3,1,10,4,'2026-08-13 09:00:00+08'),
		(4,1,10,8,'2026-08-14 09:00:00+08');
	`)
	return rollbackCompatFixture{ctx: ctx, db: db, today: time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)}
}

// 运行时确认整个隔离调用链属于本schema，禁止负控制借用public中的252。
func rollbackCompatRequireLocalFunctions(t *testing.T, ctx context.Context, db *sql.DB, schema string, guardEnabled bool) {
	t.Helper()
	var current string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&current))
	require.Equal(t, schema, current)
	require.NotEqual(t, "public", current)
	for _, signature := range []string{
		"apply_group_usage_rollup_invalidation(date)",
		"invalidate_group_usage_rollups_at(timestamp with time zone,text)",
		"invalidate_group_usage_rollup_state()",
		"invalidate_group_usage_rollup_state_after_insert()",
	} {
		var namespace string
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT n.nspname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE p.oid=to_regprocedure($1)::oid`, signature).Scan(&namespace))
		require.Equal(t, schema, namespace, signature)
	}
	const predicateSignature = "group_usage_rollup_can_ignore_expired_changes(timestamp with time zone,timestamp with time zone,text,text)"
	var predicateNamespace sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT n.nspname FROM (SELECT to_regprocedure($1)::oid AS oid) resolved
		LEFT JOIN pg_proc p ON p.oid=resolved.oid
		LEFT JOIN pg_namespace n ON n.oid=p.pronamespace`, predicateSignature).Scan(&predicateNamespace))
	require.Equal(t, guardEnabled, predicateNamespace.Valid, "禁用guard时predicate必须不可解析，而非落到public")
	if guardEnabled {
		require.Equal(t, schema, predicateNamespace.String)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.tgname, n.nspname FROM pg_trigger t
		JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE t.tgrelid='usage_logs'::regclass AND NOT t.tgisinternal ORDER BY t.tgname`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var triggers []string
	for rows.Next() {
		var trigger, namespace string
		require.NoError(t, rows.Scan(&trigger, &namespace))
		require.Equal(t, schema, namespace, trigger)
		triggers = append(triggers, trigger)
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []string{
		"usage_logs_group_rollup_invalidate_insert",
		"usage_logs_group_rollup_invalidate_delete",
		"usage_logs_group_rollup_invalidate_update",
	}, triggers)
}

func (f rollbackCompatFixture) begin(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func (f rollbackCompatFixture) register(t *testing.T) *groupUsageRollupBuildPlan {
	t.Helper()
	tx := f.begin(t)
	plan, err := beginGroupUsageRollupBuild(f.ctx, tx, f.today)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.NoError(t, tx.Commit())
	return plan
}

func (f rollbackCompatFixture) assertSummaries(t *testing.T) {
	t.Helper()
	expected := make(map[int64][3]float64)
	rows, err := f.db.QueryContext(f.ctx, `
		SELECT g.id, COALESCE(SUM(u.actual_cost),0)::float8,
		COALESCE(SUM(u.actual_cost) FILTER(WHERE u.created_at >= $1),0)::float8,
		COALESCE(SUM(u.actual_cost) FILTER(WHERE u.created_at >= $2 AND u.created_at < $1),0)::float8
		FROM groups g LEFT JOIN usage_logs u ON u.group_id=g.id GROUP BY g.id
	`, f.today, f.today.AddDate(0, 0, -1))
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		var cost [3]float64
		require.NoError(t, rows.Scan(&id, &cost[0], &cost[1], &cost[2]))
		expected[id] = cost
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	old, err := (&rollbackCompatD78UsageRepository{sql: f.db}).getAllGroupUsageSummaryFromRollups(f.ctx, f.today)
	require.NoError(t, err)
	oldSummary := make(map[int64][3]float64)
	for _, row := range old {
		oldSummary[row.GroupID] = [3]float64{row.TotalCost, row.TodayCost, row.YesterdayCost}
	}
	require.Equal(t, expected, oldSummary, "d78 summary must match live synthetic detail")
	require.Equal(t, expected, groupUsageSummaryByGroup(t, f.ctx, f.db, f.today), "new summary must neither miss nor double count")
}

func (f rollbackCompatFixture) assertRefreshed(t *testing.T) {
	t.Helper()
	require.NoError(t, newDashboardAggregationRepositoryWithSQL(f.db).SyncGroupUsageRollups(f.ctx, f.today))
	state := readGroupUsageRollupTestState(t, f.ctx, f.db)
	require.Equal(t, "2026-08-14", state.closedBefore)
	require.False(t, state.buildingUntil.Valid)
	require.False(t, state.buildDirtyFrom.Valid)
	requireGroupUsageRollupsMatchUsageLogs(t, f.ctx, f.db, service.GroupUsageTimezoneName(), state.retainedFrom, f.today)
	f.assertSummaries(t)
}

// An old background transaction already owns FOR UPDATE. The real new three
// phases must wait, then observe its commit rather than publish an older image.
func TestCompositeUsageV0222RollbackOldBackgroundBeforeNewBegin(t *testing.T) {
	f := newRollbackCompatFixture(t)
	oldTx := f.begin(t)
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: oldTx}).syncGroupUsageRollupsInTx(f.ctx, f.today))
	conn, err := f.db.Conn(f.ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var pid int
	require.NoError(t, conn.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	done := make(chan error, 1)
	go func() {
		done <- syncGroupUsageRollupRounds(f.ctx, rollbackCompatRunner(conn), f.today)
	}()
	rollbackCompatWaitBlocked(t, f.ctx, pid, done)
	require.NoError(t, oldTx.Commit())
	rollbackCompatAwait(t, f.ctx, done)
	f.assertRefreshed(t)
}

// Hold an actual completed new compute transaction open. Old inserts/updates/
// deletes finish while KEY SHARE is held; the old background FOR UPDATE then
// waits for that exact PID. Its rebuild runs before new publish, preserving the
// dirty marker which makes publish retreat instead of trusting stale buckets.
func TestCompositeUsageV0222RollbackOldBackgroundDuringNewCompute(t *testing.T) {
	f := newRollbackCompatFixture(t)
	plan := f.register(t)
	buildTx := f.begin(t)
	require.NoError(t, buildGroupUsageRollups(f.ctx, buildTx, plan))
	writeCtx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	_, err := f.db.ExecContext(writeCtx, `
		INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES(5,1,10,16,'2026-08-12 10:00:00+08');
		UPDATE usage_logs SET actual_cost=32 WHERE id=3;
		DELETE FROM usage_logs WHERE id=1;
	`)
	require.NoError(t, err, "old source writes must complete before compute releases KEY SHARE")
	state := readGroupUsageRollupTestState(t, f.ctx, f.db)
	require.Equal(t, "2026-08-11", state.buildDirtyFrom.String)
	oldTx := f.begin(t)
	var pid int
	require.NoError(t, oldTx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	done := make(chan error, 1)
	go func() {
		oldErr := (&rollbackCompatD78AggregationRepository{sql: oldTx}).syncGroupUsageRollupsInTx(f.ctx, f.today)
		if oldErr == nil {
			oldErr = oldTx.Commit()
		} else {
			_ = oldTx.Rollback()
		}
		done <- oldErr
	}()
	rollbackCompatWaitBlocked(t, f.ctx, pid, done)
	require.NoError(t, buildTx.Commit())
	rollbackCompatAwait(t, f.ctx, done)
	f.assertSummaries(t)
	publishTx := f.begin(t)
	dirty, err := publishGroupUsageRollups(f.ctx, publishTx, plan)
	require.NoError(t, err)
	require.True(t, dirty)
	require.NoError(t, publishTx.Commit())
	require.Equal(t, "2026-08-11", readGroupUsageRollupTestState(t, f.ctx, f.db).closedBefore)
	f.assertSummaries(t)
	f.assertRefreshed(t)
}

func TestCompositeUsageV0222RollbackOldBackgroundBetweenRegisterAndCompute(t *testing.T) {
	f := newRollbackCompatFixture(t)
	plan := f.register(t)
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	f.assertSummaries(t)
	// The old writer executes a multirow INSERT against the replaced trigger.
	tx := f.begin(t)
	var before, after int64
	const counter = `SELECT n_tup_upd FROM pg_stat_xact_user_tables WHERE schemaname=current_schema() AND relname='usage_group_rollup_state'`
	require.NoError(t, tx.QueryRowContext(f.ctx, counter).Scan(&before))
	_, err := tx.ExecContext(f.ctx, `
		INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES
		(5,1,10,16,'2026-08-12 10:00:00+08'),(6,1,10,32,'2026-08-13 10:00:00+08');
	`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(f.ctx, counter).Scan(&after))
	require.Equal(t, int64(1), after-before, "one transition-table invalidation per batch")
	_, err = tx.ExecContext(f.ctx, `UPDATE usage_logs SET actual_cost=actual_cost+1 WHERE id IN(5,6)`)
	require.NoError(t, err)
	var afterUpdate int64
	require.NoError(t, tx.QueryRowContext(f.ctx, counter).Scan(&afterUpdate))
	require.Equal(t, after, afterUpdate, "already dirty dates must not rewrite the state row")
	require.NoError(t, tx.Commit())
	buildTx := f.begin(t)
	require.NoError(t, buildGroupUsageRollups(f.ctx, buildTx, plan))
	require.NoError(t, buildTx.Commit())
	publishTx := f.begin(t)
	dirty, err := publishGroupUsageRollups(f.ctx, publishTx, plan)
	require.NoError(t, err)
	require.True(t, dirty)
	require.NoError(t, publishTx.Commit())
	f.assertSummaries(t)
	f.assertRefreshed(t)
}

func TestCompositeUsageV0222RollbackFailedCandidateOldReadsAndNewRecovery(t *testing.T) {
	f := newRollbackCompatFixture(t)
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	execGroupUsageRollupSQL(t, f.ctx, f.db, `UPDATE usage_logs SET actual_cost=16 WHERE id=2`)
	plan := f.register(t)
	execGroupUsageRollupSQL(t, f.ctx, f.db, `
		CREATE FUNCTION rollback_compat_fail_build() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'rollback compatibility injected build failure' USING ERRCODE='40001'; END $$;
		CREATE TRIGGER rollback_compat_fail_build BEFORE INSERT ON usage_group_daily_rollups
		FOR EACH ROW EXECUTE FUNCTION rollback_compat_fail_build();
	`)
	buildTx := f.begin(t)
	err := buildGroupUsageRollups(f.ctx, buildTx, plan)
	require.ErrorContains(t, err, "rollback compatibility injected build failure")
	require.NoError(t, buildTx.Rollback())
	// The public pool/advisory-lock path must also return an actual SQL error,
	// keep its registration, and release the session lock on failure.
	err = newDashboardAggregationRepositoryWithSQL(f.db).SyncGroupUsageRollups(f.ctx, f.today)
	require.ErrorContains(t, err, "rollback compatibility injected build failure")
	state := readGroupUsageRollupTestState(t, f.ctx, f.db)
	require.True(t, state.buildingUntil.Valid, "registration survives failed compute transaction")
	require.Equal(t, "2026-08-12", state.closedBefore, "failed candidate must not publish")
	f.assertSummaries(t)
	execGroupUsageRollupSQL(t, f.ctx, f.db, `DROP TRIGGER rollback_compat_fail_build ON usage_group_daily_rollups; DROP FUNCTION rollback_compat_fail_build()`)
	// Actual d78 background still works with a persisted candidate registration.
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	f.assertSummaries(t)
	execGroupUsageRollupSQL(t, f.ctx, f.db, `INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES(5,1,10,32,'2026-08-12 10:00:00+08')`)
	f.assertSummaries(t)
	f.assertRefreshed(t)
	// Exercise recovery through the public *sql.DB path and advisory-lock cleanup.
	conn, connErr := f.db.Conn(f.ctx)
	require.NoError(t, connErr)
	defer func() { _ = conn.Close() }()
	var locked, unlocked bool
	require.NoError(t, conn.QueryRowContext(f.ctx, groupUsageRollupBuildLockSQL).Scan(&locked))
	require.True(t, locked, "retry must release its session advisory lock")
	require.NoError(t, conn.QueryRowContext(f.ctx, groupUsageRollupBuildUnlockSQL).Scan(&unlocked))
	require.True(t, unlocked)
}

// SAFETY GATE, intentionally NOT an expected-error/skip characterization:
// new cleanup commits expired_before before deleting source rows. A failure in
// between is realistic. d78 can rebuild those rows but the replaced trigger
// ignores their subsequent deletion. The final normal consistency invariant
// exposes stale rollback totals until the compatibility issue is addressed.
func TestCompositeUsageV0222RollbackExpiredFloorDeleteConsistency(t *testing.T) {
	f := newRollbackCompatFixture(t)
	cutoff := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC) // Aug 12 local midnight.
	// Run the actual CleanupUsageLogs entry point. Inject an error at its next
	// query after the floor update, leaving its real committed persistent state.
	failed := newDashboardAggregationRepositoryWithSQL(rollbackCompatFailedCleanup{DB: f.db})
	err := failed.CleanupUsageLogs(f.ctx, cutoff)
	require.ErrorContains(t, err, "rollback compatibility injected partition metadata failure")
	require.True(t, cutoff.Equal(readGroupUsageRollupTestState(t, f.ctx, f.db).expiredBefore))
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	// Record the counterexample's intermediate bucket without requiring unsafe
	// behavior: a future floor-enforcing fix may correctly omit it altogether.
	var expiredBucket float64
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT COALESCE(SUM(actual_cost),0)::float8 FROM usage_group_daily_rollups WHERE group_id=10 AND bucket_date='2026-08-11'`).Scan(&expiredBucket))
	t.Logf("expired bucket after old rebuild=%g; expired_before=%s", expiredBucket, cutoff.Format(time.RFC3339))
	// d78 usage_log_repo_query.go:90-93 has this identical Delete path;
	// service/usage_service.go:285-290 exposes it as an administrator operation.
	// Unlike d78 scheduled cleanup's explicit invalidation, it relies on the
	// replaced DELETE trigger. This is a real old caller, not invented cleanup.
	require.NoError(t, newUsageLogRepositoryWithSQL(nil, f.db).Delete(f.ctx, 1))
	// Old background sees closed_before=today and returns early. Safe app-only
	// rollback requires both readers to match the remaining 2+4+8 detail rows.
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	f.assertSummaries(t)
}

// The same hazard needs no administrator operation or fully failed process:
// old rebuild can run between the new cleanup's committed floor and source
// deletion. Gate the actual batch DELETE at the SQL-executor boundary, run the
// exact old transaction, then release the new query and verify both readers.
func TestCompositeUsageV0222RollbackCleanupWindowWithOldBackground(t *testing.T) {
	f := newRollbackCompatFixture(t)
	cutoff := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	require.NoError(t, expireGroupUsageRollupsBefore(f.ctx, f.db, cutoff))
	ready, release := make(chan struct{}), make(chan struct{})
	barrier := &rollbackCompatCleanupBarrier{DB: f.db, ready: ready, release: release}
	done := make(chan error, 1)
	go func() {
		done <- newDashboardAggregationRepositoryWithSQL(barrier).cleanupUsageLogsBatches(f.ctx, cutoff)
	}()
	select {
	case <-ready:
	case <-f.ctx.Done():
		t.Fatal("cleanup did not reach the controlled source-delete boundary")
	}
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	close(release)
	rollbackCompatAwait(t, f.ctx, done)
	require.NoError(t, newDashboardAggregationRepositoryWithSQL(f.db).SyncGroupUsageRollups(f.ctx, f.today))
	f.assertSummaries(t)
}

// A retained floor aligned in the original timezone need not be midnight in
// the new timezone. Old readers may retain their original raw-source semantics
// until deletion; neither reader may lose the retained suffix or keep deleted
// rows in a stale historical bucket. No state clamping is used.
func TestCompositeUsageV0222RollbackTimezoneResetKeepsPartialRetainedDay(t *testing.T) {
	f := newRollbackCompatFixture(t)
	cutoff := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	require.NoError(t, expireGroupUsageRollupsBefore(f.ctx, f.db, cutoff))
	execGroupUsageRollupSQL(t, f.ctx, f.db, `INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES(5,1,10,16,'2026-08-12 01:00:00+08')`)
	var schema string
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT current_schema()`).Scan(&schema))
	useGroupUsageRepositoryTestTimezone(t, "UTC")
	db := openGroupUsageRollupSchemaDB(t, schema, "UTC")
	today := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	plan, err := beginGroupUsageRollupBuild(f.ctx, tx, today)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.NoError(t, tx.Commit())
	// No compute has run. New reader excludes the expired 1-cost prefix; old
	// reader can still see it in its raw tail until real deletion. Both must
	// preserve the retained 16-cost suffix of the UTC boundary day.
	want := map[int64][3]float64{10: {30, 8, 4}, 20: {0, 0, 0}}
	require.Equal(t, want, groupUsageSummaryByGroup(t, f.ctx, db, today))
	oldRows, err := (&rollbackCompatD78UsageRepository{sql: db}).getAllGroupUsageSummaryFromRollups(f.ctx, today)
	require.NoError(t, err)
	old := make(map[int64][3]float64)
	for _, row := range oldRows {
		old[row.GroupID] = [3]float64{row.TotalCost, row.TodayCost, row.YesterdayCost}
	}
	require.Equal(t, map[int64][3]float64{10: {31, 8, 4}, 20: {0, 0, 0}}, old)
	require.NoError(t, newUsageLogRepositoryWithSQL(nil, db).Delete(f.ctx, 1))
	buildTx, err := db.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer func() { _ = buildTx.Rollback() }()
	require.NoError(t, buildGroupUsageRollups(f.ctx, buildTx, plan))
	require.NoError(t, buildTx.Commit())
	publishTx, err := db.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer func() { _ = publishTx.Rollback() }()
	_, err = publishGroupUsageRollups(f.ctx, publishTx, plan)
	require.NoError(t, err)
	require.NoError(t, publishTx.Commit())
	require.NoError(t, newDashboardAggregationRepositoryWithSQL(db).SyncGroupUsageRollups(f.ctx, today))
	require.Equal(t, want, groupUsageSummaryByGroup(t, f.ctx, db, today))
	oldRows, err = (&rollbackCompatD78UsageRepository{sql: db}).getAllGroupUsageSummaryFromRollups(f.ctx, today)
	require.NoError(t, err)
	for _, row := range oldRows {
		old[row.GroupID] = [3]float64{row.TotalCost, row.TodayCost, row.YesterdayCost}
	}
	require.Equal(t, want, old, "deleted expired prefix must not remain in old buckets")
}

func TestCompositeUsageV0222RollbackMigrationPredicateAndMetadataTransitions(t *testing.T) {
	f := newRollbackCompatFixture(t)
	for _, tc := range []struct {
		name, retained, floor, stored, writer string
		want                                  bool
	}{
		{"aligned_shanghai", "2026-08-12 01:00+08", "2026-08-12 00:00+08", "Asia/Shanghai", "Asia/Shanghai", true},
		{"legacy_metadata", "2026-08-11 09:00+08", "2026-08-12 00:00+08", "Asia/Shanghai", "Asia/Shanghai", false},
		{"partial_utc_day", "2026-08-11 17:00+00", "2026-08-11 16:00+00", "UTC", "UTC", false},
		{"timezone_mismatch", "2026-08-12 01:00+08", "2026-08-12 00:00+08", "Asia/Shanghai", "UTC", false},
		{"alias_conservative", "2026-08-12 01:00+00", "2026-08-12 00:00+00", "UTC", "Etc/UTC", false},
		{"epoch_utc", "1970-01-01 00:00+00", "1970-01-01 00:00+00", "UTC", "UTC", true},
		{"epoch_not_local_midnight", "1970-01-01 00:00+00", "1970-01-01 00:00+00", "Asia/Shanghai", "Asia/Shanghai", false},
		{"santiago_missing_midnight", "2026-09-06 05:00+00", "2026-09-06 04:00+00", "America/Santiago", "America/Santiago", false},
		{"new_york_spring_midnight", "2026-03-08 06:00+00", "2026-03-08 05:00+00", "America/New_York", "America/New_York", true},
		{"new_york_fall_midnight", "2026-11-01 05:00+00", "2026-11-01 04:00+00", "America/New_York", "America/New_York", true},
		{"havana_first_midnight_roundtrip", "2026-11-01 06:00+00", "2026-11-01 04:00+00", "America/Havana", "America/Havana", false},
		{"havana_repeated_midnight_previous_date", "2026-11-01 06:00+00", "2026-11-01 05:00+00", "America/Havana", "America/Havana", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT group_usage_rollup_can_ignore_expired_changes($1::timestamptz,$2::timestamptz,$3,$4)`, tc.retained, tc.floor, tc.stored, tc.writer).Scan(&got))
			require.Equal(t, tc.want, got)
		})
	}
	var unknown sql.NullBool
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT group_usage_rollup_can_ignore_expired_changes(NULL, TIMESTAMPTZ '2026-08-12 00:00+00','UTC','UTC')`).Scan(&unknown))
	require.False(t, unknown.Valid)
	cutoff := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	require.NoError(t, expireGroupUsageRollupsBefore(f.ctx, f.db, cutoff))
	require.NoError(t, newDashboardAggregationRepositoryWithSQL(f.db).SyncGroupUsageRollups(f.ctx, f.today))
	// New metadata proves all old historical buckets begin at the aligned
	// floor. Expired INSERT/UPDATE/DELETE retain 251's zero-state-write fast path.
	tx := f.begin(t)
	var before, after int64
	const counter = `SELECT n_tup_upd FROM pg_stat_xact_user_tables WHERE schemaname=current_schema() AND relname='usage_group_rollup_state'`
	require.NoError(t, tx.QueryRowContext(f.ctx, counter).Scan(&before))
	_, err := tx.ExecContext(f.ctx, `
		INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES(5,1,10,16,'2026-08-11 10:00+08');
		UPDATE usage_logs SET actual_cost=17 WHERE id=5;
		DELETE FROM usage_logs WHERE id=1;
	`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(f.ctx, counter).Scan(&after))
	require.Equal(t, before, after, "keep zero state writes when expired invisibility is proved")
	require.NoError(t, tx.Commit())
	// TRUE分支不只检查零状态写入：两个真实reader还必须等于floor内有效明细。
	// 不能调用assertSummaries，其默认明细期望故意包含物理尚存的expired记录。
	retainedExpected := make(map[int64][3]float64)
	retainedRows, err := f.db.QueryContext(f.ctx, `
		SELECT g.id, COALESCE(SUM(u.actual_cost),0)::float8,
		COALESCE(SUM(u.actual_cost) FILTER(WHERE u.created_at >= $1),0)::float8,
		COALESCE(SUM(u.actual_cost) FILTER(WHERE u.created_at >= $2 AND u.created_at < $1),0)::float8
		FROM groups g LEFT JOIN usage_logs u ON u.group_id=g.id AND u.created_at >= $3 GROUP BY g.id
	`, f.today, f.today.AddDate(0, 0, -1), cutoff)
	require.NoError(t, err)
	for retainedRows.Next() {
		var id int64
		var cost [3]float64
		require.NoError(t, retainedRows.Scan(&id, &cost[0], &cost[1], &cost[2]))
		retainedExpected[id] = cost
	}
	require.NoError(t, retainedRows.Err())
	require.NoError(t, retainedRows.Close())
	oldRows, err := (&rollbackCompatD78UsageRepository{sql: f.db}).getAllGroupUsageSummaryFromRollups(f.ctx, f.today)
	require.NoError(t, err)
	oldRetained := make(map[int64][3]float64)
	for _, row := range oldRows {
		oldRetained[row.GroupID] = [3]float64{row.TotalCost, row.TodayCost, row.YesterdayCost}
	}
	require.Equal(t, retainedExpected, oldRetained, "忽略过期变更后d78必须只读到有效保留明细")
	require.Equal(t, retainedExpected, groupUsageSummaryByGroup(t, f.ctx, f.db, f.today), "忽略过期变更后新reader不能遗漏或重复有效明细")
	// A real old rebuild reverts retained_from. Its next expired delete must
	// therefore invalidate, rather than reuse the former proof indefinitely.
	execGroupUsageRollupSQL(t, f.ctx, f.db, `UPDATE usage_logs SET actual_cost=3 WHERE id=2`)
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	state := readGroupUsageRollupTestState(t, f.ctx, f.db)
	require.True(t, state.retainedFrom.Before(cutoff))
	require.NoError(t, newUsageLogRepositoryWithSQL(nil, f.db).Delete(f.ctx, 5))
	require.Equal(t, "2026-08-11", readGroupUsageRollupTestState(t, f.ctx, f.db).closedBefore)
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	f.assertRefreshed(t)
}

func TestCompositeUsageV0222RollbackWriterTimezoneCoversStoredBucketDate(t *testing.T) {
	f := newRollbackCompatFixture(t)
	var schema string
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT current_schema()`).Scan(&schema))
	useGroupUsageRepositoryTestTimezone(t, "America/Los_Angeles")
	f.db = openGroupUsageRollupSchemaDB(t, schema, "America/Los_Angeles")
	f.today = time.Date(2026, 8, 14, 7, 0, 0, 0, time.UTC)
	writer := openGroupUsageRollupSchemaDB(t, schema, "UTC")
	require.NoError(t, (&rollbackCompatD78AggregationRepository{sql: f.db}).SyncGroupUsageRollups(f.ctx, f.today))
	for _, statement := range []string{
		`INSERT INTO usage_logs(id,user_id,group_id,actual_cost,created_at) VALUES(5,1,10,16,TIMESTAMPTZ '2026-08-12 01:00+00')`,
		`UPDATE usage_logs SET actual_cost=32 WHERE id=5`,
		`DELETE FROM usage_logs WHERE id=5`,
		`SELECT invalidate_group_usage_rollups_at(TIMESTAMPTZ '2026-08-12 01:00+00','UTC')`,
	} {
		_, err := writer.ExecContext(f.ctx, statement)
		require.NoError(t, err)
		require.Equal(t, "2026-08-11", readGroupUsageRollupTestState(t, f.ctx, f.db).closedBefore, "UTC Aug12 must invalidate the stored Los Angeles Aug11 bucket")
		f.assertSummaries(t)
		f.assertRefreshed(t)
	}
}

type rollbackCompatFailedCleanup struct{ *sql.DB }

func (q rollbackCompatFailedCleanup) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "pg_partitioned_table") {
		return nil, fmt.Errorf("rollback compatibility injected partition metadata failure")
	}
	return q.DB.QueryContext(ctx, query, args...)
}

type rollbackCompatCleanupBarrier struct {
	*sql.DB
	ready   chan struct{}
	release <-chan struct{}
	entered bool // only the single cleanup goroutine accesses this field
}

func (q *rollbackCompatCleanupBarrier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !q.entered && strings.Contains(query, "DELETE FROM usage_logs") {
		q.entered = true
		close(q.ready)
		select {
		case <-q.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return q.DB.ExecContext(ctx, query, args...)
}

func rollbackCompatRunner(conn *sql.Conn) groupUsageRollupTxRunner {
	return func(ctx context.Context, fn func(sqlExecutor) error) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err = fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
}

func rollbackCompatWaitBlocked(t *testing.T, ctx context.Context, pid int, done <-chan error) {
	t.Helper()
	// Existing helper observes pg_stat_activity for this exact backend, returning
	// early if the worker finishes. Polling is a condition barrier, not a sleep.
	blocked, err := waitForGroupUsageRollupStateLock(ctx, pid, done)
	require.NoError(t, err)
	require.True(t, blocked, "backend %d must reach the intended conflicting state-row lock", pid)
}

func rollbackCompatAwait(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("mixed-version worker exceeded bounded deadline: %v", ctx.Err())
	}
}

// Frozen d78 source fixtures follow. Only receiver/type/function names and the
// constructor call are namespaced; SQL literals and algorithm bodies are kept.
// d78 backend/internal/repository/custom_group_usage_rollup_repo.go:16-283; blob 1b0e1b292094597cf68984031fc4e1cd66380613.
type rollbackCompatD78UsageRepository struct{ sql sqlExecutor }
type rollbackCompatD78AggregationRepository struct{ sql sqlExecutor }

type rollbackCompatD78Snapshot struct {
	valid        bool
	closedBefore string    // date，历史日桶的右开边界
	retainedDate string    // retained_from 在服务端时区内的日期，历史日桶的左闭边界
	tailStart    time.Time // 尾段（还没进日桶的那部分 usage_logs）的起点
}

// readGroupUsageRollupSnapshot 读一次汇总水位并在 Go 侧判定有效性。
//
// 判定条件与原先 SQL 里的 state_values CTE 完全一致：
// 恰好一行、时区名与当前服务端配置相同、且水位不在未来。
func (r *rollbackCompatD78UsageRepository) readGroupUsageRollupSnapshot(ctx context.Context, timezoneName, todayDate string) (rollbackCompatD78Snapshot, error) {
	epoch := time.Unix(0, 0).UTC()
	invalid := rollbackCompatD78Snapshot{
		closedBefore: "1970-01-01",
		retainedDate: "1970-01-01",
		tailStart:    epoch,
	}

	var rowCount int
	var closedBefore sql.NullString
	var retainedFrom sql.NullTime
	var stateTimezone sql.NullString
	if err := scanSingleRow(ctx, r.sql, `
		SELECT
			COUNT(*),
			MAX(closed_before)::text,
			MAX(retained_from),
			MAX(timezone_name)
		FROM usage_group_rollup_state
		WHERE id = 1
	`, nil, &rowCount, &closedBefore, &retainedFrom, &stateTimezone); err != nil {
		return rollbackCompatD78Snapshot{}, fmt.Errorf("读取分组用量汇总水位: %w", err)
	}
	if rowCount != 1 || !closedBefore.Valid || !retainedFrom.Valid ||
		!stateTimezone.Valid || stateTimezone.String != timezoneName ||
		closedBefore.String > todayDate {
		return invalid, nil
	}

	tailStart, err := rollbackCompatD78ParseGroupUsageDate(closedBefore.String)
	if err != nil {
		// 水位值解析不出来，按无效处理：宁可多扫一次，也不要算错钱。
		return invalid, nil //nolint:nilerr // 与上面的 valid 判定同语义，均降级为全量重算
	}
	return rollbackCompatD78Snapshot{
		valid:        true,
		closedBefore: closedBefore.String,
		retainedDate: rollbackCompatD78GroupUsageDate(retainedFrom.Time),
		tailStart:    tailStart.UTC(),
	}, nil
}

func (r *rollbackCompatD78UsageRepository) getAllGroupUsageSummaryFromRollups(ctx context.Context, todayStart time.Time) (results []usagestats.GroupUsageSummary, err error) {
	todayStart = rollbackCompatD78GroupUsageTodayStart(todayStart)
	yesterdayStart := rollbackCompatD78GroupUsageYesterdayStart(todayStart)
	timezoneName := rollbackCompatD78GroupUsageTimezoneName()
	todayDate := rollbackCompatD78GroupUsageDate(todayStart)
	yesterdayDate := rollbackCompatD78GroupUsageDate(yesterdayStart)

	state, err := r.readGroupUsageRollupSnapshot(ctx, timezoneName, todayDate)
	if err != nil {
		return nil, err
	}

	// 尾段起点必须以**查询参数**的形式给进来。
	//
	// 此前它是在同一条 SQL 里由 state CTE 算出、再 CROSS JOIN 给 tail 用的，
	// 于是 created_at 的下界对 planner 来说是个运行期才知道的值：既进不了
	// index cond，也估不准选择性，只能退化成 usage_logs 全表扫。
	// 生产实测（1721 万行 / PostgreSQL 17）：
	//   Seq Scan on usage_logs … rows=17212870, Rows Removed by Join Filter: 17178082
	//   Execution Time: 48720 ms
	// 换成参数之后走 idx_usage_logs_created_at：
	//   Index Scan … Index Cond: (created_at >= $7)
	//   Execution Time: 34 ms
	const query = `
		WITH historical AS (
			SELECT
				rollup.group_id,
				COALESCE(SUM(rollup.actual_cost), 0) AS actual_cost,
				COALESCE(SUM(rollup.actual_cost) FILTER (
					WHERE rollup.bucket_date = $3::date
				), 0) AS yesterday_cost
			FROM usage_group_daily_rollups rollup
			WHERE $4::boolean
				AND rollup.bucket_date >= $5::date
				AND rollup.bucket_date < $6::date
			GROUP BY rollup.group_id
		),
		tail AS (
			SELECT
				ul.group_id,
				COALESCE(SUM(ul.actual_cost), 0) AS actual_cost,
				COALESCE(SUM(ul.actual_cost) FILTER (WHERE ul.created_at >= $1), 0) AS today_cost,
				COALESCE(SUM(ul.actual_cost) FILTER (
					WHERE ul.created_at >= $2
						AND ul.created_at < $1
				), 0) AS yesterday_cost
			FROM usage_logs ul
			WHERE ul.created_at >= $7
			GROUP BY ul.group_id
		)
		SELECT
			g.id AS group_id,
			COALESCE(historical.actual_cost, 0) + COALESCE(tail.actual_cost, 0) AS total_cost,
			COALESCE(tail.today_cost, 0) AS today_cost,
			COALESCE(historical.yesterday_cost, 0) + COALESCE(tail.yesterday_cost, 0) AS yesterday_cost
		FROM groups g
		LEFT JOIN historical ON historical.group_id = g.id
		LEFT JOIN tail ON tail.group_id = g.id
		ORDER BY g.id
	`

	rows, err := r.sql.QueryContext(
		ctx,
		query,
		todayStart,
		yesterdayStart,
		yesterdayDate,
		state.valid,
		state.retainedDate,
		state.closedBefore,
		state.tailStart,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			results = nil
		}
	}()

	results = make([]usagestats.GroupUsageSummary, 0)
	for rows.Next() {
		var row usagestats.GroupUsageSummary
		if err := rows.Scan(&row.GroupID, &row.TotalCost, &row.TodayCost, &row.YesterdayCost); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// SyncGroupUsageRollups 将服务端配置时区今日以前的用量发布为分组日桶。
func (r *rollbackCompatD78AggregationRepository) SyncGroupUsageRollups(ctx context.Context, todayStart time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	todayStart = rollbackCompatD78GroupUsageTodayStart(todayStart)
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		txRepo := &rollbackCompatD78AggregationRepository{sql: tx}
		if err := txRepo.syncGroupUsageRollupsInTx(ctx, todayStart); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.syncGroupUsageRollupsInTx(ctx, todayStart)
}

func (r *rollbackCompatD78AggregationRepository) syncGroupUsageRollupsInTx(ctx context.Context, todayStart time.Time) error {
	var closedBefore string
	var previousRetainedFrom time.Time
	var stateTimezoneName string
	if err := scanSingleRow(ctx, r.sql, `
		SELECT closed_before::text, retained_from, timezone_name
		FROM usage_group_rollup_state
		WHERE id = 1
		FOR UPDATE
	`, nil, &closedBefore, &previousRetainedFrom, &stateTimezoneName); err != nil {
		return fmt.Errorf("读取分组用量汇总水位: %w", err)
	}

	todayDate := rollbackCompatD78GroupUsageDate(todayStart)
	timezoneName := rollbackCompatD78GroupUsageTimezoneName()
	timezoneChanged := stateTimezoneName != timezoneName
	var closedTime time.Time
	if !timezoneChanged {
		var err error
		closedTime, err = rollbackCompatD78ParseGroupUsageDate(closedBefore)
		if err != nil {
			return fmt.Errorf("解析分组用量汇总水位 %q: %w", closedBefore, err)
		}
		todayDateTime, err := rollbackCompatD78ParseGroupUsageDate(todayDate)
		if err != nil {
			return err
		}
		if closedTime.After(todayDateTime) {
			return fmt.Errorf("分组用量汇总水位位于未来: %s", closedBefore)
		}
		if closedBefore == todayDate {
			return nil
		}
	}

	var earliest sql.NullTime
	if err := scanSingleRow(ctx, r.sql, "SELECT MIN(created_at) FROM usage_logs", nil, &earliest); err != nil {
		return fmt.Errorf("读取最早用量记录: %w", err)
	}
	retainedFrom := todayStart
	if earliest.Valid {
		retainedFrom = earliest.Time.UTC()
	}
	retainedDate := rollbackCompatD78GroupUsageDate(retainedFrom)
	retainedDateTime, err := rollbackCompatD78ParseGroupUsageDate(retainedDate)
	if err != nil {
		return err
	}
	rebuildStartDate := retainedDate
	if !timezoneChanged && closedTime.After(retainedDateTime) {
		rebuildStartDate = closedBefore
	}
	rebuildStart, err := rollbackCompatD78ParseGroupUsageDate(rebuildStartDate)
	if err != nil {
		return err
	}

	if _, err := r.sql.ExecContext(ctx, `
		DELETE FROM usage_group_daily_rollups
		WHERE bucket_date < $1::date
			OR (bucket_date >= $2::date AND bucket_date < $3::date)
			OR bucket_date >= $3::date
	`, retainedDate, rebuildStartDate, todayDate); err != nil {
		return fmt.Errorf("清理分组用量日桶: %w", err)
	}

	if _, err := r.sql.ExecContext(ctx, `
		INSERT INTO usage_group_daily_rollups (bucket_date, group_id, actual_cost, computed_at)
		SELECT
			(created_at AT TIME ZONE $3::text)::date AS bucket_date,
			group_id,
			COALESCE(SUM(actual_cost), 0) AS actual_cost,
			NOW()
		FROM usage_logs
		WHERE group_id IS NOT NULL
			AND created_at >= $1
			AND created_at < $2
		GROUP BY 1, 2
		ON CONFLICT (bucket_date, group_id)
		DO UPDATE SET
			actual_cost = EXCLUDED.actual_cost,
			computed_at = EXCLUDED.computed_at
	`, rebuildStart.UTC(), todayStart.UTC(), timezoneName); err != nil {
		return fmt.Errorf("重建分组用量日桶: %w", err)
	}

	if _, err := r.sql.ExecContext(ctx, `
		UPDATE usage_group_rollup_state
		SET closed_before = $1::date,
			retained_from = $2,
			timezone_name = $3,
			updated_at = NOW()
		WHERE id = 1
	`, todayDate, retainedFrom, timezoneName); err != nil {
		return fmt.Errorf("更新分组用量汇总水位: %w", err)
	}
	return nil
}

// d78 backend/internal/repository/usage_log_repo_insert.go:229-306; blob 5ad4619f116bf241110909627e84748df395e6dc.
const rollbackCompatD78InsertSQL = `
		INSERT INTO usage_logs (
			user_id,
			api_key_id,
			account_id,
			request_id,
			model,
			requested_model,
			upstream_model,
			upstream_response_model,
			upstream_model_mismatch,
			group_id,
			subscription_id,
			input_tokens,
			output_tokens,
			cache_creation_tokens,
			cache_read_tokens,
			cache_creation_5m_tokens,
			cache_creation_1h_tokens,
			image_output_tokens,
			image_output_cost,
			image_input_tokens,
			image_input_cost,
			input_cost,
			output_cost,
			cache_creation_cost,
			cache_read_cost,
			total_cost,
			actual_cost,
			rate_multiplier,
			account_rate_multiplier,
			billing_type,
			request_type,
			stream,
			openai_ws_mode,
			duration_ms,
			first_token_ms,
			user_agent,
			ip_address,
			image_count,
			image_size,
			image_input_size,
			image_output_size,
			image_size_source,
			image_size_breakdown,
			video_count,
			video_resolution,
			video_duration_seconds,
			service_tier,
			reasoning_effort,
			requested_reasoning_effort,
			inbound_endpoint,
			upstream_endpoint,
			cache_ttl_overridden,
			long_context_billing_applied,
			channel_id,
			model_mapping_chain,
			billing_tier,
			billing_mode,
			account_stats_cost,
			upstream_request_id,
			session_id,
			native_compaction_v2,
			composite_route_id,
			composite_route_kind,
			composite_fallback_chain,
			created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19,
			$20, $21, $22, $23, $24, $25,
			$26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57, $58, $59, $60, $61, $62, $63, $64, $65
		)
		ON CONFLICT (request_id, api_key_id) DO NOTHING
		RETURNING id, created_at
	`

// d78 backend/internal/repository/usage_log_repo_insert.go:1279-1402; only function name changed.
func rollbackCompatD78PrepareUsageLogInsert(log *service.UsageLog) usageLogInsertPrepared {
	createdAt := log.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	requestID := strings.TrimSpace(log.RequestID)
	log.RequestID = requestID

	rateMultiplier := log.RateMultiplier
	log.SyncRequestTypeAndLegacyFields()
	requestType := int16(log.RequestType)

	groupID := nullInt64(log.GroupID)
	subscriptionID := nullInt64(log.SubscriptionID)
	duration := nullInt(log.DurationMs)
	firstToken := nullInt(log.FirstTokenMs)
	userAgent := nullString(log.UserAgent)
	ipAddress := nullString(log.IPAddress)
	imageSize := nullString(log.ImageSize)
	imageInputSize := nullString(log.ImageInputSize)
	imageOutputSize := nullString(log.ImageOutputSize)
	imageSizeSource := nullString(log.ImageSizeSource)
	imageSizeBreakdown := nullStringIntMapJSON(log.ImageSizeBreakdown)
	videoResolution := nullString(log.VideoResolution)
	videoDurationSeconds := nullInt(log.VideoDurationSeconds)
	serviceTier := nullString(log.ServiceTier)
	reasoningEffort := nullString(log.ReasoningEffort)
	requestedReasoningEffort := nullString(log.RequestedReasoningEffort)
	inboundEndpoint := nullString(log.InboundEndpoint)
	upstreamEndpoint := nullString(log.UpstreamEndpoint)
	channelID := nullInt64(log.ChannelID)
	modelMappingChain := nullString(log.ModelMappingChain)
	billingTier := nullString(log.BillingTier)
	billingMode := nullString(log.BillingMode)
	upstreamRequestID := nullString(log.UpstreamRequestID)
	sessionID := nullString(log.SessionID)
	requestedModel := strings.TrimSpace(log.RequestedModel)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(log.Model)
	}
	upstreamModel := nullString(log.UpstreamModel)
	upstreamResponseModel := nullString(log.UpstreamResponseModel)
	upstreamModelMismatch := nullBool(log.UpstreamModelMismatch)

	var requestIDArg any
	if requestID != "" {
		requestIDArg = requestID
	}

	return usageLogInsertPrepared{
		createdAt:      createdAt,
		requestID:      requestID,
		rateMultiplier: rateMultiplier,
		requestType:    requestType,
		args: []any{
			log.UserID,
			log.APIKeyID,
			log.AccountID,
			requestIDArg,
			log.Model,
			nullString(&requestedModel),
			upstreamModel,
			upstreamResponseModel,
			upstreamModelMismatch,
			groupID,
			subscriptionID,
			log.InputTokens,
			log.OutputTokens,
			log.CacheCreationTokens,
			log.CacheReadTokens,
			log.CacheCreation5mTokens,
			log.CacheCreation1hTokens,
			log.ImageOutputTokens,
			log.ImageOutputCost,
			log.ImageInputTokens,
			log.ImageInputCost,
			log.InputCost,
			log.OutputCost,
			log.CacheCreationCost,
			log.CacheReadCost,
			log.TotalCost,
			log.ActualCost,
			rateMultiplier,
			log.AccountRateMultiplier,
			log.BillingType,
			requestType,
			log.Stream,
			log.OpenAIWSMode,
			duration,
			firstToken,
			userAgent,
			ipAddress,
			log.ImageCount,
			imageSize,
			imageInputSize,
			imageOutputSize,
			imageSizeSource,
			imageSizeBreakdown,
			log.VideoCount,
			videoResolution,
			videoDurationSeconds,
			serviceTier,
			reasoningEffort,
			requestedReasoningEffort,
			inboundEndpoint,
			upstreamEndpoint,
			log.CacheTTLOverridden,
			log.LongContextBillingApplied,
			channelID,
			modelMappingChain,
			billingTier,
			billingMode,
			log.AccountStatsCost, // account_stats_cost
			upstreamRequestID,    // upstream_request_id
			sessionID,            // session_id
			log.NativeCompactionV2,
			nullInt64(log.CompositeRouteID),
			nullString(log.CompositeRouteKind),
			nullString(log.CompositeFallbackChain),
			createdAt,
		},
	}
}

// d78 backend/internal/repository/usage_log_repo_query.go:22; blob cb4747ad06c47091ba611046502b014070bacd55.
const rollbackCompatD78SelectColumns = "id, user_id, api_key_id, account_id, request_id, model, requested_model, upstream_model, upstream_response_model, upstream_model_mismatch, group_id, subscription_id, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, image_output_tokens, image_output_cost, image_input_tokens, image_input_cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, total_cost, actual_cost, rate_multiplier, account_rate_multiplier, billing_type, request_type, stream, openai_ws_mode, duration_ms, first_token_ms, user_agent, ip_address, image_count, image_size, image_input_size, image_output_size, image_size_source, image_size_breakdown, video_count, video_resolution, video_duration_seconds, service_tier, reasoning_effort, requested_reasoning_effort, inbound_endpoint, upstream_endpoint, cache_ttl_overridden, long_context_billing_applied, channel_id, model_mapping_chain, billing_tier, billing_mode, account_stats_cost, upstream_request_id, session_id, native_compaction_v2, composite_route_id, composite_route_kind, composite_fallback_chain, created_at"

// d78 backend/internal/service/custom_group_usage_rollup.go:17-40; blob d14dc9bfd9d11d0bbcc9b28b7ce0786970f5172f.
const rollbackCompatD78DateFormat = "2006-01-02"

// rollbackCompatD78GroupUsageTimezoneName 返回服务端配置的时区名称。
func rollbackCompatD78GroupUsageTimezoneName() string {
	return appTimezone.Location().String()
}

// rollbackCompatD78GroupUsageTodayStart 返回指定时刻在服务端配置时区内的自然日 UTC 起点。
func rollbackCompatD78GroupUsageTodayStart(at time.Time) time.Time {
	return appTimezone.StartOfDay(at).UTC()
}

// rollbackCompatD78GroupUsageYesterdayStart 返回指定时刻前一个本地日历日的 UTC 起点。
func rollbackCompatD78GroupUsageYesterdayStart(at time.Time) time.Time {
	return appTimezone.StartOfDay(at).AddDate(0, 0, -1).UTC()
}

// rollbackCompatD78GroupUsageDate 返回指定时刻在服务端配置时区内的日期。
func rollbackCompatD78GroupUsageDate(at time.Time) string {
	return at.In(appTimezone.Location()).Format(rollbackCompatD78DateFormat)
}

// rollbackCompatD78ParseGroupUsageDate 解析服务端配置时区内的日期并返回其零点。
func rollbackCompatD78ParseGroupUsageDate(value string) (time.Time, error) {
	return appTimezone.ParseInLocation(rollbackCompatD78DateFormat, value)
}
