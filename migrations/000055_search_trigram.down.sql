DROP INDEX IF EXISTS groups_name_trgm_idx;
DROP INDEX IF EXISTS apps_search_trgm_idx;
DROP INDEX IF EXISTS users_search_trgm_idx;
-- The extension stays: something other than these indexes may use it by now,
-- and an unused extension costs nothing.
