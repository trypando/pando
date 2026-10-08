-- An app's auto-deploy webhook secret (R-142, O-58): one per app, sealed by the
-- secrets adapter like a subscription's signing key. Ciphertext or an external
-- reference, never the value (R-190), and no column a value could be put in.
CREATE TABLE auto_deploy_webhook_secrets (
    app_id       text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    adapter_ref  text NOT NULL,
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);
