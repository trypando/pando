-- Auto-deploy: why a revision exists, and what the last check found (issue #40).

-- A revision auto-deploy cut because its branch or release moved (R-141), so
-- revision history says why it exists rather than calling it detected.
ALTER TABLE spec_revisions DROP CONSTRAINT spec_revisions_origin_check;
ALTER TABLE spec_revisions ADD CONSTRAINT spec_revisions_origin_check
    CHECK (origin IN ('detected', 'edited', 'redetected', 'imported', 'manual', 'auto_deploy'));

-- The last time auto-deploy looked at an app, one row per app.
--
-- found_* is what the watched branch or release pointed at then; the console
-- shows it. attempted_commit is the last commit auto-deploy tried to deploy,
-- whatever came of it: a commit is tried once (O-56), so a broken push is not
-- retried every five minutes. A new commit is tried; a person can always
-- deploy by hand.
CREATE TABLE auto_deploy_checks (
    app_id           text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    checked_at       timestamptz NOT NULL,
    found_ref        text,
    found_commit     text,
    attempted_commit text,
    attempted_at     timestamptz,
    deployment_id    text REFERENCES deployments(id) ON DELETE SET NULL,
    -- Why the last check or attempt went nowhere, in words a person can act
    -- on. Never a secret: it is an error envelope's message.
    error            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
