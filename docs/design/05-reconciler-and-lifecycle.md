# 05 — Reconciler and Lifecycle

R-148 says reconcile when possible, report when not. R-151 says a failed app stays failed. R-140 lists states without transitions. This document turns that into an algorithm.

---

## 1. State machine

**[D]** Two fields, deliberately separate:

- `apps.desired_state` — what a human asked for: `running` | `stopped`
- `apps.state` — what is true: the observed lifecycle state

```
draft ──accept proposal──> proposed ──deploy──> deploying
                                                    │
                          ┌─────────────────────────┼──────────────┐
                          ▼                         ▼              ▼
                       running                   failed        (build fails)
                          │                         │               │
              ┌───────────┼───────────┐             │               ▼
              ▼           ▼           ▼             │        stays running
          degraded     stopped     deploying        │        (R-146)
              │                                     │
              └──────── recovers ───────────────────┘
                        (auto)              intervene (manual only, R-151)

any state ──delete──> archived
```

### 1.1 States

| State | Meaning | Reconciler acts? |
|---|---|---|
| `draft` | Created, detection incomplete or unaccepted | No |
| `proposed` | Spec pinned, never deployed | No |
| `deploying` | A deployment is in flight | No — the deployment owns it |
| `running` | Observed matches spec, health passing | Yes |
| `degraded` | Observed matches spec, health failing or restarting | Yes |
| `stopped` | `desired_state = stopped`, workloads down | Yes (keeps them down) |
| `failed` | Give-up threshold reached (R-150) | **No** (R-151) |
| `archived` | Soft-deleted | No |

**[D]** `failed` is the only state the reconciler refuses to touch. That is what makes R-151's "stays failed until a human intervenes" true rather than aspirational — it is the absence of a code path, not a flag.

**[D]** `degraded` vs `failed` is the distinction the requirements gestured at but never named: degraded is recoverable and still being worked; failed is terminal and needs a person.

### 1.2 Transitions

| From | Event | To | Notes |
|---|---|---|---|
| `draft` | proposal accepted | `proposed` | Spec revision 1 written |
| `proposed` | deploy requested | `deploying` | Not while it awaits approval: the app's state does not move (§3.3) |
| `deploying` | apply succeeded, health passing | `running` | |
| `deploying` | apply succeeded, health never passes | `degraded` | Enters backoff |
| `deploying` | build failed | *unchanged* | R-146 — old version keeps serving |
| `deploying` | apply failed | `failed` | Nothing partial is left behind |
| `running` | health fails | `degraded` | |
| `running` | workload missing | `degraded` | Drift; reconciler restores |
| `degraded` | health passes | `running` | Restart counter resets |
| `degraded` | give-up threshold | `failed` | Notification fires |
| `degraded` \| `running` | stop requested | `stopped` | |
| `stopped` | start requested | `deploying` | |
| `failed` | **human** retries or edits spec | `deploying` | Only exit from `failed` |
| any | delete | `archived` | After the backup decision (R-204) |

**[D]** Build failure leaves state unchanged. It does not enter `degraded` — nothing about the running app changed. The *deployment* is `failed`; the *app* is not. Keeping these separate is why `deployments` is its own table (§02 2.3).

---

## 2. The loop

```go
func (r *Reconciler) Tick(ctx context.Context) {
    apps := r.state.AppsNeedingReconcile(ctx)  // excludes draft/proposed/deploying/failed/archived
    for _, app := range apps {
        r.sem.Acquire()
        go func(a App) {
            defer r.sem.Release()
            r.reconcileOne(ctx, a)
        }(app)
    }
}
```

**[P]** Interval: 15 seconds. Concurrency: 8 apps at once. Per-app work is serialized by an advisory lock on `app_id` so two ticks cannot overlap on one app.

```go
func (r *Reconciler) reconcileOne(ctx context.Context, app App) {
    spec := r.state.PinnedSpec(ctx, app.ID)
    rt   := r.registry.Runtime(spec.Runtime.AdapterRef)

    observed, err := rt.Observe(ctx, app.BundleRef())
    if err != nil {
        r.markUnobservable(ctx, app, err)   // adapter down ≠ app broken
        return
    }

    want := r.planner.BundlePlanFor(ctx, spec)   // resolved, but no secrets fetched yet
    drift := Diff(want, observed)

    switch {
    case drift.None() && observed.Healthy():
        r.transition(ctx, app, StateRunning)
        r.clearBackoff(ctx, app.ID)

    case drift.None() && !observed.Healthy():
        r.handleUnhealthy(ctx, app, observed)

    case drift.Reconcilable():
        r.applyWithBackoff(ctx, app, want)

    default:
        // R-148: cannot reconcile. Report, do not guess.
        r.transition(ctx, app, StateDegraded)
        r.notify(ctx, app, DriftUnreconcilable, drift.Describe())
    }
}
```

**[D]** An adapter being unreachable is not app failure. `markUnobservable` stamps
`apps.unobservable_since` (§02 2.3) and surfaces it as a platform problem; it does not increment the
app's restart counter or move it toward `failed`. Otherwise a Docker daemon restart marks every app on
the host as failed.

**[D]** `unobservable_since` is deliberately not a value of `state`. An app whose adapter is
unreachable has not changed — Pando has merely stopped being able to see it, and the honest rendering
is the last known state plus a notice that it is stale. Adding an `unknown` state would push that
distinction into every consumer of the state machine. The field clears on the first successful
`Observe`.

**[D]** Environment drift is the one form of drift that cannot be observed, because `ObservedWorkload`
carries no environment and deliberately should not — see §02 2.4 for the fingerprint comparison that
stands in for it. This is worth knowing before adding env to the observation struct: doing so would
require every runtime adapter to read back resolved environment, which is exactly the secret-bearing
data the adapter interface works to keep out of adapters' hands.

### 2.1 What counts as reconcilable drift

**[D]** Reconcilable — the reconciler acts:
- A workload is present in spec, absent in reality → recreate it
- A workload exists but is stopped → start it
- A workload exists with the wrong image digest → recreate it
- A workload is running against a stale environment — `apps.applied_env_fingerprint` does not match
  the current resolution, which is how a rotated secret (R-193) becomes visible → recreate it
- A route is missing → re-`Ensure` it
- A volume is missing and has never held data → create it

**[D]** Not reconcilable — report only (R-148, R-028):
- A volume is missing that previously existed. Recreating it silently produces an empty volume and an app that looks healthy while having lost everything (R-203). Report.
- An unrecognized workload exists inside the bundle. Someone put it there on purpose; destroying it is destructive and unrequested.
- Observed configuration conflicts with the spec in a way that implies a manual edit, e.g. changed mounts.

**[D]** The dividing line: **the reconciler may create and start things; it may not destroy anything a human may have wanted.** That is the rule to hold when new cases come up.

### 2.1.1 Pando stopping does not stop apps

**[D]** When the Pando process stops, deployed apps keep running. It does not stop them on the way
out, and it does not stop them on the way back in.

The objection is reasonable: the proxy is the only route to an app (R-023), so while Pando is down an
app is unreachable — what is it for? Four answers, the first decisive:

- **Not every workload needs the proxy.** `Exposed` is per-workload (R-026). A queue consumer, a
  scheduled job or a worker never takes an inbound request, and goes on doing real work while Pando
  is away. Stopping it would break something that was working.
- **A restart is not a shutdown.** Upgrading Pando, changing its configuration, recovering from a
  crash — if apps stopped each time, every Pando upgrade would become a full outage of every app plus
  a cold start for each.
- **A crash and a graceful stop would behave differently.** SIGKILL, a host reboot and an OOM leave
  apps running whatever Pando intends, so stopping-on-shutdown would only happen on the tidy path.
  One consistent behavior beats a better one that cannot be relied on.
- **`desired_state` is the human's intent**, and Pando going away does not change what was asked for.
  Stopping apps would mean overwriting that intent with Pando's own lifecycle.

There is also an availability argument. Pando is a single point of failure for *access*; it should not
become one for *availability*. Apps keep serving their internal work and are reachable again the
moment Pando returns.

**[D]** Stopping an app remains an explicit act — `desired_state = stopped`, or deletion. Those are
things a person decides, not consequences of a process exiting.

**[P]** The cost is that apps survive `docker compose down`, which surprises people, because Pando
creates them outside the Compose project. `docker ps --filter label=io.pando.managed=true` finds
them. What Pando owes in exchange is knowing it was away when it comes back and converging quickly —
that is this loop's job, not a reason to stop anything.

### 2.2 Backoff

```go
var backoff = []time.Duration{0, 5*time.Second, 15*time.Second, 60*time.Second, 5*time.Minute}
```

**[P]** R-149. Capped at 5 minutes. Counter and window live on the app row.

```go
const (
    failureThreshold = 10               // R-150
    failureWindow    = 30 * time.Minute // measured from the LAST failure
)
```

**[D] Both are configurable, and unset in production.** `reconciler.backoff`,
`reconciler.failure_threshold` and `reconciler.failure_window` override the numbers above; a zero
value means the shipped default, so an install that sets nothing gets the requirement.

They exist for one reason. The acceptance test for R-151 — a crash-looping app reaches `failed` and
stays there — has to wait out the real schedule, and at these numbers that is **forty minutes of a
forty-three minute suite**. The test asserts the state machine and was paying for the durations.
Compressed to `0s,1s,2s,3s,4s` it runs in three minutes and asserts exactly the same transitions:
changing how long each step waits changes nothing about which step comes next.

There is no floor, because a floor would put the schedule back out of reach of the test that needed
it. Instead Pando **warns at startup** when the cap is below `MinProductionCap`, naming it as a
testing setting — an install retrying a broken app every second forever is a real way to melt a host,
and this is exactly the kind of line that gets copied out of a test compose file into a real one.

**[P]** `MinProductionCap` is **30 seconds**, compared against the last step of the schedule. Not the
shipped 5-minute cap: an install that has deliberately tuned backoff down to a minute is making a
reasonable choice and should not be warned at every start, and a warning that fires on reasonable
settings is a warning people learn to scroll past. Thirty seconds is where the schedule stops being a
tuning choice and starts being a load generator.

**[D]** Counter resets when the app reaches `running` with health passing. A flapping app that recovers between failures still accumulates toward the threshold, which is correct — flapping is a failure mode.

**[D] The window is measured from the last failure, not the first.** Measured from the first, the
threshold is arithmetically unreachable and R-150 never fires: backoff caps at five minutes, so ten
attempts span `5+15+60+300×6` seconds — **31.3 minutes** — against a window that resets at 30. The
app is retried forever, which is the outcome R-150 exists to prevent. As an idle timeout it means what
the requirement means: consecutive failures always reach the threshold however long backoff stretches
them out, and unrelated failures a day apart never accumulate. Both published numbers are unchanged,
and backoff can be retuned without the coupling returning. See
[notes](notes-give-up-threshold-was-unreachable.md).

**[D] The counter counts attempts, not `Apply` errors.** A crash-looping app — the ordinary failure
mode — has a workload that exists, has exited, is recreated, and exits again. `Apply` *succeeds* every
time, because the container really is created. Counting only errors means nothing is ever counted and
the threshold is unreachable for a second, independent reason. An app that needed correcting was not
working; whether the correction returned an error is a detail of how it was not working.

**[P]** `RestartSettleWindow = 30s`. A workload that has restarted before and started again moments
ago is looping, not recovered. Pando sets a restart policy on its containers — wanted, because the
runtime recovers faster than a 15s tick and keeps doing it while Pando is away (§2.1.1) — so a
crash-looping app is briefly `Running` between crashes. A tick landing in that window would read it as
recovered and clear the count, and the app would never reach `failed`: every glimpse of it up undoes
the progress toward giving up on it. `ObservedWorkload.RestartCount` and `StartedAt` are what make the
distinction, and both were already observed.

**[D]** On reaching the threshold: transition to `failed`, fire a notification, write an audit event, **and stop**. No long-interval retry (R-151).

---

## 3. Deployment

A deployment is an operation somebody asked for, not the reconciler's work. The reconciler skips apps in `deploying`.

**[D] Deployments are queued (O-32, issue #72).** Steps 1–7 run in the request. A deploy that passes
them is recorded `pending` with no replica and the request returns 202; steps 8–16 run when a replica's
deploy queue claims it (`FOR UPDATE SKIP LOCKED`, `deploy.Queue`). Each replica runs at most
`work.deploys` at once **[P: one per CPU, at least two]**, so N replicas run N times as many and none
takes on more than it can build. Detection is queued the same way (`detection.Queue`,
`work.detections`). An app still has at most one deploy in flight — queued counts — and a second is
refused, as before (§5's reasoning). A queued deploy survives a restart of the replica that took the
request: nothing is lost until something claims it.

**[D] A deploy whose replica stops is resumed, not failed.** The claimant's heartbeat is the lease. The
leader's sweeper first records a replica silent past `state.ReplicaStale` as stopped (so one that was only
paused restarts at its next heartbeat instead of carrying on), then puts that replica's claimed deploys
back in the queue, where another replica starts them again from step 8. That is safe because every step
before the commit repeats cleanly: fetching a pinned commit and building it give the same image, a scan of
the same source is reused, provisioning finds the instance it made, and applying converges on the spec
whatever an interrupted apply left behind (recreate, R-144). After `state.MaxAttempts` (3) claims the deploy
is recorded as interrupted instead, so a build that takes down whichever replica runs it does not go round
for ever. A replica shutting down cleanly cancels its deploys and hands them straight back, uncounted.
A replica that lost its claim can no longer move the deploy's status (`SetStatus` and `Finish` are
fenced on `replica_id`).

```
1.  Validate spec                          → VALID_*
2.  Evaluate host policy                   → POLICY_*
3.  Check source allowlist (pre-clone)     → POLICY_SOURCE_NOT_ALLOWED   (R-092)
4.  Resolve adapters, check capabilities   → PLAN_CAPABILITY_UNSUPPORTED (R-254)
4b. Resolve egress against policy          → PLAN_EGRESS_LOOSENING_FORBIDDEN (R-183)
                                           → PLAN_CAPABILITY_UNSUPPORTED (R-186)
5.  Check isolation floors (build+runtime) → PLAN_NO_ADAPTER_MEETS_POLICY (R-024, R-114)
6.  Check every required slot is resolved  → PLAN_SLOT_UNFILLED          (R-132)
7.  Check capacity                         → CAPACITY_WOULD_OVERSUBSCRIBE (R-242)
    ── plan boundary: nothing has been created yet ──
7b. Needs approval? → deployment awaiting_approval; stop here (§3.3, R-154)
8.  Clone source, resolve ref → commit SHA
9.  Build (isolated, no socket)            → BUILD_*                     (R-024, R-112)
10. Provision unfilled provisioned slots
11. Fetch secrets, materialize env
12. Ensure volumes
13. Apply bundle (strategy per R-144/145)
14. Ensure route
15. Wait for health
16. Commit: pin spec, update state, audit
```

**[D]** Steps 1–7 are the `:plan` endpoint (§04 2.3). Everything before the plan boundary is side-effect-free, which is what makes plan-time failure meaningful rather than a label on a mid-deploy crash.

**[D] Resolved (O-10): newly-violating apps keep running and fail at the next plan.** The question was
what happens when policy is applied to an install with running apps that violate it — block, force a
change, or report. The answer falls out of two decisions already made rather than needing a mechanism
of its own.

Running apps are untouched, because the reconciler may create and start things but never destroys
anything a human may have wanted (§2.1). Killing a running app because an admin saved a policy is the
most destructive thing Pando could do, and it would do it to *every* violating app at once.

The next deploy of a violating app fails at step 2 with a `POLICY_*` error, because policy is
evaluated live at plan time and is deliberately not stored in the spec (§01 1) — which is exactly what
R-274 asks for. So the effect is: report now, block on next deploy. Nothing is forced, nothing is
killed, and the violation is visible immediately rather than discovered at deploy time.

**[D]** The console lists violating apps when a policy is saved, before it is saved. An admin
tightening a policy is entitled to know it will block four apps' next deploy, and finding out one
deploy at a time is how a policy gets rolled back in anger.

**[D]** Step 9 failing leaves the running app untouched (R-146). Steps 12–14 failing is where recreate's downtime cost is paid.

### 3.1 Recreate

```
stop old workloads → apply new → wait for health
```

**[D]** Default (R-144). If health never passes: `degraded`, and auto-rollback only if opted in (R-147). The app is down in the meantime — this is the accepted cost.

### 3.2 Start-then-swap

```
apply new alongside old → wait for health → repoint proxy → stop old
```

**[D]** Opt-in only (R-145). Requires `RuntimeCapabilities.SupportsStartThenSwap`. Failure to become healthy leaves the old version serving and the deployment marked failed — no state change to the app.

**[D]** The console must show the R-145 warning text at the point of enabling, not in a tooltip: *two copies of your app run at the same time during a deploy. Do not enable this if your app writes to a local file or runs migrations on startup.*

### 3.3 Approval and egress

**[D] A deployment has a lifecycle of its own, and approval lives entirely in it.**

```
            needs approval (step 7b)                 enough approvals: plan re-run
request ──────────────────────────> awaiting_approval ─────────────────────────> pending ──> building ──> applying ──> succeeded | failed
   │                                   │  │  │  │
   │ no approval needed                │  │  │  └─ re-run plan refuses the revision ──> failed (deployment)
   └──────────────> pending            │  │  └──── newer request or deploy ─────────> superseded
                                       │  └─────── any rejection ──────────────────> rejected
                                       └────────── expiry passes ──────────────────> expired
```

The **app's** state machine (§1) is untouched. A deploy waiting for approval does not move the app to
`deploying`, and the app keeps running what it ran. `rejected`, `expired` and `superseded` are terminal
for the deployment and say nothing about the app. In particular approval adds **no path to `failed`**
(R-151): nothing about waiting, being refused, or timing out is an app failure.

**[D]** Whether a deploy needs approval is `approval.Reasons(policy, app, running, next)`, decided after
the plan succeeds (step 7b): host policy for every app; host policy for this app; the app's own
`deploy.require_approval` in the running **or** the next spec; a **new** egress loosening under
`egress_loosening: approval` (R-154). A loosening the running spec already carries was approved when it
first ran. "Running" is the spec of the app's newest **successful** deploy, not its pinned spec: a
deploy normally deploys the pinned spec, and comparing a revision with itself would never find a
loosening new. A pinned spec that asks for approval still counts when nothing has run it yet. The request copies the approval count and expiry from policy (design 02 §2.3). Approving
re-runs steps 1–7 against policy as it now is (R-156) and continues at step 8 with the same spec
revision. If that plan refuses the revision itself (a `PLAN_*`, `POLICY_*`, `VALID_*` or `CAPACITY_*`
code), the approval is recorded and the deployment ends `failed` with that code: no further approval
could make it pass, and fixing it makes a new revision anyway. If it cannot plan at all (an adapter
unreachable), the request keeps waiting. Approving while another deploy of the app is in flight is
refused and the request keeps waiting; the start re-checks, so two approvals arriving together start
it once. Exempt: rollback to a revision that previously ran, restarts, and secret rotations (R-157).

**[D]** A newer request for an app supersedes an older `awaiting_approval` one (R-156), and so does a
deploy that needs no approval: approving the old request afterwards would put back an older revision.
An **expiry sweep**, once a minute beside the auto-deploy job, moves requests past
`approval_expires_at` to `expired`, and approving or rejecting checks expiry too; a NULL expiry waits
until answered. A restart's abandon step leaves waiting requests alone: they are not in flight.
Every request, approval, rejection, expiry and supersession is an audit event — `deploy.request`,
`deploy.approve`, `deploy.reject`, `deploy.expire`, `deploy.supersede` (R-159).

**[D] Egress rules take effect at deploy and are recorded there.** The deploy runner resolves them with
the same `Planner.Egress` the plan showed, so what was shown is what runs, and writes them to
`deployments.egress_rules`. The reconciler converges toward the **recorded** rules (`state.Reconcilable`),
never a fresh resolution against today's policy, so a policy edit does not change a running app
underneath it (R-183, O-10). When policy stops permitting a loosening an app runs with, the app keeps
running it and its next deploy is refused at step 4b, naming the entries.

---

## 4. Health

**[D]** Source precedence (R-221): compose healthcheck → configured HTTP endpoint → TCP connect → process liveness.

**[P]** Defaults: 30s interval, 5s timeout, 3 consecutive failures to mark unhealthy, 1 success to mark healthy.

**[D]** Health is observed by the runtime adapter and reported through `ObservedWorkload.Healthy`, a nullable bool. `nil` means no signal available, which is **not** unhealthy — an app with no health check is `running`, not perpetually `degraded`. This is also why auto-rollback defaults off (R-147).

**[D]** Restarting is not healthy, per §1.1's "health failing **or** restarting". A nil `Healthy` on a
workload that is in a restart loop must not read as "no signal, therefore fine" — see the
`RestartSettleWindow` note in §2.2 for why this is the difference between an app reaching `failed` and
being retried forever.

---

## 5. Triggers

**[D]** Auto-deploy (R-141) is a separate scheduled job, not the reconciler. It never modifies a running app directly — it creates a spec revision with the new commit SHA and enqueues a deployment in the deploy queue (§3). Everything then flows through the normal path, including plan-time checks.

**[P]** Poll interval: 5 minutes for branch tracking, 15 for release tags. Apps are checked eight at a
time (`work.auto_deploy`): in series, a `git ls-remote` per app outlasted the interval past a few
thousand apps (issue #72).

**[D]** If a deployment is already in flight for an app, the trigger is skipped, not queued. Queued auto-deploys on a fast-moving branch produce a backlog nobody wants.

**[D]** The same reasoning keeps auto-deploy and approval apart (R-158). A spec save refuses
`auto_deploy.enabled` while the app needs approval for every deploy (`approval.BlocksAutoDeploy`; an
egress loosening is not such a reason, since it needs approval only for the deploy that introduces it).
When policy starts requiring approval for an app that already auto-deploys, the job **skips** it rather
than queuing a request per push, and `GET /apps/{id}/status` reports `auto_deploy_paused` so the
console can say why nothing is deploying.

---

## 6. Garbage collection

**[P]** Leader jobs (issue #72), each on its own clock so a slow one does not hold up the rest:

- **GC**, hourly: spec-revision pruning, reclaiming backed-up storage of deleted apps, and the security
  pass, which places every app against the threshold in one batched call (`security.Place`) rather than
  three queries per app. **Teardown** of deleted apps runs on a loop of its own — at once when a delete
  asks, and every minute — so a delete is not queued behind the hourly pass.
- **Rolling backups**, every five minutes: asks for a page of apps *due* one (no rolling backup in 24
  hours, no attempt in the last hour) and takes them two at a time (`work.backups`). Then expiry.
- **Retention**, hourly: batched deletes of what only grew — deploys, scans, sessions, notifications,
  idempotency keys, sign-in flows, the event outbox, and deleted apps' detections and backup records. The
  windows and what each never removes are in design 02 §2.11.

What those jobs do:

- Trim logs to `Retention.LogBytes` per app (R-223), and to the aggregate host disk budget (R-224). **Aggregate wins.** If total retention exceeds the disk budget, every app's cap is scaled down proportionally rather than letting one app's allowance brick the host. **Not implemented — see O-16.** Pando does not hold app logs; it streams them from the runtime, and there is no mechanism on the adapter interface to trim them. Scaling caps down proportionally would mean recreating every container, which is destruction on a schedule triggered by an unrelated app being chatty.
- Take a rolling backup of each `running` or `degraded` app with storage that has none from the last
  24 hours (R-211). A stored spec with no `backup_daily_count` is read as the default of 7, not as
  "keep none": no stored spec can ask for zero, because defaults turn a zero into 7 before it is
  stored. Every attempt, taken, skipped or failed, is recorded in `backup_attempts` with why and what
  to do (issue #87); "took a rolling backup" is logged only when one was.
- Expire rolling backups past `BackupDaily` (R-211). Never touches `kind = 'on_delete'` (R-204).
- Prune spec revisions past `SpecRevisions`, skipping any revision that was ever pinned.
- Reap idle per-user instances **[LATER]** (R-293).

**[D]** R-224 is the reason this job exists. Log retention that is per-app only can still fill a disk with twenty apps. The aggregate check is the real constraint and the per-app cap is a fairness mechanism under it.
