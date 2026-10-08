DROP INDEX IF EXISTS audit_events_action_prefix_idx;
DROP INDEX IF EXISTS audit_events_target_id_idx;
DROP INDEX IF EXISTS audit_events_on_behalf_of_idx;
DROP INDEX IF EXISTS audit_events_principal_id_idx;
DROP INDEX IF EXISTS audit_events_app_id_idx;

CREATE INDEX audit_events_app_idx ON audit_events (app_id, occurred_at DESC);
CREATE INDEX audit_events_principal_idx ON audit_events (principal_id, occurred_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, occurred_at DESC);
