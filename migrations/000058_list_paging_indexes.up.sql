-- Indexes for lists that are now read a page at a time (issue #72).

-- GET /apps/{id}/grants: who has access to one app, keyset-paged by principal
-- so one principal's grants on both planes arrive on the same page. The
-- anonymous grant has no principal_id; coalesce gives it a key, and it sorts
-- first ('anonymous' < 'group' < 'token' < 'user').
CREATE INDEX IF NOT EXISTS grants_app_list_idx
    ON grants (app_id, principal_kind, (coalesce(principal_id, '')), plane, id)
    WHERE app_id IS NOT NULL;

-- GET /subscriptions: newest first, keyset on (created_at, id), narrowed to an
-- owner (a person or an account token), an app, or neither. Each replaces the
-- single-column index on the same leading column, which still serves the
-- lookups and foreign-key checks that used it.
DROP INDEX IF EXISTS subscriptions_owner_user_idx;
DROP INDEX IF EXISTS subscriptions_owner_token_idx;
DROP INDEX IF EXISTS subscriptions_app_idx;
CREATE INDEX subscriptions_owner_user_list_idx ON subscriptions (owner_user_id, created_at DESC, id DESC)
    WHERE owner_user_id IS NOT NULL;
CREATE INDEX subscriptions_owner_token_list_idx ON subscriptions (owner_token_id, created_at DESC, id DESC)
    WHERE owner_token_id IS NOT NULL;
CREATE INDEX subscriptions_app_list_idx ON subscriptions (app_id, created_at DESC, id DESC);
CREATE INDEX subscriptions_list_idx ON subscriptions (created_at DESC, id DESC);

-- GET /backups: every backup newest first, keyset on (created_at, id). One
-- app's are served by backups_app_idx.
CREATE INDEX IF NOT EXISTS backups_list_idx ON backups (created_at DESC, id DESC);

-- GET /backups: the most recent scheduled attempts, at most a page of them.
CREATE INDEX IF NOT EXISTS backup_attempts_recent_idx ON backup_attempts (attempted_at DESC);
