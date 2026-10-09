-- Audit streaming and export (issue #129, design 12).

-- The transaction that wrote each event (design 12 §2). The sequence behind id
-- is taken at insert, not at commit, so two writers can commit 41 after 42; a
-- reader that only takes events below the oldest running transaction, in
-- (txid, id) order, can never have one appear behind its cursor (R-381).
--
-- Rows already written get 0 and sort by id: every one of them committed long
-- ago. Added with a constant default and then given the real one, so existing
-- rows are not rewritten — their values are what the default says they are.
ALTER TABLE audit_events ADD COLUMN txid xid8 NOT NULL DEFAULT '0';
ALTER TABLE audit_events ALTER COLUMN txid SET DEFAULT pg_current_xact_id();

-- What every event carries (R-379, design 12 §3). Nullable: rows written before
-- this have none of it, and say so with schema_version NULL (read as 1).
ALTER TABLE audit_events ADD COLUMN schema_version smallint;
ALTER TABLE audit_events ALTER COLUMN schema_version SET DEFAULT 2;
ALTER TABLE audit_events ADD COLUMN outcome text
    CONSTRAINT audit_events_outcome_check CHECK (outcome IN ('success', 'denied', 'failed'));
ALTER TABLE audit_events ADD COLUMN source_ip text;
ALTER TABLE audit_events ADD COLUMN peer_ip text;
ALTER TABLE audit_events ADD COLUMN user_agent text;
ALTER TABLE audit_events ADD COLUMN actor_name text;
ALTER TABLE audit_events ADD COLUMN actor_email text;

CREATE INDEX audit_events_stream_idx ON audit_events (txid, id);

-- The thirteenth adapter category (R-252, R-382).
ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_category_check;
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_category_check
    CHECK (category IN ('runtime', 'routing', 'builder', 'secrets',
                        'services', 'identity', 'notify', 'backup', 'scanner', 'ai',
                        'source', 'image_registry', 'audit_sink'));

-- https://user:pass@collector is a password stored in the clear (R-190), as
-- 000066 refuses for registries.
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_audit_sink_url_no_credential
    CHECK (category <> 'audit_sink' OR coalesce(config->>'url', '') !~ '://[^/]*@');

-- Where each audit sink has got to (design 12 §5.2). One row per adapter,
-- written by the delivery engine and read by the archiver (R-386). Not keyed
-- to adapter_configs: a sink declared in the config file has no row there.
CREATE TABLE audit_sink_state (
    adapter_id      text PRIMARY KEY,

    -- The last event delivered: (txid, id) as design 12 §2 orders them.
    -- NULL until the first delivery, which starts at the oldest event in the
    -- live log, or at start_txid/start_id for a sink created with start: now.
    cursor_txid     xid8,
    cursor_id       bigint,
    -- When the cursor last moved, and the occurred_at of the event it points
    -- at: the archiver holds a month while that time is inside or before it.
    delivered_at    timestamptz,
    cursor_at       timestamptz,
    delivered_count bigint NOT NULL DEFAULT 0,

    last_error      text,
    last_error_at   timestamptz,
    failing_since   timestamptz,
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,

    disabled_at     timestamptz,
    disabled_reason text,

    -- A range a disabled sink missed when the archiver removed it (R-386).
    gap_from        timestamptz,
    gap_to          timestamptz,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CHECK ((cursor_txid IS NULL) = (cursor_id IS NULL))
);

-- deploy.finish (R-389): a deploy's outcome is an audit event, written by the
-- system in the transaction that records it. The deploy runner finishes a
-- deployment by writing its status from six places; the row is the one place
-- all of them reach, as the events_deploy_outcome trigger already relies on.
-- An INSERT, which is all the application role may do to audit_events (R-027).
CREATE FUNCTION audit_deploy_outcome() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO audit_events (principal_kind, principal_id, action, app_id, target_kind, target_id, outcome, detail)
    VALUES ('system', 'system', 'deploy.finish', NEW.app_id, 'deployment', NEW.id,
            CASE WHEN NEW.status = 'succeeded' THEN 'success' ELSE 'failed' END,
            jsonb_strip_nulls(jsonb_build_object(
                'deployment_id', NEW.id,
                'spec_revision', NEW.spec_id,
                'trigger', NEW.trigger,
                'status', NEW.status,
                'error_code', NEW.error_code,
                'message', NEW.error_detail->>'message')));
    RETURN NULL;
END $$;

CREATE TRIGGER deployments_outcome_audit
    AFTER UPDATE OF status ON deployments
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                       AND NEW.status IN ('succeeded', 'failed'))
    EXECUTE FUNCTION audit_deploy_outcome();

-- install.audit.export: sending the audit log off the installation (R-385).
-- The Administrator's, and in no other built-in role. Built-in roles change
-- only by migration (R-081).
ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles
SET verbs = verbs || 'install.audit.export'::text
WHERE id = 'role_administrator';

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;
