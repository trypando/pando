DROP TABLE deployment_approvals;

DROP INDEX deployments_awaiting_idx;
ALTER TABLE deployments DROP COLUMN approval_reasons;
ALTER TABLE deployments DROP COLUMN approval_expires_at;
ALTER TABLE deployments DROP COLUMN approvals_required;

-- A request still waiting has nowhere to go in the older schema; it never ran.
UPDATE deployments SET status = 'superseded'
WHERE status IN ('awaiting_approval', 'rejected', 'expired');
ALTER TABLE deployments DROP CONSTRAINT deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN ('pending', 'building', 'applying', 'succeeded', 'failed', 'superseded'));

ALTER TABLE deployments DROP COLUMN egress_rules;

UPDATE host_policy SET body = body - 'deploy_approval_expiry_hours'
    - 'deploy_approval_required' - 'deploy_approval_apps' - 'deploy_approval_count'
WHERE body ?| array['deploy_approval_expiry_hours', 'deploy_approval_required',
                    'deploy_approval_apps', 'deploy_approval_count'];

UPDATE host_policy
SET body = jsonb_set(body, '{disabled_verbs}',
        (SELECT coalesce(jsonb_agg(CASE WHEN v = 'app.egress.loosen' THEN 'app.egress.override' ELSE v END), '[]'::jsonb)
         FROM jsonb_array_elements_text(body->'disabled_verbs') AS v))
WHERE body->'disabled_verbs' ? 'app.egress.loosen';

UPDATE host_policy
SET body = jsonb_set(body, '{agent_disabled_verbs}',
        (SELECT coalesce(jsonb_agg(CASE WHEN v = 'app.egress.loosen' THEN 'app.egress.override' ELSE v END), '[]'::jsonb)
         FROM jsonb_array_elements_text(body->'agent_disabled_verbs') AS v))
WHERE body->'agent_disabled_verbs' ? 'app.egress.loosen';

ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles SET verbs = array_remove(verbs, 'install.deploys.approve'::text)
WHERE 'install.deploys.approve' = ANY (verbs);
UPDATE roles SET verbs = array_remove(verbs, 'app.deploy.approve'::text)
WHERE 'app.deploy.approve' = ANY (verbs);
UPDATE roles SET verbs = array_remove(verbs, 'app.egress.tighten'::text)
WHERE 'app.egress.tighten' = ANY (verbs);
UPDATE roles
SET verbs = array_replace(verbs, 'app.egress.loosen'::text, 'app.egress.override'::text)
WHERE 'app.egress.loosen' = ANY (verbs);

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;
