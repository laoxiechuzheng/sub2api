-- 诊断单条预算放宽到 128 MiB 后，JSON 编码（含转义）后的正文可超过 16 MiB。
-- 242 中的 payload_bytes 上限 16 MiB 会让这类记录整条写库失败，这里改为 512 MiB，
-- 与 service.maxDiagnosticJSONBytes 保持一致。诊断表保留期很短，直接重建约束即可。
ALTER TABLE usage_request_diagnostics
    DROP CONSTRAINT IF EXISTS usage_request_diagnostics_payload_bytes_check,
    ADD CONSTRAINT usage_request_diagnostics_payload_bytes_check
        CHECK (payload_bytes >= 0 AND payload_bytes <= 536870912);
