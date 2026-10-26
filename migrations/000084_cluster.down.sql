ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles
SET verbs = array_remove(verbs, 'install.cluster.manage'::text)
WHERE id = 'role_administrator';

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

DROP TABLE IF EXISTS cluster_hosts;
DROP TABLE IF EXISTS join_tokens;
