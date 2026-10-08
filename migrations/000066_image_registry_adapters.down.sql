-- 000051's tables, empty: what was in them is not restored.
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

ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_registry_url_no_credential;
DELETE FROM adapter_configs WHERE category = 'image_registry';
ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_category_check;
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_category_check
    CHECK (category IN ('runtime', 'routing', 'builder', 'secrets',
                        'services', 'identity', 'notify', 'backup', 'scanner', 'ai',
                        'source'));
