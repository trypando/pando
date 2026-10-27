-- The owner's third review of #347 (notes-cluster.md "As built: third review
-- fixes").

-- SHA-256 of the public key a host joined with, hex (R-415): what tells a
-- joining host trying again, which may replace its own pending join, from
-- another machine that took the same name, which may not while that join is
-- within its lifetime. Empty for hosts recorded before this migration and for
-- the install's own first host.
ALTER TABLE cluster_hosts ADD COLUMN key_sha256 text
    CHECK (key_sha256 IS NULL OR key_sha256 ~ '^[0-9a-f]{64}$');

-- A deleted app keeps no uploaded source (R-417, R-224): apps are kept as
-- rows once deleted, so app_uploads' foreign key never removed one, and an
-- upload moved in from an old file after the app was deleted was kept, and
-- carried in every backup, for good. Deleting the app removes it, however the
-- app is deleted; and what earlier versions kept is removed here.
DELETE FROM app_uploads u USING apps a WHERE a.id = u.app_id AND a.deleted_at IS NOT NULL;

CREATE FUNCTION app_uploads_forget_deleted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM app_uploads WHERE app_id = NEW.id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER apps_forget_upload_on_delete
    AFTER UPDATE OF deleted_at ON apps
    FOR EACH ROW
    WHEN (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL)
    EXECUTE FUNCTION app_uploads_forget_deleted();
