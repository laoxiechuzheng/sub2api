-- Request-conditioned Composite routes: a route may additionally require the
-- inbound User-Agent and/or request body to contain one of its configured
-- signatures. Conditional routes override unconditional routes.
ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS user_agent_contains TEXT NOT NULL DEFAULT '';

ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS body_contains TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN composite_model_routes.user_agent_contains IS
    'Newline-separated User-Agent substrings; empty disables the condition';
COMMENT ON COLUMN composite_model_routes.body_contains IS
    'Newline-separated request-body substrings; empty disables the condition';

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_match_type_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_match_type_check
    CHECK (match_type IN ('exact', 'prefix', 'contains'));

-- Include the conditions in the dedup key. md5() keeps long signature lists
-- within PostgreSQL btree size limits.
DROP INDEX IF EXISTS idx_composite_model_routes_unique_active;

CREATE UNIQUE INDEX IF NOT EXISTS idx_composite_model_routes_unique_active
    ON composite_model_routes (
        group_id,
        endpoint,
        match_type,
        public_model,
        md5(user_agent_contains),
        md5(body_contains)
    )
    WHERE deleted_at IS NULL;
