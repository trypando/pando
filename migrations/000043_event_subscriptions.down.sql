ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles
SET verbs = array_remove(verbs, 'install.events.manage')
WHERE 'install.events.manage' = ANY (verbs);

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

DROP TABLE notification_preferences;
DROP TABLE delivery_attempts;
DROP TABLE event_deliveries;
DROP TABLE subscription_secrets;
DROP TABLE subscriptions;

DROP TRIGGER backup_attempts_failed_event ON backup_attempts;
DROP TRIGGER backups_created_event ON backups;
DROP TRIGGER deployments_outcome_event ON deployments;
DROP TRIGGER apps_state_event ON apps;
DROP FUNCTION events_backup_attempt();
DROP FUNCTION events_backup_created();
DROP FUNCTION events_deploy_outcome();
DROP FUNCTION events_app_state();

DROP TABLE events;
DROP FUNCTION pando_ulid();
