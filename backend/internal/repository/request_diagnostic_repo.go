package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type requestDiagnosticRepository struct{ db *sql.DB }

func NewRequestDiagnosticRepository(db *sql.DB) service.RequestDiagnosticRepository {
	return &requestDiagnosticRepository{db: db}
}

func (r *requestDiagnosticRepository) Save(ctx context.Context, record *service.RequestDiagnosticRecord, maxStorageBytes int64) error {
	if r.db == nil || record == nil {
		return errors.New("request diagnostic storage unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 跨副本容量检查串行化；忙时丢诊断，不阻塞请求或计费。
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(242, 1)`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return errors.New("request diagnostic storage busy")
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO usage_request_diagnostics
			(api_key_id, usage_request_id, usage_created_at, captured_at, expires_at, payload_gzip, payload_bytes)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE EXISTS (
			SELECT 1 FROM usage_logs
			WHERE api_key_id = $1 AND request_id = $2
			AND created_at = $3::timestamptz
		)
		ON CONFLICT (api_key_id, usage_request_id) DO NOTHING`,
		record.APIKeyID, record.UsageRequestID, record.UsageCreatedAt,
		record.CapturedAt, record.ExpiresAt, record.PayloadGzip, record.PayloadBytes)
	if err != nil {
		return err
	}

	// 按实际压缩字节封顶；被挤出的行保留空的元数据，详情不伪称完整。
	_, err = tx.ExecContext(ctx, `
		WITH budget AS (
			SELECT api_key_id, usage_request_id,
				sum(octet_length(payload_gzip)) OVER (ORDER BY captured_at DESC, api_key_id, usage_request_id) AS total
			FROM usage_request_diagnostics WHERE payload_bytes > 0
		)
		UPDATE usage_request_diagnostics d SET payload_gzip = ''::bytea, payload_bytes = 0
		FROM budget b WHERE d.api_key_id = b.api_key_id AND d.usage_request_id = b.usage_request_id AND b.total > $1`, maxStorageBytes)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *requestDiagnosticRepository) Get(ctx context.Context, apiKeyID int64, requestID string) (*service.RequestDiagnosticRecord, error) {
	if r.db == nil {
		return nil, errors.New("request diagnostic storage unavailable")
	}
	record := &service.RequestDiagnosticRecord{}
	err := r.db.QueryRowContext(ctx, `SELECT api_key_id, usage_request_id, usage_created_at, captured_at, expires_at,
		CASE WHEN expires_at > now() THEN payload_gzip ELSE ''::bytea END,
		CASE WHEN expires_at > now() THEN payload_bytes ELSE 0 END
		FROM usage_request_diagnostics WHERE api_key_id = $1 AND usage_request_id = $2`, apiKeyID, requestID).Scan(
		&record.APIKeyID, &record.UsageRequestID, &record.UsageCreatedAt,
		&record.CapturedAt, &record.ExpiresAt, &record.PayloadGzip, &record.PayloadBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get request diagnostic: %w", err)
	}
	return record, nil
}

func (r *requestDiagnosticRepository) Cleanup(ctx context.Context) error {
	if r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		WITH expired AS (
			SELECT api_key_id, usage_request_id FROM usage_request_diagnostics
			WHERE expires_at <= now() AND payload_bytes > 0 ORDER BY expires_at LIMIT 500
		)
		UPDATE usage_request_diagnostics d SET payload_gzip = ''::bytea, payload_bytes = 0
		FROM expired e WHERE d.api_key_id = e.api_key_id AND d.usage_request_id = e.usage_request_id`)
	if err != nil {
		return err
	}
	// 用量删除/分区清理后的正文也清除；到期墓碑最多额外保留七天。
	_, err = r.db.ExecContext(ctx, `
		WITH stale AS (
			SELECT d.api_key_id, d.usage_request_id FROM usage_request_diagnostics d
			WHERE d.expires_at < now() - interval '7 days'
			OR (d.captured_at < now() - interval '1 minute' AND NOT EXISTS (
				SELECT 1 FROM usage_logs u WHERE u.api_key_id = d.api_key_id AND u.request_id = d.usage_request_id
				AND u.created_at = d.usage_created_at
			)) ORDER BY d.expires_at LIMIT 500
		)
		DELETE FROM usage_request_diagnostics d USING stale s
		WHERE d.api_key_id = s.api_key_id AND d.usage_request_id = s.usage_request_id`)
	return err
}
