# What the detection corpus found on its first run

The corpus (`test/corpus`, `make detection-corpus`) was built before detection
was finished, because the risk register says detection quality is the top risk
and the mitigation is measurement rather than review. Its first run against
real repositories failed 2 of 10 cases. Diagnosing them turned up **three**
detector defects and **two** defects in the corpus itself.

Recording it because the defects share a shape, and the shape is worth
remembering: *every one of them was a detector treating a file's existence as
evidence without regard to where the file sat.* All five unit-test suites passed
throughout. Synthetic fixtures contain the files the test author thought to put
in them, and real repositories contain everything else.

## The three detector defects

### 1. A Dockerfile under `examples/` is not how the repository builds

`vercel/turbo` carries five Dockerfiles. Not one of them builds turbo: two are
sample projects under `examples/with-docker`, two are lockfile test fixtures,
one is a devcontainer.

`DockerfileDetector` bid 0.45 for the `dockerfile` strategy and offered a choice
between all five — five wrong answers presented as the way forward, to a person
R-005 says may not know what a Dockerfile is. **Saying nothing is better than
offering a menu of wrong answers.**

Fixed by `notDeployable` in `internal/detect/detectors.go`: paths under
`examples/`, `fixtures/`, `testdata/`, `docs/`, `.devcontainer/` and friends are
filtered out, and a detector with nothing left bids zero.

### 2. A workspace declaration is the repository saying it is not one app

With defect 1 fixed, turbo fell through to `BuildpackDetector`, which found the
root `package.json` and bid "this looks like a Node.js project, what is the
start command?" Honest-sounding, and wrong: turbo is a Rust CLI, a docs site and
twenty packages.

The missing signal was that `pnpm-workspace.yaml` exists. A workspace manifest
is the repository declaring, in as many words, that it holds several projects —
which is exactly the R-021 situation where Pando must not invent topology.

Fixed by `MonorepoDetector`, which bids `StrategyUnknown` at 0.8. Being
confident that a repository is *not* a single deployable app is a real detection
result, and it needed no new machinery in the auction: the ranking already
handles it. It is a veto rather than a bid, so it stands down when a root
Dockerfile or compose file answers the question it would otherwise ask.

Issue #90 refined what counts as a declaration (`internal/detect/workspace.go`).
A `pnpm-workspace.yaml` that lists no package beyond the root holds pnpm settings,
not a workspace. The veto also stands down when the repository already says what
to deploy: a JavaScript workspace whose root `package.json` has a `start` script, a
`go.work` with one program, or a Cargo workspace with one binary. Each of these is
built from the root. When the question is asked, it lists the members the
manifests name. Answering it re-runs detection inside the chosen project
(`Proposal.Subproject`). A repository with no app at its root and no workspace
manifest offers the projects one level down. If there is only one, detection
reads it without asking.

### 3. Where an `index.html` sits decides what a `package.json` beside it means

`StaticDetector` dropped its bid from 0.7 to 0.35 whenever a `package.json`
existed, reasoning that a build step probably existed and serving the source
directory would ship an unbuilt site.

That reasoning is right for one case and wrong for the other, and the detector
could not tell them apart:

- **`index.html` at the root** — the file is source. A Vite app's root
  `index.html` references `/src/main.jsx`, which no browser can run unbuilt.
  Serving it produces a blank page rather than an error, which is the worst kind
  of failure. Bid low and let buildpack win.
- **`index.html` in `dist/` or `public/`** — the file is build output that is
  committed and already sitting there. That is *stronger* evidence than no
  `package.json` at all, not weaker.

`h5bp/html5-boilerplate` is the second case. The blanket penalty had it losing
to a buildpack that would have tried to `npm start` a folder of HTML.

The fix splits the two, and sharpens the first: a *declared build script* is
stronger evidence than a bare `package.json`, so the root case bids 0.2 rather
than 0.35 when `scripts.build` is present. At 0.35 it landed within the
auction's tie margin of buildpack's 0.45 and triggered a spurious "which of
these two?" question.

The committed-output case asks one question instead — whether to publish the
committed copy or run the build first — because a stale committed `dist/`
deploys successfully and looks fine while being several commits behind.

## The two corpus defects

Worth recording separately, because a corpus that is wrong is worse than no
corpus: it makes a detector's failure look like a passing test.

### `static-with-build-step` pointed at a repository that was not that shape

The case aimed at "a build step whose output is static — the interesting failure
is detecting `static` and skipping the build," and pointed at `vitejs/vite`.
But `vitejs/vite` is the Vite project's own monorepo, not a site built with
Vite. It passed only because the root `package.json` matched buildpack — the
right answer for the wrong reason, which is why fixing defect 2 turned it red.

Repointed at `vitejs/vite` subdir `packages/create-vite/template-react`, which
genuinely is the shape: `index.html` at the root, `package.json` declaring
`vite build`, no committed output.

### The detection-rate metric counted correct "unknown" answers as failures

`must_detect_strategy: 0.9` was measured over every case — but two cases in this
corpus are *supposed* to come back unknown, the monorepo and the near-empty
repository. As written, the metric went **up** when detection got worse at
recognizing that it did not know, and it was only green before because the
monorepo was being wrongly detected as `dockerfile`.

Now measured over the cases where a strategy was the right answer.

## Questions per deploy, and R-097

The mean sat at 1.60 against a budget of 1.00. The budget was not the thing that
was wrong.

Four cases asked two questions each: a start command and a port. But R-097 is
the trial run — Pando starts the app and watches what it binds — and it exists
precisely so that nobody is asked for a port. Counting a question the system is
designed to answer itself against the user's budget charges a cost nobody pays.

`Question.Deferred` marks these. `detect.Asked` filters them out, detection
status is computed from what remains, and the corpus prints deferred counts
beside each case so that moving a question out of the budget stays visible
rather than becoming a way to hide it. A deferred question is not discarded: the
trial run can fail to observe an answer, at which point it becomes a real
question and still has to meet R-105.

With that accounting, and the three detector fixes:

```
questions per deploy: mean 0.90 (budget 1.00), worst 1 (budget 3)
strategy detected:    100% of 8 cases where one was expected (budget 90%)
```

Per-case budgets have since been ratcheted down to what detection actually
asks, so a new question is a deliberate decision rather than slack absorbed by a
loose ceiling.

**This accounting was half right, and the half that was wrong hid four cases.**
A deferred question is only free when the trial run can answer it, and the trial
run needs an image to start — which a source build does not have until it has
been built. So for `dockerfile`, `buildpack` and `static` the trial never runs,
`undefer` promotes the question back, and the person pays for it after all. The
count above was taken before that step, so it recorded 0.90 while a plain Go
module put two questions on the screen. See
[deferring to a trial that cannot run](notes-deferring-to-a-trial-that-cannot-run.md).

## What to keep doing

Each of the three detector defects is now also a unit test in
`internal/detect/detect_test.go`, so a regression fails on a laptop rather than
only in CI, which needs the network. But none of the three would have been
*written* without the corpus run. Grow the corpus toward 30 before trusting the
numbers — 10 repositories found three defects, and there is no reason to think
it has found the last of them.
