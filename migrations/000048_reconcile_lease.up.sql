-- The reconciler's lease on an app (issue #72, PR 3).
--
-- Replaces two things that stopped working past a couple of hundred apps:
--
-- 1. Which apps a pass looks at. The loop read the 200 due apps with the
--    oldest updated_at, and a healthy pass writes nothing — so the same 200
--    were visited forever and an install's 201st app was never observed, and
--    never repaired when its container was killed. A pass now claims apps in
--    order of when they were last visited, so every app gets its turn.
--
-- 2. How two passes are kept off one app. A session advisory lock held a
--    pooled connection for the whole reconciliation, Apply included, while the
--    reconciliation took more connections of its own: eight in flight per
--    replica could use up the pool. A lease is a row update and holds nothing.
--
-- While a pass holds the app, reconcile_lease_until is in the future and is
-- extended while the work runs. When it lets go, it is set back to when that
-- visit started, which is what orders the next pass: never-visited first, then
-- least recently visited. A replica that dies holding a lease loses it when the
-- time passes; no connection or clock of its own is involved, only the
-- database's.
ALTER TABLE apps ADD COLUMN reconcile_lease_until timestamptz;

-- Which pass holds the lease, so extending and releasing touch only the
-- holder's own apps. NULL when nobody holds it.
ALTER TABLE apps ADD COLUMN reconcile_lease_holder text;

-- The claim's scan. The predicate is the reconciler's own WHERE clause —
-- failed is absent from it as it is from the loop (R-151).
CREATE INDEX apps_reconcile_lease_idx ON apps (reconcile_lease_until NULLS FIRST)
    WHERE deleted_at IS NULL
      AND pinned_spec_id IS NOT NULL
      AND state IN ('running', 'degraded', 'stopped');

-- The pass that holds a lease extends it by holder.
CREATE INDEX apps_reconcile_holder_idx ON apps (reconcile_lease_holder)
    WHERE reconcile_lease_holder IS NOT NULL;
