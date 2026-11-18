-- 把 kiro 加入渠道监控 provider 的 CHECK 约束（channel_monitors 与
-- channel_monitor_request_templates），来自 nianzs/sub2api 229。
--
-- 渠道监控 provider 表示已实现的探测能力；kiro 与 antigravity 一致，只允许配额模式
-- （check_mode='quota'），因为 Kiro 走 event-stream 协议，没有探活 adapter。
--
-- user_platform_quotas / composite_model_routes 的 platform CHECK 已由
-- 242_drop_platform_check_constraints 移除，改由 domain 平台清单校验，本迁移不再触碰。
--
-- 幂等守卫的探测键必须是 'kiro'。新约束是 238 约束的超集，存量行瞬时校验通过。

DO $$
DECLARE
    monitor_constraint_def TEXT;
    template_constraint_def TEXT;
BEGIN
    SELECT pg_get_constraintdef(c.oid)
      INTO monitor_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitors'
       AND c.conname = 'channel_monitors_provider_check';

    IF monitor_constraint_def IS NULL OR position('kiro' IN monitor_constraint_def) = 0 THEN
        ALTER TABLE channel_monitors
            DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
        ALTER TABLE channel_monitors
            ADD CONSTRAINT channel_monitors_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok',
                                'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));
    END IF;

    SELECT pg_get_constraintdef(c.oid)
      INTO template_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitor_request_templates'
       AND c.conname = 'channel_monitor_request_templates_provider_check';

    IF template_constraint_def IS NULL OR position('kiro' IN template_constraint_def) = 0 THEN
        ALTER TABLE channel_monitor_request_templates
            DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
        ALTER TABLE channel_monitor_request_templates
            ADD CONSTRAINT channel_monitor_request_templates_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok',
                                'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));
    END IF;
END $$;
