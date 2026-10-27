-- Renewing the cluster's certificates, refusing a removed host's, and keeping
-- the install's own host told apart from hosts that joined (R-414, R-415,
-- R-421; notes-cluster.md "As built: review fixes, certificates and
-- security").

-- The certificate a host held before its latest renewal. Both are the host's
-- until it presents the new one (hosts.Service.Peers), so a renewal whose
-- answer was lost on the way leaves the host reachable with the one it kept.
ALTER TABLE cluster_hosts
    ADD COLUMN prev_cert_sha256 text CHECK (prev_cert_sha256 IS NULL OR prev_cert_sha256 ~ '^[0-9a-f]{64}$');

-- Certificates of hosts removed from the cluster, which nothing accepts again.
CREATE TABLE cluster_revoked_certs (
    cert_sha256 text PRIMARY KEY CHECK (cert_sha256 ~ '^[0-9a-f]{64}$'),
    host_id     text NOT NULL CHECK (host_id LIKE 'host\_%'),
    revoked_at  timestamptz NOT NULL DEFAULT now()
);

-- The install's own first host is the one with no join token
-- (hosts.InstallHost). Deleting a token must never turn the hosts that
-- joined with it into install hosts.
ALTER TABLE cluster_hosts
    DROP CONSTRAINT cluster_hosts_joined_with_fkey,
    ADD CONSTRAINT cluster_hosts_joined_with_fkey
        FOREIGN KEY (joined_with) REFERENCES join_tokens(id) ON DELETE RESTRICT;
