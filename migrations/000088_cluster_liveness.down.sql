ALTER TABLE cluster_hosts
    DROP CONSTRAINT cluster_hosts_lost_by_with_lost_at,
    DROP COLUMN last_fence_id,
    DROP COLUMN lost_by,
    DROP COLUMN lost_at;
