DROP INDEX IF EXISTS apps_allocation_idx;
DROP TRIGGER IF EXISTS app_scans_pinned_score ON app_scans;
DROP FUNCTION IF EXISTS app_scans_pinned_score();
DROP TRIGGER IF EXISTS apps_pinned_derived ON apps;
DROP FUNCTION IF EXISTS apps_pinned_derived();
DROP FUNCTION IF EXISTS pando_pinned_scan(text, text);
DROP FUNCTION IF EXISTS pando_json_bigint(jsonb);
ALTER TABLE apps
    DROP COLUMN IF EXISTS alloc_runtime_ref,
    DROP COLUMN IF EXISTS alloc_cpu_millis,
    DROP COLUMN IF EXISTS alloc_memory_bytes,
    DROP COLUMN IF EXISTS alloc_disk_bytes,
    DROP COLUMN IF EXISTS alloc_log_bytes,
    DROP COLUMN IF EXISTS pinned_scan_score,
    DROP COLUMN IF EXISTS pinned_scan_score_fixable;
