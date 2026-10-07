-- Certificates Pando issues for the edge itself (issue #72, PR 6; R-169,
-- R-174). On Kubernetes the edge runs as several replicas, so Pando's leader
-- is the one ACME client and every replica serves what it issued
-- (notes-kubernetes-runtime-issue-72.md).
--
-- Numbered 000052: 000050 is the last migration on the stack, PR 5 adds none,
-- and PR 7 may take 000051. The numbers only need to be distinct and ascending
-- when the stack lands.
--
-- Nothing secret is stored in the clear (R-190): the account key and each
-- certificate with its key are sealed by the install's secrets adapter, and
-- these rows hold only its ciphertext or an external reference to it.

CREATE TABLE edge_acme_accounts (
    email         text NOT NULL,
    directory     text NOT NULL,
    adapter_ref   text NOT NULL,
    ciphertext    bytea,
    external_ref  text,
    registration  jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (email, directory),
    CONSTRAINT edge_acme_accounts_sealed CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);

CREATE TABLE edge_certificates (
    name          text PRIMARY KEY,
    domains       text[] NOT NULL,
    adapter_ref   text,
    -- The PEM certificate chain and key, sealed together. Null until the first
    -- issuance succeeds; a failure before that is still recorded, so the next
    -- attempt waits rather than spending the CA's rate limit.
    ciphertext    bytea,
    external_ref  text,
    not_after     timestamptz,
    issued_at     timestamptz,
    attempted_at  timestamptz,
    last_error    text,
    CONSTRAINT edge_certificates_sealed CHECK (
        not_after IS NULL OR ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);

-- Pending HTTP-01 challenges, answered by whichever replica the CA's request
-- reaches. A key authorization is public by design: it is what the CA reads.
CREATE TABLE edge_acme_challenges (
    token              text PRIMARY KEY,
    key_authorization  text NOT NULL,
    domain             text NOT NULL,
    expires_at         timestamptz NOT NULL
);
