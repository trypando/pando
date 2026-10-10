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

CREATE OR REPLACE FUNCTION app_activity_started() RETURNS trigger AS $$
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

ALTER TABLE apps
    DROP COLUMN disk_over_at,
    DROP COLUMN disk_warned_at,
    DROP COLUMN stopped_for_disk;
