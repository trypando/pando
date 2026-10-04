-- Upgrading Pando in place (R-356, issue #53).
--
-- install.upgrade: the Administrator's, and in no other built-in role. Built-in
-- roles change only by migration (R-081).
ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles
SET verbs = verbs || 'install.upgrade'::text
WHERE id = 'role_administrator';

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

-- Agents do not upgrade Pando by default, like the other highest-consequence
-- verbs in policy.Default(). A stored policy row is read instead of Default(),
-- so the verb is added to it here. Nobody can have chosen to let agents hold a
-- verb that did not exist, so this overrides no decision.
UPDATE host_policy
SET body = jsonb_set(body, '{agent_disabled_verbs}',
        coalesce(body->'agent_disabled_verbs', '[]'::jsonb) || '["install.upgrade"]'::jsonb)
WHERE NOT coalesce(body->'agent_disabled_verbs', '[]'::jsonb) ? 'install.upgrade';
