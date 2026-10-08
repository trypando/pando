-- Indexes for the audit log's filters, in the order the log is read (issue #72).
--
-- GET /audit reads newest first by id, a page at a time before an id, and
-- narrows by app, actor, the person a token acted for, target, an action
-- prefix, or any of actor and target at once ("involving"). The indexes were
-- on (column, occurred_at), which no list orders by, and on_behalf_of and
-- target_id had none: a search for everything involving one account read every
-- event in the log.
--
-- Made on the partitioned parent, so every month's partition has them and a
-- partition made later is given them as it is attached. An index changes no
-- privilege: the application role still holds no UPDATE or DELETE on any of
-- it (R-027).
DROP INDEX IF EXISTS audit_events_app_idx;
DROP INDEX IF EXISTS audit_events_principal_idx;
DROP INDEX IF EXISTS audit_events_action_idx;

CREATE INDEX audit_events_app_id_idx ON audit_events (app_id, id DESC)
    WHERE app_id IS NOT NULL;
CREATE INDEX audit_events_principal_id_idx ON audit_events (principal_id, id DESC)
    WHERE principal_id IS NOT NULL;
CREATE INDEX audit_events_on_behalf_of_idx ON audit_events (on_behalf_of, id DESC)
    WHERE on_behalf_of IS NOT NULL;
CREATE INDEX audit_events_target_id_idx ON audit_events (target_id, id DESC)
    WHERE target_id IS NOT NULL;
-- text_pattern_ops, so `action LIKE 'app.%'` is a range scan whatever the
-- database's collation.
CREATE INDEX audit_events_action_prefix_idx ON audit_events (action text_pattern_ops, id DESC);
