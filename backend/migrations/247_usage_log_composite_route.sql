-- Composite route attribution for admin usage records. Nullable columns are
-- metadata-only additions, so historical rows stay untouched.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS composite_route_id BIGINT;

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS composite_route_kind VARCHAR(20);

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS composite_fallback_chain TEXT;

COMMENT ON COLUMN usage_logs.composite_route_id IS
    'Explicit composite route that served the request; NULL when no explicit route matched';
COMMENT ON COLUMN usage_logs.composite_route_kind IS
    'Request classification at route time: compaction, conversation, or unknown';
COMMENT ON COLUMN usage_logs.composite_fallback_chain IS
    'Failed upstream targets before the serving fallback, e.g. "model-a (429) -> model-b"; NULL when the primary served';
