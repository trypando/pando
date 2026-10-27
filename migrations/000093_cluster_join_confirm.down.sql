DELETE FROM cluster_hosts WHERE confirmed_at IS NULL;
ALTER TABLE cluster_hosts DROP COLUMN next_cert_pem;
DROP INDEX cluster_hosts_pending_idx;
ALTER TABLE cluster_hosts DROP COLUMN confirmed_at;
