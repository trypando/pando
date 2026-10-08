-- What an app's pinned revision reserves, and the score of its newest scan,
-- kept on the app so that reading them for every app is a scan of one table
-- rather than a JSON walk or a lookup per app (issue #72,
-- docs/design/notes-background-costs-issue-72.md).
--
-- Both are derived, never written by hand: triggers set them from the rows
-- they are derived from, so no code path that moves pinned_spec_id or records
-- a scan can leave them behind. They are a cache with one writer each.
--
-- Allocation (R-242). The planner's capacity check sums what every running app
-- on a runtime reserves. It used to join every such app to its pinned revision
-- and read four fields out of the JSON body, on every deploy plan and every
-- view of the capacity screen. A revision never changes (R-152), so what an
-- app reserves changes only when its pin does: copied here at that moment, the
-- sum is an index-only scan and stays exact. Which apps count — running,
-- degraded, deploying — is still decided when the sum is taken, from state.
--
-- Security (R-315, R-316). The security pass places every live app against the
-- installation's threshold, and found each app's score with a lookup into
-- app_scans per app. Kept here, the pass asks only for the apps that are below
-- the threshold or already marked, in one set-based query.

ALTER TABLE apps
    ADD COLUMN alloc_runtime_ref         text,
    ADD COLUMN alloc_cpu_millis          bigint,
    ADD COLUMN alloc_memory_bytes        bigint,
    ADD COLUMN alloc_disk_bytes          bigint,
    ADD COLUMN alloc_log_bytes           bigint,
    ADD COLUMN pinned_scan_score         int,
    ADD COLUMN pinned_scan_score_fixable int;

-- A JSON number as a bigint, and NULL for anything else. The old query cast
-- the text and failed the whole plan on a value that was not a number; a
-- trigger that did that would fail the pin instead, so it reads nothing.
CREATE FUNCTION pando_json_bigint(v jsonb) RETURNS bigint
    LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN jsonb_typeof(v) = 'number' THEN round(v::text::numeric)::bigint END
$$;

-- The pinned revision's newest scan, as Scans.Latest finds it: a scan of that
-- revision before a scan of no revision, newest first.
CREATE FUNCTION pando_pinned_scan(p_app text, p_spec text,
                                  OUT score int, OUT score_fixable int)
    LANGUAGE sql STABLE AS $$
    SELECT sc.score, sc.score_fixable
    FROM app_scans sc
    WHERE sc.app_id = p_app AND (sc.spec_id = p_spec OR sc.spec_id IS NULL)
    ORDER BY (sc.spec_id IS NOT NULL) DESC, sc.ran_at DESC, sc.id DESC
    LIMIT 1
$$;

CREATE FUNCTION apps_pinned_derived() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    body jsonb;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.pinned_spec_id IS NOT DISTINCT FROM OLD.pinned_spec_id THEN
        RETURN NEW;
    END IF;
    IF NEW.pinned_spec_id IS NOT NULL THEN
        SELECT r.body INTO body FROM spec_revisions r WHERE r.id = NEW.pinned_spec_id;
    END IF;
    NEW.alloc_runtime_ref  := body->'runtime'->>'adapter_ref';
    NEW.alloc_cpu_millis   := pando_json_bigint(body->'resources'->'cpu_millis');
    NEW.alloc_memory_bytes := pando_json_bigint(body->'resources'->'memory_bytes');
    NEW.alloc_disk_bytes   := pando_json_bigint(body->'resources'->'disk_bytes');
    NEW.alloc_log_bytes    := pando_json_bigint(body->'retention'->'log_bytes');
    SELECT s.score, s.score_fixable INTO NEW.pinned_scan_score, NEW.pinned_scan_score_fixable
    FROM pando_pinned_scan(NEW.id, NEW.pinned_spec_id) s;
    RETURN NEW;
END $$;

CREATE TRIGGER apps_pinned_derived
    BEFORE INSERT OR UPDATE OF pinned_spec_id ON apps
    FOR EACH ROW EXECUTE FUNCTION apps_pinned_derived();

-- A scan recorded or removed changes which scan is newest for its app.
CREATE FUNCTION app_scans_pinned_score() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    app text := CASE WHEN TG_OP = 'DELETE' THEN OLD.app_id ELSE NEW.app_id END;
BEGIN
    UPDATE apps a SET (pinned_scan_score, pinned_scan_score_fixable) =
        (SELECT s.score, s.score_fixable FROM pando_pinned_scan(a.id, a.pinned_spec_id) s)
    WHERE a.id = app;
    RETURN NULL;
END $$;

CREATE TRIGGER app_scans_pinned_score
    AFTER INSERT OR DELETE ON app_scans
    FOR EACH ROW EXECUTE FUNCTION app_scans_pinned_score();

-- Every app as it stands now.
UPDATE apps a SET
    alloc_runtime_ref  = r.body->'runtime'->>'adapter_ref',
    alloc_cpu_millis   = pando_json_bigint(r.body->'resources'->'cpu_millis'),
    alloc_memory_bytes = pando_json_bigint(r.body->'resources'->'memory_bytes'),
    alloc_disk_bytes   = pando_json_bigint(r.body->'resources'->'disk_bytes'),
    alloc_log_bytes    = pando_json_bigint(r.body->'retention'->'log_bytes')
FROM spec_revisions r
WHERE r.id = a.pinned_spec_id;

UPDATE apps a SET (pinned_scan_score, pinned_scan_score_fixable) =
    (SELECT s.score, s.score_fixable FROM pando_pinned_scan(a.id, a.pinned_spec_id) s)
WHERE EXISTS (SELECT 1 FROM app_scans sc WHERE sc.app_id = a.id);

-- The capacity sum: every app that holds resources on a runtime, read from the
-- index alone.
CREATE INDEX apps_allocation_idx ON apps (alloc_runtime_ref)
    INCLUDE (id, alloc_cpu_millis, alloc_memory_bytes, alloc_disk_bytes, alloc_log_bytes)
    WHERE deleted_at IS NULL AND state IN ('running', 'degraded', 'deploying');
