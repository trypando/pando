DROP TRIGGER app_standbys_version ON app_standbys;
DROP FUNCTION app_standbys_next_version();
ALTER TABLE app_standbys DROP COLUMN version;
