-- d78 app-only rollback may publish retained_from before expired_before.
-- Such old readers still consume historical buckets for physically present
-- expired rows. Their later deletion must invalidate those buckets as well.
-- Keep 251's nonblocking locks and dirty tracking; do not move any watermark.

SET LOCAL lock_timeout = '5s';

-- Sufficient proof that a floor-before change cannot affect old historical
-- buckets. Alias/timezone changes, partial days, and ambiguous day boundaries
-- conservatively use ordinary invalidation. STRICT callers coalesce NULL to
-- false. This predicate never asserts that any new bucket has been built.
CREATE OR REPLACE FUNCTION group_usage_rollup_can_ignore_expired_changes(
    state_retained_from TIMESTAMPTZ,
    state_expired_before TIMESTAMPTZ,
    state_timezone TEXT,
    writer_timezone TEXT
)
RETURNS BOOLEAN
LANGUAGE SQL
STABLE
STRICT
AS $$
    SELECT CASE
        WHEN state_timezone <> writer_timezone
            OR state_retained_from < state_expired_before THEN FALSE
        ELSE
            (state_expired_before AT TIME ZONE writer_timezone)::time = TIME '00:00:00'
            AND ((state_expired_before AT TIME ZONE writer_timezone)::date::timestamp AT TIME ZONE writer_timezone)
                = state_expired_before
            AND ((state_expired_before - INTERVAL '1 microsecond') AT TIME ZONE writer_timezone)::date
                < (state_expired_before AT TIME ZONE writer_timezone)::date
        END;
$$;

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollups_at(affected_at TIMESTAMPTZ, bucket_timezone TEXT)
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    state_expired_before TIMESTAMPTZ;
    state_retained_from TIMESTAMPTZ;
    state_timezone TEXT;
    ignore_expired BOOLEAN;
    affected_date DATE;
BEGIN
    SELECT expired_before, retained_from, timezone_name
    INTO state_expired_before, state_retained_from, state_timezone
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    IF NOT FOUND OR affected_at IS NULL THEN
        RETURN;
    END IF;

    ignore_expired := COALESCE(group_usage_rollup_can_ignore_expired_changes(
        state_retained_from, state_expired_before, state_timezone, bucket_timezone
    ), FALSE);
    IF ignore_expired THEN
        affected_at := GREATEST(affected_at, state_expired_before);
    END IF;
    affected_date := LEAST(
        (affected_at AT TIME ZONE bucket_timezone)::date,
        (affected_at AT TIME ZONE state_timezone)::date
    );
    PERFORM apply_group_usage_rollup_invalidation(affected_date);
END;
$$;

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    configured_timezone TEXT := current_setting('TimeZone');
    old_date DATE;
    new_date DATE;
    affected_date DATE;
    state_closed_before DATE;
    state_building_until DATE;
    state_build_dirty_from DATE;
    state_expired_before TIMESTAMPTZ;
    state_retained_from TIMESTAMPTZ;
    state_timezone TEXT;
    ignore_expired BOOLEAN;
BEGIN
    SELECT closed_before, building_until, build_dirty_from, expired_before, retained_from, timezone_name
    INTO state_closed_before, state_building_until, state_build_dirty_from, state_expired_before, state_retained_from, state_timezone
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    IF FOUND THEN
        ignore_expired := COALESCE(group_usage_rollup_can_ignore_expired_changes(
            state_retained_from, state_expired_before, state_timezone, configured_timezone
        ), FALSE);
        IF OLD.group_id IS NOT NULL AND (NOT ignore_expired OR OLD.created_at >= state_expired_before) THEN
            old_date := LEAST(
                (OLD.created_at AT TIME ZONE configured_timezone)::date,
                (OLD.created_at AT TIME ZONE state_timezone)::date
            );
        END IF;
        IF TG_OP = 'UPDATE' THEN
            IF NEW.group_id IS NOT NULL AND (NOT ignore_expired OR NEW.created_at >= state_expired_before) THEN
                new_date := LEAST(
                    (NEW.created_at AT TIME ZONE configured_timezone)::date,
                    (NEW.created_at AT TIME ZONE state_timezone)::date
                );
            END IF;
        END IF;

        affected_date := LEAST(old_date, new_date);
        IF state_closed_before > affected_date
            OR (
                state_building_until > affected_date
                AND (state_build_dirty_from IS NULL OR state_build_dirty_from > affected_date)
            ) THEN
            PERFORM apply_group_usage_rollup_invalidation(affected_date);
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state_after_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    configured_timezone TEXT := current_setting('TimeZone');
    affected_date DATE;
    state_closed_before DATE;
    state_building_until DATE;
    state_build_dirty_from DATE;
    state_expired_before TIMESTAMPTZ;
    state_retained_from TIMESTAMPTZ;
    state_timezone TEXT;
    ignore_expired BOOLEAN;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM inserted_usage_logs WHERE group_id IS NOT NULL) THEN
        RETURN NULL;
    END IF;

    SELECT closed_before, building_until, build_dirty_from, expired_before, retained_from, timezone_name
    INTO state_closed_before, state_building_until, state_build_dirty_from, state_expired_before, state_retained_from, state_timezone
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    ignore_expired := COALESCE(group_usage_rollup_can_ignore_expired_changes(
        state_retained_from, state_expired_before, state_timezone, configured_timezone
    ), FALSE);
    SELECT MIN(LEAST(
        (created_at AT TIME ZONE configured_timezone)::date,
        (created_at AT TIME ZONE state_timezone)::date
    ))
    INTO affected_date
    FROM inserted_usage_logs
    WHERE group_id IS NOT NULL
        AND (NOT ignore_expired OR created_at >= state_expired_before);

    IF state_closed_before > affected_date
        OR (
            state_building_until > affected_date
            AND (state_build_dirty_from IS NULL OR state_build_dirty_from > affected_date)
        ) THEN
        PERFORM apply_group_usage_rollup_invalidation(affected_date);
    END IF;

    RETURN NULL;
END;
$$;
