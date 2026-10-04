# State store — read this before touching the schema

Design: `docs/design/02-data-model.md`. PostgreSQL, `sqlc` for typed queries, `golang-migrate` for
versioning. **No ORM.**

R-020 makes this store the sole record of how every app runs, which sets the bar: every mutation is
audited, every object is exportable, nothing important lives only in memory.

## Conventions

- IDs are `text`, prefixed ULIDs — **not `uuid`**. The prefix is load-bearing for readability.
- Timestamps are `timestamptz`, always UTC.
- Soft delete **only** where an object must survive its own deletion for audit purposes. Apps and
  users soft-delete; grants and sessions hard-delete.
- Every table with a mutable row carries `created_at`, `updated_at`.

## Constraints that enforce requirements

These exist because a code review will eventually miss the thing they catch. Do not remove one to make
a migration simpler.

| Constraint | Enforces |
|---|---|
| Trigger rejecting `UPDATE`/`DELETE` on `spec_revisions` | R-152 — rollback is always to something that provably existed |
| `REVOKE UPDATE, DELETE` on `audit_events` **and every partition of it** from the app role | R-027 — the audit log is not rewritable, by adapters *or* core. A month is a table; `applyGrants` revokes on each one at every start |
| `audit_drop_month`, executable by `pando_audit_archiver` only | R-347, R-348 — the only way an event leaves the log: a whole month, three months old at least, with a recorded archive of every row. The floor is a literal in the function; changing it is a migration |
| App role has `SELECT` only on `audit_archives` | R-348 — the drop function trusts that record, so the role serving traffic must not be able to write it |
| Trigger protecting `roles` rows with `builtin = true` | R-081 — built-in roles change only by migration |
| `host_policy.id integer PRIMARY KEY DEFAULT 1 CHECK (id = 1)` | R-015 — one install, one org; nobody accidentally builds multi-tenancy |
| `volumes.app_id … ON DELETE RESTRICT` | R-204 — an app cannot be deleted out from under its volumes |
| `UNIQUE (app_id, plane, principal_kind, principal_id)` on `grants` | Two planes stay two rows (R-073) |
| `grants (role_id, role_scope) → roles (id, scope)` + `grants_install_has_no_app` | R-080 — an install role cannot be granted on one app and an app role cannot be granted install-wide; "no app" and "carries install verbs" cannot come apart |
| `grants_data_plane_is_app_scoped` | R-070 — data-plane use is per-app and binary; there is no install-wide "use" |
| `user_identities` PK and `users_alias_is_suspended` | O-1 — an identity reaches one account; linking aliases and never merges |
| `identity_adapters_no_inline_credentials` | R-190 — an identity provider's secret lives only in `identity_adapter_credentials` |

## Things deliberately absent from the schema

Adding any of these is a design change, not a refactor:

- **Hosts.** R-256: multi-machine capability lives entirely in adapters. A `hosts` table is the first
  step toward the scheduler R-010 forbids.
- **Tenants / orgs.** R-015.
- **Per-user instances.** R-290 is LATER. When it lands it is an `app_instances` table keyed on
  `(app_id, user_id)` with its own volume rows; nothing needs restructuring to accommodate it.
- **Log storage.** Logs stream from the runtime adapter and are retained on disk under a size cap
  (R-222). Putting them in Postgres makes R-224's disk accounting harder, not easier.
- **A plaintext secret column.** None exists anywhere. The local adapter stores ciphertext; external
  adapters store only a reference (R-190, R-191).

## Notes that are easy to get wrong

- `users.id` is what goes in the assertion `sub` claim (R-054). **The single most important stability
  guarantee in the schema** — apps key their data on it. Stable across email change, independent of
  the identity adapter's own identifiers.
- `users.status` is three-valued: suspended is **not** deleted (R-049, R-282). Destruction rules
  (R-280) fire on `deleted`, never on `suspended`.
- Group membership is read live at authorization time (R-079), never denormalized into grants.
  Authorization reads it through the `effective_group_members` view (direct members plus those of
  linked provider groups); a new query asking "which groups is this person in" must read the view,
  not `group_members`, or linked groups stop counting there.
- `grants.app_id IS NULL` means **install-scoped** (O-17), and the unique index above is
  `NULLS NOT DISTINCT` — so it already reads "one control grant per principal, install-wide" for
  those rows. Do not add a second index for install grants; it would be redundant.
- An administrator is a principal with a grant. **There is no `is_admin` column and adding one is a
  design change**, not a shortcut: a boolean cannot express "manages users but not policy", which is
  what R-265's "scoped to whatever privileges they hold" requires.
- A delegated token has no grants of its own; authorization resolves through `owner_user_id` live
  (R-059). No cascade to write, so no cascade to miss.
- `secrets.version` increments on rotation so the reconciler can detect that a restart is required
  (R-193).
- `backups` rows with `kind = 'on_delete'` have `retain_until IS NULL` — kept until explicitly
  discarded, never aged out (R-204).
- Sessions are server-side rows, not stateless cookies. The cookie carries only `ses_…`. Revocation
  has to be immediate when an adapter can push (R-048), which a stateless cookie cannot do.
