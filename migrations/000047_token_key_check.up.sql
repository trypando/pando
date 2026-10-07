-- HMAC-SHA-256 of a fixed label under the install's API token key, which
-- every replica compares at start (R-063, issue #72). The key is not in the
-- database — that is the point of it: a dump alone cannot test a guess at a
-- token — so replicas must each be given the same one, and this is how a
-- replica given a different one finds out before it rejects every token the
-- others issued. The digest protects nothing; it identifies the key.
--
-- Numbered 50 so that it applies after 48 and 49, which later branches of
-- issue #72 already add.
CREATE TABLE token_key_check (
    id         integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    digest     bytea NOT NULL CHECK (length(digest) = 32),
    created_at timestamptz NOT NULL DEFAULT now()
);
