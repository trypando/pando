-- Registry credentials for apps deployed from a private image (issue #41).
--
-- App-owned, as O-3 decided for source credentials: an app keeps deploying
-- after the person who supplied the credential leaves, and the supplier is in
-- the audit event rather than here. Not an app secret: a row in `secrets` can
-- be named by an env entry and so reach the app, and a credential that pulls
-- the app's image is never the app's to read. Ciphertext only, sealed by the
-- install's secrets adapter under the scope `registry:` (R-190, R-194).
CREATE TABLE registry_credentials (
    app_id       text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    field        text NOT NULL CHECK (field ~ '^[a-z][a-z0-9_]*$'),
    adapter_ref  text NOT NULL,     -- the secrets adapter that sealed it
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, field),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);
