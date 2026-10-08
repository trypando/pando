# Costs that grew with every app (issue #72)

Issue #72 targets an installation of about 20,000 apps. Six paths did work in
proportion to the number of apps on every request or every tick. This note
records what each one does now, and why each choice keeps the answer it gives
the same as before.

## 1. GET /capacity: a background reading

Before: every view sampled live use from every runtime (`InUse`, which on Docker
reads stats for every running container, about a second), and summed every
running app's pinned spec JSON.

Now: `internal/core/capacity.Snapshots` holds the latest reading of every
runtime — totals, running workloads, live use — taken every 30 seconds while
somebody has asked within the last 5 minutes [P], and shared by every request on
the replica. A request that finds no reading, or one older than the idle window,
waits for a new one (shared with anyone else waiting). The response carries
`as_of` (when the runtimes were read) and `refresh_seconds`, and the console
shows both and polls at that interval.

What is committed is not part of the snapshot. It is read on every request from
the same sum the planner reads (section 2), so the screen and a refused deploy
agree to the moment (R-242).

## 2. Allocation at plan time: exact, never cached (R-242)

R-242 refuses a deploy that would oversubscribe CPU or memory (unless host policy
allows it, each separately) and always refuses one that would oversubscribe disk.
The check sums what every running, degraded or deploying app on the runtime
reserves. A cached sum would let a deploy through onto a full host, so the sum
stays live; the work it does is what changed.

Migration 60 copies what an app's pinned revision reserves — runtime ref, CPU,
memory, disk, log bytes — onto `apps` (`alloc_*` columns), set by a `BEFORE
INSERT OR UPDATE OF pinned_spec_id` trigger and backfilled. A revision never
changes (R-152), so what an app reserves changes only when its pin does, and the
trigger runs at exactly that moment in the pinning transaction: no code path can
move a pin without moving the columns. Which apps count is still decided when
the sum is taken, from `state`. The sum is then an index-only scan of
`apps_allocation_idx` (partial on live, resource-holding states, covering the
four amounts) instead of a join to every pinned revision and four JSON reads.

Alternatives considered: a per-runtime running total updated on every pin and
state change. It is cheaper to read, but every pin and every state change on a
runtime would contend for one row, and a missed transition (a state change
written by a path that forgot to adjust it) silently drifts the total forever.
The indexed sum has no such state to drift.

`TestR242_CommittedResourcesStayExactUnderConcurrentPins` pins apps concurrently,
moves them between runtimes and sizes, changes their states and rolls them back,
and checks the sum against the old JSON query each time.

Unchanged and worth knowing: a first deploy holds nothing until it is pinned, at
the end of the deploy, so two first deploys planned at the same moment can each
see room the other is about to take. That was true before this change and is
not made worse by it.

## 3. POST /policy/preview: read only what the candidate could block

Before: every live app's pinned spec, with two correlated subqueries each, on
every preview, as an administrator edits the policy form.

Now: the planner works out from the candidate policy and the adapters which apps
any of its checks could fail (`planner.InventoryFilter`), and `Apps.LiveApps`
reads only those, in one query, with public sharing read from one pass over the
anonymous grants. The checks then run, unchanged, on what was read. An app the
filter leaves out passes every check, so the result equals checking every app:

| Check | Apps named |
|---|---|
| Source allowlist (R-092) | Every app, when an allowlist is set. |
| Public sharing (R-076) | Apps shared with everyone; under passcode-only, those without a passcode. |
| Egress loosening forbidden (R-183) | Apps with egress settings of their own (only those loosen). |
| Egress restriction unsupported (R-186) | On a runtime that cannot enforce egress: apps with settings of their own, or every app when the candidate restricts an app with none. |
| Isolation floors (R-024, R-114) | Apps whose runtime's (or builder's) class is below the higher of the candidate's floor and the app's own. |

"What the changed fields can affect" was considered and rejected: an app that
already violates a field the administrator did not touch is still blocked by
the candidate, and full evaluation reports it, so narrowing by the diff would
drop it. `TestR092_APolicyPreviewReadsOnlyTheAppsItCouldBlockAndSaysTheSame`
compares the narrowed preview with full evaluation over a fixture of 48 apps and
twelve candidate policies.

The console asks less often too: the AI policy dialog asks once the kept changes
have stayed put for 400 ms, and the Policy screen's check is not offered again
for a draft it has already answered.

## 4. The security pass: one set-based query (R-315, R-316)

Before: every live app, each with a lookup into `app_scans` for its pinned
revision's newest scan, every GC interval.

Now: migration 60 keeps that scan's two scores on the app
(`pinned_scan_score`, `pinned_scan_score_fixable`), maintained by trigger when a
scan is recorded or removed and when the pin moves, computed by the same rule as
`Scans.Latest` (`pando_pinned_scan`). The pass asks only for apps it could act
on: marked (insecure since, or stopped for security — these can recover or come
due) or not meeting the threshold under policy's choice of score, including
apps never scanned. An unmarked app meeting the threshold is one the pass would
place and leave alone. The verdict is still `security.Evaluate`'s.

## 5. Webhook routing: a list kept until a change is told

Before: every replica read every enabled subscription (three joins) every
3 seconds.

Now: migration 61 adds a trigger that sends `NOTIFY pando_subscriptions` when a
subscription is made, removed, turned on or off, or given other events or
another app — not when a delivery's outcome updates its failure count. Each
replica's dispatcher LISTENs on its own connection (`state.DB.Listen`, which
reconnects with backoff) and drops its list on any notification, on losing the
connection and on regaining it. While it listens, the list is kept up to
5 minutes [P] as a backstop, and an event is routed against it if it occurred
before the list's database time plus the time elapsed on this replica since,
less the existing one-second margin for a transaction still committing. While it
does not listen, the 3-second behavior is unchanged. The `Subscriptions.List`
query itself is not changed here.

This is the first use of LISTEN/NOTIFY in Pando; `state.DB.Listen` is written to
be reused.

## 6. GET /apps/{id}/usage: capabilities from the cache

The handler asked the runtime for its capabilities and capacity on every poll.
Both now come from the observation cache (`observe.Cache.Capabilities`,
`observe.Cache.Capacity`), keyed per runtime and kept 30 seconds [P]. These are
for display; the planner still asks the runtime itself.
