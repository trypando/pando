ALTER TABLE app_scans DROP CONSTRAINT app_scans_spec_id_fkey;
ALTER TABLE app_scans ADD CONSTRAINT app_scans_spec_id_fkey
    FOREIGN KEY (spec_id) REFERENCES spec_revisions(id) ON DELETE SET NULL;
