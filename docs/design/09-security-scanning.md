# 09 — Security scanning

Requirements §23 (R-310 – R-320). This document is how the score is produced, stored, enforced and
undone.

---

## 1. The shape of it

A scan is a **fact about one spec revision and the image built from it**, produced by an adapter,
stored append-only beside the app, and read by three places: the planner, the reconciler, and the
console.

```
deploy ──▶ build ──▶ scan ──▶ score ──▶ plan check ──▶ run
                       │                    │
on demand ─────────────┘                    └── refuse: PLAN_SECURITY_BELOW_THRESHOLD
policy change ──────────────────────────▶ re-evaluate the stored score, never rescan
```

**[D] A policy change re-evaluates; it never rescans.** Lowering the threshold must not queue a scan
of every app on the host, and the score of a revision does not change because the rule changed.

**[D] The scan does not gate the build.** It runs after the image exists, because the image is what
it has to look at, and because a build that succeeded is worth keeping even when its result is not
deployable.

---

## 2. The adapter (R-317)

A seventh adapter category, in `adapter/api`:

```go
type Scanner interface {
    Adapter
    Scan(ctx context.Context, req ScanRequest) (ScanResult, error)
    ScannerCapabilities() ScannerCapabilities
}

type ScanRequest struct {
    AppID    string
    SpecID   string
    Image    string // what was built, when there is one
    SourceDir string // the checkout, for scanners that read the tree
}

type ScanResult struct {
    Findings []Finding
    Scanner  string // what produced it, and at what version
    Ran      time.Time
}

type Finding struct {
    ID       string   // CVE-2024-…, or the rule that fired
    Severity Severity // critical | high | medium | low | unknown
    Title    string
    Target   string   // the package, file or layer it is in
    Fix      string   // the version that fixes it, when the scanner knows
}
```

**[D] Severity is Pando's vocabulary, not the scanner's.** Every scanner has its own scale and its
own extra levels; the adapter maps into these five and the score arithmetic never sees a string it
has not defined. `unknown` scores as `low` and is shown as unknown — inventing a severity for a
finding whose severity nobody has decided is how a score stops meaning anything.

**[P] The first adapter is Trivy**, in a container, with no daemon and no socket: it reads the image
from the local runtime and the source from a directory bind. It reports vulnerable dependencies,
leaked secrets and misconfiguration. Static analysis of the app's own code is **O-19** — a different
class of tool, per-language and noisy, and a score that swings on lint opinions is a score people
learn to ignore.

**[D] The scanner is not given the app's secrets, its network, or its volumes.** It sees an image and
a checkout. A component that scans untrusted code is the last component that should hold anything.

---

## 3. The score (R-313)

```
score = max(0, 100 − 25·critical − 10·high − 3·medium − 1·(low + unknown))
```

Deliberately not an average, a ratio, or a curve: one critical finding costs more than fifty low
ones, and the number a threshold is set against does not move because an app grew.

**[D] A scan taken before there was a revision still belongs to the app.** Detection scans the
checkout before a spec exists, so that scan has no revision — and accepting the proposal pins one. A
lookup that insisted on the pinned revision answered "this app has not been scanned yet" a minute
after scanning the very source that revision was written from. So the rule is: the newest scan of
this revision, and failing that the newest scan of no revision, and never another revision's. The
console says which of the two it is showing.

**[D] Detection builds what the app would deploy, and the plan shows that image's score (R-311).**
The plan is where somebody decides whether to run an app, and the score is part of that decision.
So for an app Pando builds, detection builds each image the app would deploy — the single app, or
every compose service with a `build:` — through the builder adapter, exactly as a deploy builds it:
the same Dockerfile or plan, build egress (R-118), no runtime socket in the build (R-112). It scans
that image and the source, and the plan's score is the union of the two scans' findings, a finding
both report counted once (by ID and target, as a scanner given both at once reports it), recorded as
one scan of scope `image_and_source`, keyed by the commit and the image's digest. For gabrielecirulli/
2048 that is 61 / 100, not the source's 100. The first deploy runs that image and reuses that scan
(§4.1), so the deploy's score is the plan's. A build that fails fails the detection with the build
log, which is the answer (R-107).

Nothing waits on what it does not need. After the auction, the build and the source scan run at
once; once the image exists, the image scan and the trial run (R-097, which now starts the built
image) run at once; then the proposal is assembled. A running detection reports its `stage` and
every step under way (`running`), so clients show both. A compose app's services build concurrently,
at most host policy's `max_concurrent_deploys` at a time. A build failure cancels the scan beside
it. A scan failure does not cancel the build: a scan never fails a detection (R-318), and the
plan then shows the scan as failed.

**[P] A detection that builds gets the time a deploy's build gets** (issue #90): ten minutes to read
the app — clone, auction and trial run — plus the default build timeout, 30 minutes, so 40 in all,
set by `work.detection_timeout`. Before detection built, ten minutes was the whole of it; kept after,
it failed builds a deploy would have finished, four in ten of them on a six-CPU host with a dozen
apps added at once. The outcome is written under a context the deadline does not cancel, so a
detection that reaches its result as its time runs out records it rather than "could not record".

**[D] A scan of the source alone, taken before there was a revision, is not the app's score
(R-311).** Detection scans the source alone when it cannot build yet: an open question changes what
would be built (a required build argument with no default — build arguments are never secrets,
design 01), or the installation has no builder. That scan read no image, and its number is the
source's. Showing it as the app's score made a plan read 100 / 100 for an app whose first deploy,
scanning the image it built, scored 61. So such a scan keeps its findings and its own number
(`scan.score`), and the app has no score until a scan reads its image: the report's
`standing.score` is empty and `awaiting_image` says why, an app's row in `GET /apps` carries
`security_awaiting_image` and no `security_score`, and the plan, the overview during the first
deploy and the apps list say "Scored once the image is built." beside the source's findings.
Answering the question builds and scans the image (the detection reads as `running`, at the
`building` stage, until it has), and the score appears; accepting is refused until then, so the
score accepted is the score of what runs. The report's `scope` (`source`, `image`, `image_and_source`) says what any scan
read. A threshold placed against such an app finds no score, as it would for an app never scanned. A
scan of the source alone that belongs to a revision — a compose app whose primary service builds no
image (§4.1), or a scan from before the image column existed — is still that revision's score, and
says what it read.

**[D] A score belongs to a revision, not to an app.** `app_scans` is append-only, keyed by
`(app_id, spec_id, scanner_ref, ran_at)`, and the app's current score is the newest scan of its
pinned revision. A rollback therefore restores the score of what it rolled back to, with no rescan
and no lag.

**[D] No score is not a score of zero.** Unscanned, unscannable and "scanned and found nothing" are
three different states and the model keeps them apart (R-318).

**[P] A scan stores two numbers** (R-313a): every finding, and only the findings with a fix. Which of
them an installation means is `ignore_unfixable_findings` in host policy, and policy changes without
rescanning — so deriving the second on read would mean re-deriving it for every row of every list.
Both are written when the findings are in hand. A scan from before the second column existed derives
it from its own stored findings, which is exact.

**[D] The filter drives the list as well as the number.** An installation that ignores unfixable
findings does not see them, and the console says why the list is shorter than the scanner's output.

**[D] Findings are stored and served worst first** (R-313b), by severity and then by ID, so the same
scan orders the same way twice.

---

## 4. Enforcement

### 4.1 At deploy (R-314)

**[D] Scanned once per source and image, not once per deploy (R-311, R-312).** A scan records what it
read. The source: the commit for a repository, the digest for a published image, and for an upload the
SHA-256 of the archive (`sha256:<hex>`, never written into the spec as a commit — R-120). And the image
(`app_scans.image`): what the deploy built or pulled, by digest — an image ID (`sha256:…`) for a build
loaded into the runtime, `repository@sha256:…` for one pushed to the install's registry, for an image
app's pinned digest, and for a repository's published image (R-094 tier 1, `prebuilt`) — or nothing,
for a scan of the source alone. A repository's published image is named in the spec by tag, so the
deploy pins the tag to the digest it names at that moment, scans that digest with the checkout, and
runs that digest: the image scanned is the image that runs, and a tag that has moved since is a new
image and is scanned. A registry that cannot be read leaves the image unscanned and says so in the
deploy log, as a scanner that cannot run does (R-318). Detection scans the source it reads, and the
published image the plan runs when there is one — a repository's (`prebuilt`) pinned the way a deploy
pins it, or an image app's pinned digest — so a deploy that resolves the same digest reuses detection's
scan and the plan's score is the deployed app's. For an app Pando builds, detection scans the image it
built (§3).

**[D] Built once.** The first deploy after detection runs the image detection built, and reuses its
scan, rather than building again: "Using the image built when Pando read this app (…); nothing it is
built from has changed" in the deploy log. It does so only when the revision builds the same thing
from the same source — the same commit or upload, and the same build inputs (`spec.BuildInputs`: the
strategy, Dockerfile, context, target, build arguments, generated plan, builder, runtime, isolation and
egress, each service's build, and for a buildpack the start command). An answer, an edit or a
re-accept that changed any of those builds once, at the deploy, and scans what it built. Ports,
variables, routing and limits change how the image runs, not what it is, and cost no rebuild. A
compose app is reused whole or not at all. So nothing runs that was not scanned, and the score
accepted is the score of what runs. An image detection built for an app that is deleted, accepted or
not, goes with the app's bundle at teardown (R-224): the proposal names the runtime and builder that
hold it. A person can ask for a scan (`POST /security/scan`), and a deploy scans the source and the image
together — unless a successful scan **by the scanner configured now** already read that same source and
that same image, in which case it uses that one and says so in the deploy log ("Using the security scan
of 3f9a2c1 from … (source and image unchanged)", or "of the uploaded source" for an upload). The
threshold below is checked either way, and a refusal carries the reused scan's worst findings.

What is not reused: detection's scan of the source alone for a deploy that built an image (R-311 — the
image's OS packages are what it did not read; reusing it once showed an app on a base image with two
critical findings as 100 / 100), a scan of another image built from the same commit (a rebuild that
produced a different image is scanned), an image named by a tag rather than a digest (the name says
nothing about the content), a failed scan (R-318 — the next deploy tries again), another scanner's scan
(its findings are not the ones this installation's scanner reports), and a scan of another source. A
scan somebody asks for always runs. A compose app has no single image; its scan reads the image its
primary service builds — what the app's address serves, and what detection scanned — with the
source. One whose primary service builds nothing is scanned as its source alone, and a scan of that
source alone matches it.

**A reused scan is attached to the revision being deployed.** A revision's score is the newest scan of
that revision (§3), and a new revision of an unchanged source and image — a variable edited, or every
`pando deploy .` of the same directory — would otherwise have none: refused as never scanned where a
threshold is set. The reuse is recorded as a new `app_scans` row for the new revision with the
original's findings, score and `ran_at`, and `reused_from` naming the scan that ran. The table stays
append-only; the scan history lists scans that ran and leaves reuses out, so one scan is shown once.

**[O] How old a reused scan may be** (O-20). Nothing bounds it today: an app redeployed from an
unchanged source for a month carries a month-old score, as an app not redeployed at all does. New CVEs
against an unchanged source are the job of the on-demand scan and a scheduled rescan, not of redeploys —
a score that refreshed with every click on deploy would describe the clicking. Whether a maximum age
belongs in host policy, and whether a scanner database update invalidates reuse, are part of O-20.

In the planner, beside the other plan-time refusals, so it fails before anything is created:

- Threshold set, scanner configured, newest scan of the revision being deployed is **below** it →
  `PLAN_SECURITY_BELOW_THRESHOLD`, with the score, the threshold and the three findings that cost
  the most in `details`, and a remedy naming both ways forward: fix the findings, or ask whoever
  holds `install.policy.manage`.
- Threshold set, scanner configured, **no scan at all** for that revision → the same refusal with a
  different message: it has never been scanned, and here is how to scan it.
- Threshold set, **no scanner configured** → nothing is refused (R-317). The policy screen says the
  threshold is inert, because a rule that silently blocks everything is worse than a rule that is
  visibly off.
- Scan failed → the previous score stands (R-318).

### 4.2 While running (R-315, R-316)

The reconciler's own pass, not the deploy path, because the trigger is usually neither: a policy
change, or a rescan that found something new in an app nobody has touched for a month.

```
insecure_since = the first time this app was found below the threshold
```

- Found below → set `insecure_since` if unset, warn, notify the owner, audit.
- Found at or above → clear `insecure_since`, audit the recovery.
- `insecure_since` older than the grace, and policy says stop → `desired_state = stopped`, audited
  with the score, the threshold and the grace it was given.

**[D] Stopping is `desired_state = stopped`.** Reversible, survives a restart, and visible in the
console as a stopped app with a reason. Never a delete, never an edit to the app's configuration,
and never `failed` — `failed` is terminal and means the reconciler gave up (R-151), which is not
what happened here.

**[D] The grace is stated in the policy and shown to the owner at the moment it starts.** A deadline
nobody was told about is an outage with extra steps.

---

## 5. Host policy

Three fields, defaulting to off, following R-272's pattern — a permissive default that policy may
raise:

| Field | Default | Meaning |
|---|---|---|
| `min_security_score` | `0` | Below this, a deploy is refused. `0` is off. |
| `insecure_action` | `warn` | `warn` or `stop`. |
| `insecure_grace_hours` | `24` | How long an app has after it is first found insecure, before `stop` applies. |
| `ignore_unfixable_findings` | `false` | Leave findings with no published fix out of the score and out of the list. |

**O-20** is whether a score ages: nothing rescans an app that has not been deployed since, so a
clean score can describe three-week-old vulnerability data. A scheduled rescan is the obvious answer
and needs a decision about what it costs on a small host.

---

## 6. Surfaces

**Console.** The score lives on the app's **overview**, beside the card carrying its status, address
and last deploy — in the column a deploy log used to occupy. The score is a property of what is
running, and the person who has to act on it is the one reading that page; a log is a history and
belongs with the other histories on the Logs tab. The section carries the mark, the counts, when it was taken, every finding, and **Scan now**.
It is not in settings: settings is where an app is configured, and a score is not a setting.

**[D] The section says what the scan read** (R-311), from the scan's `spec_id`, `commit` and `image`:
"from the source, before this configuration" for detection's scan of the source alone (with no score:
§3), "of the image it will run and its source" or "of the image it will run" for detection's scan of a
published image or of the image it built, "of the source only, not the image it runs" for a revision scored by a scan that read no image, "of the running image only, not its source"
for **Scan now** on a deployed app, "of the image it runs" for an image app (its source is the image's
own digest), and "of the image it runs and its source" for a deploy's scan of a built or published
image with its repository. A number that covered less than both does not stand for both.

The **apps list** carries the same mark, at `score / 100`, beside status — which is the question a
list is for: which of these needs me. An app whose only scan read its source before anything was
built says "Not scored yet" (§3).

**[D] The score is a shield and a number, together** (R-320). The shield carries the color and the
number carries the fact; neither appears without the other. Color is the second signal, never the
only one:
"F" tells a deployer nothing they can act on, and a red pill tells somebody who cannot see red
nothing at all. The color says what the number means *here* — against the installation's threshold
where one is set, and against bands where none is, because a score of 20 is worth noticing on an
installation that enforces nothing.

Host policy's screen carries the three fields, with the consequence written at the point of setting
them.

**API.** `GET /apps/{id}/security` for the current score and findings, `POST /apps/{id}/security/scan`
to take a new one. Both behind `app.view` and `app.deploy` respectively — asking for a scan changes
what a deploy will do, so it is not a read.

**[P] A scan in progress is reported, whoever started it** (issue #68). A scan can take a minute,
and a deploy, detection, the CLI, MCP or the console can start one. The report carries
`scanning_since` while a scan of the app runs, and each row of `GET /apps` carries
`security_scanning`, so every client shows the same state rather than only the one that asked
(R-261). The console shows it as the design system's *building* status, the hollow ring, in the
Security section and the list's Security column, and polls while it lasts. Onboarding already has
its own steps for the build and the scan (the `building` and `scanning` detection stages), and shows
both under way while both are in `running`.

The state is held in memory by the security service, not in `app_scans`. That table is append-only
and records what a scan found (R-319), and an unfinished scan has found nothing yet. A scan also
ends when the process running it ends, so a stored "running" row would be left behind by a restart.
An in-memory marker can't be left behind. It depends on Pando running as a single server process,
which it does. Two scans of one app at once, such as a deploy's and a **Scan now**, count as one
running scan until the last of them ends.

**CLI and MCP.** They follow from the API without further design (R-261); the MCP tool is a read,
because an agent asking Pando to rescan until it passes is a loop nobody wants.

---

## 7. What the first implementation does, and does not

**Three moments, and each scans what exists at it.**

- **Detection**, while the checkout is still on disk. An app that has been added
  and not yet deployed has no image, and waiting for one means the first thing
  anybody sees about a new app is "not scanned yet" — when a committed key or a
  vulnerable lockfile is exactly what they want to know before deciding to
  deploy it. The scan has no spec revision, because there is no revision yet.
  Never fatal: the proposal is what that run is producing. An app that runs a
  published image has that image already, and detection scans it by digest
  with the checkout, so the plan carries the score of what will run. An app
  Pando builds is built at detection, and its image is scanned with the
  checkout, so its plan carries that score too (§3). Only when the build has
  to wait — a required build argument nobody has given, or no builder — is
  the source scanned alone, and the plan shows that scan's findings and
  "Scored once the image is built" until the build can run.
- **Deploy**, after the build: the image and the checkout together, against the
  revision being deployed — or, for the first deploy of what detection built,
  detection's scan of that same image and checkout, reused (§4.1).
- **On demand**, against whatever the app has. A deployed app's newest
  successful image, and for one that has never been built, a fresh checkout —
  fetched for the scan and dropped after it, because a copy of somebody's code
  held between scans is a copy Pando is responsible for, and a clone on a button
  press is a cost the person pressing it chose. Host policy's source allowlist
  is checked again before the clone (R-092), because it can change between
  creation and now.

**The scanner is seeded on and enforces nothing.** A fresh installation gets
Trivy as its default scanner, so every deploy produces a score — and
`min_security_score` defaults to 0, so nothing is refused until an administrator
sets one (R-270). A score nobody has is a score nobody acts on, and a threshold
nobody chose is an installation that cannot deploy.

**The scanner's handoff is a file, not a socket.** Pando saves the image with
the runtime it already has a socket for, and copies the tar into the scanner's
own filesystem. Two things were learned building it:

- A source file's mode travels through a tar. A checkout copied in at 0600
  owned by a host uid was unreadable inside the scanner, which reported a clean
  tree — a pass nobody earned, and the worst failure this component has. Modes
  and ownership are normalized on the way in.
- The category had to reach the database. `adapter_configs.category` carries a
  CHECK enumerating the categories, and seeding the default scanner against a
  schema that had never heard of `scanner` failed at startup, on every restart.
  Migration 000016 is that fix; the lesson is in 000010's comment already and is
  worth repeating: a category that exists only in Go is a category no adapter
  can be configured in.

**Not built yet.** A scheduled rescan (O-20), static analysis of the app's own
code (O-19), and the plan-time half of R-314 — `POST /apps/{id}/plan` does not
yet report the standing, so a below-threshold app is refused when its deploy
reaches the scan rather than at `:plan`. The refusal is the same error with the
same detail either way; what is missing is the earlier warning.
