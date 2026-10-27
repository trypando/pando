-- The S3 backup destination (R-217, R-417; resolves O-69). Its access key ID
-- and secret access key are adapter credentials, sealed in
-- adapter_credentials (R-190), never settings. A secret access key is a
-- credential for every kind that takes one (the ECR image registry too), and
-- the S3 kind's key ID is one as well, so neither may be a stored setting.
ALTER TABLE adapter_configs ADD CONSTRAINT adapter_configs_no_inline_s3_keys
    CHECK (NOT (config ? 'secret_access_key')
           AND NOT (category = 'backup' AND kind = 's3' AND config ? 'access_key_id'));
