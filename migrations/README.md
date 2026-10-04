# Migrations

`golang-migrate`, versioned up/down, embedded in the binary.

Naming: `{version}_{description}.{up|down}.sql`, e.g. `000001_audit_events.up.sql`.

## Migrations are a requirement-enforcement mechanism here

Several requirements are enforced by DDL rather than by code, because a code review will eventually
miss the thing they catch. When you write a migration, these are not optional extras:

| DDL | Enforces |
|---|---|
| `REVOKE UPDATE, DELETE ON audit_events` from the app role | R-027 — the audit log is not rewritable |
| `audit_drop_month` refusing a month under three months old or with no archive of every row; `audit_ensure_partition` revoking `UPDATE`/`DELETE` on each new partition | R-347, R-348 — retention removes only old, archived months, and a partition is not a way around R-027 |
| Trigger rejecting `UPDATE`/`DELETE` on `spec_revisions` | R-152 — rollback is always to something that provably existed |
| Trigger protecting `roles` where `builtin = true` | R-081 — built-in roles change only by migration |
| `host_policy.id integer PRIMARY KEY DEFAULT 1 CHECK (id = 1)` | R-015 — one install, one org |
| `volumes.app_id … ON DELETE RESTRICT` | R-204 — volumes survive app deletion |
| `CHECK (NOT (config ? 'credentials'))` on `adapter_configs` | R-190 — an adapter credential is never stored in the clear (O-20) |
| `identity_adapters_no_inline_credentials` refusing `credentials`, `client_secret`, `scim_token` | R-190 — an identity provider's secret is never stored in the clear |
| `user_identities` primary key `(adapter_id, external_id)` | O-1 — one identity reaches exactly one account |
| `users_alias_is_suspended` | O-1 — an account whose identities were moved away cannot be reactivated |
| `group_links_direction` trigger | R-078 — a provider's group feeds a Pando group, never the reverse |

**Adding a verb to a built-in role is done by migration, and only by migration** (R-081). That is the
upgrade mechanism the requirement promises; there is no runtime path.

See `docs/design/02-data-model.md` for the full schema and `internal/core/state/CLAUDE.md` for the
constraints that must not be relaxed.
