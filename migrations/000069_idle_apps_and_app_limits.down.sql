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
                       OR OLD.address_port IS DISTINCT FROM NEW.address_port)
    EXECUTE FUNCTION proxy_cache_changed();

ALTER TABLE groups DROP COLUMN max_apps;
ALTER TABLE users  DROP COLUMN max_apps;

ALTER TABLE apps
    DROP COLUMN idle_delete_noticed_at,
    DROP COLUMN idle_stop_noticed_at,
    DROP COLUMN stopped_for_idle,
    DROP COLUMN idle_delete_days,
    DROP COLUMN idle_stop_days;

DROP TRIGGER app_activity_started ON apps;
DROP FUNCTION app_activity_started();
DROP TRIGGER app_activity_deployed ON deployments;
DROP FUNCTION app_activity_deployed();
DROP TABLE app_activity;
