ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;
UPDATE roles SET verbs = array_remove(verbs, 'install.upgrade'::text);
ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

UPDATE host_policy
SET body = jsonb_set(body, '{agent_disabled_verbs}',
        (SELECT coalesce(jsonb_agg(v), '[]'::jsonb)
         FROM jsonb_array_elements_text(body->'agent_disabled_verbs') AS v
         WHERE v <> 'install.upgrade'))
WHERE body->'agent_disabled_verbs' ? 'install.upgrade';

UPDATE host_policy SET body = body - 'upgrade_in_place' - 'auto_upgrade_patches' - 'maintenance_window';
