DROP INDEX apps_reconcile_holder_idx;
DROP INDEX apps_reconcile_lease_idx;
ALTER TABLE apps DROP COLUMN reconcile_lease_holder;
ALTER TABLE apps DROP COLUMN reconcile_lease_until;
