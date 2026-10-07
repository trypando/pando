# Running Pando as several replicas (issue #72)

Issue #72 asked whether Pando's own process can run as more than one copy against one Postgres —
the test case being a Kubernetes `Deployment` with `replicas: 3` — and to fix what stops it. This
note is the verdict. It is about Pando's control plane and proxy, not the apps Pando hosts: app
replicas remain out of scope (R-010, R-099, R-153).

## Verdict

**Supported [P]: N replicas of the `pando` process, one Postgres, one Docker host, behind a load
balancer.** Rolling restarts, scaling between one and N, and losing a replica outright (SIGKILL, no
shutdown) leave the install working. `make test-replicas` demonstrates it: it runs two replicas
behind HAProxy, restarts them in turn, kills one in the middle of a deploy, scales to one and back,
and then runs the design 07 sequences through the same balancer.

What this buys is availability of the control plane and the proxy: an upgrade, a crash or a lost
container no longer takes every app offline while Pando restarts, which is the outage note in
design 00 §1.4. It is not more capacity for apps. Apps still run where the runtime adapter puts
them, and with the Docker adapter that is one Docker host.

**Not supported by this PR** — each is a later PR in the stack (below):

- **Replicas on different Docker hosts.** The Docker runtime adapter reaches apps by joining each
  app's bridge network as Pando's own container (design 03 §4, `attachProxy`). A replica on another
  host cannot join that network, so its proxy cannot reach the app. The fix is a runtime adapter
  that spans machines, which is what R-256 already says multi-machine capability is: PRs 6 and 7.
- **Kubernetes as the place the replicas run, with no Docker host beside it.** A pod has no Docker
  socket, and pods on different nodes cannot share one host's bridge networks. PR 6 (O-33).
- **Horizontal throughput of deploys and detections.** Each runs in the process that started it, as
  before. N replicas share the requests, not a work queue. PR 4 (O-32) adds the queue; see
  [PR 4](#pr-4-background-work-and-data-growth) below.

**Not supported at all: the in-place upgrade (R-359) with more than one live replica.** It replaces
the one container it runs in. The upgrade plan now says so and refuses; upgrade by rolling every
replica to the new image with whatever runs them.

## What a replica needs

| Prerequisite | Why | In the shipped Compose overlay |
|---|---|---|
| One Postgres for all replicas | All state is there: sessions, tokens, specs, audit, the replica table | the `postgres` service |
| The same Docker daemon | Runtime, builder and services adapters are Docker adapters on the local socket | the socket, mounted as before |
| The same `/var/lib/pando` | The secrets key, uploaded sources, local backups, build cache, audit archives and Traefik's dynamic config are files there (below) | the `pando-data` volume, shared |
| A load balancer with a health check on `/healthz` | Requests may land on any replica; no stickiness is needed | `lb` (HAProxy, `test/replicas/haproxy.cfg`) |
| The app port range forwarded too, if port-mode routing is used | Every replica listens on every allocated port | `lb` publishes it |
| `PANDO_SERVER_ADVERTISE_URL` reachable between replicas | A deploy's live log is read from the replica running the deploy | default `http://<hostname>:8080`, which resolves on a Compose network; in Kubernetes set `http://$(POD_IP):8080` |

**Shared files rather than object storage [P].** Moving uploads, backup staging and keys into
Postgres or an object store would remove the shared-volume prerequisite at the cost of a storage
adapter category Pando does not have. Every replica in the supported topology is on one Docker host,
where a shared named volume costs nothing, so the volume is the requirement. The secrets key is the
one file that must never move into Postgres — R-190's threat is a leaked database dump — and it is
now checked (below).

## What broke, and what changed

### Broke as soon as a second replica started

| Problem | Fix | Test |
|---|---|---|
| **Every start gave the database roles a new password** (`provisionRole`), held only in memory. The second replica's start refused every new connection the first made. | The password is kept in `pando_private.role_passwords`, a schema nothing is granted on, and reused while it works. Bootstrap — migrate, provision, grant, verify — runs under an advisory lock so replicas starting together take turns. The DR bundle's `pg_dump` excludes the schema, so a bundle carries no database password (R-194). | `TestR256_ASecondReplicaStartingDoesNotLockTheFirstOut`, `TestR348_TheApplicationRoleCannotReadTheStoredRolePasswords` |
| **Each process signed assertions with its own key** and published only that key. An app holding one replica's JWKS could not verify another's assertion (R-051). | Each replica still signs with a key that never leaves its memory, and publishes the public half in `pando_replicas`. Every replica's JWKS lists every key that may have signed a still-valid assertion: live replicas', and stopped ones' for `assertion.Lifetime` after. The private key is never stored, so nothing about R-212's bundle changes. The JWKS `max-age` drops from 300 to 60 seconds, under an assertion's lifetime. | `TestR051_AnyReplicasJWKSVerifiesAnyReplicasAssertion`; the JWKS step of `TestR256_PandoServesAsSeveralReplicasAgainstOneDatabase` |
| **The secrets key is a local file, generated if missing.** A replica with its own volume generated its own key and wrote secrets nobody else could read. | The key stays a file — in Postgres it would protect nothing (R-190). Replicas must be given the same one (the shared volume, or one mounted Secret). Every replica proves it at start by opening a canary sealed with the install's key (`secrets_canary`), and refuses to start, naming the remedy, if it cannot. Creating the key is now atomic (write, then `link`), so two replicas on an empty shared volume end with one key. | `TestR190_AReplicaWithADifferentSecretsKeyRefusesToStart` |

Two more broke on the second start and were not in the issue:

| Problem | Fix | Test |
|---|---|---|
| **Startup failed every in-flight deploy and detection**, including the ones another live replica was running. | Deploys and detections record the replica running them (`replica_id`). Only work whose replica has stopped or gone silent for `state.ReplicaStale` (45 s) is recovered — at startup, and every 15 s by the leader, because a lost pod is not followed by a restart of itself. PR 4 changed recovery from "recorded as interrupted" to "put back in the queue" (below). | `TestR256_OnlyAStoppedReplicasWorkIsRecovered`; the lost-replica step of the topology test |
| **`PANDO_ADMIN_PASSWORD` made the first administrator on every replica that started at once.** All but one failed on the username and did not start. | `bootstrap.Run` takes the same lock as the setup form (`state.FirstAccountLock`). | `TestR046_ReplicasStartingTogetherMakeOneAdministrator` |

### Races and duplicated work

**[P] One leader for the install-wide jobs, rather than a lock per job.** A replica leads while it
holds a session advisory lock on a connection of its own (`state.Replicas.TryLead`). A dead process's
connection closes and Postgres releases the lock; no lease or clock is involved. A leader that loses
its connection cancels its jobs and waits for them to stop before anyone else can start them.

| Loop | Runs on | Why |
|---|---|---|
| Reconciler | every replica | Already locks per app (`Reconciles.Lock`); replicas share the apps |
| Deploy and detection queues (PR 4) | every replica | Claim with `FOR UPDATE SKIP LOCKED`, each up to its own limit |
| Event delivery | every replica | Claims with `FOR UPDATE SKIP LOCKED` |
| Network rejoin | every replica, every 15 s | Each replica's container must be on every app's network (below) |
| Port listeners | every replica | Each replica is a front door; the balancer forwards the range |
| Update check | every replica | Its result is kept in memory and each replica answers from its own |
| Heartbeat and restart watch | every replica | Membership is per process |
| GC (teardown, pruning, volume reclaim, security pass) | leader | Destructive, and was running once per replica |
| Rolling backups (PR 4: split out of the GC) | leader | Once per install |
| Retention, including the event outbox (PR 4) | leader | Destructive; the outbox prune ran on every replica |
| Auto-deploy | leader | Deployed one commit once per replica |
| Audit retention, approval expiry, adapter health events, edges, upgrade loop | leader | Once per install |
| Sweep of stopped replicas' work; pruning old replica rows and passcode failures | leader | Once per install |

**Network reclaim at startup** now skips the network of any app with a deploy in flight. Another
replica's deploy makes the app's network before its containers, so for a moment it is empty and
container-less and looks like a dead app's; a deleted app has nothing in flight.

### Per-process state a load balancer split

| State | Now |
|---|---|
| **Deploy logs** in memory | **[P] Stay in memory, and the request goes to the replica that has them.** A replica asked for a log it does not hold relays the request, credentials included, to the advertise URL of the replica running the deploy, which authorizes it again. A deploy whose replica has since stopped answers with a line saying so instead of a stream that never ends. Postgres was the alternative and was not taken: the state store guide lists log storage as deliberately absent (R-224's disk accounting), and a deploy's outcome and error are already on the deploy row. |
| **Deploys and detections** in request goroutines | A queue in Postgres since PR 4 (O-32): any replica with room runs them, and a lost replica's are resumed elsewhere. |
| **Passcode limiter** per process | In `passcode_failures`, so N replicas give an attacker one allowance, not N (R-075a). `TestR075a_WrongPasscodesAreCountedAcrossReplicas`. |
| **Adapter registry**, and `POST /restart` reaching one replica | The restart is recorded in `cluster_signals`; every replica started before it restarts at its next heartbeat (≤10 s), so replicas do not drift onto different adapter configurations. They restart close together rather than in turn, which is a short gap if every replica restarts at once. |
| **App networks** joined by whichever replica deployed | Every replica rejoins every app network that has one of the app's containers on it, every 15 s. A new app may answer 502 from another replica for up to that long. A network with only Pando replicas on it is not rejoined, so a deleted app's network empties as replicas roll and is reclaimed. |
| **Loopback routing table** | Not read by anything that matters across replicas: the proxy resolves every request from Postgres (`proxy.StateResolver`), and nothing sets route presence for drift. |
| **Proxy counters** | Per replica by design. They are evidence for R-023 — every request that replica served — and stay correct per replica. |

### Accepted costs

- A deleted app's network stays until every replica that joined it has been replaced, not until the
  next restart of one process. Pando's own address pool (10.213.0.0/16 in /26 blocks) holds about
  1,000; PR 3 widens it.
- A request already relayed for a deploy log is cut if the replica holding it stops; the reader
  retries and gets the "has since stopped" line.
- Restoring a DR bundle (R-212) replaces `pando_replicas` with the bundle's. Every replica then finds
  its row gone at its next heartbeat and restarts, which is what a restore should cause anyway.

## Requirements

- **R-256 [P]** is amended in the same change: a single control plane may run as several replicas
  sharing one database. They are one control plane — one database, one leader for install-wide
  work, one policy and one audit log — and none of them models a host or places a workload.
- **R-010 and R-153** are unchanged. Running several copies of Pando is not scheduling apps:
  nothing in this change decides where a workload runs, and `pando_replicas` is read by nothing
  that plans or places one.
- **R-013:** Pando is still not a disaster-recovery product. Replicas protect against losing a
  Pando process, not against losing the database or the Docker host; Postgres availability is the
  database's (a managed Postgres, or Postgres's own replication), and host loss is still what the DR
  bundle is for.
- **R-023, R-053, R-173:** every replica runs the same proxy with the same stripping, and the
  topology test is followed by the full design 07 run — forged headers and cookie stripping included
  — through the balancer.
- **R-027, R-152:** both are database mechanisms (grants and a trigger), and hold however many
  writers there are. An audit event's ID comes from one Postgres sequence and its `occurred_at`
  from the database's clock, never a replica's — the same as one process writing from many
  goroutines, which it already was.

## Is all of Pando scalable? The rest of the stack

Replicas answer one question — can Pando's own process be more than one copy. Issue #72 asks the
larger one: can a whole organization put its apps on one Pando install. That was audited end to end
(request path, app capacity, background work, data growth) and the answer is **not yet**. This PR is
the first of a stack; each later PR is based on the one before, and #72 closes with the last.

**Targets [D].** Two tiers, which the load harness proves:

| Tier | Users | Apps | Concurrent console users | Runtime |
|---|---|---|---|---|
| Single VM | ~3,000 | ~1,000 | — | Docker on the VM |
| Cluster | ~100,000 | ~20,000 | ~5,000 | Kubernetes, or Docker on several hosts; N Pando replicas |

**Decisions [D]** (the issue's author, for this stack):

- **Several machines without Kubernetes are supported** by a multi-host Docker runtime adapter, as well
  as by a Kubernetes one. Placement is the adapter's; core still places nothing (R-256).
- **Built images go to a registry**, which Pando runs by default and setup may point at the
  organization's own. Both multi-machine adapters need it: neither can load a built image into the
  host the way single-host Docker does.
- **Capacity is not oversubscribed by default** (R-242), and host policy or config may allow CPU and
  memory oversubscription. Disk is never oversubscribed: it is not a reservation, and a full disk
  stops everything.
- **API tokens are hashed with HMAC-SHA-256**, not argon2id. They are 256-bit random secrets Pando
  generates, so a slow hash adds nothing but cost (about 64 MiB per concurrent request). Passwords
  stay argon2id.
- **Anonymous data-plane denials stay audited by default**, and host policy or config may turn that
  off, since anyone can cause one write per request.

**What the audit found, and which PR fixes it:**

| PR | Fixes |
|---|---|
| 1 (this) | Replicas, above. The application pool's size is now set (`PANDO_DATABASE_MAX_CONNS`, default 32): pgx's default of the CPU count could be used up by the reconciler's held locks plus the leader's |
| 2 — request path and a load harness | A new HTTP transport per proxied request (no upstream connection reuse); hostname and port app lookups scanning every pinned spec's JSON; `AppVerbs` running the full control check sixteen times per request; the launcher query and unpaginated user, group, app and approval lists; console polling that calls Docker on every `/status` and `/usage`; token hashing and the per-request `last_used_at` write; the anonymous-denial audit toggle; a proxied websocket not closing when its session is revoked or its user suspended (R-048, a security fix); the deploy log store never freeing a finished deploy. The harness seeds the two tiers and drives proxy, API and console traffic through the replicas stack |
| 3 — single-host capacity | **The reconciler only ever visits 200 apps**: `Due` orders by `updated_at`, which a healthy pass never changes, so past 200 apps the rest are never observed or repaired. Replaced by a lease column claimed with `SKIP LOCKED`, which also spreads apps across replicas and stops holding a connection per app. The oversubscription toggle. Smaller subnets (/28) and a shared egress bridge, lifting the network pool from about 1,000 apps to about 4,000. The rejoin loop inspecting every network every 15 s; unbounded usage sampling; the per-app build cache with no total cap |
| 4 — background work and data growth | **Done** ([below](#pr-4-background-work-and-data-growth)). A bounded deploy and detection queue in Postgres that any replica takes from (O-32), so a lost replica's deploy resumes elsewhere. GC, rolling backups and auto-deploy made concurrent and due-driven rather than serial over every app. A retention job for the tables that only grow (deployments, sessions, notifications, idempotency keys, sign-in flows, the event outbox, scans, deleted apps' detections) — never spec revisions (R-152). Missing indexes: `event_deliveries(event_id)`, in-flight `deployments`, `deployments(spec_id)`, `apps(owner_user_id)` |
| 5 — image registry | The registry above, and builds that push and pin by digest |
| 6 — Kubernetes runtime adapter | O-33, with a design note first: a Service per app reachable only from Pando's proxy (R-023), NetworkPolicy for R-025, PersistentVolumeClaims |
| 7 — multi-host Docker runtime adapter | Docker on several hosts, with placement in the adapter and a design for how the proxy reaches an app on another host without routing around Pando |

## PR 4: background work and data growth

Migration `000049_work_queue_and_retention` (numbered past 000046–000048, which other PRs in the stack
may take; the numbers only need to be distinct and ascending when the stack lands).

### The deploy and detection queue (O-32)

| Decision | What | Why |
|---|---|---|
| **[D] Queued in Postgres** | A deploy is created `pending` with no `replica_id`; detection is `running` with none. Every replica's queue (`internal/core/work.Pool`) claims what it has room for with `FOR UPDATE SKIP LOCKED`, stamping `replica_id`, `claimed_at` and `attempts`. Approval's `StartApproved` and auto-deploy queue the same way. | Any replica runs any deploy; none runs more than its limit; a deploy queued when every replica was busy, or whose requesting replica restarted, still runs. |
| **[P] Limit per replica** | `work.deploys` and `work.detections`, default one per CPU, at least two. | A build already spreads across cores; more than one per core mostly slows every build. Raising the limit is a setting, not a code change. |
| **[D] The lease is the replica's heartbeat** | No per-row lease column. A claim is held while its replica heartbeats; the leader's sweeper records a replica silent past `ReplicaStale` (45 s) as stopped (`Replicas.StopSilent`) before recovering its work, so a replica that was only paused restarts at its next heartbeat. | One heartbeat per replica rather than one write per running deploy every few seconds, and the same liveness PR 1 already relies on. |
| **[D] Resume, not record-interrupted** | A stopped replica's claimed deploys and detections go back in the queue and start again from the top on whichever replica claims them. After `state.MaxAttempts` (3) claims, the deploy is recorded as interrupted ("Pando stopped while this deploy was under way, 3 times…"). | Every step before a deploy commits is safe to repeat: fetch and build of a pinned commit are idempotent, a scan of the same source is reused, provisioning finds what it made, and applying converges (recreate, R-144). The cap stops a build that kills its replica (out of memory) from killing each replica in turn. Detection writes only its own row, so it always resumes. |
| **[D] Shutdown hands work back** | A replica stopping cancels its running deploys, waits up to ten seconds, and releases them uncounted (`Deployments.Release`). | A rolling restart is not a deploy's fault, and another replica takes the deploy at once rather than 45 s later. |
| **[D] Fencing** | `SetStatus` and `Finish` only write a deploy this replica claimed, or one nobody has. | A replica that lost its claim while paused cannot move a deploy another replica is now running. Its other side effects are bounded by its restart at the next heartbeat (≤10 s). |
| **[P] Live log of a queued deploy** | Following a deploy nobody has claimed waits (up to a minute) for a replica to claim it before relaying to that replica, as PR 1 relays any deploy log. | Until a claim nobody holds the log, and answering "this replica" would follow a log another replica is about to write. |
| Clones in `server.work_dir` | `source.Sources.WorkDir`; the system temporary directory only when the work directory cannot be made. | Several concurrent clones fill a small tmpfs. |

Handlers contain none of this: `POST /apps` and `POST /detection/rerun` call `DetectionQueue.Enqueue`,
and deploys start through the approval service as before (R-261). Idempotency keys (R-262) and the
approval flow are unchanged; an app still has at most one deploy in flight, queued included.

### Leader jobs

| Was | Now |
|---|---|
| Rolling backups for every app with storage, in series, inside the hourly GC | Its own job every 5 minutes: one query for a page of apps **due** a backup (none in 24 h, no attempt in the last hour), taken `work.backups` (2) at a time, then expiry |
| Security pass: a report — policy load plus two queries — per app | One query for every app's state and score, one `security.Place` call |
| Teardown of a deleted app waited while `Collect` ran | Its own loop in the GC job: at once when a delete asks, and every minute |
| Auto-deploy: `git ls-remote` per app in series | `work.auto_deploy` (8) at a time, into the queue |
| Event routing read every enabled subscription with three joins each pass, and matched every event against every subscription | The list is cached for 3 s and indexed by event name. An event newer than the cached list (less a second for a transaction still committing) waits for the next list, so a subscription never misses an event that happened after it was made (R-367). Authorization is still checked at send (R-368), and a disabled subscription's deliveries are never claimed |
| Delivery claimed a batch and waited for the slowest send in it | Senders pull continuously, eight at a time |
| Outbox prune on every replica, one unbounded `DELETE` cascading into `event_deliveries` with no index on `event_id` | The retention job, 1,000 events at a time; `event_deliveries(event_id)` indexed |

### Retention

One leader job, hourly, deleting 1,000 rows at a time until a batch comes back short. The windows are
`retention.*` settings, defaults **[P]**; what each never removes is in the query, not the setting
(design 02 §2.11): 50 deploys per app, keeping any in flight and the newest successful deploy of every
revision ever pinned (R-157); scans 90 days, keeping each revision's newest (R-319); expired or revoked
sessions 30 days; idempotency keys a day (the sweep existed and was never called); sign-in flows a day
past expiry (moved out of every sign-in); notifications past their `retain_until` (set, and nothing
read it); the event outbox 30 days, taking deliveries and their attempts by cascade; deleted apps'
detection and backup-attempt rows 30 days. **Spec revisions are never deleted by it** (R-152), and the
audit log keeps R-347's archiver.

### Tests

`TestR256_TheDeployQueueRunsNoMoreThanItsLimit`, `TestR256_AStoppingReplicaHandsBackWhatItWasRunning`,
`TestR256_TwoReplicasNeverClaimOneDeployment`, `TestR256_ADeadReplicasClaimedDeployIsResumedElsewhere`,
`TestR256_AReleasedDeployIsNotCountedAsAnAttempt`, `TestR256_QueuedWorkSurvivesARestart`,
`TestR256_QueuedDeploysRunOnceOnWhicheverReplicaHasRoom`, `TestR256_ADeadReplicasDetectionIsResumedElsewhere`,
`TestR315_TheSecurityPassPlacesEveryAppInOneCall`, `TestR224_ADeleteIsTornDownWhileTheSlowPassRuns`,
`TestR141_AutoDeployChecksAppsConcurrentlyWithinItsLimit`, `TestR367_ACachedSubscriptionListNeverMissesALaterSubscription`,
`TestR366_PruningTheOutboxIsBatchedAndKeepsWhatIsStillOwed`, `TestR224_RetentionRemovesOldDeploysButNeverARollbackTarget`,
`TestR224_RetentionRemovesExpiredRowsFromTablesThatOnlyGrew`; the lost-replica step of `make test-replicas`
now expects the deploy to be resumed by the surviving replica.

## The issue's open questions, answered

1. **Scope.** Both: surviving a rolling restart and a lost pod (this PR), and horizontal throughput
   to the two tiers above (the rest of the stack).
2. **Leader or lock per job.** A leader (above).
3. **Kubernetes without a Kubernetes runtime adapter.** Not useful except with a Docker host beside
   the cluster. PR 6 adds the adapter (O-33).
4. **Deploy logs.** Routed to the owning replica (above).
5. **Shared storage.** A shared volume (above).
