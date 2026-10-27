-- Review fixes to the cluster (#347, notes-cluster.md "As built: review
-- fixes, standby and runtime").
--
-- A host has two places other hosts reach it at. Its agent, at address with
-- cert_sha256, carries runtime calls and proxied traffic (R-416); a control
-- host also answers the cluster's liveness checks on its cluster listener
-- (R-421), with the certificate it serves there. Keeping one pair for both
-- had a control host's listener overwrite its agent, so a host holding both
-- roles could not run apps. Empty is "where the agent is".
ALTER TABLE cluster_hosts
    ADD COLUMN ping_address     text NOT NULL DEFAULT '',
    ADD COLUMN ping_cert_sha256 text NOT NULL DEFAULT ''
        CHECK (ping_cert_sha256 = '' OR ping_cert_sha256 ~ '^[0-9a-f]{64}$'),
    -- Since when the host has said it is fenced: cut off from most of the
    -- cluster, its apps stopped (R-421). The leader records what each host
    -- answers; every replica's reconciler leaves the host's apps stopped
    -- while it is set (R-148 does not undo a fence). Cleared when the host
    -- says it is not.
    ADD COLUMN fenced_at        timestamptz;

-- A promotion is written ahead (R-153, R-419): before the standby is started
-- the app's home is its standby's host, and promoting_at says the start has
-- not finished. Nothing else touches the app's standby or its containers
-- while it is set: the standby pass skips it and the reconciler leaves the
-- app alone. Cleared when the promotion ends.
ALTER TABLE app_standbys
    ADD COLUMN promoting_at timestamptz;
