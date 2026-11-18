-- 网关设置：Kiro 计量算法与缓存模拟分桶方式（合并自 nianzs/sub2api 246 与 250）。
--
-- gateway_kiro_tokenizer: official = Anthropic 参考 BPE 精确计数（默认）；
--   charclass / weighted = 轻量启发式估算口径。
-- gateway_kiro_cache_scope_client: false = 按 Kiro 账号分桶（默认）；
--   true = 按「API key + 会话」分桶，切换上游账号不丢缓存命中。仅影响计费口径。

INSERT INTO settings (key, value, updated_at)
VALUES ('gateway_kiro_tokenizer', 'official', NOW())
ON CONFLICT (key) DO NOTHING;

INSERT INTO settings (key, value, updated_at)
VALUES ('gateway_kiro_cache_scope_client', 'false', NOW())
ON CONFLICT (key) DO NOTHING;
