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

**[P]** Both services start at once, so Pando must tolerate Postgres not yet accepting connections —
connect with bounded retry at startup rather than assuming readiness. This is the standard Compose
race and the standard fix.

**[O]** Whether a non-Docker install topology (Incus, Podman, bare host) ships an equivalent, or is
simply expected to use the external-database path, is unspecified. The external path covers it
functionally; only the first-run experience differs.

### 1.2 Library choices [P]

| Concern | Choice | Note |
|---|---|---|
| HTTP router | `go-chi/chi/v5` | **[D]** |
| Logging | `uber-go/zap` | **[D]** Structured, one logger threaded through context |
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

**Not here yet.** The **in-place upgrade** is the second half of issue #53: a signed image verified with cosign, a backup first, a helper container that replaces Pando's, and a rollback that is always restore-from-backup, never down migrations. It is opt-in through the deployment configuration (a host policy field like the two above). The reason is drift: a version pinned in a Compose file or IaC puts the old image back on the next apply, so the person who owns that configuration has to decide. Every app is unreachable while Pando restarts, because the proxy is in-process (§1.3), so the upgrade will keep that window short by pulling and verifying the new image before stopping the old one. An **"update available" notification** waits for the console to have somewhere to show notifications (R-231; issue #50).

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
│   │   ├── api/          # the seven interface definitions. No implementations.
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

---

## 4. Acceptance criteria convention [P]

Requirements currently have no way to be called done. Convention going forward: each requirement that is testable gets at least one acceptance test named for it.

```go
// TestR132_UnfilledRequiredSlotBlocksDeploy asserts R-132.
```

CI reports which R-IDs have coverage. Requirements with no test are either philosophy (R-002), deferred (R-290), or a gap.

The four sequences in `07-sequences.md` are the integration-level acceptance tests. If those four pass end to end against real Postgres and real Docker, v1 works.
