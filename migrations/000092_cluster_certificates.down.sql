ALTER TABLE cluster_hosts
    DROP CONSTRAINT cluster_hosts_joined_with_fkey,
    ADD CONSTRAINT cluster_hosts_joined_with_fkey
        FOREIGN KEY (joined_with) REFERENCES join_tokens(id) ON DELETE SET NULL;

DROP TABLE cluster_revoked_certs;

ALTER TABLE cluster_hosts DROP COLUMN prev_cert_sha256;
