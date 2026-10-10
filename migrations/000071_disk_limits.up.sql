-- Disk limits (R-403, issue #130).
--
-- Every app has a disk limit (R-240), and until now it was only counted
-- against the host's capacity (R-242): nothing stopped an app writing past it.
-- The disk pass measures each running app and, when it stays over its limit,
-- stops it. stopped_for_disk records that Pando stopped it, as
-- stopped_for_idle does; disk_warned_at is when its owner was told it was
-- near the limit; disk_over_at is the first reading over it, so one reading
-- over is a warning and a second is a stop.
ALTER TABLE apps
    ADD COLUMN stopped_for_disk boolean NOT NULL DEFAULT false,
    ADD COLUMN disk_warned_at   timestamptz,
    ADD COLUMN disk_over_at     timestamptz;

-- Starting an app clears what Pando recorded about why it stopped it, in the
-- same write (migration 69), now including the disk marks.
CREATE OR REPLACE FUNCTION app_activity_started() RETURNS trigger AS $$
BEGIN
    INSERT INTO app_activity (app_id, last_activity_at)
    VALUES (NEW.id, now())
    ON CONFLICT (app_id) DO UPDATE
        SET last_activity_at = GREATEST(app_activity.last_activity_at, EXCLUDED.last_activity_at);
    NEW.stopped_for_idle := false;
    NEW.idle_stop_noticed_at := NULL;
    NEW.idle_delete_noticed_at := NULL;
    NEW.stopped_for_disk := false;
    NEW.disk_warned_at := NULL;
    NEW.disk_over_at := NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The proxy reads stopped_for_disk to tell a visitor why the app is down.
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
                       OR OLD.stopped_for_idle IS DISTINCT FROM NEW.stopped_for_idle
                       OR OLD.stopped_for_disk IS DISTINCT FROM NEW.stopped_for_disk)
    EXECUTE FUNCTION proxy_cache_changed();
