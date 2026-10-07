-- Indexes for the lists that grow with the organization (issue #72).
--
-- Numbered 000049 rather than the next free 000046: another change in the same
-- stack (PR 4, retention and deployment indexes) takes 000046. IF NOT EXISTS
-- because that change also lists apps(owner_user_id); whichever lands second
-- finds it made.

-- The launcher (Apps.ListForUse): the apps a person owns.
CREATE INDEX IF NOT EXISTS apps_owner_user_idx ON apps (owner_user_id) WHERE deleted_at IS NULL;

-- The launcher: apps open to anyone. principal_id is NULL on these, so
-- grants_principal_idx cannot find them.
CREATE INDEX IF NOT EXISTS grants_data_anonymous_idx ON grants (app_id)
    WHERE plane = 'data' AND principal_kind = 'anonymous';

-- GET /apps, keyset-paged newest first on (created_at, id).
CREATE INDEX IF NOT EXISTS apps_live_created_idx ON apps (created_at DESC, id DESC) WHERE deleted_at IS NULL;

-- GET /groups, keyset-paged on (Pando's own first, lower(name), id).
CREATE INDEX IF NOT EXISTS groups_list_order_idx ON groups ((adapter_id IS NOT NULL), lower(name), id);

-- GET /approvals, keyset-paged oldest first on (started_at, id).
CREATE INDEX IF NOT EXISTS deployments_awaiting_order_idx ON deployments (started_at, id)
    WHERE status = 'awaiting_approval';

-- SCIM lists (RFC 7644 §3.4.2): startIndex/count paging in creation order,
-- and the totalResults count every page must carry, served from an index
-- rather than a scan of every identity.
CREATE INDEX IF NOT EXISTS user_identities_scim_list_idx ON user_identities (adapter_id, created_at, user_id)
    WHERE scim_resource IS NOT NULL;
CREATE INDEX IF NOT EXISTS user_identities_scim_external_idx ON user_identities (adapter_id, scim_external_id)
    WHERE scim_external_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS groups_scim_list_idx ON groups (adapter_id, created_at, id)
    WHERE adapter_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS groups_scim_name_idx ON groups (adapter_id, lower(name))
    WHERE adapter_id IS NOT NULL;
