-- Ordered fallback targets for composite routes. The default keeps legacy
-- routes and writes from older binaries on their single upstream model.
ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS fallback_models JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN composite_model_routes.fallback_models IS
    'Ordered [{target_platform?, upstream_model}] tried when the primary upstream model fails';

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_fallback_models_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_fallback_models_check
    CHECK (jsonb_typeof(fallback_models) = 'array' AND jsonb_array_length(fallback_models) <= 8);
