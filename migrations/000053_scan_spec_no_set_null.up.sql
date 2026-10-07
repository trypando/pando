-- A scan's revision reference must never be rewritten (R-319), so it cannot be
-- ON DELETE SET NULL.
--
-- SET NULL is an UPDATE of app_scans, which the append-only trigger refuses.
-- Once a revision that had been scanned fell outside its app's retention
-- window, retention pruning (R-152) tried to delete it, the cascade tried to
-- null the scan's spec_id, the trigger refused, and every prune from then on
-- failed. 000038 hit the same wall from the other side and gave reused_from no
-- foreign key for exactly this reason.
--
-- NO ACTION instead. Pruning skips a revision a scan describes (state.Apps.
-- PruneSpecRevisions), and this constraint is what makes that rule hold if a
-- later change forgets it. An app's own deletion still cascades to both
-- tables in the same statement, and NO ACTION is checked at the end of it.
ALTER TABLE app_scans DROP CONSTRAINT app_scans_spec_id_fkey;
ALTER TABLE app_scans ADD CONSTRAINT app_scans_spec_id_fkey
    FOREIGN KEY (spec_id) REFERENCES spec_revisions(id);
