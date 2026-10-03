-- 请求正文独立于 usage_logs 保存；分区用量表不建立不兼容的单列外键。
CREATE TABLE IF NOT EXISTS usage_request_diagnostics (
    api_key_id BIGINT NOT NULL,
    usage_request_id VARCHAR(255) NOT NULL,
    usage_created_at TIMESTAMPTZ NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    payload_gzip BYTEA NOT NULL,
    payload_bytes INTEGER NOT NULL CHECK (payload_bytes >= 0 AND payload_bytes <= 16777216),
    PRIMARY KEY (api_key_id, usage_request_id)
);

CREATE INDEX IF NOT EXISTS usage_request_diagnostics_expiry_idx
    ON usage_request_diagnostics (expires_at);

CREATE INDEX IF NOT EXISTS usage_request_diagnostics_captured_idx
    ON usage_request_diagnostics (captured_at);
