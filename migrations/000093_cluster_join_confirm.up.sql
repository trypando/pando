-- A join is recorded pending until the joining host confirms it, and a
-- renewal is answered once per certificate (R-415, R-421; notes-cluster.md
-- "As built: second review").

-- When the joining host first presented the certificate its join issued
-- (hosts.Service.Peers). Until then the host is not listed and does not take
-- part in the cluster; a pending row past state.PendingJoinLifetime, or one
-- whose name joins again, is removed, its certificate revoked and its join
-- token's use given back. Hosts recorded before this migration, and the
-- install's own first host, are confirmed.
ALTER TABLE cluster_hosts ADD COLUMN confirmed_at timestamptz;
UPDATE cluster_hosts SET confirmed_at = joined_at;
CREATE INDEX cluster_hosts_pending_idx ON cluster_hosts (joined_at) WHERE confirmed_at IS NULL;

-- The certificate the host's latest renewal answered with, PEM, kept until
-- the host presents it: a renewal presenting the certificate before it, for
-- the same key, is answered with it again rather than issued another, so two
-- renewals of one certificate never leave the host holding one the cluster
-- does not pin.
ALTER TABLE cluster_hosts ADD COLUMN next_cert_pem text;
