-- Notices on an app a person has chosen to ignore (R-412).
--
-- Per person, like favorites (000023): ignoring is that person's alone, and
-- everyone else still sees the notice. Kept by Pando rather than the browser
-- so the API, CLI and MCP see the same thing the console does (R-261).
--
-- `fingerprint` is what the notice said when it was ignored. A notice whose
-- current fingerprint differs is shown again — another variable empty, a later
-- backup failed — so an ignore is keyed on what a notice says, not on its kind.
-- A row whose notice has changed or gone simply hides nothing.
--
-- An ignore hides a reminder and nothing more. Nothing that plans, deploys or
-- gates on security reads this table.
--
-- Ignoring a notice again, once it has changed, replaces the fingerprint in
-- place; updated_at says when.
CREATE TABLE notice_ignores (
    user_id     text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    app_id      text NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    notice_key  text NOT NULL CHECK (char_length(notice_key) BETWEEN 1 AND 200),
    fingerprint text NOT NULL CHECK (char_length(fingerprint) BETWEEN 1 AND 128),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, app_id, notice_key)
);
