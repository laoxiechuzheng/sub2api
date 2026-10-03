//go:build integration

package repository

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const requestDiagnosticIntegrationStorageBudget int64 = 1 << 30

type requestDiagnosticIntegrationFixture struct {
	ctx       context.Context
	repo      service.RequestDiagnosticRepository
	usageRepo service.UsageLogRepository
	user      *service.User
	account   *service.Account
	keys      []*service.APIKey
}

func newRequestDiagnosticIntegrationFixture(t *testing.T) *requestDiagnosticIntegrationFixture {
	t.Helper()
	ctx := context.Background()
	var existing int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_request_diagnostics`).Scan(&existing))
	// Save 的容量上限和 Cleanup 是全表操作，因此本组不并行，也不清空其它测试数据。
	require.Zero(t, existing, "诊断表必须没有其它 fixture 记录；不能为隔离测试删除已有记录")
	f := &requestDiagnosticIntegrationFixture{
		ctx: ctx, repo: NewRequestDiagnosticRepository(integrationDB),
		usageRepo: NewUsageLogRepository(testEntClient(t), integrationDB),
	}
	// 只按本 fixture 的精确 ID 清理，禁止 TRUNCATE 或无 WHERE 的 DELETE。
	t.Cleanup(func() {
		for _, key := range f.keys {
			for _, query := range []string{
				`DELETE FROM usage_request_diagnostics WHERE api_key_id = $1`,
				`DELETE FROM usage_logs WHERE api_key_id = $1`,
				`DELETE FROM api_keys WHERE id = $1`,
			} {
				_, err := integrationDB.ExecContext(ctx, query, key.ID)
				require.NoError(t, err, "清理诊断测试专用 API Key 记录")
			}
			// API Key DELETE 会触发 outbox，按唯一测试 key 的摘要清除副产物。
			_, err := integrationDB.ExecContext(ctx, `DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')`, key.Key)
			require.NoError(t, err, "清理诊断测试专用失效事件")
		}
		if f.account != nil {
			_, err := integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, f.account.ID)
			require.NoError(t, err, "清理诊断测试专用账号")
		}
		if f.user != nil {
			_, err := integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, f.user.ID)
			require.NoError(t, err, "清理诊断测试专用用户")
		}
	})
	client := testEntClient(t)
	f.user = mustCreateUser(t, client, &service.User{Email: "diagnostic-it-" + uuid.NewString() + "@example.com"})
	f.account = mustCreateAccount(t, client, &service.Account{Name: "diagnostic-it-" + uuid.NewString(), Platform: service.PlatformOpenAI})
	f.newKey(t)
	return f
}

func (f *requestDiagnosticIntegrationFixture) newKey(t *testing.T) *service.APIKey {
	t.Helper()
	key := mustCreateApiKey(t, testEntClient(t), &service.APIKey{UserID: f.user.ID, Key: "sk-diagnostic-it-" + uuid.NewString(), Name: "diagnostic-it"})
	f.keys = append(f.keys, key)
	return key
}

func (f *requestDiagnosticIntegrationFixture) now(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT now()`).Scan(&now))
	return now.UTC()
}

func (f *requestDiagnosticIntegrationFixture) createUsage(t *testing.T, key *service.APIKey, requestID string, createdAt time.Time) *service.UsageLog {
	t.Helper()
	usage := &service.UsageLog{
		UserID: f.user.ID, APIKeyID: key.ID, AccountID: f.account.ID,
		RequestID: requestID, Model: "diagnostic-test-model", InputTokens: 10, OutputTokens: 5, CreatedAt: createdAt,
	}
	inserted, err := f.usageRepo.Create(f.ctx, usage)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotZero(t, usage.ID)
	stored, err := f.usageRepo.GetByID(f.ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	return stored
}

func (f *requestDiagnosticIntegrationFixture) requireOwnDiagnosticRows(t *testing.T) {
	t.Helper()
	ids := make([]int64, 0, len(f.keys))
	for _, key := range f.keys {
		ids = append(ids, key.ID)
	}
	var foreign int
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT count(*) FROM usage_request_diagnostics WHERE NOT (api_key_id = ANY($1::bigint[]))`, pq.Array(ids)).Scan(&foreign))
	require.Zero(t, foreign, "不能让容量淘汰或 Cleanup 修改其它 fixture 的诊断记录")
}

func (f *requestDiagnosticIntegrationFixture) save(t *testing.T, record *service.RequestDiagnosticRecord, budget int64) {
	t.Helper()
	f.requireOwnDiagnosticRows(t)
	require.NoError(t, f.repo.Save(f.ctx, record, budget))
}

func (f *requestDiagnosticIntegrationFixture) service(t *testing.T) *service.RequestDiagnosticService {
	t.Helper()
	// 关闭新增采集但保留真实读取，避免测试通过异步队列伪造入库结果。
	svc := service.NewRequestDiagnosticService(f.repo, &config.Config{RequestDiagnostics: config.RequestDiagnosticsConfig{RetentionHours: 24}})
	t.Cleanup(svc.Stop)
	return svc
}

func requestDiagnosticIntegrationRecord(t *testing.T, usage *service.UsageLog, bindingTime, capturedAt, expiresAt time.Time, prompt string) *service.RequestDiagnosticRecord {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"input": prompt})
	require.NoError(t, err)
	snapshot := requestdiagnostic.Snapshot{
		SchemaVersion: requestdiagnostic.SchemaVersion,
		Binding:       requestdiagnostic.UsageBinding{APIKeyID: usage.APIKeyID, CanonicalID: usage.RequestID, UsageCreatedAt: bindingTime},
		Meta: requestdiagnostic.Inbound{
			RequestID: "ops-id-not-canonical", ClientRequestID: "client-id-not-canonical",
			Endpoint: "/v1/responses", Method: "POST", StartedAt: bindingTime,
			Body: requestdiagnostic.Payload{Content: string(payload), OriginalBytes: len(payload), StoredBytes: len(payload)},
		},
		Status: 200, CapturedAt: capturedAt, CaptureStatus: requestdiagnostic.CaptureStatusCaptured,
		Attempts: []requestdiagnostic.Attempt{}, StoredBytes: len(payload),
	}
	body, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err = writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return &service.RequestDiagnosticRecord{
		APIKeyID: usage.APIKeyID, UsageRequestID: usage.RequestID, UsageCreatedAt: bindingTime,
		CapturedAt: capturedAt, ExpiresAt: expiresAt, PayloadGzip: compressed.Bytes(), PayloadBytes: len(body),
	}
}

func TestRequestDiagnosticRepositoryIntegrationMigration242(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	var applied int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE filename = '242_usage_request_diagnostics.sql'`).Scan(&applied))
	require.Equal(t, 1, applied, "必须由 harness 应用真实 242 迁移，而不是测试自建替代表")
	var table sql.NullString
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT to_regclass('public.usage_request_diagnostics')`).Scan(&table))
	require.True(t, table.Valid)
	requireColumn(t, tx, "usage_request_diagnostics", "api_key_id", "bigint", 0, false)
	requireColumn(t, tx, "usage_request_diagnostics", "usage_request_id", "character varying", 255, false)
	requireColumn(t, tx, "usage_request_diagnostics", "usage_created_at", "timestamp with time zone", 0, false)
	requireColumn(t, tx, "usage_request_diagnostics", "payload_gzip", "bytea", 0, false)
	requireColumn(t, tx, "usage_request_diagnostics", "payload_bytes", "integer", 0, false)
	requireConstraintDefinitionContains(t, tx, "usage_request_diagnostics", "usage_request_diagnostics_pkey", "api_key_id, usage_request_id")
	requireConstraintDefinitionContains(t, tx, "usage_request_diagnostics", "usage_request_diagnostics_payload_bytes_check", "payload_bytes >= 0", "payload_bytes <= 16777216")
	requireIndex(t, tx, "usage_request_diagnostics", "usage_request_diagnostics_expiry_idx")
	requireIndex(t, tx, "usage_request_diagnostics", "usage_request_diagnostics_captured_idx")
}

func TestRequestDiagnosticRepositoryIntegrationNanosecondBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nanos  int
		offset int
	}{
		{name: "round_down", nanos: 123456499},
		{name: "round_up", nanos: 123456501},
		{name: "round_into_next_second", nanos: 999999501},
		{name: "non_utc_zone", nanos: 123456789, offset: 8 * 60 * 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRequestDiagnosticIntegrationFixture(t)
			now := f.now(t)
			rawTime := now.Truncate(time.Second).Add(-time.Minute + time.Duration(tc.nanos))
			if tc.offset != 0 {
				rawTime = rawTime.In(time.FixedZone("diagnostic-test-zone", tc.offset))
			}
			// Create 会回填 PG 时间，必须另存捕获时的纳秒原值再执行 Save。
			usage := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), rawTime)
			var pgTime time.Time
			require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT $1::timestamptz`, rawTime).Scan(&pgTime))
			require.True(t, pgTime.Equal(rawTime.Round(time.Microsecond)), "PG 应把纳秒参数舍入到微秒")
			require.True(t, usage.CreatedAt.Equal(pgTime), "真实 UsageLogRepository 与 PG 参数转换必须一致")
			require.NotZero(t, rawTime.Nanosecond()%1000)
			require.Zero(t, usage.CreatedAt.Nanosecond()%1000)
			t.Logf("created_at original=%s PostgreSQL=%s", rawTime.Format(time.RFC3339Nano), pgTime.Format(time.RFC3339Nano))

			record := requestDiagnosticIntegrationRecord(t, usage, rawTime, now, now.Add(time.Hour), "nanosecond-bound-prompt")
			f.save(t, record, requestDiagnosticIntegrationStorageBudget)
			got, err := f.repo.Get(f.ctx, usage.APIKeyID, usage.RequestID)
			require.NoError(t, err)
			require.NotNil(t, got, "Save 不得因 created_at 舍入不一致而静默丢弃合法快照")
			require.True(t, got.UsageCreatedAt.Equal(usage.CreatedAt))
			require.Equal(t, record.PayloadGzip, got.PayloadGzip)
			require.Equal(t, record.PayloadBytes, got.PayloadBytes)
			detail, err := f.service(t).Get(f.ctx, usage)
			require.NoError(t, err)
			require.Equal(t, "captured", detail.Status)
			require.NotNil(t, detail.Payload)
			require.True(t, detail.Payload.Binding.UsageCreatedAt.Equal(rawTime))
			require.Contains(t, detail.Payload.Meta.Body.Content, "nanosecond-bound-prompt")
		})
	}
}

func TestRequestDiagnosticRepositoryIntegrationRejectsWrongCanonicalBinding(t *testing.T) {
	for _, mismatch := range []string{"request_id", "api_key", "microsecond", "millisecond"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newRequestDiagnosticIntegrationFixture(t)
			now := f.now(t)
			usage := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), now)
			record := requestDiagnosticIntegrationRecord(t, usage, usage.CreatedAt, now, now.Add(time.Hour), "must-not-cross-bind")
			switch mismatch {
			case "request_id":
				record.UsageRequestID = "client-id-not-canonical"
			case "api_key":
				record.APIKeyID = f.newKey(t).ID
			case "microsecond":
				record.UsageCreatedAt = record.UsageCreatedAt.Add(time.Microsecond)
			case "millisecond":
				record.UsageCreatedAt = record.UsageCreatedAt.Add(time.Millisecond)
			}
			f.save(t, record, requestDiagnosticIntegrationStorageBudget)
			got, err := f.repo.Get(f.ctx, record.APIKeyID, record.UsageRequestID)
			require.NoError(t, err)
			require.Nil(t, got, "不属于精确 canonical 用量行的快照不能入库")
		})
	}
}

func TestRequestDiagnosticRepositoryIntegrationSeparatesSameCanonicalIDAcrossAPIKeys(t *testing.T) {
	f := newRequestDiagnosticIntegrationFixture(t)
	now := f.now(t)
	canonicalID := "diagnostic-it-" + uuid.NewString()
	secondKey := f.newKey(t)
	for index, key := range []*service.APIKey{f.keys[0], secondKey} {
		usage := f.createUsage(t, key, canonicalID, now)
		prompt := []string{"first-key-prompt", "second-key-prompt"}[index]
		record := requestDiagnosticIntegrationRecord(t, usage, usage.CreatedAt, now, now.Add(time.Hour), prompt)
		f.save(t, record, requestDiagnosticIntegrationStorageBudget)
	}
	for index, key := range []*service.APIKey{f.keys[0], secondKey} {
		got, err := f.repo.Get(f.ctx, key.ID, canonicalID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, key.ID, got.APIKeyID)
		reader, err := gzip.NewReader(bytes.NewReader(got.PayloadGzip))
		require.NoError(t, err)
		var snapshot requestdiagnostic.Snapshot
		require.NoError(t, json.NewDecoder(reader).Decode(&snapshot))
		require.NoError(t, reader.Close())
		require.Contains(t, snapshot.Meta.Body.Content, []string{"first-key-prompt", "second-key-prompt"}[index])
	}
}

func TestRequestDiagnosticRepositoryIntegrationDuplicateDoesNotReplaceBody(t *testing.T) {
	f := newRequestDiagnosticIntegrationFixture(t)
	now := f.now(t)
	usage := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), now)
	first := requestDiagnosticIntegrationRecord(t, usage, usage.CreatedAt, now, now.Add(time.Hour), "first-captured-prompt")
	f.save(t, first, requestDiagnosticIntegrationStorageBudget)
	replacement := requestDiagnosticIntegrationRecord(t, usage, usage.CreatedAt, now.Add(time.Minute), now.Add(2*time.Hour), "must-not-replace-first")
	f.save(t, replacement, requestDiagnosticIntegrationStorageBudget)
	got, err := f.repo.Get(f.ctx, usage.APIKeyID, usage.RequestID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, first.PayloadGzip, got.PayloadGzip)
	require.Equal(t, first.PayloadBytes, got.PayloadBytes)
	require.True(t, first.CapturedAt.Equal(got.CapturedAt))
	require.True(t, first.ExpiresAt.Equal(got.ExpiresAt))

	// 无旧快照时，Create 的重复回填也不能让新请求正文挂到历史 usage。
	old := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), now)
	newRequestTime := now.Add(time.Second)
	duplicate := &service.UsageLog{UserID: f.user.ID, APIKeyID: old.APIKeyID, AccountID: f.account.ID, RequestID: old.RequestID, Model: "diagnostic-test-model", CreatedAt: newRequestTime}
	inserted, err := f.usageRepo.Create(f.ctx, duplicate)
	require.NoError(t, err)
	require.False(t, inserted)
	require.Equal(t, old.ID, duplicate.ID)
	require.True(t, old.CreatedAt.Equal(duplicate.CreatedAt))
	newBody := requestDiagnosticIntegrationRecord(t, duplicate, newRequestTime, now, now.Add(time.Hour), "new-request-not-historical")
	f.save(t, newBody, requestDiagnosticIntegrationStorageBudget)
	missing, err := f.repo.Get(f.ctx, old.APIKeyID, old.RequestID)
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestRequestDiagnosticRepositoryIntegrationCapacityEvictsOldestBody(t *testing.T) {
	f := newRequestDiagnosticIntegrationFixture(t)
	now := f.now(t)
	oldUsage := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), now)
	newUsage := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), now)
	oldRecord := requestDiagnosticIntegrationRecord(t, oldUsage, oldUsage.CreatedAt, now.Add(-time.Minute), now.Add(time.Hour), "oldest-body")
	newRecord := requestDiagnosticIntegrationRecord(t, newUsage, newUsage.CreatedAt, now, now.Add(time.Hour), "latest-body")
	f.save(t, oldRecord, requestDiagnosticIntegrationStorageBudget)
	budget := int64(len(newRecord.PayloadGzip))
	f.save(t, newRecord, budget)
	old, err := f.repo.Get(f.ctx, oldUsage.APIKeyID, oldUsage.RequestID)
	require.NoError(t, err)
	require.NotNil(t, old, "淘汰保留元数据，不应伪装成从未捕获")
	require.Zero(t, old.PayloadBytes)
	require.Empty(t, old.PayloadGzip)
	latest, err := f.repo.Get(f.ctx, newUsage.APIKeyID, newUsage.RequestID)
	require.NoError(t, err)
	require.Equal(t, newRecord.PayloadGzip, latest.PayloadGzip)
	var stored int64
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT coalesce(sum(octet_length(payload_gzip)), 0) FROM usage_request_diagnostics`).Scan(&stored))
	require.LessOrEqual(t, stored, budget, "全局容量按实际压缩字节限制")
	svc := f.service(t)
	detail, err := svc.Get(f.ctx, oldUsage)
	require.NoError(t, err)
	require.Equal(t, "evicted", detail.Status)
	require.Nil(t, detail.Payload)
	detail, err = svc.Get(f.ctx, newUsage)
	require.NoError(t, err)
	require.Equal(t, "captured", detail.Status)
}

func TestRequestDiagnosticRepositoryIntegrationTTLReadAndCleanup(t *testing.T) {
	f := newRequestDiagnosticIntegrationFixture(t)
	now := f.now(t)
	usages := make(map[string]*service.UsageLog)
	for _, name := range []string{"active", "expired", "orphan", "recent_orphan", "old_tombstone"} {
		usage := f.createUsage(t, f.keys[0], "diagnostic-it-"+uuid.NewString(), now)
		usages[name] = usage
		captured, expires := now.Add(-2*time.Minute), now.Add(time.Hour)
		switch name {
		case "expired":
			expires = now.Add(-time.Minute)
		case "recent_orphan":
			captured = now
		case "old_tombstone":
			captured, expires = now.Add(-9*24*time.Hour), now.Add(-8*24*time.Hour)
		}
		f.save(t, requestDiagnosticIntegrationRecord(t, usage, usage.CreatedAt, captured, expires, name+"-body"), requestDiagnosticIntegrationStorageBudget)
	}
	for _, name := range []string{"orphan", "recent_orphan"} {
		usage := usages[name]
		result, err := integrationDB.ExecContext(f.ctx, `DELETE FROM usage_logs WHERE id = $1 AND api_key_id = $2`, usage.ID, usage.APIKeyID)
		require.NoError(t, err)
		deleted, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, deleted)
	}

	expiredUsage := usages["expired"]
	var physicalBytes int
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT octet_length(payload_gzip) FROM usage_request_diagnostics WHERE api_key_id = $1 AND usage_request_id = $2`, expiredUsage.APIKeyID, expiredUsage.RequestID).Scan(&physicalBytes))
	require.Positive(t, physicalBytes, "先证明正文物理存在，Get 才能验证 TTL 读取遮蔽")
	expired, err := f.repo.Get(f.ctx, expiredUsage.APIKeyID, expiredUsage.RequestID)
	require.NoError(t, err)
	require.NotNil(t, expired)
	require.Empty(t, expired.PayloadGzip)
	require.Zero(t, expired.PayloadBytes)
	detail, err := f.service(t).Get(f.ctx, expiredUsage)
	require.NoError(t, err)
	require.Equal(t, "expired", detail.Status)
	require.Nil(t, detail.Payload)

	f.requireOwnDiagnosticRows(t)
	require.NoError(t, f.repo.Cleanup(f.ctx))
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT octet_length(payload_gzip) FROM usage_request_diagnostics WHERE api_key_id = $1 AND usage_request_id = $2`, expiredUsage.APIKeyID, expiredUsage.RequestID).Scan(&physicalBytes))
	require.Zero(t, physicalBytes, "Cleanup 必须物理清掉过期正文，仍保留近期过期墓碑")
	for _, name := range []string{"orphan", "old_tombstone"} {
		usage := usages[name]
		got, err := f.repo.Get(f.ctx, usage.APIKeyID, usage.RequestID)
		require.NoError(t, err)
		require.Nil(t, got, "陈旧 orphan 或超过七天的墓碑必须被删除")
	}
	for _, name := range []string{"active", "recent_orphan"} {
		usage := usages[name]
		got, err := f.repo.Get(f.ctx, usage.APIKeyID, usage.RequestID)
		require.NoError(t, err)
		require.NotNil(t, got, "有效记录和一分钟内 orphan 宽限记录必须保留")
		require.Positive(t, got.PayloadBytes)
		require.NotEmpty(t, got.PayloadGzip)
	}
}
