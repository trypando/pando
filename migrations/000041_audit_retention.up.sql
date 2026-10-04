-- Audit retention (issue #60, R-347, R-348, design 02 §2.6).
--
-- The audit log kept every event forever, and R-224 says Pando must not be able
-- to brick a host through what it accumulates. Retention has to remove rows,
-- and the audit log is built so the server cannot: the application role has
-- INSERT and nothing else (R-027). That stays true. What this adds is narrow:
--
--   1. audit_events is partitioned by calendar month (UTC), so a month leaves
--      as one DROP TABLE rather than as row deletes.
--   2. Two owner-defined functions are the only way a month leaves or a
--      partition is made. Neither is executable by the application role; the
--      grants are applied with every other grant, by state.applyGrants, to a
--      separate login role that does nothing but archive.
--   3. audit_drop_month refuses, in the database, any month that ended less
--      than three months ago (R-348's floor) and any month without a recorded
--      archive holding exactly the rows it would remove. Policy can lengthen
--      retention; nothing the running server holds can shorten it below the
--      floor or remove a month nobody archived.

-- Every archive written. Inserted by the archiver role after the archive has
-- been written and read back; readable by the application role, which lists
-- and serves them, and writable by nothing else (state.applyGrants).
CREATE TABLE audit_archives (
    id          text PRIMARY KEY,                -- aar_...
    month       date NOT NULL CHECK (month = date_trunc('month', month)::date),

    -- NULL when Pando keeps it under its own directory; otherwise the backup
    -- adapter it was exported to (R-217). Resolved once and recorded, like a
    -- backup's: an archive is not looked for anywhere else.
    adapter_ref text,
    object_name text NOT NULL,

    -- The manifest. row_count and the id range are what audit_drop_month
    -- checks against the rows it removes; sha256 is of the stored bytes.
    row_count   bigint NOT NULL CHECK (row_count > 0),
    first_id    bigint NOT NULL,
    last_id     bigint NOT NULL,
    first_at    timestamptz NOT NULL,
    last_at     timestamptz NOT NULL,
    size_bytes  bigint NOT NULL CHECK (size_bytes > 0),
    sha256      text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT audit_archives_object_is_unique UNIQUE NULLS NOT DISTINCT (adapter_ref, object_name)
);
CREATE INDEX audit_archives_month_idx ON audit_archives (month);

-- The table, partitioned. Same columns and constraints as 000001; the primary
-- key gains occurred_at because a partitioned table's must include the
-- partition key. id stays unique in practice — it comes from one sequence.
ALTER SEQUENCE audit_events_id_seq OWNED BY NONE;
ALTER TABLE audit_events RENAME TO audit_events_unpartitioned;
ALTER INDEX audit_events_pkey RENAME TO audit_events_unpartitioned_pkey;
ALTER INDEX audit_events_app_idx RENAME TO audit_events_unpartitioned_app_idx;
ALTER INDEX audit_events_principal_idx RENAME TO audit_events_unpartitioned_principal_idx;
ALTER INDEX audit_events_action_idx RENAME TO audit_events_unpartitioned_action_idx;

CREATE TABLE audit_events (
    id             bigint NOT NULL DEFAULT nextval('audit_events_id_seq'),
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    principal_kind text NOT NULL
        CONSTRAINT audit_events_principal_kind_check
        CHECK (principal_kind IN ('user', 'token', 'system', 'anonymous')),
    principal_id   text,
    on_behalf_of   text,
    action         text NOT NULL,
    app_id         text,
    target_kind    text,
    target_id      text,
    request_id     text,
    detail         jsonb NOT NULL DEFAULT '{}',

    CONSTRAINT audit_events_principal_id_present
        CHECK (principal_kind = 'anonymous' OR principal_id IS NOT NULL),
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- A month with no partition yet still takes writes. Without this, an archiver
-- that had not run for two months would make every audit write fail, and the
-- audit write comes before the privileged action (R-228).
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;

CREATE INDEX audit_events_app_idx ON audit_events (app_id, occurred_at DESC);
CREATE INDEX audit_events_principal_idx ON audit_events (principal_id, occurred_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, occurred_at DESC);

-- audit_ensure_partition makes the partition for the month holding `month`,
-- if it does not exist, and returns its name.
--
-- Rows already in the default partition for that month move into it: Postgres
-- refuses a new partition whose range the default already holds rows for.
-- They move unchanged, in the same transaction.
--
-- A new partition inherits the default privileges the owner hands out, which
-- include UPDATE and DELETE for the application role. They are revoked here,
-- from everyone but the owner, before the partition is visible: a partition is
-- a table, and DELETE on one is DELETE on the audit log (R-027).
CREATE FUNCTION audit_ensure_partition(month date) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    first_day date := date_trunc('month', month)::date;
    lower_ts  timestamptz := first_day::timestamp AT TIME ZONE 'UTC';
    upper_ts  timestamptz := (first_day + interval '1 month')::timestamp AT TIME ZONE 'UTC';
    part      text := 'audit_events_' || to_char(first_day, 'YYYY_MM');
    holder    oid;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('pando.audit_partitions'));
    IF to_regclass('public.' || part) IS NOT NULL THEN
        RETURN part;
    END IF;

    IF to_regclass('pg_temp.audit_moving') IS NULL THEN
        CREATE TEMP TABLE audit_moving (LIKE public.audit_events) ON COMMIT DROP;
    END IF;
    TRUNCATE audit_moving;
    WITH moved AS (
        DELETE FROM public.audit_events_default
        WHERE occurred_at >= lower_ts AND occurred_at < upper_ts
        RETURNING *
    )
    INSERT INTO audit_moving SELECT * FROM moved;

    EXECUTE format('CREATE TABLE public.%I PARTITION OF public.audit_events FOR VALUES FROM (%L) TO (%L)',
                   part, lower_ts, upper_ts);

    FOR holder IN
        SELECT DISTINCT a.grantee
        FROM pg_class c, aclexplode(c.relacl) a
        WHERE c.oid = ('public.' || part)::regclass AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('REVOKE UPDATE, DELETE, TRUNCATE ON public.%I FROM %s', part,
                       CASE WHEN holder = 0 THEN 'PUBLIC' ELSE holder::regrole::text END);
    END LOOP;

    INSERT INTO public.audit_events SELECT * FROM audit_moving;
    TRUNCATE audit_moving;
    RETURN part;
END
$$;

-- audit_drop_month removes one calendar month of audit events, and is the only
-- thing that can (R-348).
--
-- It refuses unless:
--   * the month ended at least three months ago — the floor, held here rather
--     than in policy so that nothing the running server can write lowers it;
--   * the month holds exactly expected_rows events; and
--   * an archive of that month is recorded with that row count, that digest,
--     and the same first and last event id.
--
-- The rows are counted under a lock that stops writes to the log, so nothing
-- arrives between the count and the drop. The removal is itself an audit
-- event, written in the same transaction: the log says what left it.
CREATE FUNCTION audit_drop_month(archive_month date, expected_rows bigint, archive_sha256 text) RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    first_day date := date_trunc('month', archive_month)::date;
    lower_ts  timestamptz := first_day::timestamp AT TIME ZONE 'UTC';
    upper_ts  timestamptz := (first_day + interval '1 month')::timestamp AT TIME ZONE 'UTC';
    part      text := 'audit_events_' || to_char(first_day, 'YYYY_MM');
    held      bigint;
    low_id    bigint;
    high_id   bigint;
    archived  text;
BEGIN
    IF archive_month <> first_day THEN
        RAISE EXCEPTION 'audit_drop_month takes the first day of a month, not %', archive_month;
    END IF;
    IF (first_day + interval '1 month') > (now() AT TIME ZONE 'UTC') - interval '3 months' THEN
        RAISE EXCEPTION 'audit events from % are less than three months old and cannot be removed',
            to_char(first_day, 'YYYY-MM') USING ERRCODE = 'insufficient_privilege';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtext('pando.audit_partitions'));
    LOCK TABLE public.audit_events IN SHARE MODE;

    SELECT count(*), min(id), max(id) INTO held, low_id, high_id
    FROM public.audit_events
    WHERE occurred_at >= lower_ts AND occurred_at < upper_ts;

    IF held <> expected_rows THEN
        RAISE EXCEPTION 'audit events from % number %, not the % archived', to_char(first_day, 'YYYY-MM'),
            held, expected_rows;
    END IF;

    IF held > 0 THEN
        SELECT id INTO archived
        FROM public.audit_archives a
        WHERE a.month = first_day AND a.row_count = held AND a.sha256 = archive_sha256
          AND a.first_id = low_id AND a.last_id = high_id
        ORDER BY a.created_at DESC
        LIMIT 1;
        IF archived IS NULL THEN
            RAISE EXCEPTION 'audit events from % have no archive holding all % of them',
                to_char(first_day, 'YYYY-MM'), held USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;

    IF to_regclass('public.' || part) IS NOT NULL THEN
        EXECUTE format('DROP TABLE public.%I', part);
    END IF;
    DELETE FROM public.audit_events_default WHERE occurred_at >= lower_ts AND occurred_at < upper_ts;

    IF held > 0 THEN
        INSERT INTO public.audit_events (principal_kind, principal_id, action, target_kind, target_id, detail)
        VALUES ('system', 'system', 'audit.archive', 'audit_archive', archived,
                jsonb_build_object('month', to_char(first_day, 'YYYY-MM'), 'rows', held, 'sha256', archive_sha256,
                                   'first_id', low_id, 'last_id', high_id));
    END IF;
    RETURN held;
END
$$;

-- Nobody may call either until state.applyGrants says who may.
REVOKE ALL ON FUNCTION audit_ensure_partition(date) FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_drop_month(date, bigint, text) FROM PUBLIC;

-- A partition for every month the log already holds, and for this month and
-- the next two, then the rows.
SELECT audit_ensure_partition(m::date)
FROM (
    SELECT DISTINCT date_trunc('month', occurred_at AT TIME ZONE 'UTC') AS m FROM audit_events_unpartitioned
    UNION
    SELECT date_trunc('month', now() AT TIME ZONE 'UTC') + n * interval '1 month' FROM generate_series(0, 2) AS n
) AS months;

INSERT INTO audit_events (id, occurred_at, principal_kind, principal_id, on_behalf_of, action,
                          app_id, target_kind, target_id, request_id, detail)
SELECT id, occurred_at, principal_kind, principal_id, on_behalf_of, action,
       app_id, target_kind, target_id, request_id, detail
FROM audit_events_unpartitioned;

DROP TABLE audit_events_unpartitioned;
ALTER SEQUENCE audit_events_id_seq OWNED BY audit_events.id;
