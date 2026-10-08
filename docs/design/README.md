# Pando — Design Documents

Internal. Companion to `../requirements.md`, which is the authority on *what* Pando is. These documents cover *how* it is built.

For build order and what to pick up next, see [`../plan/`](../plan/). For which requirements are
designed, planned, and proven, see [`../traceability/`](../traceability/) — generated, not hand-kept.

**Requirements churn slowly; design churns every sprint.** Keep them separate. When design contradicts a requirement, the requirement wins or the requirement gets amended — never a silent divergence.

Tags carry the same meaning throughout: **[D]** decided, **[P]** proposed, **[O]** open.

| # | Document | Covers |
|---|---|---|
| 00 | `00-stack-and-conventions.md` | Go/chi/zap/Postgres, repo layout, error taxonomy, logging, acceptance-test convention |
| 01 | `01-spec-schema.md` | The `AppSpec` — the central object. Read this first. |
| 02 | `02-data-model.md` | Postgres schema, constraints that enforce requirements |
| 03 | `03-adapter-interfaces.md` | The adapter categories as Go interfaces |
| 04 | `04-api.md` | REST surface, MCP tool mapping, CLI shape |
| 05 | `05-reconciler-and-lifecycle.md` | State machine, the loop, drift classification, deployment pipeline |
| 06 | `06-authorization-and-proxy.md` | Evaluation order, the proxy request path, assertion minting |
| 07 | `07-sequences.md` | Four end-to-end flows = integration acceptance criteria |
| 08 | `08-console-and-plan.md` | Console architecture, build order, risk register |
| 09 | `09-security-scanning.md` | The security score: the scanner adapter, the arithmetic, and the two places policy enforces it |
| 10 | `10-ai-assistance.md` | The AI category, and screening a deployment plan (R-106, §7.4) |
| 11 | `11-events-and-subscriptions.md` | The event catalog and outbox, subscriptions, signed webhooks, notification channels and preferences (issue #50) |
| 12 | `12-audit-export.md` | The commit-ordered audit cursor, audit sinks (syslog, HTTPS), OCSF, on-demand export, and audit coverage (issue #129) |

## Reading order

**New engineer:** requirements §1–3 (what it is, non-goals, invariants) → 01 → 07 → then whatever they're building.

**Anyone touching authorization or the proxy:** 06 in full, then Sequence C in 07. Do not skip.

**Anyone writing an adapter:** 03, then the relevant section of 01. An AI adapter: 10, then 03 §1.

**Anyone touching detection:** 07 Sequence A, then 10 — screening runs at the end of it.

## Requirements needing structural enforcement

These are requirements a code review will eventually fail to catch. Each has a mechanism, and the mechanism is the point.

| Requirement | Mechanism | Where |
|---|---|---|
| R-027 — adapters cannot touch authz/audit/state | CI import-lint rule | 03 §9 |
| R-027 — audit log is not rewritable | DB grant: no UPDATE/DELETE | 02 §2.6, 06 §6 |
| R-194 — secrets never logged | `secret.Value` refuses to render | 00 §3.3 |
| R-112 — no runtime socket in builds | Integration test on container mounts | 07 B |
| R-053 — assertion headers not spoofable | Unconditional inbound strip + test | 06 §4, 07 C |
| R-081 — built-in roles immutable | DB trigger | 02 §2.2 |
| R-152 — spec revisions append-only | DB trigger | 02 §2.3 |
| R-015 — one install, one org | Singleton constraint on `host_policy` | 02 §2.5 |
| R-151 — failed apps stay failed | Absence of a code path, not a flag | 05 §1.1 |
| R-204 — volumes survive app deletion | `ON DELETE RESTRICT` | 02 §2.4 |

## Blocking

**Nothing.** O-11 — how Postgres is supplied — is resolved (§00 1.1): Pando ships a Compose file
defining a `pando` service and a `postgres` service that start together, with an external database
supported by configuration. Phase 0 can begin.

The earlier framing of O-11 said to decide it *before writing migrations*, which was wrong in a way
worth recording: the migration SQL is identical under every option, because Postgres is Postgres.
What actually depended on the answer was whether Pando would hold administrative rights on its
database — and therefore whether the audit grant in §02 2.6 was achievable — and whether Pando would
need a runtime adapter in order to reach its own state store.
