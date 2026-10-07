# Load harness

Seeds a Pando install to one of the two scale tiers in issue #72, drives console, API and proxy
traffic through its load balancer in steps, and reports the step at which it stopped keeping up and
which requests and queries were responsible. The tiers come from the targets table in
`docs/design/notes-multiple-replicas-issue-72.md`:

| Tier | Users | Apps | Groups | Admins | API tokens | Real apps | Console users at peak | API req/s at peak | Proxy req/s at peak | Replicas |
|---|---|---|---|---|---|---|---|---|---|---|
| `vm` | 3,000 | 1,000 | 60 | 5 | 100 | 10 | 300 | 50 | 200 | 2 |
| `cluster` | 100,000 | 20,000 | 2,000 | 20 | 2,000 | 20 | 5,000 | 500 | 2,000 | 4 |

The targets table gives users and apps for both tiers and concurrent console users for the cluster
only. The single-VM console figure (one user in ten online), the request rates and the counts of
groups, administrators and tokens are this harness's choices, and are flags (below).

## Running it

```
make load-test TIER=vm
make load-test TIER=cluster LOAD_REPLICAS=8
```

The target:

1. builds the harness into `test/load/out/load`;
2. brings up its own stack, project `pando-load`: the shipped `docker-compose.yml`, the replicas
   overlay (`test/replicas/docker-compose.replicas.yml`), and `test/load/docker-compose.load.yml`,
   scaled to `LOAD_REPLICAS` Pando replicas behind HAProxy on port 28080. It uses the same settings
   as `make test-replicas` (`REPLICAS_ENV`) on different ports, so it can run beside that stack or a
   development one;
3. **seeds** the tier (below);
4. **runs** the ramp and writes `test/load/out/results-<tier>.json` after every step;
5. writes the **report** to `test/load/out/report-<tier>.md`;
6. deletes the real apps through the API, takes the stack down with its volumes, and removes any of
   the real apps' containers and networks that are left, by their `io.pando.bundle` label.

It does all of step 6 whether or not anything before it failed. `test/load/out/` is gitignored.

| Variable | Default | |
|---|---|---|
| `TIER` | `vm` | `vm` or `cluster` |
| `LOAD_REPLICAS` | 2 (`vm`), 4 (`cluster`) | Pando replicas behind the balancer, up to 32 |
| `LOAD_REAL_APPS` | 10 (`vm`), 20 (`cluster`) | Apps deployed as real containers, for proxy load that reaches an app |
| `LOAD_HOLD` | `3m` | How long each step is measured, once its users are online |
| `LOAD_ARGS` | | More flags for `run`, such as `-keep-going` or `-steps 0.5,1` |
| `LOAD_PORT`, `LOAD_APP_PORT_START`, `LOAD_DB_PORT` | 28080, 29000, 25432 | Host ports for the balancer, the real apps and Postgres (loopback only) |
| `LOAD_DIR` | `test/load/out` | Where results and reports go |

Postgres is configured with `max_connections` of 40 per replica plus 60, so that every replica's
pool (`PANDO_DATABASE_MAX_CONNS`, 32) fits. The shipped default of 100 would be exhausted at three
replicas, which is a configuration finding rather than a load one; the report still flags any step
that uses 90% of what is allowed.

### By hand, against a stack already up

The subcommands take the database as its owner (`-db`, or `$LOAD_DATABASE_URL`) and the balancer
(`-url`). Every flag is listed by `-h`.

```
go run ./test/load seed    -tier vm -db "$DB" -url http://localhost:28080 -admin-password "$PASSWORD"
go run ./test/load run     -tier vm -db "$DB" -url http://localhost:28080 -out results.json
go run ./test/load report  -in results.json -out report.md
go run ./test/load cleanup -db "$DB" -url http://localhost:28080 -admin-password "$PASSWORD"
```

`report -in a.json,b.json` puts several runs in one report, such as both tiers, or one tier before
and after a change.

## What it seeds

`seed` claims setup on a fresh install (R-046) with `-admin-password`, then writes directly into
Postgres as the database owner, using the real schema and real prefixed ULIDs (`internal/id`):

- **users**, local accounts named `load-u000000` onward, all with one password
  (`-user-password`). They share one argon2id hash: a hash carries its own salt and parameters, so
  one verifies for every account, and hashing a hundred thousand would take longer than the run;
- **groups** `load-g00000` onward, each user in one to three;
- **administrators**: the first `-admins` users hold the built-in administrator role install-wide;
- **apps** `load-a00000` onward, each owned by a user, with one pinned spec revision (the replicas
  test's prebuilt nginx spec) and a `spec_pins` row, as `state.Apps.Pin` writes them. Half are
  addressed by hostname (`<slug>.localtest.me`) and half by path (`/load/<slug>`), so the address
  columns are filled. Every app is `stopped` and wanted `stopped`, so the reconciler observes it and
  does nothing (R-140), and no container is started;
- **grants**: every app's owner holds `owner` and use (R-073); 60% of apps are usable by a group,
  30% by two named users, 10% by anyone (R-074), and 10% are visible in the console to a group with
  `viewer`;
- **API tokens** `load-token-00000` onward, delegated, each owned by an app owner and sharing one
  secret (`-token-secret`) for the same reason the users share a password.

Every row has a fixed ID computed from its kind and index, and is inserted with
`ON CONFLICT DO NOTHING` in batches that commit separately (`COPY` into a staging table, then
`INSERT … SELECT`). A second `seed`, or one after an interruption, writes only what is missing.

Then it deploys the **real apps** through the API as the administrator would: `load-real-000`
onward, each the nginx prebuilt spec in port mode on its own port from `LOAD_APP_PORT_START`.
Each is shared with a seeded group, and every other one with anyone. Nothing is audited for the
rows written directly; the audit log only holds what went through the API.

## What it measures

`run` reads the seeded install back from the database and ramps through steps — by default 10%,
25%, 50%, 75% and 100% of the tier's peak — on three surfaces at once.

**Console sessions.** Each simulated user signs in (`POST /sessions`, at `-sign-in-rate` per
second), loads what every console screen loads (`/me`, `/apps`, `/me/notifications`), and then
behaves like an open tab, polling at the console's own intervals (read from `console/src`):

| Persona | Who | Requests |
|---|---|---|
| launcher | users who own no app | `/me/apps`; the inbox every 60 s; the launcher and `/apps` again every `-navigate` (3 m), since nothing polls them and the query is stale after 10 s |
| owner | users who own an app | one of their apps open on its overview: `/apps/{id}`, its deployments and specs once; status every 5 s, usage every 10 s, approvals every 30 s, the inbox every 60 s |
| admin | install administrators | `/users` and `/approvals`, approvals every 30 s, the app list and accounts every `-navigate`, and one app's overview as above |

Users stay online for the rest of the run, so each step adds to the last. About one seeded user in
five owns an app.

**API clients**, open loop at the step's rate, each request with a random seeded token against its
owner's app: `GET /apps` (40%), `/apps/{id}` (30%), `/apps/{id}/deployments` (20%), `/me/apps`
(10%).

**Proxied requests**, open loop at the step's rate. Half go to seeded apps, by `Host` or by path;
those have no containers, so the proxy resolves the app and answers 503, and the measurement is the
lookup. The other half go to real apps, by slug on the balancer's port or by the app's own port,
either anonymously (200 for an app shared with anyone, otherwise 302 to sign in) or with a
signed-in user's session (200 for someone the app is shared with, otherwise 403). That is the whole
request path: authentication, `CheckData`, the assertion and the forward.

Each request is judged against the answer Pando should give, not against 2xx. Open loop means the
next request is sent on schedule whether or not the last has answered, so a slow server shows as
latency and errors rather than as the harness quietly sending less. If every worker (`-workers`) is
waiting, the request is dropped and the report says how many.

For each step, measured only while it holds (after its users have signed in and ten seconds have
passed), the harness records per endpoint class: requests, achieved rate, p50, p95, p99, maximum,
errors, and every status seen. Sign-ins are recorded while users come online. Postgres is sampled
every five seconds as the owner: connections in use against `max_connections`, active, idle in a
transaction, and waiting on a lock. `pg_stat_statements` is reset at the start of each hold, and
its top 15 statements by total time are kept; on a Postgres without it, the statements most often
seen running in `pg_stat_activity` are kept instead.

## Reading the report

The **verdict** says whether the install held through the last step or the first step at which it
broke, what broke it, and the last step that held. A step breaks when any class with at least 20
requests:

- has a p95 above `-p95` (1 s), or
- has an error rate more than `-max-error-rate` (1 percentage point) above its own rate at the
  first step;

or when sign-ins fail beyond that rate, or Postgres uses 90% of its connections. Sign-ins are not
judged on latency: argon2id is slow by design (R-042).

Errors are measured against the first step so that an endpoint that fails at the lightest load is
reported once, under "Failing from the first step", rather than as load breaking it. It still needs
fixing; it is a bug, not a capacity limit.

By default the run stops after the first step that breaks; `-keep-going` runs the rest, which shows
whether it degrades or collapses.

The **steps** table compares each step's target rates with what was achieved, and gives its error
rate, slowest class and peak Postgres connections. A large gap between target and achieved with
dropped requests means the harness machine ran out before Pando did; rerun with more `-workers`, or
from a bigger machine.

Each **step** section has the per-class table (a breaching class is in bold) and Postgres's top
queries for that step. Read the breaking step's queries first: a statement whose total time
dominates, or whose mean grows from step to step while its calls grow linearly, is usually the
cause. A class whose p95 climbs while Postgres is idle points at the replica instead (the Docker
API, CPU, or a lock in the process).

## Known limits

- **One Docker host.** The stack runs on one machine, and so does every replica. The cluster tier
  measures N replicas against one Postgres, not Kubernetes or several hosts: replica CPU, the
  balancer and Postgres all share the host's cores, so its absolute numbers are a lower bound.
  Point `-url` and `-db` at a real cluster to measure one.
- **Apps without containers.** One host cannot run 20,000 containers, and Pando reserves CPU and
  memory per app (R-242), so seeded apps are never started. They exercise the control plane, the
  console's lists and per-app endpoints, and the proxy's lookup by hostname and path, but not
  forwarding. Forwarding, `CheckData` and assertions are measured on the real apps, which are a
  configurable handful (`LOAD_REAL_APPS`), so proxy traffic that reaches an app is spread over few
  upstreams. Status and usage for a seeded app ask the Docker API about an app that has no
  containers, which may not cost what asking about a running one does.
- **Real apps are capped by the port range and CPU.** Each takes a port (`LOAD_APP_PORT_START`
  onward; the range widens to fit) and the replicas stack's 0.1 CPU and 128 MiB reservation. A
  deploy past the host's capacity is refused at plan time, and `seed` fails.
- **No writes under load.** The run reads; it does not deploy, edit specs, share or sign out, and
  nobody's inbox has notifications in it. Deploy throughput is a separate question (O-32).
- **One client machine.** Thousands of simulated users share one process and one network stack. On
  a laptop the harness may saturate before the cluster tier's peak; the dropped counts say when.
- **Shared credentials.** Every seeded user has the same password hash and every token the same
  secret. Pando verifies each sign-in and token in full, so the cost per request is the same as
  with distinct ones.
- **Not in CI.** The target takes tens of minutes at the `vm` tier and over an hour at `cluster`,
  and needs a Docker host with room for it. Only the harness's pure parts (seed plan, rate math,
  percentiles, report) have unit tests, which `make test` runs.
