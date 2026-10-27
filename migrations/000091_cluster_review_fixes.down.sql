ALTER TABLE app_standbys DROP COLUMN promoting_at;
ALTER TABLE cluster_hosts
    DROP COLUMN ping_cert_sha256,
    DROP COLUMN ping_address;
