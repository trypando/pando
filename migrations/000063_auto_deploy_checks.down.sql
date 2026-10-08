DROP TABLE IF EXISTS auto_deploy_checks;

-- Revisions are append-only (R-152), so any auto_deploy revision stays; the
-- old constraint comes back for new rows only.
ALTER TABLE spec_revisions DROP CONSTRAINT spec_revisions_origin_check;
ALTER TABLE spec_revisions ADD CONSTRAINT spec_revisions_origin_check
    CHECK (origin IN ('detected', 'edited', 'redetected', 'imported', 'manual')) NOT VALID;
