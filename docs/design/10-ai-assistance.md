# 10 — AI Assistance

R-106 has been in the requirements since draft 1 and has never had a design. It says AI assistance is
optional supporting functionality, names three places it may be applied, and sets the constraint that
matters: it emits the same spec object and passes the same review gate, and with nothing configured
each tap degrades to a question rather than a dead end.

This document designs the tenth adapter category (R-258) and **screening a deployment plan** (§7.4,
R-330 through R-339), which is two functions: **repairing a detection that failed** and **answering
the questions detection could not**. Screening runs only in those two cases (R-336, §4.2). The
remaining uses R-106 names — reading README prose, and repairing a failed build at deploy time — are
not built here. The capabilities struct is shaped so they arrive without changing the interface.

Each function is assigned to one adapter at a time, optionally on a model of its own (R-259, §9).
Four further functions serve administrators rather than detection — drafting access and policy,
searching the audit log, and answering from the reference (R-343 – R-346, §10). They share the
adapter category and the assignment, and none of them touches a spec.

---

## 1. What screening is

Detection produces a proposal: a draft spec, the evidence behind it, the questions it could not
answer, and the trial run's log. It is deterministic — the auction is a pure function of the source,
and §07 A is the sequence it follows.

Screening runs **after** that, on the finished proposal, and only when the proposal failed or asked
something (§4.2). It asks one question: *given this repository, what did that proposal get wrong or
leave out?* — narrowed, when the proposal only asked something, to *which of these questions does the
repository answer?* The answer comes back as amendments to the spec.

**[D] It is not a detector and does not bid** (R-330). The tempting design is a tenth detector that
bids in the auction alongside the Dockerfile and compose ones, and it is wrong in two directions. A
model that bids competes with evidence it should be using: the compose detector's reading of a compose
file is better than a model's, and a confidence score is the wrong way to express "that reading is
right and it missed the `depends_on`". And it would make the auction non-deterministic, which is the
one property that makes a proposal explainable — R-102 promises the user sees how the answer was
reached, and "a model ranked it highest" is not that.

**[D] What it is for is R-103.** R-103 counts questions per deploy. The related number is how often
an app that deployed cleanly then failed to start. Both usually come from something in the repository
that no detector checks: a start script, a bind address, an environment variable named only in a
README. Each detector handles the layouts it was written for, so there will always be repositories
outside that set. Screening reads the repository for this reason, rather than only the spec.

### 1.1 What it is applied to

**[D] The draft spec, not the bundle plan.** "The deployment plan" is ambiguous in Pando and the two
readings have different answers:

| | | Screened? |
|---|---|---|
| The **draft spec** | what detection proposes and a person reviews (§01) | **Yes** |
| The **bundle plan** | `api.BundlePlan`, what the planner hands a runtime adapter (§03 2.1) | No |

The spec is the sole record of how an app runs (R-020) and is the thing a person reviews before
anything is pinned. The bundle plan is derived from it at deploy time, in adapter vocabulary, with
secrets already materialized into `WorkloadPlan.Env` — amending it would put a model's output inside
the plan boundary §07 B draws, after the last point at which a person saw anything, and would mean the
running app no longer matched its spec. The spec is what a person reviews and what export and
rollback work from, so it is the thing screening amends.

**[D] Screening is applied to the assembled spec, after the install's defaults.** A screener that sees
`Routing.Mode: path` can say something useful about an app that writes absolute asset paths; one that
sees a bare draft cannot. The defaults are applied in `core/detection.Runner` before the proposal is
shown (§07 A step 12), and screening runs after them so it sees the same spec the person reviewing
will see.

---

## 2. The interface

Package `internal/adapter/api`, alongside the other nine.

```go
type AIAdapter interface {
    Adapter
    Capabilities(ctx context.Context) (AICapabilities, error)
    RepairPlan(ctx context.Context, req ScreenRequest) (ScreenResult, error)      // repair_plan
    AnswerQuestions(ctx context.Context, req ScreenRequest) (ScreenResult, error) // answer_questions
}

type AICapabilities struct {
    Functions    []AIFunction // repair_plan, answer_questions, revise_plan, and §10's four
    Model        string       // the adapter's own model; shown in the review
    ChoosesModel bool         // an assignment may name another model (§9)
    Models       []string     // when set, the only models an assignment may name
    MaxFiles     int
    MaxBytes     int64
    LooksUp      bool         // calls the lookup tools in §10.6 (O-54)
}
```

**[D]** `Functions` is data, not a type assertion (R-254, R-259). An adapter that does not perform the
function detection needs is skipped with a reason in the proposal rather than by a failed assertion
nobody can plan around. Both functions take the same `ScreenRequest` and return the same
`ScreenResult`; what differs is the job the adapter is given and the amendments core will accept from
it (§4.2). `screen_plan`, the single function this replaced, is gone, and the unbuilt `repair_build`
became `repair_plan`.

**[D]** `Model` is on the capabilities rather than only in the adapter's config because the review
shows it, for example "Anthropic (`claude-opus-5-5`) read 7 files and changed 3 things". Core cannot show
a value it never receives.

```go
type ScreenRequest struct {
    Source    SourceView     // read-only, structurally (R-020)
    Spec      spec.AppSpec   // the proposal, complete, with install defaults applied
    Evidence  []string       // why the winning detector bid what it did
    Questions []Question     // what detection could not work out (R-102)
    Trial     TrialSummary   // what the trial run saw, including the log
    Budget    ScreenBudget
}

type TrialSummary struct {
    Ran            bool
    Crashed        bool
    ObservedPorts  []int
    ObservedWrites []string
    Log            string
}

type ScreenBudget struct {
    MaxFiles int
    MaxBytes int64
    Timeout  time.Duration
}
```

**[D]** The adapter gets the `SourceView`, not a digest core assembled. The whole value of screening is
in the files Pando's detectors do not read — a `Procfile` beside a `fly.toml`, a `config/database.yml`
that never says `DATABASE_URL`, a README that names the one environment variable without which the app
exits. Core cannot pre-select those without already knowing what it is looking for, which is the
problem. `SourceView` has `Open`, `Stat` and `Glob` and no write method, structurally, so handing it
over grants reading and nothing else.

**[D] `Trial` is passed in whole, including the log.** R-107 is explicit that a crashed trial run shown
to a person is the correct outcome, not a gap to close with inference. That stays true — screening does
not rescue R-107's repository, because nothing in it says it needs Postgres. But the log is the single
most informative thing Pando has about an app that did not start, and withholding it from the one
component positioned to read it would be withholding it for no reason. What the log may *produce* is
bounded by §3, not by keeping it secret.

```go
type ScreenResult struct {
    Amendments []Amendment
    Notes      []string   // things worth saying that change nothing
    FilesRead  []string   // what left the host. Audited (R-337)
    Model      string
}
```

**[D]** `FilesRead` is reported by the adapter and recorded in the audit event. It is not a security
boundary, since an adapter that misreported it would already have read the file. It is the operator's
record of what was sent to the provider, which R-337 requires.

---

## 3. Amendments: the closed set

**[D] This is the load-bearing decision of the document** (R-332).

The obvious interface is `ScreenPlan` returning a `spec.AppSpec` — the amended proposal, whole. It is
wrong, and the reason is not that a model might be careless. It is that a returned spec can express
every field in the spec, including `Runtime.IsolationFloor`, `Build.EgressMode`, `Routing.AdapterRef`
and `Resources`. Those are the install's answers, not the repository's (R-104, R-272); an isolation
floor in particular is a policy floor that host policy sets and app configuration may only move within.
A whole-spec return makes "a model lowered an install's isolation floor and a person clicked accept" a
thing the type system permits, and leaves a denylist in core as the only thing standing between it and
happening. A denylist has to be extended by hand every time the spec gains a field.

So a screener does not return a spec. It returns amendments drawn from a fixed list, and no amendment
in the list changes any of those fields.

| Kind | Changes | Refused when |
|---|---|---|
| `set_command` | a workload's command | the command is empty, or the workload does not exist |
| `set_env` | one literal environment entry | the key is a declared slot, or the existing entry resolves from a slot or a secret |
| `set_port` | the primary port | a port on that workload was observed (R-333) |
| `set_health` | health path and port | the path is not absolute |
| `add_slot` | a declared dependency Pando missed | the key already exists |
| `set_build_context` | the build context directory | the path is absent from the source, or escapes it |
| `set_dockerfile` | which Dockerfile builds | the path is absent from the source, or escapes it |
| `set_static_dir` | which directory is served | the path is absent from the source, or escapes it |
| `add_volume` | persistent storage at a path | the path is not absolute, or is already mounted |
| `answer_question` | one of detection's questions | no outstanding question has that key |
| `add_warning` | says something, changes nothing | — |

**[D] Every amendment requires evidence** (R-334): at least one path in the repository it rests on,
and the paths are checked against the source. A model that names a file that is not there has its
amendment refused with that as the reason. This check catches amendments based on what a framework
usually does rather than on what this repository contains, and it costs one `Stat` per path.

**[D] Refusals are recorded.** A refused amendment appears in the proposal with its reason, so the
person reviewing can see it and whoever maintains the prompt can see which rules the model is
breaking.

**[D] `add_warning` writes one code, `WARN_SCREENING_ADVISORY`.** Not the screener's choice of code.
The codes in §01 2.8 mean specific things that specific parts of Pando produced — a screener able to
emit `WARN_COMPOSE_CONSTRUCT_REWRITTEN` can claim the compose importer rewrote something it did not.

### 3.1 An observation outranks a screening

**[D]** R-333, and it is the rule most likely to be argued with. The trial run watched the process bind
3000 and wrote `Port.Source: observed`. A screener that has read the framework's documentation and
believes it serves on 8080 is refused.

The rule runs one way only. Where nothing was observed, a screened port is better than a question.
Where something was observed, the observation is kept, because R-097 prefers watching the process to
inferring from its source, and a model's reading of the source is an inference.

The same rule covers writes: a screener may add a volume at a path the trial run never reported, and
may not remove one it did.

### 3.2 Required slots

**[D] A slot a screener adds is not `Required` unless the trial run crashed.**

R-132 blocks a deploy on an unfilled required slot, so a screener that marks slots required too freely
converts "the app might not start" into "the app cannot be deployed" — which is worse, and worse in the
way R-104 warns about, because it is a blocker where configuration would do.

The rule is not invented for this. O-4's `[P]` fallback in §01 2.5 already says exactly this: default
`Required: false` for anything not typed to a known service, and let the trial run settle it — a slot
whose absence crashes the trial run is promoted to required with the crash log as evidence. Screening
adopts it unchanged. A screener that finds `DATABASE_URL` in `config/database.yml` on a repository
whose trial run came up clean adds an optional slot, visible in the review, fillable in one click. One
that finds it on a repository whose trial run died on a connection refused adds a required one.

---

## 4. Where it runs

```
core/detection.Runner.Detect
  ├─ policy: source allowlist (R-092)
  ├─ source.Fetch                     → checkout
  ├─ detect.Job.Run                   → proposal   ← deterministic, unchanged
  ├─ applyDefaults                    → the install's answers
  ├─ needed?                          → repair_plan | answer_questions | nothing (§4.2)
  ├─ screening.Run                    → amendments, applied and refused   ← only when needed
  ├─ audit: detection.screen (R-337)  ← only when a call was attempted
  └─ Detections.Save
```

**[D] In `core/detection`, not in `internal/detect`.** `detect` is the auction and the trial run, and
keeping it free of this preserves the property §1 argued for: the auction is a pure function of the
source, testable without a network and without a model. It is also where the audit event has to be
written, because R-027 forbids an adapter writing one, and where host policy is already read.

**[D] The enforcement lives in `internal/core/screening`, not in the adapter and not in the handler.**
The amendment types are in `adapter/api` because an adapter returns them; deciding which ones are
allowed to land is core's, for the same reason authorization is (R-027). An adapter cannot amend a spec
— it can only propose an amendment to one, and `screening.Apply` is the only thing in the tree that
turns the second into the first.

### 4.1 Failure is the deterministic proposal

**[D]** R-335. Every one of these leaves the proposal untouched and records why in `Outcome.Skipped`:

- no AI adapter is assigned the function detection needs (§9)
- detection did not need one (§4.2) — the common case, `not_needed`
- the adapter does not advertise the function detection needed
- host policy forbids screening
- the adapter's `HealthCheck` fails, or `ScreenPlan` returns an error
- the budget's timeout expires
- the result parses to nothing

None of them is an error returned from `Detect`. Everything the auction produced is still in the
proposal, including any questions, so failing the detection would discard a usable result because an
optional step did not run.

### 4.2 When it runs

**[D] Only when detection failed or asked something** (R-336). `core/detection.needed` decides, from
the finished proposal:

| The proposal | Function | What core accepts from it |
|---|---|---|
| the trial run crashed | `repair_plan` | the whole closed set (§3) |
| no detector could read the repository (`unknown` strategy) | `repair_plan` | the whole closed set (§3) |
| neither, but it has questions a person would be asked | `answer_questions` | `answer_question` only; anything else is refused with a reason |
| blocked (R-099) | nothing | — |
| anything else | nothing | — |

A plan that worked and asked nothing is the common case, and it makes no call: no latency, no cost, no
repository contents sent anywhere, and no audit event because nothing left the host. The outcome is
recorded as `not_needed` and the console shows nothing for it. The "Checking with AI" progress stage
is reported only when a call is made.

Questions deferred to the trial run are not asked of anyone, so they are not a trigger. A repair is
handed the questions as well and may answer them, so a detection that both failed and asked is one
call, never two.

**[D] Answering changes nothing else.** The call was made because of the questions, and the plan did
not fail, so an amendment other than `answer_question` from `answer_questions` is refused and listed
with the other refusals. The Anthropic adapter narrows its `submit_findings` schema to that one kind
for the same call, so the model does not spend its budget writing changes core would discard.

**[D] An AI answer is a suggestion on its question, not a rewrite of the draft** (R-338). The
question stays in `questions` with `suggested: {value, reason, evidence}`, and counts as answered
(`detect.Open`, `StatusFor`, the accept check) until a person answers it. Accepting applies the
suggestions a person did not override (`detect.Proposal.WithAnswers`, a port from one recorded as
`screened`). A person changes an AI answer with the ordinary `POST /detection/answers`, and restores
it by sending the suggested value. Folding answers into the draft at screening time, as first built,
made the question vanish and the answer impossible to change; the review gate R-331 requires was a
page that could only show it.

**[D] The failure stays visible** (R-107). A repair amends the draft spec; it does not touch
`trial_log`, so the log that showed the failure is still on the proposal next to what was changed and
why. `Outcome.Why` states what made the call necessary in one sentence.

**[P] One attempt.** A repair is not retried, and the repaired plan is not trial-run again before it is
offered. A second trial of a repaired image-based plan would tell the person whether the repair worked
before they accept it, and is the obvious next step; it is left out because a trial takes up to 90
seconds and the value is not yet measured.

**[P] Detection only.** A build that fails at deploy time is R-106's other repair case and is not
wired here. It would take the build log rather than the trial log, and would amend a pinned spec,
which means a new revision rather than a draft — a different review gate from this one.

### 4.3 When a person asks

**[D] The third trigger is a person reviewing the proposal** (R-336). They know things the repository
does not say out loud — "it serves on 8080", "you missed the Redis" — and used to carry each one to a
settings screen after accepting. `POST /apps/{id}/detection/revise {message}` calls
`core/detection.Runner.Revise`, which:

1. refuses an empty or over-long message, no adapter (`ADAPTER_UNAVAILABLE`), host policy
   (`PERM_DENIED`), or a proposal that is not finished (`STATE_INVALID`);
2. fetches the source **at the reviewed commit** (R-120), not wherever the branch has moved;
3. calls `revise_plan` with the plan accepting would pin — the reading the build-method answer picks,
   a runner-up's `spec` when it adopts one — its evidence, the questions, the trial, the person's
   message and the conversation so far;
4. applies what comes back exactly as screening does: answers become suggestions on their questions,
   everything else goes through `screening.Apply` and its refusals (R-332 – R-334). Asking is not a way
   around the rules; a claim the repository does not support is answered, not applied;
5. appends the person's turn and the adapter's (its reply, the changes, and what Pando refused) to
   `proposal.conversation`, recomputes the status, stores the proposal, and writes a
   `detection.revise` audit event naming what was read (R-337).

A provider that fails leaves the proposal and the conversation as they were and returns
`ADAPTER_FAILED` with the reason (R-335). The call is synchronous — one bounded call (R-339), and the
person is waiting on the reply — unlike a re-run, which clones and builds in the background.

**[D] `reply` is required** on the tool the adapter submits with, for this function only: a person who
asked is never met with silence. It is one to three sentences, held to R-105 like any reason. A
message that is not about the plan — a joke, a greeting — changes nothing and gets a short reply that
steers back to what the plan still needs; a light touch is allowed there.

**[D] What was read before is handed over, not read again.** Each call is a new conversation with the
model, which starts knowing nothing. Each AI turn records its `files_read`; the next revision passes
every file read about this proposal so far (screening's and earlier turns', once each) as `Known`, and
the adapter puts their contents in the first message. Same commit, same contents; still read through
the budgeted reader, so they count against R-339 and are recorded as sent (R-337). It saves the round
trips of finding them again, not the tokens of sending them.

**[P] The conversation lives on the proposal.** It is about this reading of this commit; detecting
again starts a new one. The console shows it only when an AI adapter is available for this app (the
outcome's `skip_code` is not `not_configured`, `policy` or `unsupported`).

---

## 5. Review and provenance

**[D] Amendments are applied, not queued for per-item approval.** Holding each one for a click would
add a question per amendment, which R-103 counts against the product. The proposal a person reviews is
the amended one, and it passes the same review gate as any other proposal (R-331, R-098).

**[D] The spec records screened provenance**, alongside the sources it already records:

```go
spec.EnvFromScreening  EnvSource    = "screened"
spec.PortScreened      PortSource   = "screened"
spec.VolumeFromScreening VolumeSource = "screened"
```

Three properties follow from reusing those fields rather than adding a flag. The review UI already
renders provenance, so "we watched your app bind 3000" and "Anthropic read your Dockerfile and set
NODE_ENV" are shown by the same code. `spec.Carry` (§01, re-detection) already keeps only `user`
sources, so a screened value is replaced by the next screening rather than carried forward as though a
person had chosen it — which is right: re-detection re-screens. And R-333's refusal is a comparison
between two values of a field the spec already has.

**[D] The outcome is recorded on the proposal**, not only in the audit log:

```go
type Outcome struct {
    Ran        bool
    Skipped    string            // why nothing ran (§4.1); empty when it did
    SkipCode   SkipCode          // the same, as a stable value: not_configured | not_needed | policy | unsupported | unavailable | blocked
    Function   AIFunction        // repair_plan | answer_questions; empty when not needed (§4.2)
    Why        string            // what made the call necessary, in one sentence
    AdapterRef string
    Model      string
    FilesRead  []string
    Applied    []Applied
    Refused    []Refused
    Answers    map[string]string // detection questions it answered (R-338)
    Notes      []string
    DurationMS int64
}
```

The console renders it as a section of the review, directly under the winning bid: the model, each
change with its reason and the files it cites, the files read, and what it asked for that Pando would
not do. An answered question is listed among the changes with its reason, so the review shows why the
question stopped being asked. When `skip_code` is `not_configured`, `not_needed` or `blocked` the
section is absent: an install without an AI adapter is not degraded, a detection that needed none is
the ordinary case, and a blocked proposal already says why it stopped. Any other skip is shown as one
line with its reason. The onboarding page's layout around this is to be reworked for the
exception-only model (issue #69); until then it renders the same outcome as before.

---

## 6. The Anthropic adapter

`internal/adapter/ai/anthropic`, kind `anthropic`. The first implementation, and the one the interface
was shaped against.

**[P] The official Go SDK**, `github.com/anthropics/anthropic-sdk-go`. Hand-rolling the HTTP would
avoid a dependency and would mean owning the request shape, the retry policy, the streaming envelope
and the error taxonomy for a provider whose API is not ours to keep up with.

**[P] `claude-opus-5-5` is the default model**, overridable per install. Screening runs at most once per
detection, and only on one that failed or asked something (§4.2), on a repository somebody is about to
deploy; the thing being optimized is whether the app comes up without a person stepping in — this is not a high-volume path where a cheaper model pays for itself.
It replaced `claude-opus-5` as the default because it is newer and costs less per token. An install
that disagrees sets `model` in the adapter's config.

**[P] The model reads the repository through two tools**, `list_files` and `read_file`, backed by the
`SourceView` and counted against `ScreenBudget`. The alternative — core packing a bundle of files into
the prompt — has to guess which files matter, which is the thing screening exists to stop guessing at.
Both tools refuse a path that escapes the checkout and stop returning content once the budget is spent.

**[P] Amendments come back through a `submit_findings` tool with `strict: true`** rather than as prose
to be parsed. The schema is the closed set from §3, so a malformed amendment is rejected by the API
before it reaches Pando, and the shape core validates is the shape the model was given. For
`answer_questions` the item schema is its own: one kind, `answer_question`, with `key`, `value`,
`reason` and `evidence` all required and `key` an enum of the questions actually asked. The general
schema leaves `key` optional because most kinds have none, and a model answered a question correctly
with no key, which core then had to refuse.

**[P] One loop, two system prompts.** `repair_plan` tells the model the plan did not work and that
changing nothing is right when the failure is real (R-107); `answer_questions` tells it the plan works
and only the questions are open. The rules both are held to — evidence, the closed set, observations
win, no invented dependencies — are shared text. Each prompt is identical across calls of its
function, so the cache breakpoint on it still pays.

**[D] The API key is a `secret.Value`** (§00 3.3), so it renders `[redacted]` in every marshaler and
cannot reach a log line (R-194).

**[D] The adapter imports nothing from core but `spec` and `secret`,** like every other adapter. The
depguard rule in `.golangci.yml` covers it without a new entry, because it matches `**/internal/adapter/**`.

### 6.1 What every AI adapter shares

**[D] `internal/adapter/ai/aikit` holds everything that is not a provider's**: the system and user
prompts, the tools (`list_files`, `read_file`, `submit_findings`, and `submit` and the lookup tools of
§10.6 for the administrative functions) with their JSON schemas, the budgeted reader, what a tool call does, and each
administrative function's task. An adapter translates these into its provider's request types and holds
the conversation; nothing in `aikit` speaks to a provider. One copy, so every adapter asks the same
questions under the same rules and is refused the same way by core — a second copy of a prompt is a copy
that drifts. The Anthropic adapter was moved onto it with its tests unchanged.

**[D] Tool use is asked for, never forced.** Current models refuse forced tool use (`tool_choice` of a
named tool or `any`) with a 400. Every adapter sets tool choice to automatic, says in the system prompt
to answer through the tool, and asks once more when a model answers in prose.

### 6.2 The OpenAI adapter

`internal/adapter/ai/openai`, kind `openai`, through the **Responses API** with the official SDK
(`github.com/openai/openai-go/v3`). All seven functions, and any model per function.

- **[P] `gpt-5.5` is the default model**, the SDK's newest general model when this was written. An install
  sets its own; each function may run on another (§9).
- **[D] The conversation is stored by OpenAI, not by Pando, and the adapter says so.** Each turn continues
  the last by `previous_response_id` rather than resending the conversation, which the Responses API makes
  possible by keeping each response on OpenAI's servers, under OpenAI's retention terms. Pando stores none
  of it. The adapter's description in the console's Add adapter form states this, so whoever configures it
  knows before a repository or an audit record is sent. An organization under zero data retention cannot
  use `previous_response_id`; supporting one means resending the conversation each turn, which is not
  built.
- **[P] Not strict.** OpenAI's strict mode needs every property required, and the amendment schema's
  optional fields are what let one shape carry every kind. Core validates every answer regardless.
- The key is a credential like Anthropic's, or `api_key_env`, or `OPENAI_API_KEY`.

### 6.3 The local adapter

`internal/adapter/ai/local`, kind `local`, for a model on the install's own hardware through **any
server that speaks the OpenAI Chat Completions API** — Ollama, LM Studio, llama.cpp's server, vLLM — by
the same SDK. Nothing is sent to a provider, which is the reason to choose it; R-337's audit event is
still written, because the server is somebody's machine and what was sent to it is still worth knowing.

- **[P] The default address is `http://host.docker.internal:11434/v1`**, Ollama on the machine Pando's
  container runs on. Docker Desktop provides that name; `docker-compose.yml` maps it to the host gateway
  on Linux.
- **[D] A model is required.** There is no model every local server has. The health check lists the
  server's models and says which it serves when the configured one is not among them.
- **[D] No key by default, and never `OPENAI_API_KEY`.** A key meant for OpenAI sent to somebody's local
  server would be a leak. A key may be stored as a credential for a server that asks for one.
- **[P] Longer defaults**: a 300-second request timeout and a smaller read budget (30 files, 192 KiB).
- **[P] An answer in the reply is read.** A small model often answers in prose where a tool call was
  asked for, or writes the call out as text. A reply that is a JSON object is taken as the answer tool's
  input, and one shaped `{"name", "arguments"}` as a call; an answer that does not decode is handed back
  to be fixed. Core validates the result like any other, so a weaker model can be wrong but is trusted
  no further.
- **[D] One per install**, like every AI provider (§9): different models on one server are chosen per
  function.
- **[P] It does not look things up** (`LooksUp` false, §10.6). A small model is uneven at calling
  tools, and a lookup loop multiplies the rounds a slow server serves, so core searches the request's
  own words for it and sends the matches.

---

## 7. Configuration

An `adapter_configs` row like any other (§02 2.5), seeded **disabled and absent**: unlike the eight
categories `seedDefaultAdapters` fills, there is no AI adapter that works without a credential, and
seeding a broken one would put a permanently unhealthy adapter in every install's console. An
install may instead declare the adapter in its config file (§7.1).

```json
{
  "model": "claude-opus-5-5",
  "screen_plans": true,
  "max_files": 40,
  "max_bytes": 262144,
  "timeout_seconds": 120
}
```

Configured through `POST /api/v1/adapters` like any other category, with `category: "ai"`,
`kind: "anthropic"`, the settings above as `config`, and the key as `credentials: {"api_key": "…"}`.
It takes effect at the next restart, because adapters are registered at startup (R-253).

**[D] There is no default AI adapter.** `Registry.DefaultAI`, which returned the category default or
else the first AI adapter registered, is gone, and the console does not offer "use as default" for
an AI adapter. A caller asks the registry for the adapter assigned one function
(`Registry.AIFor`), and an adapter does nothing until it is assigned something (§9).

**[D] One AI adapter per provider** (R-259). A partial unique index on
`adapter_configs (kind) WHERE category = 'ai'` refuses a second Anthropic adapter, and the refusal
says to set a model on an assignment instead: besides the credential, a model is the only thing two
adapters of one provider would differ by. Migration `000033` stops with the query to run if an
install already has two.

**[D] The API key is never stored in the clear (O-20).** `credentials` is write-only and is sealed by
the install's secrets adapter into `adapter_credentials`, bound to the adapter's ID. The database
refuses a `credentials` key in `adapter_configs.config`, the handler refuses `api_key` and similar
names there, and the adapter refuses to start if it finds one at the top level of its stored
configuration. Core decrypts the credential at startup and passes it to `Configure` in memory.
`GET /adapters` reports `credentials_set: ["api_key"]` and nothing more. An operator who prefers the
environment can instead set `api_key_env`, or `ANTHROPIC_API_KEY`, which the adapter reads when no
credential is stored.

**[P] `screen_plans` defaults to true** (R-336). False stops the adapter advertising `repair_plan`,
`answer_questions` and `revise_plan`, so an assignment of them to it is off. The key kept its name
when screening split into two functions, so existing configurations read unchanged. Assignment is now
the ordinary way to turn screening off; the key stays so a stored configuration keeps its meaning.

**[D] An upgrade keeps screening.** Migration `000033` assigns `repair_plan`, `answer_questions` and
`revise_plan` to the install's one enabled AI adapter — the default, else the first by ID — unless it
set `screen_plans: false`. §10's functions start unassigned: they send a provider things screening
never did (account names, audit records, policy), so they are off until an administrator assigns them.

**[P] Host policy carries `DisableAIScreening`**, install-wide, evaluated in `core/detection` beside
the source allowlist. It can only deny, like everything else in the document (R-272). It covers
screening, not §10's functions, which are each gated by a verb instead.

### 7.1 Declared in the config file

**[D]** An install kept as code declares adapters, and the AI functions each handles, in an
`adapters:` section of the startup YAML (R-271), beside `policy:`. There is no environment-variable
form: a map of adapters with nested credentials and functions does not flatten into variables anyone
could read.

```yaml
adapters:
  ai_anthropic:
    category: ai
    kind: anthropic
    name: Anthropic
    config:
      model: claude-opus-5-5
    credentials:
      api_key: {env: ANTHROPIC_API_KEY}   # or {file: /run/secrets/anthropic}
    functions:
      repair_plan: {}
      answer_questions: {}
      search_audit: {model: claude-haiku-4-5}
```

Keys are adapter IDs. `category` and `kind` are required; `name` (the ID), `default` (false),
`enabled` (true), `config` and `credentials` are optional. `functions` is a list of names, or a map of
names to `{model: …}`, and only an AI adapter may have it. Parsing is `internal/config/adapters.go`.

**[D] Credentials are references only** (R-190). Each is `{env: VARIABLE}` or `{file: PATH}`, read at
startup and handed to `Configure` in memory, as a decrypted stored credential is. A value written
inline under `credentials`, or a key under `config` named like a credential (`api_key`, `token`,
`secret`, `password` and similar), stops startup with a message saying how to reference it instead.
The config file is not encrypted.

**[D] A declaration overrides what is stored, and is read-only elsewhere while it is declared.** A
declared adapter replaces a stored adapter with the same ID, a stored AI adapter of the same provider,
and a stored default in its category. A declared assignment replaces a stored one for the same
function. The stored rows are not changed: `GET /adapters` lists an overridden adapter with
`status: "overridden"` and `overridden_by` naming the declaration, `GET /ai/functions` lists an
overridden assignment under `overridden`, and removing the declaration and restarting brings them
back. `POST /adapters` for a declared ID, or for an AI adapter of a declared provider, and
`PUT`/`DELETE /ai/functions/{function}` for a declared function, return `STATE_SET_AT_STARTUP` naming
the file and key. `GET /adapters` marks each declared adapter `declared: true` with its `source`, and
`GET /config` lists the declared adapters with their credential names (never values) and functions.
Declared and console-managed adapters may be mixed, and a declared AI adapter may be assigned, from
the console, a function the file does not assign (O-21).

**[D] A declaration that contradicts itself stops startup, in every category.** Two enabled defaults
in one category, two AI adapters of one kind, one AI function under two adapters, and two declared
services adapters that provide the same slot type each stop startup with an error naming both keys,
for example *"the config file /etc/pando/pando.yaml assigns the AI function search_audit twice, at
adapters.ai_anthropic.functions.search_audit and adapters.ai_openai.functions.search_audit. Each AI
function is handled by one adapter: remove it from one of them"*. This follows `policy.NewOverlay`: a
declaration that silently resolved one way does not do what its author wrote. The services check runs
after registration, because which slot types an adapter provides is the adapter's answer (`Supports`).

**[D] A declared adapter that fails to configure is logged and skipped,** as a stored one is: an unset
variable, an unreadable file, a key the provider refuses. The declaration does not contradict itself;
the environment is wrong, and one broken adapter does not take the install offline. An assignment to
it reads as off, with the reason, in `GET /ai/functions`.

---

## 8. What this is not

**[D] It does not remediate the app** (R-028). It amends Pando's description of how to run the
repository. It does not write to the repository, open a pull request, patch a Dockerfile, or inject
configuration into a running workload. `SourceView` has no write method.

**[D] It does not invent topology** (R-021). Every amendment rests on a path in the repository, checked
to exist. Finding a `DATABASE_URL` in a file Pando's detectors do not read is finding a declaration;
deciding an app needs Postgres because it imports an ORM is not, and nothing in §3's list can express
it — `add_slot` needs a key, and a key comes from somewhere in the source.

**[D] It is not required.** Everything the auction produces is produced whether or not a screener
runs (R-106). §4.1 lists the cases in which screening is skipped.

---

## 9. Assigning functions to adapters

**[D] Each AI function is assigned to at most one adapter, and an adapter may hold any number**
(R-259). With Anthropic and OpenAI both configured, an administrator might give screening and access
drafting to one, and audit search and reference help to the other.

| Function | Title | Called by |
|---|---|---|
| `repair_plan` | Plan repair | detection, when the proposal failed (§4.2) |
| `answer_questions` | Answering detection questions | detection, when it asked something (§4.2) |
| `revise_plan` | Plan revision | a person reviewing a proposal (§4.3) |
| `draft_access` | Access drafting | `POST /ai/access/draft` (§10.1) |
| `draft_policy` | Policy drafting | `POST /ai/policy/draft` (§10.2) |
| `search_audit` | Audit search | `POST /ai/audit/search` (§10.3) |
| `answer_reference` | Reference help | `POST /ai/reference/answer` (§10.4) |

`read_readme` is declared and not assignable, because nothing calls it. `api.AIFunctions()` is the
list, and `AIFunction.Title()` names each in a sentence.

**[D] The database enforces one adapter per function.** `ai_assignments` has `function` as its
primary key (§02 2.5). `PUT /ai/functions/{function}` inserts, or updates the model only when the row
already names the same adapter; a row naming another adapter is not moved. The refusal is
`STATE_AI_FUNCTION_ASSIGNED`, to R-105's standard: *"Audit search is already handled by the adapter
ai_openai. Each AI function is handled by one adapter at a time. To move audit search to the
Anthropic adapter (ai_anthropic), remove it from the adapter ai_openai first."* Moving a function is
two requests, so no adapter loses work someone gave it without someone taking it away.

**[D] `adapter_id` has no foreign key.** A declared adapter has no `adapter_configs` row and may
still be assigned a function from the console. Core checks at assignment that the adapter is a running
AI adapter that advertises the function; an adapter that later fails to start, or is removed, leaves
the assignment in place and the function off, with the reason (R-335).

**[D] An adapter is assigned only functions it advertises** (`AICapabilities.Functions`), checked at
assignment and again at each call. A refusal lists what the adapter does perform.

**[D] A model per assignment, when the adapter can choose one.** `AICapabilities.ChoosesModel` says
the adapter can run a model other than its own for one call; `Models`, when set, lists the only ones
it may. An assignment's `model` is optional and falls back to the adapter's `Model`. A model on an
adapter that cannot choose is refused, as is one outside a non-empty `Models`. The model reaches the
adapter on each request (`ScreenRequest.Model`, and the `Model` field on §10's requests), and the
result, the review and the audit event name the model that ran. The Anthropic adapter sets
`ChoosesModel` and leaves `Models` empty: any model the Messages API serves, and a name it does not
serve fails that call rather than the assignment.

**[D] An unassigned function is off, not an error** (R-106, R-335). Detection records it as
`not_configured` with *"Plan repair is not assigned to an AI adapter on this installation."* §10's
endpoints return `ADAPTER_UNAVAILABLE` with the remedy of assigning it, and the console does not
offer them (§08 1.1).

**[D] Assignments take effect immediately.** Adapters are registered at startup (R-253); an
assignment is not an adapter. After each change core reloads the stored assignments, lays the
declared ones over them (§7.1), and hands the result to the registry (`Registry.SetAIAssignments`).
The next call uses it.

**[D] Every change is audited**, as `ai.function.assign` and `ai.function.unassign`, with the adapter
and model. Listing is `install.view`; assigning and unassigning are `install.adapters.manage`, the
verb that manages the adapters themselves.

```
GET    /api/v1/ai/functions               each function: adapter, model, effective_model, on/off and why, source, overridden
PUT    /api/v1/ai/functions/{function}    {adapter_id, model?}
DELETE /api/v1/ai/functions/{function}
```

`pando ai functions|assign|unassign` and the MCP tools `pando_list_ai_functions`,
`pando_assign_ai_function` and `pando_unassign_ai_function` are clients of these (R-261).

---

## 10. Administrative functions

R-343 – R-346, in `internal/core/assist` (`Service`), the service layer both `httpapi` and `mcp` call.
They share three rules with screening, adjusted to what they touch.

**[D] Each proposes and none applies.** The result is a draft, a filter or an answer. Creating the
role, saving the policy or paging the log is a separate request, made by a person through the
endpoint that already exists for it, under their own authority. An agent holding a token can ask for
a draft and still lacks whatever verb applying it needs.

**[D] Core checks what comes back against a closed set, whatever the model said,** as
`screening.Apply` checks amendments: verbs against the catalog, policy fields against the overlay,
citations against the reference. What core drops is returned under `refused` or `declined`, with the
reason, so the person reviewing sees it.

**[D] Each call is audited, and the content is not.** `ai.<function>` (`ai.draft_access`,
`ai.search_audit`, …) names the adapter and model, with target kind `ai_function`, and for audit
search the number of records sent. The description or question is not recorded: it is free text a
person typed, and the log is readable by everyone with `install.audit.read`. This is R-337's record of
what left the host, applied to these functions (R-227).

Each is one bounded call (two for audit search), with a 90-second timeout and a 2,000-character limit
on what a person types. Each adapter implements each with a `submit` tool whose schema is the
function's result, asked for rather than forced (§6.1), on the assignment's model. Drafting access and
searching the audit log may also have the lookup tools of §10.6, which is the only loop these
functions have; none of them reads a repository.

### 10.1 Access (`draft_access`, R-343)

`POST /api/v1/ai/access/draft {description}`, `install.users.manage`. The adapter receives the verb
catalog and the existing roles, finds the accounts (ID, username, name, email) and groups the
description names through §10.6 rather than being handed every one, and returns
`{role?: {name, scope, verbs}, group?: {name, members}, reply}`. When refining a draft, the accounts
already in its group are named for it, by ID, up to twenty.

The catalog handed over is what the caller could grant: every app verb, since `POST /roles` lets any
holder of `install.users.manage` define an app role, and only the install verbs the caller holds.
Core then refuses a verb outside the catalog, a verb of the other scope (R-080), a role left with no
verb, a role or group name that already exists (built-in names included, R-081 and R-082), and a
member who is not an account. Each check is a bounded read: the group name by one search, the members
by ID, at most 500 of them, beyond which a draft's members are cut and the refusal says so. The console creates what survives with `POST /roles`, `POST /groups`
and, for an install-scoped role, `PUT /groups/{id}/role`; an app role is granted per app, which is
that app's decision. R-088's last-administrator rule is untouched, because a draft only adds.

### 10.2 Policy (`draft_policy`, R-344)

`POST /api/v1/ai/policy/draft {description}`, `install.policy.manage`. The adapter receives the
current effective document, each field's name and type and whether it is fixed at startup, and the
verb catalog, and returns `{changes: {field: value}, reply}`.

Core builds the result: `proposed` (the document as it would be saved), `changes` as
`{key, from, to}`, `declined`, and `refused`. A change to a field the startup overlay fixes is
declined **here**, whatever the adapter returned, citing the overlay's source: *"disabled_verbs is set
in /etc/pando/pando.yaml (policy.disabled_verbs) and can't be changed here. Remove it from that file
and restart Pando to manage it from the console."* The adapter is told which fields are fixed so it
can say so, but the refusal does not depend on it. A value that does not read as its field
(`policy.CheckValue`, the check startup settings get) and a verb list naming a verb that does not
exist are refused. The proposal is saved only by `PUT /policy`, which runs its own checks again.

### 10.3 Audit search (`search_audit`, R-345)

`POST /api/v1/ai/audit/search {question}`, `install.audit.read`. Two calls:

1. `SearchAudit` receives the question, the current UTC time and every action name Pando records
   (`audit.Actions`, §10.6), finds the accounts and apps the question names through §10.6, and returns
   one filter — `actions` (prefixes,
   any of which matches), `app_id`, `principal_id`, `principal_kind`, `target_kind`, `target_id`,
   `involving`, `since`, `until` — and a `note`. Core trims it, drops an unknown principal kind, keeps
   at most ten actions, and refuses a range that ends before it starts.
2. Core runs the filter (`audit.Reader.List`, up to 200 records). `SummarizeAudit` receives up to 100
   of them with the question, and the accounts and apps those records and the filter name, by ID, up
   to fifty of each, and returns a summary written from those alone.

The response carries `filter`, `note`, `summary`, `matched` and `truncated`. The filter is ordinary
`GET /audit` parameters, so the console puts it in the audit log's own filter fields and the table
below is the ordinary table (§08 1.1). `GET /audit` accepts `action` more than once for this; any of
them matches.

**[D] The adapter never reads the log** (R-027, R-226). It proposes a filter; core runs it and decides
how many records to send.

**[D] "Accessed" is `app.use`** (O-22, R-227): the proxy records each visit to an app, anonymous ones
included unless host policy turns that off. The adapter is told what a visit is, and uses its `note`
for what the log cannot answer, such as individual requests within a visit. A person named by
username, email or name in the filter is resolved to their ID by core, with one bounded search for
an exact match, so "admin" finds admin's events whether or not the model looked the ID up.

### 10.4 Reference help (`answer_reference`, R-346)

`POST /api/v1/ai/reference/answer {question}`, any signed-in user, like `GET /reference`. The adapter
receives the question and the generated reference as Markdown — the document `make reference` writes
to `docs/api.md`, `docs/cli.md` and `docs/mcp.md`, rendered by `internal/reference` and built once per
process — and returns `{answer, cites, covered}`. Core drops any citation that does not occur in the
reference text, which catches an answer drawn from the model's memory of another version of Pando.
`covered: false` means the reference does not answer the question, and the answer says so.

### 10.5 Surfaces

Each function is an endpoint first (R-261). The CLI is `pando ai ask`, `pando ai audit`,
`pando ai draft-access` and `pando ai draft-policy`. The MCP tools are `pando_ai_draft_access`,
`pando_ai_draft_host_rules`, `pando_ai_search_audit` and `pando_ai_ask_reference`; the policy tool's
name avoids "policy" because `TestO12_TheMostDangerousActionsAreNotOfferedAsTools` refuses any tool
name containing it (§04 3), and drafting a policy changes nothing. The console's entry points are in
§08 1.1.

### 10.6 Looking people, apps and groups up (O-54)

**[D] The adapter is never handed a whole table** (O-54). Drafting access and searching the audit log
once sent every account, every app and every group, and the action names by a `DISTINCT` over the
whole log, so the prompt and the reads behind it grew with the installation. Now:

- **[D] Lookup tools, as callbacks core passes in.** `api.Lookup` carries `People`, `Apps` and
  `Groups` functions; `aikit` offers them to the model as `find_people`, `find_apps` and
  `find_groups`, each taking a `query` (part of a name, username, email or slug). The adapter calls
  them; it never touches state or authorization (R-027, depguard). Group lookup is offered for
  drafting access only.
- **[D] They run as the person who asked**, and return only what that person could read through the
  API: accounts and groups with `install.view` (the verb `GET /users` and `GET /groups` require), every
  app with `install.apps.view`, otherwise the apps the person holds a grant on (`GET /apps`). A lookup
  the person may not make is nil, and its tool is not offered. The same rule filters the fallback
  below and the names a summary is given.
- **[D] Bounded three ways.** Each lookup returns at most `api.LookupLimit` (20) matches, from a store
  search with that limit (`Users.Search`, `Groups.Search`, `Apps.List*Page` with a query), each field
  clipped to 200 characters. A conversation makes at most `aikit.MaxLookups` (10) lookups; past that a
  lookup is answered with "submit now" instead of being run. And each adapter's loop stops after its
  `maxIterations` rounds (24).
- **[D] A capability, not a type assertion** (R-254). `AICapabilities.LooksUp` says the adapter calls
  the lookup tools. The Anthropic adapter (Messages API tool use) and the OpenAI adapter (Responses API
  function calling) set it.
- **[P] Without it, core pre-searches the request's words.** For an adapter that does not look things
  up — the local adapter (§6.3) — core takes up to eight words of the description or question
  (longest first, words of three letters or more, a short list of words that name nobody left out),
  searches each as a person, an app and, for access, a group, keeping five matches of each kind per
  word and twenty of each kind in all, and sends those. The prompt says the lists are matches, not
  everything, so a name it does not find is left out and said so rather than guessed.
- **[D] Action names come from the code.** `audit.Actions` is the catalog of every action Pando
  writes; `TestEveryActionWrittenIsCatalogued` scans the tree for literal actions and fails when one
  is missing from it. `audit.Reader.ActionNames` is gone.
- **[D] Roles are still sent whole.** A draft must not duplicate a role (R-082), and their number is
  set by administrators defining roles by hand, not by the organization's size.
- When refining an access draft, the accounts in its group are named by ID (up to twenty), and the
  audit summary is given the accounts and apps its records name, by ID (up to fifty of each). Both are
  bounded by the request, not the installation. `TestO54_NoOperationReadsAWholeTable` runs both
  functions on ten thousand accounts, apps and groups against stores that refuse any unbounded read.
