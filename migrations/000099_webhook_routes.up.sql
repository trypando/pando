-- A webhook route's secret (R-427): one per route, sealed by the secrets adapter
-- like an auto-deploy webhook's. Ciphertext or an external reference, never the
-- value (R-190). The routes themselves are in the spec.
CREATE TABLE webhook_route_secrets (
    app_id       text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    route_name   text NOT NULL,
    adapter_ref  text NOT NULL,
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, route_name),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);

-- Signatures seen on an hmac_sha256 route (R-433). The primary key is the claim:
-- a second insert of a live signature loses, on any replica. A digest of the
-- signature only, never the body or the secret.
CREATE TABLE webhook_deliveries (
    app_id           text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    route_name       text NOT NULL,
    signature_digest bytea NOT NULL,
    accepted         boolean NOT NULL DEFAULT false,
    expires_at       timestamptz NOT NULL,
    PRIMARY KEY (app_id, route_name, signature_digest)
);

CREATE INDEX webhook_deliveries_expires_at ON webhook_deliveries (expires_at);
