# Phase 6 — Detection

**Goal:** a non-technical user points Pando at a repo and gets a working proposal. This is where R-005
(the deployer may not know what a port is) is honored or lost.

**Prerequisites:** phase 5.

**Design:** `../design/07-sequences.md` A; `../design/03-adapter-interfaces.md` §3;
`../design/01-spec-schema.md` §2.5; `../design/04-api.md` §2.2.

## Tasks

- [x] Read-only `SourceView` — no write methods, structurally (R-020)
- [x] Source allowlist check **before clone** (R-092); a blocked source produces zero disk writes — checked again on re-detection, since the allowlist can change after an app is created
- [x] Registry check tier (R-094) — ghcr.io only by default; Docker Hub namespaces do not correspond to source owners ([note](../design/notes-registry-tier-namespaces.md))
- [x] Maintainer's-own-build-commands tier (R-094 tier 3) — a Makefile `build` target drives the plan
      instead of convention-matching, via Railpack's own `RAILPACK_BUILD_CMD`/`RAILPACK_START_CMD`/`RAILPACK_PACKAGES`
      ([note](../design/notes-the-maintainers-own-build-commands.md))
- [x] The rest of the ladder: `.github/workflows`, `Taskfile.yml`, `justfile`, `Procfile`, and the
      embed/output pairing for a repository that names no commands at all
      ([note](../design/notes-declared-builds-without-a-runner.md)). Workflows are read only when one
      job is unambiguously the build — a matrix, a composite action or a `$VERSION` is declined — and
      only Vite's `outDir` is read for a client's output
- [x] The detector auction: every builder adapter `Bid()`, ranked (R-093)
- [x] Confidence ladder; `runners_up` returned so the user can see the auction, not just a verdict
- [x] Compose import, with rejected constructs raising `PLAN_COMPOSE_CONSTRUCT_REJECTED` (R-099) and
      rewritten ones raising `WARN_COMPOSE_CONSTRUCT_REWRITTEN`
- [x] Trial run in throwaway isolation (R-097): observe bound ports, observe writes outside declared
      volumes, capture crash logs — on the runtime, not the builder ([note](../design/notes-trial-run-placement.md))
- [x] Slot promotion from the trial run — the O-4 fallback (design 01 §2.5), promoting only slots the app itself named
- [x] Question generation held to R-105, enforced by a validator on every question rather than by review
- [x] Warnings: `WARN_NO_PERSISTENT_VOLUME` with the observed directory (R-201, R-202),
      `WARN_PATH_ROUTING_INCOMPATIBLE` (R-168). R-201's base warning fires whether or not the trial
      ran — it used to be attached inside the trial's branch, so every source build got neither it
      nor the improvement ([note](../design/notes-deferring-to-a-trial-that-cannot-run.md))
- [x] Detection endpoints: get, rerun, diff, answers, accept

## Requirements in scope

R-020, R-021, R-092–R-099, R-101–R-107, R-131, R-132, R-168, R-201, R-202.

## Done when

**Sequence A passes against a set of real public repos** — a Dockerfile app, a compose stack, a static
site, a Node app with no deployment artifacts, and a monorepo.

**Done.** `test/acceptance/sequence_a_detection_test.go` drives all five over HTTP against the shipped
Compose stack, plus the R-092 assertion. The detection corpus (`make detection-corpus`) covers ten
repositories and reports **0.90 questions per deploy** against a budget of 1.00, worst case 1, with a
strategy found in 100% of the cases where one existed.

Three detector defects and two corpus defects came out of the corpus's first run against real
repositories, and three more out of running the trial run against a real daemon. All are written up:
[detection corpus findings](../design/notes-detection-corpus-findings.md),
[trial run placement](../design/notes-trial-run-placement.md).

Still open for a decision: [R-099's override clause](../design/notes-compose-override-conflict.md)
contradicts R-272 and cites the wrong requirement, and [R-094 names two registries](../design/notes-registry-tier-namespaces.md)
as though they were the same kind of evidence.

## Traps

- **Build the corpus of ~30 real repos early**, not at the end. Detection quality below the R-103 bar
  is the top risk in the register, and "questions per deploy" is a tracked metric, not a vibe.
- **R-105 is a hard content requirement, not a style note.** A question must be answerable by a model
  that cannot see the repo, because the intended workflow is pasting it into the assistant that wrote
  the app. "Which port?" fails review. Enforce this on every generated question.
- A blocked source must produce **zero disk writes** — `git clone` is never invoked. Test it.
- Ports discovered by observation carry `Source: "observed"`, not `"framework"`. The distinction is
  what the review UI shows the user.
- **Accepting a proposal does not deploy.** It pins revision 1 and writes two grants.
- Pando still does not act on suspected undeclared dependencies (R-021, R-107) —
  `WARN_UNDECLARED_DEPENDENCY_SUSPECTED` is informational only.
- **O-4 (required vs optional slots) is unresolved.** The trial-run promotion fallback is the [P]
  answer; measure its false-block rate against the corpus rather than assuming it works.
