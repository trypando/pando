DROP TRIGGER IF EXISTS apps_forget_upload_on_delete ON apps;
DROP FUNCTION IF EXISTS app_uploads_forget_deleted();
ALTER TABLE cluster_hosts DROP COLUMN IF EXISTS key_sha256;
