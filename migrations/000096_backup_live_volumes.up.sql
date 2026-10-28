-- The volumes a scheduled backup copied while the app was running (R-423,
-- issue #344): a file written during the copy may be inconsistent in the
-- backup, and the app's notice names them. A database its adapter can dump is
-- dumped instead and is never listed.
ALTER TABLE backup_attempts ADD COLUMN live_volumes text[] NOT NULL DEFAULT '{}';
