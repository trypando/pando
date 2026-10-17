-- Two-factor authentication for local accounts (R-405, discussion #263).
--
-- An account from an identity provider gets its second factor from the
-- provider. A local account's is an authenticator app (TOTP, RFC 6238), with
-- recovery codes for when the authenticator is lost.

-- When the authenticator was turned on, or NULL for none. Not secret: the
-- proxy reads it, with host policy, to decide whether a session must set one
-- up before it is anything more than a way to do so.
ALTER TABLE users ADD COLUMN totp_enabled_at timestamptz;

-- The last time step a code was accepted for, so a code is good once: one read
-- over a shoulder or out of a log cannot be replayed inside its 30 seconds.
ALTER TABLE users ADD COLUMN totp_last_step bigint;

-- When the account's holder was first asked to set it up. Asked once, at the
-- first sign-in, where policy does not require it; an administrator's reset
-- clears it, so they are asked again.
ALTER TABLE users ADD COLUMN two_factor_offered_at timestamptz;

-- The authenticator's secret, sealed by the install's secrets adapter (R-190):
-- the shape of identity_adapter_credentials, keyed by the account. Field `totp`
-- is the one in use; `totp_pending` is one shown to its holder and not yet
-- confirmed with a code from it.
CREATE TABLE user_two_factor_credentials (
    user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    field        text NOT NULL CHECK (field IN ('totp', 'totp_pending')),
    adapter_ref  text NOT NULL,     -- the secrets adapter that sealed it
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, field),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);

-- Recovery codes, as SHA-256 digests. Each is 80 random bits, so a fast digest
-- is enough, as with a passcode unlock: there is nothing small to guess.
-- Hard-deleted when replaced; used_at marks one spent.
CREATE TABLE user_recovery_codes (
    user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    digest     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz,
    PRIMARY KEY (user_id, digest)
);

-- The proxy keeps who a session is for a few seconds (000068). Turning an
-- authenticator on or off, and host policy starting or stopping requiring
-- one, change what a session may reach, so each tells it.
DROP TRIGGER proxy_cache_users_update ON users;
CREATE TRIGGER proxy_cache_users_update AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                       OR OLD.alias_of IS DISTINCT FROM NEW.alias_of
                       OR OLD.email IS DISTINCT FROM NEW.email
                       OR OLD.display_name IS DISTINCT FROM NEW.display_name
                       OR OLD.adapter_id IS DISTINCT FROM NEW.adapter_id
                       OR OLD.must_change_password IS DISTINCT FROM NEW.must_change_password
                       OR OLD.totp_enabled_at IS DISTINCT FROM NEW.totp_enabled_at)
    EXECUTE FUNCTION proxy_cache_changed();

CREATE TRIGGER proxy_cache_host_policy AFTER INSERT OR UPDATE ON host_policy
    FOR EACH STATEMENT EXECUTE FUNCTION proxy_cache_changed();
