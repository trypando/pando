-- The build before this one reads the cookie as the session's ID, which the
-- audit log holds, so no session made here may survive into it.
DELETE FROM sessions;

ALTER TABLE sessions DROP COLUMN secret_hash;
