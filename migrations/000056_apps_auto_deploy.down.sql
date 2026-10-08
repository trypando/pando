DROP INDEX IF EXISTS apps_auto_deploy_idx;
DROP TRIGGER IF EXISTS apps_auto_deploy ON apps;
DROP FUNCTION IF EXISTS apps_auto_deploy_from_pin();
ALTER TABLE apps DROP COLUMN IF EXISTS auto_deploy;
