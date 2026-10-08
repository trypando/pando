-- The eleventh adapter category (R-091; O-3 re-resolved with issue #127). A
-- source connection is an adapter_configs row like any other, with its token or
-- key in adapter_credentials as ciphertext (R-190). A category that exists only
-- in Go is one no adapter can be configured in (000010, 000016, 000021).
ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_category_check;
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_category_check
    CHECK (category IN ('runtime', 'routing', 'builder', 'secrets',
                        'services', 'identity', 'notify', 'backup', 'scanner', 'ai',
                        'source'));

-- An OAuth authorization of a source connection that has started and not
-- finished: the device code or PKCE verifier the adapter needs back, the state
-- a web callback must present, and when it stops being worth waiting for.
-- Sealed by the install's secrets adapter under the scope
-- `source-authorization:`, the same shape as adapter_credentials, so a callback
-- that reaches another replica finds it and nothing here is readable in a dump.
-- Removed when the authorization completes or a new one starts.
CREATE TABLE source_authorizations (
    adapter_id   text NOT NULL REFERENCES adapter_configs(id) ON DELETE CASCADE,
    field        text NOT NULL CHECK (field ~ '^[a-z][a-z0-9_]*$'),
    adapter_ref  text NOT NULL,     -- the secrets adapter that sealed it
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (adapter_id, field),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);
