ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;
UPDATE roles SET verbs = array_remove(verbs, 'install.audit.export') WHERE id = 'role_administrator';
ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

-- A custom role naming the verb would be refused by the code once it is gone.
UPDATE roles SET verbs = array_remove(verbs, 'install.audit.export') WHERE NOT builtin;

DROP TRIGGER deployments_outcome_audit ON deployments;
DROP FUNCTION audit_deploy_outcome();

DROP TABLE audit_sink_state;

DELETE FROM adapter_credentials WHERE adapter_id IN (SELECT id FROM adapter_configs WHERE category = 'audit_sink');
DELETE FROM adapter_configs WHERE category = 'audit_sink';
ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_audit_sink_url_no_credential;
ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_category_check;
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_category_check
    CHECK (category IN ('runtime', 'routing', 'builder', 'secrets',
                        'services', 'identity', 'notify', 'backup', 'scanner', 'ai',
                        'source', 'image_registry'));

DROP INDEX audit_events_stream_idx;
ALTER TABLE audit_events DROP COLUMN actor_email;
ALTER TABLE audit_events DROP COLUMN actor_name;
ALTER TABLE audit_events DROP COLUMN user_agent;
ALTER TABLE audit_events DROP COLUMN peer_ip;
ALTER TABLE audit_events DROP COLUMN source_ip;
ALTER TABLE audit_events DROP COLUMN outcome;
ALTER TABLE audit_events DROP COLUMN schema_version;
ALTER TABLE audit_events DROP COLUMN txid;
