-- The twelfth adapter category (R-252, issue #153). The install's image
-- registry was a singleton table set from the console (000051); it is now an
-- adapter_configs row like any other, with its password or AWS secret key in
-- adapter_credentials as ciphertext (R-190).
ALTER TABLE adapter_configs DROP CONSTRAINT adapter_configs_category_check;
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_category_check
    CHECK (category IN ('runtime', 'routing', 'builder', 'secrets',
                        'services', 'identity', 'notify', 'backup', 'scanner', 'ai',
                        'source', 'image_registry'));

-- 000051's refusal of a registry address that carries a username or password,
-- kept: https://user:pass@host is a password stored in the clear (R-190), and
-- the adapter refusing it before a save is code a later change could route
-- around.
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_registry_url_no_credential
    CHECK (category <> 'image_registry' OR coalesce(config->>'url', '') !~ '://[^/]*@');

-- 000051's tables shipped in no release, so nothing in them is carried over:
-- an install that set its registry from the console since then sets it again
-- as an adapter. One set by PANDO_REGISTRY_* needs nothing.
DROP TABLE install_registry_credentials;
DROP TABLE install_registry;
