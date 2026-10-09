-- Idle apps and app limits (issue #131).

-- When an app last had activity (R-394): a request the proxy let through to
-- it, a deploy of it, or starting it. A table of its own rather than a column
-- on apps, so the proxy's once-a-minute writes do not churn the apps row the
-- reconciler leases and the proxy cache watches.
CREATE TABLE app_activity (
    app_id           text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    last_activity_at timestamptz NOT NULL
);

-- A deploy is activity. A trigger rather than a call in each place a
-- deployment is written, so no later path that writes one can forget.
CREATE FUNCTION app_activity_deployed() RETURNS trigger AS $$
BEGIN
    INSERT INTO app_activity (app_id, last_activity_at)
    VALUES (NEW.app_id, now())
    ON CONFLICT (app_id) DO UPDATE
        SET last_activity_at = GREATEST(app_activity.last_activity_at, EXCLUDED.last_activity_at);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER app_activity_deployed AFTER INSERT ON deployments
    FOR EACH ROW EXECUTE FUNCTION app_activity_deployed();

-- An app's own idle settings (R-397). NULL is the install's; 0 is off.
-- stopped_for_idle records that Pando stopped it, not its owner, the way
-- stopped_for_security does. The two notice columns are when the owner was
-- told, so a stop or delete waits out its notice (R-395).
ALTER TABLE apps
    ADD COLUMN idle_stop_days         integer CHECK (idle_stop_days >= 0),
    ADD COLUMN idle_delete_days       integer CHECK (idle_delete_days >= 0),
    ADD COLUMN stopped_for_idle       boolean NOT NULL DEFAULT false,
    ADD COLUMN idle_stop_noticed_at   timestamptz,
    ADD COLUMN idle_delete_noticed_at timestamptz;

-- Starting an app is activity, and ends a stop for inactivity and any notice
-- given (R-394, R-396): whatever starts it — a person, or the security pass
-- recovering — the reason it was stopped no longer describes it. BEFORE, so
-- the same row write clears them.
CREATE FUNCTION app_activity_started() RETURNS trigger AS $$
BEGIN
    INSERT INTO app_activity (app_id, last_activity_at)
    VALUES (NEW.id, now())
    ON CONFLICT (app_id) DO UPDATE
        SET last_activity_at = GREATEST(app_activity.last_activity_at, EXCLUDED.last_activity_at);
    NEW.stopped_for_idle := false;
    NEW.idle_stop_noticed_at := NULL;
    NEW.idle_delete_noticed_at := NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER app_activity_started BEFORE UPDATE OF desired_state ON apps
    FOR EACH ROW WHEN (NEW.desired_state = 'running' AND OLD.desired_state IS DISTINCT FROM 'running')
    EXECUTE FUNCTION app_activity_started();

-- How many apps a person may own (R-244). NULL is not set here; 0 is
-- unlimited.
ALTER TABLE users  ADD COLUMN max_apps integer CHECK (max_apps >= 0);
ALTER TABLE groups ADD COLUMN max_apps integer CHECK (max_apps >= 0);

-- The proxy's lookup reads stopped_for_idle, to tell a visitor why the app is
-- not running, so changing it empties the proxy cache like the other columns
-- the proxy reads (migration 68).
DROP TRIGGER proxy_cache_apps_update ON apps;
CREATE TRIGGER proxy_cache_apps_update AFTER UPDATE ON apps
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state
                       OR OLD.desired_state IS DISTINCT FROM NEW.desired_state
                       OR OLD.pinned_spec_id IS DISTINCT FROM NEW.pinned_spec_id
                       OR OLD.owner_user_id IS DISTINCT FROM NEW.owner_user_id
                       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
                       OR OLD.slug IS DISTINCT FROM NEW.slug
                       OR OLD.name IS DISTINCT FROM NEW.name
                       OR OLD.address_hostname IS DISTINCT FROM NEW.address_hostname
                       OR OLD.address_path IS DISTINCT FROM NEW.address_path
                       OR OLD.address_port IS DISTINCT FROM NEW.address_port
                       OR OLD.stopped_for_idle IS DISTINCT FROM NEW.stopped_for_idle)
    EXECUTE FUNCTION proxy_cache_changed();
