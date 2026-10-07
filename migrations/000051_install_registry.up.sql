-- The install's image registry, set from the console and the API (issue #72,
-- PR 5). Startup configuration (PANDO_REGISTRY_*) still wins field by field;
-- these rows are what applies where it says nothing.
--
-- Settings and credential are apart, as adapter_configs and
-- adapter_credentials are. The settings row has no column a credential could
-- go in, and its URL may not carry one either: https://user:pass@host is
-- refused, because that is a password stored in the clear (R-190).
CREATE TABLE install_registry (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),  -- one per install
    url        text NOT NULL DEFAULT ''
               CONSTRAINT install_registry_url_no_credential CHECK (url !~ '://[^/]*@'),
    username   text NOT NULL DEFAULT '',
    kind       text NOT NULL DEFAULT 'basic' CHECK (kind IN ('basic', 'ecr')),
    layout     text NOT NULL DEFAULT 'per_app' CHECK (layout IN ('per_app', 'single')),
    insecure   boolean NOT NULL DEFAULT false,
    always     boolean NOT NULL DEFAULT false,
    updated_by text,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The registry's password, ciphertext only, sealed by the install's secrets
-- adapter under the scope `install-registry:` (R-190, R-194). There is no
-- plaintext column, and a row with neither ciphertext nor an external
-- reference is refused.
CREATE TABLE install_registry_credentials (
    registry_id  text NOT NULL CHECK (registry_id = 'install'),
    field        text NOT NULL CHECK (field = 'password'),
    adapter_ref  text NOT NULL,     -- the secrets adapter that sealed it
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (registry_id, field),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);
