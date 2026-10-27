DROP INDEX app_standbys_stale_host;
DROP INDEX app_standbys_home;
ALTER TABLE app_standbys
    DROP COLUMN last_error_remedy,
    DROP COLUMN last_error_code,
    DROP COLUMN home_host;
