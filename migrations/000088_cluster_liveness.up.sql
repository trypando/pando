-- Hosts taken for lost, and the fences hosts report (R-421,
-- notes-cluster.md "Deciding a host is gone, and fencing").
--
-- Only the outcome is kept. Which host reached which, and for how long, stays
-- in the leader's memory: a decision needs only the last three periods of it,
-- and the database must not be on the path of deciding that a host is gone.
ALTER TABLE cluster_hosts
    -- When the host was taken for lost: by the majority rule, or by a person
    -- with install.cluster.manage. Its apps' standbys were started then.
    ADD COLUMN lost_at       timestamptz,
    -- 'liveness' for the majority rule, or the principal who said so.
    ADD COLUMN lost_by       text,
    -- The latest fence the host reported (fnc_…), so each is audited once.
    ADD COLUMN last_fence_id text CHECK (last_fence_id IS NULL OR last_fence_id LIKE 'fnc\_%'),
    ADD CONSTRAINT cluster_hosts_lost_by_with_lost_at CHECK ((lost_at IS NULL) = (lost_by IS NULL));
