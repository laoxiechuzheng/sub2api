package migrations

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedChannelMonitorProviders 是渠道监控 provider 允许的集合，需与
// service.monitorProviders 及 ent/schema/channel_monitor*.go 的 provider enum 同步。
// 此处硬编码而非 import service：migrations 是叶子包，被 repository 依赖，反向 import 会成环。
var expectedChannelMonitorProviders = []string{
	"anthropic", "antigravity", "deepseek", "gemini", "grok", "kimi", "kiro", "minimax", "openai", "opencode_go", "zhipu",
}

var channelMonitorProviderConstraints = []string{
	"channel_monitors_provider_check",
	"channel_monitor_request_templates_provider_check",
}

func normalizedMigration(t *testing.T, name string) string {
	t.Helper()
	content, err := FS.ReadFile(name)
	require.NoError(t, err)
	return strings.Join(strings.Fields(string(content)), " ")
}

func TestChannelMonitorKiroProviderMigration(t *testing.T) {
	sql := normalizedMigration(t, "255_channel_monitor_kiro_provider.sql")

	for _, constraint := range channelMonitorProviderConstraints {
		require.Contains(t, sql, constraint)
	}
	require.Contains(t, sql,
		"CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'))")

	// 幂等守卫按 kiro 探测，不能复用 kimi 等已写入旧约束的值，否则在存量库上 no-op。
	require.Contains(t, sql, "position('kiro' IN monitor_constraint_def) = 0")
	require.Contains(t, sql, "position('kiro' IN template_constraint_def) = 0")
	require.NotContains(t, sql, "position('kimi' IN")
}

// 防回归护栏：按文件名顺序（= 迁移执行顺序）取最后一个重建 provider 约束的迁移，
// 断言其列表覆盖全部允许 provider，避免后续重建约束时漏项。
func TestChannelMonitorProviderCheckFinalStateCoversAllProviders(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	var sqlFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			sqlFiles = append(sqlFiles, e.Name())
		}
	}
	sort.Strings(sqlFiles)

	checkRe := regexp.MustCompile(`CHECK \(provider IN \(([^)]*)\)\)`)
	for _, constraint := range channelMonitorProviderConstraints {
		var last string
		for _, name := range sqlFiles {
			body, readErr := FS.ReadFile(name)
			require.NoError(t, readErr)
			if strings.Contains(string(body), constraint) {
				last = name
			}
		}
		require.NotEmpty(t, last, "no migration touches %s", constraint)

		normalized := normalizedMigration(t, last)
		matches := checkRe.FindAllStringSubmatch(normalized, -1)
		require.NotEmpty(t, matches, "migration %s rebuilds %s but has no CHECK (provider IN (...))", last, constraint)

		var got []string
		for _, raw := range strings.Split(matches[len(matches)-1][1], ",") {
			if v := strings.Trim(strings.TrimSpace(raw), "'"); v != "" {
				got = append(got, v)
			}
		}
		sort.Strings(got)
		require.Equal(t, expectedChannelMonitorProviders, got,
			"migration %s 定义的 provider 列表与允许集合不一致；重建 %s 时必须列出全部 provider", last, constraint)
	}
}

func TestKiroGroupSettingsMigrationIsIdempotent(t *testing.T) {
	sql := normalizedMigration(t, "253_add_group_kiro_settings.sql")

	for _, column := range []string{
		"kiro_cache_emulation_enabled", "kiro_cache_emulation_ratio", "kiro_auto_sticky_enabled",
		"kiro_sticky_session_ttl_seconds", "kiro_endpoint_mode", "kiro_cache_emulation_mode",
		"kiro_cache_creation_emulation_ratio", "kiro_cache_read_emulation_ratio",
	} {
		require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS "+column)
	}
	// 约束重建必须先 DROP IF EXISTS，重复执行不报错。
	for _, constraint := range []string{
		"groups_kiro_cache_emulation_ratio_range",
		"groups_kiro_cache_emulation_mode_valid",
		"groups_kiro_cache_creation_emulation_ratio_range",
		"groups_kiro_cache_read_emulation_ratio_range",
	} {
		require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS "+constraint)
		require.Contains(t, sql, "ADD CONSTRAINT "+constraint)
	}
	// 独立比例回填为原统一比例，存量分组行为不变。
	require.Contains(t, sql, "kiro_cache_creation_emulation_ratio = kiro_cache_emulation_ratio")
	require.Contains(t, sql, "kiro_cache_read_emulation_ratio = kiro_cache_emulation_ratio")
	require.Contains(t, sql, "kiro_endpoint_mode VARCHAR(8) NOT NULL DEFAULT 'q'")
}

func TestKiroUsageLogCreditsMigration(t *testing.T) {
	sql := normalizedMigration(t, "254_add_usage_log_kiro_credits.sql")
	require.Contains(t, sql, "ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS kiro_credits NUMERIC(20,10)")
}

func TestKiroGatewaySettingsMigration(t *testing.T) {
	sql := normalizedMigration(t, "256_add_gateway_kiro_settings.sql")
	require.Contains(t, sql, "'gateway_kiro_tokenizer', 'official'")
	require.Contains(t, sql, "'gateway_kiro_cache_scope_client', 'false'")
	require.Equal(t, 2, strings.Count(sql, "ON CONFLICT (key) DO NOTHING"))
}

func TestKiroChannelMonitorSeedGrok47Migration(t *testing.T) {
	sql := normalizedMigration(t, "257_channel_monitor_v2_seed_kiro_grok47.sql")
	require.Contains(t, sql, `|| '["grok-4.7"]'::jsonb`)
	require.Contains(t, sql, "WHERE t.id = 1 AND EXISTS")
}
