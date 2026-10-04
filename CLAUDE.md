# Pando — agent working guide

Pando hosts apps on a host you own, without per-app deployment setup. Go backend, Postgres state,
React console, everything compiled into one binary.

**This repository is currently a scaffold.** The design is complete and settled; almost none of it is
implemented. Your job is usually to implement one slice of it. Do not redesign.

---

## 1. Read before you write

Two document sets, and the distinction is load-bearing:

| | Path | Authority |
|---|---|---|
| **Requirements** | `docs/requirements.md` | What Pando *is*. 242 requirements, IDs `R-###`. Churns slowly. |
| **Design** | `docs/design/00`–`10` | How it is built. Churns every sprint. |

**When design contradicts a requirement, the requirement wins** — or the requirement gets amended
in the same change. Never a silent divergence. If you believe a requirement is wrong, say so in your
report and stop; do not route around it.

Tags mean the same thing everywhere: **[D]** decided (changing it changes the product),
**[P]** proposed (a default, override freely with a reason), **[O]** open (unresolved, listed in
`docs/plan/open-decisions.md`).

**Minimum reading for any task:** `docs/requirements.md` §1–3 → `docs/design/01-spec-schema.md` →
`docs/design/07-sequences.md`. Then the design doc for your area, and your phase file in
`docs/plan/`.

**Touching authorization or the proxy?** Read `docs/design/06-authorization-and-proxy.md` in full and
Sequence C in `07-sequences.md`. This is not optional. That code has already been broken once during
design.

**Writing an adapter?** `docs/design/03-adapter-interfaces.md`, then the relevant section of `01`.

---

## 2. Invariants you may not break

These are not style preferences. Each one has a mechanism, and the mechanism is the point — a code
review will eventually fail to catch these, so they are enforced structurally. If your change makes
one of these harder to enforce, the change is wrong.

| Rule | Requirement | Mechanism | Where |
|---|---|---|---|
| Adapters never touch authz, audit, or state | R-027 | CI import lint (`.golangci.yml` depguard) | design 03 §9 |
| The audit log cannot be rewritten | R-027 | DB grant: no `UPDATE`/`DELETE` on `audit_events` or any of its monthly partitions; startup refuses otherwise | design 02 §2.6, 06 §6 |
| Retention removes only old, archived months | R-347, R-348 | Only `audit_drop_month` removes events: owner-defined, executable by `pando_audit_archiver` alone, refuses a month under three months old or without a recorded archive of every row | design 02 §2.6 |
| Secrets never reach a log line | R-194 | `secret.Value` renders `[redacted]` in every marshaler | design 00 §3.3 |
| Adapter credentials are never stored in the clear | R-190 | `adapter_credentials` and `identity_adapter_credentials` hold ciphertext only; CHECKs refuse `credentials` in `adapter_configs.config` and any secret key in `identity_adapters.config` | design 10 §7, 02 §2.1 |
| A redirect sign-in finishes only in the browser that started it | R-043 | Flow bound to a cookie digest; one-time handoff code; `TestR043_ASignInFinishesOnlyInTheBrowserThatStartedIt` | design 06 §3.2 |
| Deprovisioning suspends, never deletes | R-049 | SCIM `active:false` and `DELETE` call `SetStatusBy(suspended)`; nothing in SCIM reaches `Users.Delete` | design 04 §2.7a |
| No container runtime socket in a build | R-112 | Integration test asserting build container mounts | design 07 B |
| Inbound `X-Pando-*` headers are always stripped | R-053 | Unconditional strip in the proxy + forged-header test | design 06 §4, 07 C |
| Outbound `pando_*` cookies never reach an app | R-173 | Namespace strip in the proxy + forwarded-cookie test | design 06 §4 |
| Built-in roles are immutable | R-081 | DB trigger; new verbs added by migration only | design 02 §2.2 |
| An install-scoped grant carries install verbs and no app, and vice versa | R-080 | Composite FK `grants (role_id, role_scope) → roles (id, scope)` + two CHECKs | design 02 §2.2, 06 §2.1 |
| Spec revisions are append-only | R-152 | DB trigger rejecting `UPDATE`/`DELETE` | design 02 §2.3 |
| One install, one org — no tenant object | R-015 | Singleton constraint on `host_policy` | design 02 §2.5 |
| A failed app stays failed | R-151 | The reconciler has *no code path* touching `failed` | design 05 §1.1 |
| Volumes survive app deletion | R-204 | `ON DELETE RESTRICT` on `volumes.app_id` | design 02 §2.4 |

Three more that have no mechanism yet and therefore depend on you:

- **The two planes are never conflated** (R-029, R-070/071). `CheckData` contains exactly one
  cross-plane implication — ownership. If you find yourself adding a control-plane role check to
  `CheckData`, stop. This was reversed once already.
- **The two *scopes* are never conflated either** (R-080). `CheckControl` takes an app,
  `CheckInstall` does not, and each refuses the other's verbs with an internal error rather than
  evaluating it. The asymmetry is why: an install verb checked against an app denies, which is safe;
  an app verb checked install-wide looks for a grant that *can* exist. The one bridge is
  the `install.apps.*` verbs, one per app verb, which `CheckControl` alone reads as that app verb on
  every app (R-080, R-081, `authz.everyApp`) — that is how an administrator manages any app. It never reaches
  `CheckData`: managing an app is not using it (R-087).
- **The proxy is never routed around** (R-023). Routing adapters put traffic *in front of* Pando's
  proxy; they never point at a workload. An adapter author's instinct will be to point Traefik
  straight at the container. That is the bug.
- **Nothing is read from the repo at deploy time** (R-020). The spec is the sole record of how an app
  runs. If it isn't in the spec, it doesn't happen.

---

## 3. Layout

```
cmd/pando/              CLI and server entrypoint (cobra). Adapter registration happens here.
internal/
  core/                 Business logic. Adapters may not import any of this except adapter/api.
    authz/              Verb evaluation, both planes. NEVER importable by adapters.
    audit/              Append-only event log.
    assertion/          JWT minting, JWKS.
    spec/               AppSpec types, validation, classified diffing.
    planner/            spec + policy + adapters -> plan, or a plan-time error.
    reconciler/         The loop and the state machine.
    policy/             Host policy evaluation.
    state/              sqlc-generated queries + repository types.
  adapter/
    api/                The seven interfaces. Definitions only, no implementations.
    identity|routing|builder|runtime|secrets|services|notify/
  detect/               The auction, detectors, trial run.
  proxy/                The identity-aware reverse proxy. The single enforcement point.
  httpapi/              chi handlers. NO business logic — see §4.
  mcp/                  MCP server, a client of the same service layer as httpapi.
  console/              Embedded static assets from the Vite build (embed.FS).
  secret/               The redacting secret.Value type.
  id/, errs/, config/   Prefixed ULIDs, the error envelope, viper config.
migrations/             golang-migrate, embedded in the binary.
console/                React + TypeScript + Vite source. Built into internal/console.
test/acceptance/        The four end-to-end sequences from design 07.
docs/                   See §1.
.claude/skills/pando-design/   The design system. Tokens, 24 components, brand and voice rules.
```

`internal/console` (embedded build output) and `console/` (source) are different things. The
duplication is inherited from the design doc; do not "fix" it by merging them.

---

## 4. Conventions

**The API is the product** (R-261). Console, CLI, and MCP are all clients of it and none may have a
capability the API lacks. Enforcement: **handlers contain no business logic.** Everything lives in a
service layer under `internal/core`, and both `httpapi` and `mcp` call it. If a capability exists in
one surface and not the other, someone put logic in a handler.

**IDs** are prefixed, sortable, opaque: `app_01HQ8…`, `spec_…`, `usr_…`, `tok_…`, `vol_…`, `gr_…`.
ULID body. The prefix is load-bearing for readability — do not switch to bare UUIDs.

**Errors** crossing an API boundary use the envelope in design 00 §3.2: stable machine `Code`, a human
`Message`, an optional `Remedy`, `Details`, `RequestID`. Message text is held to the R-105 standard
wherever a user might act on it — self-contained, pasteable into an assistant, no undefined terms.
"Which port?" fails. "This app appears to be a Node.js service. Pando could not determine which port
it serves HTTP on. Valid answer: a port number such as 3000." passes.

**Logging** is zap, structured, one logger threaded through context. Every in-request line carries
`request_id`, `principal_id`, and where applicable `app_id`.

**Time** is UTC everywhere, `timestamptz` in Postgres, RFC 3339 on the wire. Use the `Clock`
interface in core so backoff is testable without sleeping.

**Context** carries request ID, principal, logger, deadline. Adapters get a context on every call and
must honor cancellation — the reconciler cancels work when a spec changes underneath it.

**Capabilities are data, never type assertions** (R-254). An adapter returns a capabilities struct.
A type assertion is invisible to the planner and cannot produce a readable plan-time error.

**Warnings are never blockers.** Blockers are `PLAN_*` errors. A warning that looks like an error
teaches people to ignore both.

**There is a design system, and it is not optional.** Any user-facing surface — the console, the
marketing site, docs, a mockup — is built from `.claude/skills/pando-design/`. Invoke the
`pando-design` skill before writing UI code or CSS. Colors, type, spacing, radius and motion all come
from its tokens; a raw hex value, a raw `px` value, or a font that is not Newsreader / Public Sans /
IBM Plex Mono is a mistake, and `_adherence.oxlintrc.json` there is configured to catch each one.
It runs under ESLint through `adherence.eslint.mjs` beside it; oxlint cannot run it.

Its voice rules and this document's error standard are the same standard. R-105 says an error must be
self-contained and pasteable into an assistant; the design system says *"Pando couldn't find a start
command. Add one in app settings."* — no apology, no `Error:` prefix, no exclamation mark. An API
message and a console message should read as one product, because to the user they are.

---

## 5. Definition of done

A change is done when:

1. **It has an acceptance test named for its requirement.** Convention from design 00 §4:
   ```go
   // TestR132_UnfilledRequiredSlotBlocksDeploy asserts R-132.
   func TestR132_UnfilledRequiredSlotBlocksDeploy(t *testing.T) { … }
   ```
   Run `make requirements-coverage` to see which R-IDs have tests. A requirement with no test is
   either philosophy, deferred, or a gap — and you should say which in your report.
2. `make check` passes: build, vet, golangci-lint (including the adapter import rule), unit tests.
3. Your phase file's *Done when* condition is met, or you state exactly which part is not.
4. Any `[P]` default you overrode is noted, with the reason, in the design doc — not only in the code.

Integration tests use `testcontainers-go` against real Postgres and real Docker. The four sequences
in design 07 are the integration-level acceptance criteria: **if those four pass end to end, v1
works.**

---

## 6. Where to start work

`docs/plan/` has one file per phase, in build order, each with tasks and a *Done when* condition.
Phases are sequenced so each produces something runnable and the riskiest work lands while it is
still cheap to change.

**Nothing is blocking. Phase 0 can start.** O-11 — how Postgres is supplied — resolved to the
install topology supplying it: a Compose file with a `pando` service and a `postgres` service that
start together, plus an external-database override in configuration. Pando does not start Postgres,
so phase 0 needs nothing from phase 3. See `docs/design/00-stack-and-conventions.md` §1.1.

Do not skip ahead to a later phase without saying so in your report.

---

## 7. Working agreements

- **Do not invent requirements.** If the docs do not answer your question, the answer is a question
  for a human, not a decision you make quietly. Add it to `docs/plan/open-decisions.md` and flag it.
- **Do not weaken a mechanism in §2 to make a test pass.** Fix the code.
- **Prefer the boring implementation.** This system's value is in what it refuses to do (R-010: not a
  scheduler; R-011: does not test; R-012: not a marketplace). Scope creep here is a product failure,
  not just an engineering one.
- **Cite requirement IDs** in commit messages and comments where a non-obvious choice traces to one.
  `R-151` in a comment explains an absent code path better than three sentences will.
- **The reference is generated; keep it that way.** `docs/api.md`, `docs/cli.md` and `docs/mcp.md`
  are written by `make reference` from the router, the cobra tree and the MCP tool list, and the
  console's **API and tools** screen renders the same document live from
  `GET /api/v1/reference`. Adding an endpoint means adding a row to `routeDocs` in
  `internal/httpapi/reference.go` — `TestR261_EveryRouteIsDocumented` fails the build otherwise, in
  both directions. Adding an error code means adding its meaning to `meanings` in
  `internal/errs/catalog.go`. Then run `make reference` and commit the result; `make check` and CI
  both fail on a stale one. Never hand-edit those three files, and never describe a surface in prose
  somewhere else when it could be generated from the thing itself.
