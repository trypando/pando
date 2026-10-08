-- Which apps track a branch (R-141), as a column the auto-deploy poll finds
-- through an index (issue #72).
--
-- The poll runs every minute and read the pinned revision of every live app to
-- look inside its JSON. Auto-deploy is off by default, so the answer is nearly
-- always "none", and it cost a read of every app.
--
-- Auto-deploy stays a property of the spec someone reviewed and pinned: the
-- column is derived from the pinned revision by a trigger whenever the pin
-- moves, and the trigger overrides any other write to it. A revision is
-- append-only (R-152), so the value cannot go stale under the pin.
ALTER TABLE apps ADD COLUMN auto_deploy boolean NOT NULL DEFAULT false;

CREATE FUNCTION apps_auto_deploy_from_pin() RETURNS trigger AS $$
BEGIN
    NEW.auto_deploy := coalesce(
        (SELECT (r.body->'deploy'->'auto_deploy'->'enabled') = 'true'::jsonb
         FROM spec_revisions r WHERE r.id = NEW.pinned_spec_id),
        false);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER apps_auto_deploy
    BEFORE INSERT OR UPDATE OF pinned_spec_id, auto_deploy ON apps
    FOR EACH ROW EXECUTE FUNCTION apps_auto_deploy_from_pin();

-- Existing apps. Naming the column fires the trigger, which derives it.
UPDATE apps SET auto_deploy = false WHERE pinned_spec_id IS NOT NULL;

CREATE INDEX apps_auto_deploy_idx ON apps (id) WHERE auto_deploy AND deleted_at IS NULL;
