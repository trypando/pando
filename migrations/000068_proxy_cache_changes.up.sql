-- Tells every replica's proxy that something it may have cached about a
-- request has changed (issue #93, design 06 §3.1).
--
-- The proxy keeps, for a few seconds at most, who a session cookie belongs to,
-- which app a hostname or path names, and the facts CheckData reads about an
-- app and a caller. A NOTIFY on the channel below, delivered when a
-- transaction that changed any of them commits, empties that cache on every
-- replica, so a revocation takes effect at once rather than when an entry
-- ages out. A replica that is not listening caches nothing.
--
-- Triggers rather than calls in the store, so a change made by a cascade, or
-- by a path written later, is told too. On apps, users and sessions only the
-- columns the proxy reads count: an app's reconcile lease is renewed every few
-- seconds, and is not a reason to forget every app. Statement-level, and
-- Postgres folds identical notifications in one transaction into one, so a
-- batch is one message.
CREATE FUNCTION proxy_cache_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('pando_proxy_cache', '');
    RETURN NULL;
END $$;

-- Who a grant reaches, and what group membership means for it.
CREATE TRIGGER proxy_cache_grants AFTER INSERT OR UPDATE OR DELETE ON grants
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
CREATE TRIGGER proxy_cache_group_members AFTER INSERT OR UPDATE OR DELETE ON group_members
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
CREATE TRIGGER proxy_cache_group_links AFTER INSERT OR UPDATE OR DELETE ON group_links
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
CREATE TRIGGER proxy_cache_passcode_unlocks AFTER UPDATE OR DELETE ON passcode_unlocks
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();

-- A session revoked, cut short, or removed. A new session is not cached
-- until it is first used, so its insert is not news.
CREATE TRIGGER proxy_cache_sessions_delete AFTER DELETE ON sessions
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
CREATE TRIGGER proxy_cache_sessions_update AFTER UPDATE ON sessions
    FOR EACH ROW WHEN (OLD.revoked_at IS DISTINCT FROM NEW.revoked_at
                       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
                       OR OLD.user_id IS DISTINCT FROM NEW.user_id)
    EXECUTE FUNCTION proxy_cache_changed();

-- A person suspended, deleted or linked as an alias, or what the assertion
-- says about them.
CREATE TRIGGER proxy_cache_users_delete AFTER DELETE ON users
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
CREATE TRIGGER proxy_cache_users_update AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                       OR OLD.alias_of IS DISTINCT FROM NEW.alias_of
                       OR OLD.email IS DISTINCT FROM NEW.email
                       OR OLD.display_name IS DISTINCT FROM NEW.display_name
                       OR OLD.adapter_id IS DISTINCT FROM NEW.adapter_id
                       OR OLD.must_change_password IS DISTINCT FROM NEW.must_change_password)
    EXECUTE FUNCTION proxy_cache_changed();

-- An app created, removed, moved, stopped or started, given a new spec, or a
-- new owner. A new app may take a hostname or path a request was turned away
-- from a moment ago.
CREATE TRIGGER proxy_cache_apps_insert_delete AFTER INSERT OR DELETE ON apps
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
CREATE TRIGGER proxy_cache_apps_update AFTER UPDATE ON apps
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state
                       OR OLD.desired_state IS DISTINCT FROM NEW.desired_state
                       OR OLD.pinned_spec_id IS DISTINCT FROM NEW.pinned_spec_id
                       OR OLD.owner_user_id IS DISTINCT FROM NEW.owner_user_id
                       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
                       OR OLD.slug IS DISTINCT FROM NEW.slug
                       OR OLD.name IS DISTINCT FROM NEW.name
                       OR OLD.address_hostname IS DISTINCT FROM NEW.address_hostname
                       OR OLD.address_path IS DISTINCT FROM NEW.address_path
                       OR OLD.address_port IS DISTINCT FROM NEW.address_port)
    EXECUTE FUNCTION proxy_cache_changed();
