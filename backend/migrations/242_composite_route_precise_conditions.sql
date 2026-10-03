-- Defaults preserve legacy routes and writes from older binaries.
ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS request_kind VARCHAR(20) NOT NULL DEFAULT 'any';

ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS body_match_scope VARCHAR(32) NOT NULL DEFAULT 'full_body';

ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS body_match_mode VARCHAR(20) NOT NULL DEFAULT 'any';

ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS body_not_contains TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN composite_model_routes.request_kind IS
    'Request classification: any, compaction, or conversation';
COMMENT ON COLUMN composite_model_routes.body_match_scope IS
    'Body scope: full_body, instructions, last_message, or current_turn';
COMMENT ON COLUMN composite_model_routes.body_match_mode IS
    'Body substring matching: any, all, or prefix';
COMMENT ON COLUMN composite_model_routes.body_not_contains IS
    'Newline-separated body exclusions; any match rejects the route';

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_request_kind_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_request_kind_check
    CHECK (request_kind IN ('any', 'compaction', 'conversation'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_body_match_scope_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_body_match_scope_check
    CHECK (body_match_scope IN ('full_body', 'instructions', 'last_message', 'current_turn'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_body_match_mode_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_body_match_mode_check
    CHECK (body_match_mode IN ('any', 'all', 'prefix'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_body_not_contains_length_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_body_not_contains_length_check
    CHECK (octet_length(body_not_contains) <= 8192);

-- Keep distinct precise conditions separate without indexing long text values.
DROP INDEX IF EXISTS idx_composite_model_routes_unique_active;
CREATE UNIQUE INDEX IF NOT EXISTS idx_composite_model_routes_unique_active
    ON composite_model_routes (
        group_id,
        endpoint,
        match_type,
        public_model,
        request_kind,
        body_match_scope,
        body_match_mode,
        md5(user_agent_contains),
        md5(body_contains),
        md5(body_not_contains)
    )
    WHERE deleted_at IS NULL;
