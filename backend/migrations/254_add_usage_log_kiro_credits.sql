-- 每条使用记录保存 Kiro credits 消耗，用于账号用量统计（来自 nianzs/sub2api 153）。
-- 可空列且无默认值：PostgreSQL 仅改元数据，不重写 usage_logs 大表。
SET LOCAL lock_timeout = '5s';

ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS kiro_credits NUMERIC(20,10);

COMMENT ON COLUMN usage_logs.kiro_credits IS 'Kiro credits consumed by this usage log';
