# Adapters — read this before writing one

Design: `docs/design/03-adapter-interfaces.md`, then the relevant section of `01-spec-schema.md`.

R-250: **the app declares requirements; adapters translate.** R-251: **core never learns a provider's
vocabulary.** These interfaces are where that promise is kept or broken — if a Docker-shaped concept
appears in an interface signature, the design has failed.

## Adding a category

The categories are not a fixed list. When a new thing Pando talks to passes design 03 §8.1's test —
the planner asks it a question, and it has a provider's vocabulary worth hiding — it becomes a category,
and R-252 is amended in the same change. Every file a category touches: the interface and capabilities
struct in `api/`, the constant in `api/identity.go`, `satisfiesCategory` and a typed accessor in
`api/registry.go`, the kinds under `internal/adapter/<category>/`, a migration re-creating
`adapter_configs_category_check`, `categories()` in `internal/config/adapters.go`, `newAdapter` and
`adapterKinds()` in `cmd/pando/main.go`, and the console's `CATEGORY_ORDER` and `CATEGORY_NOTES`. The
generic surfaces — `POST /adapters`, `pando adapter add`, the adapters screen — then work for it with no
code of their own. Do not build an adapter-shaped thing in core instead; see design 03 §8.1 for why the
install image registry was converted (issue #153).

A category whose adapters must change without a restart — source connections, image registries — is
built from its row on each use by a core service rather than registered at startup; `liveCategory` in
`internal/httpapi` lists them.

## Hard boundary (R-027)

Nothing under `internal/adapter/` may import `internal/core/authz`, `internal/core/audit`, or
`internal/core/state`. This is enforced by a depguard rule in `.golangci.yml`, not by convention.
`internal/adapter/api` is the only core-adjacent package an adapter may import.

Adapters do not authorize, do not audit, and do not touch the state store. Core does those things and
hands the adapter a fully-resolved request.

## `internal/adapter/api` holds definitions only

No implementations there, ever. Everything is compiled in-tree (R-253) — these are ordinary Go
interfaces, freely refactorable. **There is no wire protocol and none is planned.** Do not add one.

## Rules that catch adapter authors out

- **Capabilities are a returned struct, never a type assertion** (R-254). A type assertion is
  invisible to the planner and cannot produce a readable plan-time error.
- **`IsolationClass` is an ordered integer**, not a string enum, because policy floors are compared
  (R-114, R-255). Gaps of 10 leave room to insert classes without a migration.
- **Every adapter implements `HealthCheck`.** The planner refuses to plan against an unhealthy
  adapter and returns `ADAPTER_UNAVAILABLE` rather than failing mid-deploy.
- **`Env` arrives fully resolved.** Runtime adapters never see a slot, never talk to the secrets
  adapter, and never learn a value was sensitive.
- **`Observe` reports facts and never remediates.** An adapter that silently restarts things makes
  drift undetectable and breaks R-148. The reconciler decides what to do.
- **`ObservedWorkload.Healthy` is a pointer.** "No health signal" and "unhealthy" are different states
  and must not collapse (R-221).
- **Capacity is adapter-reported, never host-inspected** (R-243). Core does not read `/proc` and has
  no concept of a host.
- **`NetworkPlan.Private` is always true** (R-026). It is a field, not an assumption, so an adapter
  that cannot provide a private network fails loudly at capability check instead of silently placing
  workloads on a shared network.

## Per-category traps

**Routing.** `Ensure` makes traffic arrive at **Pando's proxy** — it never routes to the workload.
`ProxyUpstream` is in the request rather than discovered, precisely so this is unmissable. Your
instinct will be to point Traefik straight at the container; that is the bug. Path-mode adapters strip
the prefix and set `X-Forwarded-Prefix` (R-167), and **must not rewrite response bodies** (R-028).

**Builder.** A builder must never receive or request a container runtime socket (R-112). The type
system cannot enforce this — it is a review checklist item and an integration test asserting the build
environment has no socket mounted. `Question.Prompt` carries a hard content requirement from R-105: it
must be answerable by a model that cannot see the repo. `SourceView` is read-only *structurally* — it
has no write methods (R-020).

**Identity.** Identity adapters **authenticate only** (R-044). `Subject` carries no roles, no verbs,
no permissions. Group *names* cross the boundary; what a group can *do* is Pando's (R-078). Each
adapter declares its own `SessionPolicy` (R-047), so the console can show the real revocation window
per adapter rather than implying a global guarantee.

**Secrets.** `secret.Value` is the redacting wrapper — `String()`, `MarshalJSON()`, and the zap
marshaler all return `[redacted]`. A secret cannot be accidentally logged because the type will not
render.

**Services.** A provisioned service returns workloads that join the app's private bundle. Not exposed,
not addressable from outside, **not shareable with another app** (R-134). Sharing is expressed as two
apps binding to one external target.

## Registration

Happens in `main` at startup, from compiled-in packages (R-253). Configured instances come from the
`adapter_configs` table — except identity providers, which live in `identity_adapters` and are built by
`core/idp` on first use and rebuilt when their row changes, so connecting one needs no restart.

**Identity adapters stay stateless across a redirect.** Whatever the callback must be checked against
(PKCE verifier, nonce, SAML request ID) goes back to core as `Redirect.Flow` and returns in
`Credential.Flow`; core owns the state, the endpoints, the browser binding and replay. An adapter that
keeps a map of pending sign-ins has broken R-027's spirit and will break on the first restart.

## Traefik lands last, deliberately

It is the second implementation of the routing interface, and building it is the test of whether the
abstraction actually holds. **If Traefik requires changing the interface, the interface was wrong.**
Better to learn that in phase 10 than to have assumed it was right in phase 3.
