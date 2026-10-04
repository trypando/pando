-- Back to one unpartitioned table. Events in months already archived and
-- removed are not brought back: they are in their archives, which stay where
-- they were written, and the record of those archives goes with this table.
ALTER SEQUENCE audit_events_id_seq OWNED BY NONE;
ALTER TABLE audit_events RENAME TO audit_events_partitioned;
ALTER INDEX audit_events_pkey RENAME TO audit_events_partitioned_pkey;
ALTER INDEX audit_events_app_idx RENAME TO audit_events_partitioned_app_idx;
ALTER INDEX audit_events_principal_idx RENAME TO audit_events_partitioned_principal_idx;
ALTER INDEX audit_events_action_idx RENAME TO audit_events_partitioned_action_idx;

CREATE TABLE audit_events (
    id             bigint PRIMARY KEY DEFAULT nextval('audit_events_id_seq'),
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'token', 'system', 'anonymous')),
    principal_id   text,
    on_behalf_of   text,
    action         text NOT NULL,
    app_id         text,
    target_kind    text,
    target_id      text,
    request_id     text,
    detail         jsonb NOT NULL DEFAULT '{}'
);
ALTER TABLE audit_events ADD CONSTRAINT audit_events_principal_id_present
    CHECK (principal_kind = 'anonymous' OR principal_id IS NOT NULL);
CREATE INDEX audit_events_app_idx ON audit_events (app_id, occurred_at DESC);
CREATE INDEX audit_events_principal_idx ON audit_events (principal_id, occurred_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, occurred_at DESC);

INSERT INTO audit_events (id, occurred_at, principal_kind, principal_id, on_behalf_of, action,
                          app_id, target_kind, target_id, request_id, detail)
SELECT id, occurred_at, principal_kind, principal_id, on_behalf_of, action,
       app_id, target_kind, target_id, request_id, detail
FROM audit_events_partitioned;

DROP TABLE audit_events_partitioned;
ALTER SEQUENCE audit_events_id_seq OWNED BY audit_events.id;

DROP FUNCTION audit_drop_month(date, bigint, text);
DROP FUNCTION audit_ensure_partition(date);
DROP TABLE audit_archives;
