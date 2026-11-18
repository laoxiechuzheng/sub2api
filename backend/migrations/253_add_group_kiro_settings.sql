-- Kiro 渠道分组配置（合并自 nianzs/sub2api 的 135/151/152/153/192 五个迁移，全部幂等）。
--   * 缓存模拟：启用开关、统一比例，以及 uniform / independent 两种模式下的创建/读取比例
--   * 自动会话粘性：开关与 TTL（秒）
--   * 推理 endpoint 选择：q（AWS Q，默认）/ krs（Kiro Runtime Service）
-- 存量分组的行为保持不变：缓存模拟默认关闭，独立比例回填为原统一比例。

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS kiro_cache_emulation_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS kiro_cache_emulation_ratio DECIMAL(5,4) NOT NULL DEFAULT 1.0,
    ADD COLUMN IF NOT EXISTS kiro_auto_sticky_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS kiro_sticky_session_ttl_seconds INT NOT NULL DEFAULT 3600,
    ADD COLUMN IF NOT EXISTS kiro_endpoint_mode VARCHAR(8) NOT NULL DEFAULT 'q',
    ADD COLUMN IF NOT EXISTS kiro_cache_emulation_mode VARCHAR(16) NOT NULL DEFAULT 'uniform',
    ADD COLUMN IF NOT EXISTS kiro_cache_creation_emulation_ratio DECIMAL(5,4),
    ADD COLUMN IF NOT EXISTS kiro_cache_read_emulation_ratio DECIMAL(5,4);

UPDATE groups
SET
    kiro_cache_emulation_mode = 'uniform',
    kiro_cache_creation_emulation_ratio = kiro_cache_emulation_ratio,
    kiro_cache_read_emulation_ratio = kiro_cache_emulation_ratio
WHERE kiro_cache_creation_emulation_ratio IS NULL
   OR kiro_cache_read_emulation_ratio IS NULL;

ALTER TABLE groups
    ALTER COLUMN kiro_cache_creation_emulation_ratio SET DEFAULT 1.0,
    ALTER COLUMN kiro_cache_creation_emulation_ratio SET NOT NULL,
    ALTER COLUMN kiro_cache_read_emulation_ratio SET DEFAULT 1.0,
    ALTER COLUMN kiro_cache_read_emulation_ratio SET NOT NULL;

ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_kiro_cache_emulation_ratio_range;
ALTER TABLE groups
    ADD CONSTRAINT groups_kiro_cache_emulation_ratio_range
    CHECK (kiro_cache_emulation_ratio >= 0 AND kiro_cache_emulation_ratio <= 1);

ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_kiro_cache_emulation_mode_valid;
ALTER TABLE groups
    ADD CONSTRAINT groups_kiro_cache_emulation_mode_valid
    CHECK (kiro_cache_emulation_mode IN ('uniform', 'independent'));

ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_kiro_cache_creation_emulation_ratio_range;
ALTER TABLE groups
    ADD CONSTRAINT groups_kiro_cache_creation_emulation_ratio_range
    CHECK (kiro_cache_creation_emulation_ratio >= 0 AND kiro_cache_creation_emulation_ratio <= 1);

ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_kiro_cache_read_emulation_ratio_range;
ALTER TABLE groups
    ADD CONSTRAINT groups_kiro_cache_read_emulation_ratio_range
    CHECK (kiro_cache_read_emulation_ratio >= 0 AND kiro_cache_read_emulation_ratio <= 1);
