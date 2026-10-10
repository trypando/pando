# 00 — Stack, Layout, Conventions

Companion to `../requirements.md`. Requirement IDs (`R-###`) refer to that document. Tags carry the same meaning: **[D]** decided, **[P]** proposed, **[O]** open.

---

## 1. Stack

**[D]** Backend: **Go**. HTTP routing: **chi**. Logging: **zap**.
**[D]** State store: **PostgreSQL**.
**[D]** Console: **React + TypeScript + Vite**.

### 1.1 Postgres vs. the single-binary promise

R-253 says Pando ships as a single binary and R-002 says setup cost is paid once. Requiring an operator to stand up Postgres before installing Pando adds a prerequisite to the hobbyist path that Pando exists to eliminate.

Three options were considered, and the resolution below is a fourth. They are kept because the costs they name are what the fourth option had to avoid:

- **[P] Bundled Postgres.** `pando install` starts a Postgres container Pando manages, on the same runtime adapter it uses for apps. One command, no prerequisite, still Postgres. Cost: Pando's own state depends on the runtime adapter being healthy, which complicates bootstrap ordering and DR (§05, restore).
- **Bring your own.** Connection string required at install. Clean separation, worse first-run experience.
- **Embedded Postgres binary.** `embedded-postgres`-style, unpacked and supervised by Pando itself. No container dependency, adds ~100 MB to the distribution and platform-specific binaries.

**[D] Resolved (O-11): Postgres is supplied by the install topology, not by Pando.**

Pando ships a Compose file defining two services: `pando` and `postgres`. They start together. The
operator runs one command and has never heard of a connection string. Configuration accepts an
external database (`PANDO_DATABASE_URL`) for installs that already run Postgres and want Pando to use
it.

This is a fourth option, and it takes the first option's experience without its cost. The distinction
that matters: **Pando does not start Postgres — the install topology does.** Pando connects to a
database that is already coming up beside it, exactly as it would to an external one. It therefore
needs no runtime adapter to reach its own state store, which is what made the bundled-container option
expensive:

- **No bootstrap inversion.** Phase 0 needs nothing from phase 3. Had Pando managed the Postgres
  container itself, starting up would have meant reading from Postgres to learn which runtime adapter
  to use to start Postgres.
- **No dependency of Pando's state on the runtime adapter's health.** A Docker daemon problem would
  otherwise take out the state store — the one thing that would have to survive to record it.
- **[O-14] largely dissolves.** DR restore no longer has to bring up a database before it has a state
  store telling it how; the database comes up with the topology and restore writes into it.
- **Pando keeps administrative rights on a fresh cluster**, which is what makes the audit grant in
  §02 2.6 achievable — see below.

**This adds no prerequisite.** Pando's v1 runtime and builder adapters are both containerized (§03
10), so a container runtime is table stakes already. The Compose file uses a dependency Pando cannot
run without; it does not introduce one. R-253's single binary is unaffected — the binary is still one
binary, and Compose is an install method rather than a change to the artifact.

**[D] The external-database path carries a privilege contract.** Pando's audit guarantee (R-027) is a
database grant: a restricted application role with `INSERT` on `audit_events` and no `UPDATE` or
`DELETE`, and a second restricted role, `pando_audit_archiver`, that removes months past retention only
through an owner-defined function (R-347, design 02 §2.6). Creating those roles requires administrative rights, which Pando has on a cluster it was
handed fresh and may not have on someone else's. So the external path must document the privileges it
requires and **verify them at startup, failing loudly** rather than silently running with an audit log
that can be rewritten. A degraded-but-running mode is not acceptable here: the whole value of the
grant is that it holds without anyone checking.

**[D] The in-place upgrade adds one privilege, and only for itself** (R-359). Its rollback copies Pando's
database with `CREATE DATABASE … TEMPLATE`, which needs `CREATEDB` (or ownership of a template). The
bundled Compose file's account is a superuser and has it. On an external database without it, Pando
still runs; the in-place upgrade is refused before anything starts, naming the privilege, and the
upgrade is done by changing the image (R-352).

**[P]** Both services start at once, so Pando must tolerate Postgres not yet accepting connections —
connect with bounded retry at startup rather than assuming readiness. This is the standard Compose
race and the standard fix.

**[D] The database password is generated, not defaulted (R-401, issue #130).** The Compose file used to
give Postgres `${POSTGRES_PASSWORD:-pando}`, so every install that did not set one had the same
password, written in a public file. A one-shot `secrets` service now runs first: it writes 32 random
bytes, or the operator's `POSTGRES_PASSWORD`, to `/run/pando-secrets/postgres-password` on the
`pando-secrets` volume, owned by Pando's user (65532), mode 0400. Postgres reads it through
`POSTGRES_PASSWORD_FILE`, as root before it drops privileges; Pando through
`PANDO_DATABASE_PASSWORD_FILE`, which `config.Load` writes into `PANDO_DATABASE_URL`. A file rather than
an environment variable, because a variable is in `docker inspect` and every child process. The
service leaves an existing file alone: Postgres was initialized with it, and a new one would lock
Pando out. The service uses the Postgres image, so nothing extra is pulled.

**[O]** Whether a non-Docker install topology (Incus, Podman, bare host) ships an equivalent, or is
simply expected to use the external-database path, is unspecified. The external path covers it
functionally; only the first-run experience differs.

**[P] More than one `pando` (issue #72).** The `pando` service may run as N replicas against the one
Postgres, behind a load balancer, provided every replica reaches the same Docker daemon and shares
`/var/lib/pando`. Bootstrap is serialized by an advisory lock, the restricted roles keep their
passwords across starts, one replica leads the install-wide jobs, and each replica signs assertions
with its own key and publishes every replica's. Replicas on different Docker hosts, and Kubernetes
with no Docker host beside it, are not supported. The verdict, prerequisites and what each fix cost
are in `notes-multiple-replicas-issue-72.md`; `make test-replicas` runs it.

### 1.2 Library choices [P]

| Concern | Choice | Note |
|---|---|---|
| HTTP router | `go-chi/chi/v5` | **[D]** |
| Logging | `uber-go/zap` | **[D]** Structured, one logger threaded through context |
| Metrics | OpenTelemetry Go SDK, OTLP exporters | **[D]** Pushed, never served (§3.6, R-399) |
| DB driver | `jackc/pgx/v5` | Native protocol, better types than `lib/pq` |
| Query layer | `sqlc` | Generates typed Go from SQL. No ORM. |
| Migrations | `golang-migrate` | Versioned, up/down, embedded in the binary |
| Validation | `go-playground/validator` | Struct tags on API payloads |
| JWT | `go-jose/v4` | Assertion signing (R-052), JWKS (R-057) |
| Config | `spf13/viper` | YAML + env + flags, per R-271. The YAML file can also fix host policy fields and declare adapters with their AI function assignments, read-only elsewhere while declared (§02 2.5, §10 7.1) |
| CLI | `spf13/cobra` | |
| Container runtime | `moby/moby/client` + `moby/moby/api` | Local runtime + image-scanner adapters. `docker/docker` stopped at v28.5.2 and gets no fixes. The client is pre-1.0 and a minor release can change its API, so bumping it needs `make test-integration`, not only a green build |
| BuildKit | `moby/buildkit` client | R-111 |
| Proxy | `net/http/httputil.ReverseProxy` | Wrap, don't adopt. See §1.3 |
| Testing | stdlib + `testify/require` | |
| Integration tests | `testcontainers-go` | Real Postgres, real Docker |

### 1.3 The proxy is ours [D]

R-023 makes the proxy the single enforcement point for every request to every app. Delegating that to an external proxy would put the authorization decision outside core, violating R-027.

So: Pando implements the identity-aware proxy itself, on `httputil.ReverseProxy`. Routing adapters (Traefik, Cloudflare) place traffic **in front of** the Pando proxy; they never route around it. A routing adapter's job is to get requests to Pando's proxy with the right hostname or path, not to reach the workload.

This must be stated in every routing adapter's contract, because an adapter author's instinct will be to point Traefik straight at the container.

### 1.4 Updating Pando itself (issue #53)

**Knowing a release exists [D]** (R-349 – R-351). `internal/core/update.Checker` runs beside the other loops in `serve`. At startup and every six hours **[P]** it reads `https://api.github.com/repos/trypando/pando/releases`. It reads that list because the release workflow already writes it: each release body is exactly that version's CHANGELOG.md section (`release.yml`), and `prerelease` is set for a tag with a suffix. A second feed would be one more thing to keep in step. The request sends a User-Agent of `pando/<version>` and nothing else.

- **What is kept, and where.** Only the last list fetched and when, in memory. A restart checks again, so there is nothing to migrate.
- **How the channel applies.** The channel filters that list when it is read rather than when it is fetched, so changing it in policy needs no new request.
- **When it is off.** The policy is read at every tick. While `disable_update_check` is set, nothing is sent.
- **Security and breaking marks.** `Security` means the section's `### Security` subsection says something other than "No new advisories.". `Breaking` follows docs/releasing.md: a MAJOR bump, or before 1.0 a MINOR one.

Both settings are ordinary host policy fields, so they can be fixed in the config file or as `PANDO_POLICY_DISABLE_UPDATE_CHECK` / `PANDO_POLICY_UPDATE_CHANNEL`, and are then read-only everywhere (R-271). There is no separate console toggle that could disagree with the deployment's own configuration.

**How to upgrade [D]** (R-352). The server ships only as the container image (`internal/reference/install.go`), so the server-side instructions have two cases:

- **Inside a container** (`/.dockerenv`): the release's Compose file plus `docker compose up -d`, or the image tag to set wherever infrastructure-as-code deploys Pando.
- **Anywhere else:** the release archive.

Package and Homebrew installs carry the CLI. Its upgrade command is chosen from the CLI's own executable path, in `internal/cli/skew.go`.

**Skew [D]** (R-353). The server sends `Pando-Version` only to signed-in callers. The CLI compares MAJOR.MINOR, because a PATCH release changes no interface, and warns once per run on stderr. Stdout is left alone, which keeps `pando mcp` working.

**Refusing a downgrade [D]** (R-354). Before `Up`, the migration step compares the database's schema version with the newest embedded migration. If the database's is higher, it refuses, naming both versions. Without this an older image put back by a Compose file or IaC failed with "Database migration failed." and crash-looped, and with it the proxy, so every app was unreachable for a reason nobody could read. Migrations run forward only, and going back is a restore.

**Upgrading in place [D]** (R-355 – R-362). The second half of issue #53.

- **Opt-in, and only where the deployment cannot undo it** (R-355). Host policy `upgrade_in_place` is off by default. That goes against R-270, on purpose: a version named in a Compose file or IaC puts the old image back on its next apply. The owner of that configuration usually sets `PANDO_POLICY_UPGRADE_IN_PLACE=true` there, which also locks the switch on the Policy screen (R-271).
  - The deployment's image must be a moving tag that covers the target. `latest` covers any release, a minor line (`0.3`) covers its patches, and a major (`1`) covers 1.x. Those are the tags `image.yml` publishes.
  - Pando reads its own image reference from the runtime (`SelfUpgrader.Self`) and refuses an exact version or a digest, saying which image to set.
  - The helper points the moving tag at the new image locally, only after the new version is healthy. A later `docker compose up` then resolves the tag to what is running. Had the tag moved first and a rollback followed, the next `compose up` would start the version that failed.
- **Order of operations** (`internal/core/upgrade.Service.Start`):
  1. Plan. Every refusal is collected, not just the first.
  2. Confirm breaking versions by typing the target (R-360).
  3. Verify the image (R-357, below). This happens before the backup, so a backup is never spent on an image Pando would refuse.
  4. Take the full backup with the person's passphrase, or record that they skipped it (R-358).
  5. Pull by digest while Pando is still up, so a registry failure leaves Pando running.
  6. Write the attempt to `<work_dir>/upgrade/outcome.json`.
  7. Start the helper.
- **Verification** (R-357). cosign 3 attaches a Sigstore bundle to the image's index digest as an OCI 1.1 referrer, with no legacy `.sig` tag.
  - `update.ImageVerifier` resolves the tag to that digest and fetches the referrer (go-containerregistry). sigstore-go then checks it against `image.yml`'s identity from docs/releasing.md and Sigstore's public-good trusted root.
  - The trusted root is fetched through TUF and cached under `<work_dir>/sigstore`.
  - The digest is what runs from then on.
  - The tests verify what 0.3.0 actually published, offline against a pinned root. That tests the identity and format against the release workflow's real output, not a fixture written to match expectations.
- **The helper** (R-359). A container cannot replace itself. The helper is Pando's current image, started by ID, so the code doing the replacing is the version already trusted. It runs `pando upgrade-helper` with Pando's mounts, which carry the data directory and the runtime socket, and Pando's networks, which reach Postgres by the URL's host name. It runs these steps:
  1. Stop Pando and rename it aside.
  2. Copy the database: `CREATE DATABASE <db>_pre_upgrade TEMPLATE <db>`, `state.Snapshot`.
  3. Create the new container with the old configuration and the new image. Environment variables and labels the old image set are dropped, as Watchtower does, so the new image's own defaults apply.
  4. Wait for `/readyz`. It answers only after migrations and the audit-log check pass.
  5. On success: move the tag and remove the old container.
  6. On failure: keep the new version's last log lines, discard it, restore the copy, then rename and start the old container. The restore comes first because the old version refuses a migrated database (R-354).

  The container steps are the Docker adapter's (`docker.Replacer`). The copy is the state store's, because an adapter never touches state (R-027). The helper composes them, and `upgrade.RunHelper` is tested at each failure point.
- **Compared with Watchtower and Portainer.** Both recreate a container from its own inspect output under a new image, which is what this does. Watchtower does not check that the new container became healthy and has no rollback. Portainer's agent replaces itself the same way, through a helper. Neither copies a database: they have none of their own.
- **The outcome** is the file. The Pando that starts next, new or restored, records it in the audit log once (`upgrade.succeeded`, `upgrade.rolled_back`, `upgrade.failed`). On failure it sends `upgrade_failed` to `install.upgrade` holders and removes the finished helper. After 24 hours healthy **[P]** it drops the copy. There is no "roll back days later" action: that would remove every audit event since the upgrade (R-027) and leave the state store describing apps that no longer match what runs. Going back after the soak is the full-backup restore.
- **The copy needs `CREATEDB`.** An account without it is refused by name before anything starts (§1.1).
- **Every app is unreachable while Pando restarts**, usually for under a minute, because the proxy is in Pando (§1.3, R-023). Apps keep running. `RejoinNetworks` reattaches the new container to each running app's network at startup.
  - The window is kept short: verification, the backup and the pull all happen before Pando stops.
  - It is stated in the plan's `note`, which the console's dialog and `pando upgrade` both show.
- **Scheduled patch upgrades** (R-361). `auto_upgrade_patches` with `maintenance_window` (`"sun,wed 02:00 2h"`, UTC, one string so it reads the same in the config file, an environment variable and the API).
  - The loop looks every five minutes. Inside the window it upgrades to the newest stable patch of the running minor line, and never to a version an earlier attempt failed to reach.
  - It takes no full backup, because nobody is there to give a passphrase (R-213). The database copy is its rollback, and the Policy screen says so.
- **`update_available`** (R-362) goes to `install.upgrade` holders through the notify adapters once per version, with the version last announced kept beside the outcome. Today only the console adapter exists and it stores without displaying (R-231). The Updates screen's badge carries it until issue #50 delivers notifications elsewhere.
- **`pando self-update`** (R-363) replaces an archive or `go install` CLI after checking `checksums.txt.sigstore.json` against `release.yml`'s identity and the archive against `checksums.txt`. A CLI installed by Homebrew or a Linux package is left to that package manager. It is a root command beside `version`, not one of the API's commands, because it acts on the binary and not on an installation (R-261).
- **Agents do not upgrade by default.** `install.upgrade` is in `policy.Default().AgentDisabledVerbs`, and migration 000042 adds it to a stored policy. MCP offers the plan and the last outcome but not starting one, which takes a passphrase.

---

## 2. Repository layout [P]

```
pando/
├── cmd/
│   ├── pando/            # CLI (cobra) — also the server entrypoint
│   └── pandod/           # server, if split from CLI later
├── internal/
│   ├── core/
│   │   ├── authz/        # verb evaluation. NEVER importable by adapters.
│   │   ├── audit/        # append-only event log
│   │   ├── assertion/    # JWT minting, JWKS
│   │   ├── spec/         # AppSpec types, validation, diffing
│   │   ├── planner/      # spec + policy + adapters -> plan, or plan-time error
│   │   ├── reconciler/   # the loop
│   │   ├── policy/       # host policy evaluation
│   │   └── state/        # sqlc-generated queries + repository types
│   ├── adapter/
│   │   ├── api/          # one interface per category. No implementations.
│   │   ├── identity/local/
│   │   ├── routing/{loopback,traefik}/
│   │   ├── builder/buildkit/
│   │   ├── runtime/docker/
│   │   ├── secrets/local/
│   │   ├── services/     # provisioned slot fillers
│   │   └── notify/console/
│   ├── detect/           # the auction, detectors, trial run
│   ├── proxy/            # identity-aware reverse proxy
│   ├── httpapi/          # chi handlers, the REST surface
│   ├── mcp/              # MCP server, a client of httpapi's service layer
│   └── console/          # embedded static assets from the Vite build
├── migrations/
├── console/              # React/TS/Vite source
└── docs/
```

**[D]** `internal/adapter/api` defines interfaces only. Adapter packages may import it and nothing else from `internal/core`. Enforce with an import-lint rule in CI — this is R-027 made mechanical.

---

## 3. Conventions

### 3.1 IDs [P]

Prefixed, sortable, opaque: `app_01HQ8...`, `spec_...`, `usr_...`, `tok_...`, `vol_...`, `grant_...`. ULID body. Prefixes make log lines and error messages self-describing and make copy-paste mistakes visible.

### 3.2 Errors [D]

Every error crossing an API boundary carries a stable machine code, a human message, and where relevant a remediation hint. R-242 and R-254 both promise "fail at plan time with a readable error" — that promise needs a type, not a convention.

```go
type Error struct {
    Code       Code           `json:"code"`
    Message    string         `json:"message"`     // human, specific, no jargon
    Remedy     string         `json:"remedy,omitempty"`
    Details    map[string]any `json:"details,omitempty"`
    RequestID  string         `json:"request_id"`
}
```

Message text is held to the R-105 standard where a user might act on it: self-contained, pasteable into an assistant, no undefined terms.

**Code taxonomy [P]:**

| Prefix | Class | HTTP |
|---|---|---|
| `AUTH_*` | Authentication failed or absent | 401 |
| `PERM_*` | Authenticated, not permitted | 403 |
| `POLICY_*` | Blocked by host policy | 403 |
| `VALID_*` | Malformed request | 400 |
| `PLAN_*` | Deployment cannot proceed as specified | 409 |
| `STATE_*` | Object in the wrong state for this action | 409 |
| `ADAPTER_*` | Adapter failed or is unavailable | 502 |
| `BUILD_*` | Build failed | 422 |
| `CAPACITY_*` | Insufficient host resources | 409 |
| `NOT_FOUND` | | 404 |
| `INTERNAL` | | 500 |

Named codes that must exist, since requirements promise them:

- `PLAN_SLOT_UNFILLED` — R-132. Details name each unfilled slot.
- `PLAN_NO_ADAPTER_MEETS_POLICY` — R-024, R-114. Details name the required floor and each configured adapter's class.
- `PLAN_CAPABILITY_UNSUPPORTED` — R-254. Details name the capability and the adapter lacking it.
- `POLICY_SOURCE_NOT_ALLOWED` — R-092. Raised **before clone**.
- `POLICY_EXEC_DISABLED` — R-085.
- `POLICY_ANONYMOUS_GRANT_FORBIDDEN` — R-076.
- `CAPACITY_WOULD_OVERSUBSCRIBE` — R-242. Details carry requested, allocated, and adapter-reported total.
- `PLAN_COMPOSE_CONSTRUCT_REJECTED` — R-099. Details name the construct and why.

### 3.3 Logging [P]

zap, structured, one logger in context. Every log line inside a request carries `request_id`, `principal_id`, and where applicable `app_id`. Adapter calls log at debug with a `adapter` and `category` field.

**Secrets never reach a log line.** Secret values are wrapped in a `secret.Value` type whose `String()`, `MarshalJSON()`, and `MarshalLogObject()` all return `[redacted]`. This is how R-194 is enforced structurally rather than by review.

### 3.4 Context [P]

`context.Context` carries: request ID, principal, logger, and a deadline. Adapters receive a context on every call and must honor cancellation — the reconciler cancels work when a spec changes underneath it.

### 3.5 Time [P]

All timestamps UTC, `timestamptz` in Postgres, RFC 3339 on the wire. A `Clock` interface in core so the reconciler's backoff (R-149) is testable without sleeping.

### 3.6 Metrics [D]

Pando pushes metrics about itself over OTLP to a collector the operator names, and nothing else
(R-399, issue #126). It serves no `/metrics` endpoint, keeps no series and draws no graphs (R-016,
R-245). Configuration is `metrics.otlp_endpoint`, `otlp_protocol`, `otlp_headers` and `interval`
(`PANDO_METRICS_*`), off until an endpoint is set; the headers are a secret setting, never reported
or logged. `docs/reference.md` lists every metric.

Code records through `internal/telemetry` and nowhere else: one function per kind of work —
`telemetry.Deploy(ctx, outcome, duration)` — taking typed, bounded values, never a free-form
attribute. That is R-400's mechanism (design 06 §7). Until `telemetry.Start` installs a provider every
function records into a no-op, so an install with no endpoint pays nothing for it. A test that asserts
something is measured installs a reader with `telemetrytest.Install` and reads it back.

A new metric is a function in `internal/telemetry`, a row in `docs/reference.md`'s table, and its
attribute keys in `telemetry.AttributeKeys`. Names follow OpenTelemetry's semantic conventions where
one exists (`http.server.request.duration`, `db.client.connection.count`) and `pando.` otherwise.
Durations are seconds.

Traces, and logs over OTLP, are not exported [D]: logs are structured zap on stdout (§3.3), for the
operator's own collector to read.

---

## 4. Acceptance criteria convention [P]

Requirements currently have no way to be called done. Convention going forward: each requirement that is testable gets at least one acceptance test named for it.

```go
// TestR132_UnfilledRequiredSlotBlocksDeploy asserts R-132.
```

CI reports which R-IDs have coverage. Requirements with no test are either philosophy (R-002), deferred (R-290), or a gap.

The four sequences in `07-sequences.md` are the integration-level acceptance tests. If those four pass end to end against real Postgres and real Docker, v1 works.
