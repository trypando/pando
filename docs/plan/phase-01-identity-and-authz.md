# Phase 1 — Identity and authorization

**Goal:** every principal kind, and an authorizer whose evaluation order is fully tested. This is the
phase where the two-plane distinction is either established correctly or quietly lost.

**Prerequisites:** phase 0.

**Design:** `../design/06-authorization-and-proxy.md` §1–3, §5–6; `../design/02-data-model.md`
§2.1, §2.2, §2.7. Also read `../../internal/core/authz/CLAUDE.md`.

## Tasks

- [x] `identity_adapters`, `users`, `groups`, `group_members`, `tokens`, `sessions` migrations
- [x] Local identity adapter: username/password, argon2id (R-044)
- [x] Bootstrap path for the first admin user
- [x] Server-side sessions; the cookie carries `ses_<id>.<secret>`, the secret stored as a digest (R-047, R-048, R-063)
- [x] `SessionPolicy` per adapter, and surface the effective revocation window rather than implying a
      global guarantee
- [x] Tokens, both kinds: delegated (resolves through `owner_user_id` live) and account (its own
      principal, appears in `grants`) — R-058, R-059, R-060; secret shown once (R-063)
- [x] `roles` seeded by migration with the immutability trigger (R-081)
- [x] The verb catalog (design 06 §5) and custom roles as arbitrary subsets (R-082) — **no implication
      graph**. Thirteen verbs, including `app.egress.override` (R-184); the three `*.override` verbs
      are Owner-only, not Operator. *Since issue #79 and #39:* `app.egress.override` is
      `app.egress.loosen`, `app.egress.tighten` (Owner and Operator) joined it, and the approval verbs
      `install.deploys.approve` and `app.deploy.approve` were added (design 06 §5)
- [x] `grants` table; app creation writes two rows, one per plane (R-073)
- [x] The authorizer: `CheckControl` and `CheckData`, in the fixed evaluation order
- [x] Live group resolution with the documented cache TTL (R-079)
- [x] **The revocation window** (design 06 §3.1): one number, 120s. Session check, group cache,
      assertion lifetime and long-lived-connection re-auth all sit at or below it, and the effective
      window is the largest of them — not the smallest
- [x] Audit every **denial**, not only successes

## Requirements in scope

R-044, R-047, R-048, R-049, R-058–R-063, R-070–R-082, R-087, R-229, R-272.

## Scope note taken during implementation

`grants` references `apps`, which properly belongs to phase 2. Migration 000003 creates only the
columns `grants` needs — id, name, slug, owner, state, plus the two fields phase 0 added for the
reconciler. Phase 2 adds `spec_revisions` and the `pinned_spec_id` FK, which is circular and can only
be added once both tables exist.

## Done when

**Met.** Shipped in an earlier phase; the named tests exist and the full acceptance
suite is green. The boxes below went unticked at the time — corrected here rather than
left to imply the work is outstanding.

The evaluation order in design 06 §2 is **fully covered by unit tests**, including a delegated token
orphaned by its owner's deletion. Plus: a test asserting the effective revocation window is what design
06 §3.1 says it is, so that raising any one cache later fails a test rather than silently widening it.

## Traps

- **The two planes.** `CheckData` gets exactly one cross-plane implication — ownership (R-072). Write
  the test asserting an operator on someone else's app is denied *use*, and the comment explaining
  that this was reversed once during design. Both are required, not optional.
- **Policy before grants.** A policy that disables exec install-wide denies the owner too (R-272).
- **Step 4 is a live lookup.** Not a cascade run at revocation time. A missed cascade is a permanent
  security hole; a live lookup cannot be missed.
- Suspended is not deleted (R-049, R-282). Destruction rules fire on `deleted` only.
