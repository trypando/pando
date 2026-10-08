DROP INDEX IF EXISTS deployments_reservation_idx;
DROP TRIGGER IF EXISTS deployments_reservation ON deployments;
DROP FUNCTION IF EXISTS deployments_reservation();
ALTER TABLE deployments
    DROP COLUMN IF EXISTS reserve_runtime_ref,
    DROP COLUMN IF EXISTS reserve_cpu_millis,
    DROP COLUMN IF EXISTS reserve_memory_bytes,
    DROP COLUMN IF EXISTS reserve_disk_bytes,
    DROP COLUMN IF EXISTS reserve_log_bytes;
