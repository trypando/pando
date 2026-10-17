DROP TRIGGER IF EXISTS proxy_cache_host_policy ON host_policy;

DROP TRIGGER proxy_cache_users_update ON users;
CREATE TRIGGER proxy_cache_users_update AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                       OR OLD.alias_of IS DISTINCT FROM NEW.alias_of
                       OR OLD.email IS DISTINCT FROM NEW.email
                       OR OLD.display_name IS DISTINCT FROM NEW.display_name
                       OR OLD.adapter_id IS DISTINCT FROM NEW.adapter_id
                       OR OLD.must_change_password IS DISTINCT FROM NEW.must_change_password)
    EXECUTE FUNCTION proxy_cache_changed();

DROP TABLE IF EXISTS user_recovery_codes;
DROP TABLE IF EXISTS user_two_factor_credentials;
ALTER TABLE users DROP COLUMN IF EXISTS two_factor_offered_at;
ALTER TABLE users DROP COLUMN IF EXISTS totp_last_step;
ALTER TABLE users DROP COLUMN IF EXISTS totp_enabled_at;
