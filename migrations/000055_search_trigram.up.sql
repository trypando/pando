-- Substring search that costs the matches, not the table (issue #72, O-53).
--
-- Every search box in the console and the API matches a substring anywhere in
-- a name or an email. A b-tree cannot serve a pattern with a leading
-- wildcard, so each search read every account, app or group in the install.
-- A trigram index can.
--
-- One index per table, over the fields a search matches joined by chr(31)
-- (the ASCII unit separator, which nobody types into a search box), rather
-- than one per field: a search is one pattern against one expression, so it
-- is one index lookup instead of an OR of three that the planner prices above
-- reading the table. The queries in internal/core/state write the same
-- expressions (searchUsers, searchApps); an index on an expression serves
-- only that expression, character for character.
--
-- pg_trgm ships with PostgreSQL's contrib modules, which the official images
-- carry and which RDS, Cloud SQL and Azure Database for PostgreSQL all offer.
-- It is a trusted extension since PostgreSQL 13, so the role that owns Pando's
-- database may create it without being a superuser.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Accounts: GET /users?q=, a group's members, the people pickers
-- (Users.Search) and the access assistant's search tools. Partial on what
-- those lists read: live accounts that are not aliases.
CREATE INDEX IF NOT EXISTS users_search_trgm_idx ON users USING gin
    ((external_id || chr(31) || coalesce(display_name, '') || chr(31) || coalesce(email, '')) gin_trgm_ops)
    WHERE deleted_at IS NULL AND alias_of IS NULL;

-- Apps: GET /apps?q= and GET /me/apps?q=, by name or slug.
CREATE INDEX IF NOT EXISTS apps_search_trgm_idx ON apps USING gin
    ((name || chr(31) || slug) gin_trgm_ops)
    WHERE deleted_at IS NULL;

-- Groups: GET /groups?q= and Groups.Search, by name.
CREATE INDEX IF NOT EXISTS groups_name_trgm_idx ON groups USING gin (name gin_trgm_ops);
