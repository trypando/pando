-- A session's cookie is ses_<id>.<secret> (R-063, design 02 §2.7). The ID is
-- public: it is what the audit log, logs and the API name a session by. The
-- secret is 256 random bits, and only its HMAC-SHA-256 under the API token key
-- is kept here, as for API tokens, so neither the audit log nor a copy of this
-- table is a working cookie.
--
-- A session from before has no secret, and its cookie is its ID, which the
-- audit log holds. It cannot be converted, so every one is removed: everyone
-- signs in again after this upgrade.
TRUNCATE sessions;

ALTER TABLE sessions
    ADD COLUMN secret_hash text NOT NULL
        CONSTRAINT sessions_secret_hash_form CHECK (secret_hash ~ '^hmac-sha256:[0-9a-f]{64}$');
