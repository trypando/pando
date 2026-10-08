-- What a deploy reserves on its runtime from the moment it passes the
-- plan-time capacity check until it ends (R-242, issue #72,
-- docs/design/notes-background-costs-issue-72.md).
--
-- An app holds resources in the capacity sum once its revision is pinned, and
-- a deploy pins only at its end. Two first deploys planned at the same moment
-- could each see the room the other was about to take, and both pass. A deploy
-- in flight — pending (queued included), building or applying — now counts
-- what its revision asks for, and the check and the deploy's creation are
-- serialized per runtime (state.Allocations.Hold), so the second plan sees
-- the first deploy's reservation.
--
-- Copied from the revision by trigger, as migration 60 does for the pin: the
-- revision never changes (R-152), so the reservation is fixed when the row is
-- written. It ends with the deploy: a status other than the three in flight
-- releases it, whatever set the status — the runner, a cancellation, or
-- RecoverInFlight failing a deploy a stopped replica left.
ALTER TABLE deployments
    ADD COLUMN reserve_runtime_ref  text,
    ADD COLUMN reserve_cpu_millis   bigint,
    ADD COLUMN reserve_memory_bytes bigint,
    ADD COLUMN reserve_disk_bytes   bigint,
    ADD COLUMN reserve_log_bytes    bigint;

CREATE FUNCTION deployments_reservation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    body jsonb;
BEGIN
    SELECT r.body INTO body FROM spec_revisions r WHERE r.id = NEW.spec_id;
    NEW.reserve_runtime_ref  := body->'runtime'->>'adapter_ref';
    NEW.reserve_cpu_millis   := pando_json_bigint(body->'resources'->'cpu_millis');
    NEW.reserve_memory_bytes := pando_json_bigint(body->'resources'->'memory_bytes');
    NEW.reserve_disk_bytes   := pando_json_bigint(body->'resources'->'disk_bytes');
    NEW.reserve_log_bytes    := pando_json_bigint(body->'retention'->'log_bytes');
    RETURN NEW;
END $$;

CREATE TRIGGER deployments_reservation
    BEFORE INSERT OR UPDATE OF spec_id ON deployments
    FOR EACH ROW EXECUTE FUNCTION deployments_reservation();

-- Deploys in flight now. Finished ones reserve nothing and are left alone.
UPDATE deployments d SET
    reserve_runtime_ref  = r.body->'runtime'->>'adapter_ref',
    reserve_cpu_millis   = pando_json_bigint(r.body->'resources'->'cpu_millis'),
    reserve_memory_bytes = pando_json_bigint(r.body->'resources'->'memory_bytes'),
    reserve_disk_bytes   = pando_json_bigint(r.body->'resources'->'disk_bytes'),
    reserve_log_bytes    = pando_json_bigint(r.body->'retention'->'log_bytes')
FROM spec_revisions r
WHERE r.id = d.spec_id AND d.status IN ('pending', 'building', 'applying');

CREATE INDEX deployments_reservation_idx ON deployments (reserve_runtime_ref)
    INCLUDE (app_id, reserve_cpu_millis, reserve_memory_bytes, reserve_disk_bytes, reserve_log_bytes)
    WHERE status IN ('pending', 'building', 'applying');
