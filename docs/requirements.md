# Pando — Product Requirements

**Status:** Draft 1, derived from design interview
**Date:** 2026-09-08

---

## Legend

Each requirement is tagged so its authority is unambiguous.

| Tag | Meaning |
|---|---|
| **[D]** | Decided. Settled in design discussion. Changing it changes the product. |
| **[P]** | Proposed. A default filled in to make the system buildable. Override freely. |
| **[O]** | Open. Explicitly unresolved; listed in §24. |
| **[V1]** | In the first release. |
| **[LATER]** | Designed for, deliberately deferred. |

Requirement IDs are stable. Refer to them rather than to section numbers.

---

## 1. What Pando Is

**R-001 [D]** Pando hosts your apps without you having to set up deployment pipelines, tunnels, or DNS more than once. You give Pando access to a host and to a repo, point it at the repo, and it builds and runs the app for you.

**R-002 [D]** The defining economic property: **setup cost is paid once, at the host.** Deploying the tenth app must feel like nothing. Any feature that adds per-app setup burden is suspect and must justify itself against this.

**R-003 [D]** Pando serves two audiences with one product and no tiers:
- **Hobbyist** — a person who builds apps and does not want to spend as long deploying them as building them.
- **Enterprise** — a person, often non-technical, who built something useful and needs it hosted somewhere secure and shared with coworkers or the public.

**R-004 [D]** There are no SKUs, editions, or paywalled features. All functionality is available to all users at all times. Enterprise capability comes from *host configuration*, not from a different product.

**R-005 [D]** The enterprise deployer may be non-technical. They may not know what a port is. This constrains the console, the detection pipeline, and every error message in the system. See §7.

---

## 2. Non-Goals

These are permanent. A request falling into one of these is answered "no," not "later."

**R-010 [D]** **Pando is not a scheduler.** It places; it does not schedule. One app lives in one place. No cross-host bin-packing, no autoscaling groups, no service mesh, no rescheduling on node failure. A runtime that spans machines moving a workload after a failure is not Pando scheduling: when such a runtime (Kubernetes, for example) recreates an app's workload on another node because the node it ran on failed, the runtime places it as it places any workload, and Pando has decided nothing about where it runs (R-256, O-44).

**R-011 [D]** **Pando does not test.** It builds and deploys. It does not run your test suite and does not gate deploys on test results.

**R-012 [D]** **Pando is not a marketplace.** No catalog of one-click apps, no curated app store.

**R-013 [D]** **Pando is not a disaster-recovery product.** It takes backups sufficient to recover from recent mistakes and host loss. It does not offer RPO/RTO guarantees, continuous replication, or point-in-time recovery.

**R-014 [D]** **No multi-AZ or multi-region.** Possibly revisited if a cloud provider adapter is built. Not now.

**R-015 [D]** **One Pando install serves one organization.** There is no tenant object. Multi-org means multiple Pando installs. "Install-wide" and "host-wide" are the correct terms; "tenant-wide" means install-wide.

**R-016 [D]** Pando is not a source host, not an APM product, and not a database-as-a-service. It provisions services to fill declared slots; it does not offer them as a product with its own SLA.

---

## 3. Core Invariants

These are load-bearing. Violating any of them is a design failure, not a tradeoff.

**R-020 [D]** **Nothing lives in the repo.** There is no `pando.yaml`. The source tree is strictly read-only input. Pando looks at it and never asks it for permission. Consequence: Pando's state store is the sole record of how every app runs, which makes export, backup, and audit day-one requirements rather than polish.

**R-021 [D]** **Pando fills declared slots; it never invents topology.** If the repo declares what it needs, Pando's job is to satisfy that declaration. If it doesn't declare, Pando does not go hunting through imports to guess.

**R-022 [D]** **Detection never re-runs implicitly.** Re-detection is an explicit action that shows a
diff against the pinned spec. **Accepting the result carries a person's decisions forward**: the
environment they set, how each dependency is filled, and storage they added. The build and the
workload set come from the repository, because taking those from it is what re-detecting is. Which is
which is not inferred — the spec records provenance on ports, volumes and environment entries for
exactly this.

**R-023 [D]** **Every request to every app passes through Pando's identity-aware proxy.** There is no bypass path — not for public apps, not for anonymous access, not as a performance optimization. Upstream enforcement (e.g. Cloudflare Access) is an optimization layered on top, never the only gate.

**R-024 [D]** **Builds never execute on the host.** Every build runs inside an isolation boundary supplied by a builder adapter. There is no "just build it here" fallback. If no builder is configured or none meets policy, deployment fails at plan time.

**R-025 [D]** **Apps are isolated from each other.** No app can reach another app's private network. The only path from one app to another is through the proxy, authenticated like any other client.

**R-026 [D]** **Non-exposed workloads are unreachable from outside their bundle.** Only services explicitly marked exposed receive an endpoint.

**R-027 [D]** **Authorization decisions, the audit log, the state store, and the identity assertion path live in core.** No adapter can influence or rewrite any of them.

**R-028 [D]** **Pando observes and reports; it does not remediate the app.** It will warn that something looks wrong. It will not rewrite requests, inject configuration, or patch the application to make it work.

**R-029 [D]** **Control plane and data plane are separate grants** (§6), with one exception: owning an app grants use of that app (R-072).

---

## 4. Object Model

**R-030 [D]** The following are first-class objects in Pando's state:

| Object | Notes |
|---|---|
| **App** | The unit of deployment. Owns a spec, grants, revisions, volumes, secrets. |
| **Spec** | The pinned description of how an app is built and run. Produced by detection, edited by humans, versioned. |
| **Bundle** | The set of workloads comprising one app, on one private network. |
| **Workload** | A single process/container/VM within a bundle. Exposed or internal. |
| **Volume** | Persistent storage attached to a workload. |
| **Slot** | A declared, typed dependency (a Redis, a Postgres) awaiting resolution. |
| **User** | A principal originating from an identity adapter. |
| **Group** | A collection of users. May be Pando-native or pushed from an IdP. |
| **Token** | A non-human principal. Two kinds (R-057, R-059). |
| **Role** | A named set of control-plane verbs. Three built-in, plus custom. |
| **Grant** | Binds a principal to an app on either the control or data plane. |
| **Adapter config** | A configured instance of an adapter (this Traefik, that Cloudflare account). |
| **Policy** | Host-level constraints. See §20. |
| **Audit event** | Immutable record of a mutation or privileged action. |

**R-031 [P]** An app has exactly one owner of record at any time, plus any number of additional grants. Ownership is transferable.

---

## 5. Identity and Authentication

### 5.1 Adapter model

**R-040 [D]** Identity is an adapter category like any other. Local username/password is one adapter, not a special case.

**R-041 [D] [V1]** **Local users** — username and password, stored by Pando. This is the v1 identity adapter and the default on a fresh install.

**R-042 [D]** Local users must be *secure* but are not claimed to be the most secure option. The documentation must say plainly that installs with real security requirements are expected to configure an external provider.

**R-043 [D]** Additional identity adapters: generic OIDC and SAML 2.0 (SP-initiated; signed responses or assertions required; IdP-initiated sign-in off unless an administrator allows it per provider). **[LATER]** GitHub OAuth, which is not OIDC and is tracked separately. Providers are added and changed without a restart, start turned off, and are tested with a real sign-in that reports the claims received before anyone else can use them. Setup per provider: `docs/identity-providers.md`.

**R-044 [D]** Identity adapters perform **authentication only**. Authorization is always core.

**R-045 [P]** Multiple identity adapters may be configured simultaneously. Each user record records its originating adapter. Two identities from different adapters are two users unless an admin explicitly links them — or, where an administrator turns it on for a provider, unless the provider vouches for an email address that exactly one active account has. Linking aliases and never merges (O-1). A first sign-in with no account is refused unless the provider is set to create accounts just in time (off by default); host policy can refuse just-in-time accounts install-wide.

### 5.2 Bootstrap

**R-046 [P]** A new installation has no account until it is set up: the first person to reach the console chooses the administrator's username and password there, and nothing is printed to a log. The setup endpoint is public and is refused once any account exists; whoever reaches it first becomes the administrator, which the install documentation says plainly. An operator may instead supply the first password at startup (`PANDO_ADMIN_PASSWORD`), which must be changed on first login. Passwords an administrator sets for someone else — creating an account or resetting one — are generated (18–22 characters, upper and lower case, digits and symbols), handed over out of band, and by default must be changed at the next sign-in. The account is administrative because it holds an install-scoped **Administrator** grant (R-080, R-081) — there is no admin flag on a user — so the power is revocable and grantable like any other.

### 5.3 Sessions and revocation

**R-047 [D]** Each identity adapter declares its own session policy and revocation mechanism, documented in that adapter's spec. There is no single global answer. The console and `GET /identity-providers` show each provider's session lifetime, revocation mode, and the window that results.

**R-048 [D]** **SCIM support** is the enterprise revocation and provisioning path. Where an IdP supports SCIM, Pando accepts pushed user and group lifecycle events (SCIM 2.0, a bearer token per provider). A pushed deactivation suspends and ends sessions at once; a pushed group change takes effect on the next request.

**R-049 [D]** **Suspended is not deleted.** Pando must model at minimum: active, suspended, deleted. A suspended user loses access immediately but their data-destruction rules (R-104) do not fire.

**R-050 [P]** For adapters that cannot push revocation, Pando falls back to expiry at next token refresh. The adapter's declared session lifetime is therefore the effective revocation window and must be documented as such.

### 5.4 Identity assertion to apps

**R-051 [D]** Pando **always** forwards a signed identity assertion to the upstream app on every proxied request. Apps are free to ignore it. Apps that wish to do per-user separation opt in by verifying it.

**R-052 [D]** The assertion is a **JWT** in a dedicated header, signed by Pando.

**R-053 [D]** Plain convenience headers (user, email, groups) are sent alongside the JWT. These are documented as **unverified** — an app that trusts them is trusting the network boundary, which is a legitimate but explicit choice.

**R-054 [D]** Claims:

| Claim | Content |
|---|---|
| `sub` | Pando's stable internal user ID. **Not** the email — an email change must not orphan an app's data. |
| `email` | Current email, if known. |
| `name` | Display name. |
| `groups` | Group identifiers the user currently holds. |
| `aud` | The app ID the assertion was minted for. Prevents replay against a different app. |
| `iat` / `exp` / `iss` | Standard. |

**R-055 [P]** Assertion lifetime is short — on the order of 1–2 minutes. The proxy mints a fresh assertion per request, so no refresh mechanism is needed and a leaked assertion is near-useless.

**R-056 [D]** Anonymous requests receive an assertion with `sub: anonymous`, a **constant**, not a per-visitor identifier. Public apps needing per-visitor sessions set their own cookie. Consequence: **absence of the header means the request did not come through Pando**, which is an unambiguous signal an app may reject on.

**R-057 [P]** Signing keys are published at a JWKS endpoint. Keys carry IDs; rotation is an overlap, not a cutover.

### 5.5 Tokens (non-human principals)

**R-058 [D]** **Delegated tokens.** A user creates a token and gives it to a machine. Actions taken with it are actions by that user, recorded as such.

**R-059 [D]** A delegated token's access is **continuously derived from its owner's live grants**, never frozen at creation. If the owner loses access to an app, the token loses it. If the owner is deleted, the token dies. *Operational consequence, accepted by design: integrations break at offboarding.*

**R-060 [D]** **Account-level tokens.** Created by Pando admins. The token is **its own principal** — it appears in ACLs and in the audit log under its own name, and it survives its creator. This exists so that not every automation needs a service account.

**R-061 [D]** Account-level tokens have an expiry. A never-expires option exists; host policy may forbid it.

**R-062 [P]** Tokens record a last-used timestamp so stale credentials are reviewable.

**R-063 [P]** Token secrets are displayed once at creation and never retrievable afterward.

---

## 6. Authorization

### 6.1 Two planes

**R-070 [D]** **Data plane** — permission to *use* an app. Binary.

**R-071 [D]** **Control plane** — permission to administer an app: deploy, configure, read logs, exec, share, delete.

**R-072 [D]** The planes are separate grants, with one implication only: **an app's owner has data-plane access to their own app.** No other control-plane role implies use. Holding operator on app A grants nothing on app B, and being a Pando admin does not grant use of apps you don't own.

**R-073 [D]** At app creation the creator receives both grants, recorded as two separate records. Either may be removed independently.

### 6.2 Subjects

**R-074 [D]** Grants may be issued to: a user, a group, or **anonymous**.

**R-075 [D]** Anonymous is a real ACL subject, not a bypass. An anonymous request is still proxied, logged, rate-limited, and given an assertion (R-056).

**R-075a [D]** **Public with a passcode.** The grant to anonymous may carry a passcode. The app is still shared with everyone, but the proxy sends a visitor who has not entered it to a passcode page — the sign-in page's other mode, on the same reserved path (R-172) — instead of to sign-in. Entering it sets a cookie in Pando's namespace, never forwarded to the app (R-173), that lets that browser in for a day. Only the passcode's digest is stored; changing it, or making the app private, asks everyone again. Wrong passcodes are limited per app and visitor. It is a key to *using* the app and nothing else (R-070).

**R-076 [D]** Any app owner may grant to anonymous by default. Host policy decides how (`public_sharing`): allowed, with or without a passcode; only with a passcode; or not at all. Like any policy setting it may be fixed at startup (R-271), and is then read-only in the console.

**R-077 [P]** The console must never present this as the bare word "public" **alone**. Naming the
action *Make it public* is fine and is what people look for; what may not happen is the word standing
by itself. The consequence is always stated with it: *anyone on the internet, without signing in.*

### 6.3 Groups

**R-078 [D]** Groups may be Pando-native or pushed from an IdP (R-048). **Permissions attached to a group are defined in Pando**, never inherited from the IdP. The IdP says who is in a group; Pando says what the group can do. A group may hold an installation role and roles on apps, exactly as an account can, and everyone in it holds them for as long as they are in it (R-079) — so adding someone to a team's group is how they get the team's access.

**R-079 [D]** Group membership is evaluated **live** at request time, not expanded to a member list at grant time. Otherwise upstream removals do not take effect.

### 6.4 Verbs and roles

**R-080 [D]** Control-plane permissions are individual verbs, in two scopes. **App-scoped** verbs are
held through a grant on one app. **Install-scoped** verbs are held through a grant with no app, and
confer nothing on any particular app — except the `install.apps.*` verbs, each of which confers one
app verb on every app, including apps created after the grant (issue #81).

Install-scoped:

| Verb | Grants |
|---|---|
| `install.view` | See the installation: its adapters, its capacity, its accounts |
| `install.users.manage` | Create accounts and change an account other than one's own |
| `install.policy.manage` | Edit host policy |
| `install.adapters.manage` | Configure adapters |
| `install.audit.read` | Read the install-wide audit log |
| `install.backup.manage` | Take, verify and restore backups (R-212–R-216) |
| `install.tokens.manage` | List, create and revoke service tokens (R-060) |
| `install.deploys.approve` | Approve or reject any deploy that needs approval, on any app, including one's own (R-155) |
| `app.create` | Create an app. Install-scoped despite the name: there is no app yet when it is checked |
| `install.apps.*` | One per app verb, below: that app verb on every app, without a grant on it |

Every app verb has exactly one install-scoped counterpart, which grants it on every app and nothing
else. There are no bundles; a combination is a role. Host policy is asked about the app verb, so a verb
it disables stays disabled for someone holding it install-wide (R-272). None of them is use: opening
an app still needs a data grant or ownership (R-072, R-087).

| App verb | Install-wide counterpart |
|---|---|
| `app.view` | `install.apps.view` |
| `app.logs.read` | `install.apps.logs.read` |
| `app.deploy` | `install.apps.deploy` |
| `app.restart` | `install.apps.restart` |
| `app.spec.edit` | `install.apps.spec.edit` |
| `app.secrets.write` | `install.apps.secrets.write` |
| `app.secrets.read` | `install.apps.secrets.read` |
| `app.exec` | `install.apps.exec` |
| `app.grants.manage` | `install.apps.grants.manage` |
| `app.routing.override` | `install.apps.routing.override` |
| `app.resources.override` | `install.apps.resources.override` |
| `app.egress.tighten` | `install.apps.egress.tighten` |
| `app.egress.loosen` | `install.apps.egress.loosen` |
| `app.deploy.approve` | `install.deploys.approve` |
| `app.delete` | `install.apps.delete` |

App-scoped:

| Verb | Grants |
|---|---|
| `app.view` | See the app exists and read its configuration |
| `app.logs.read` | Read application logs |
| `app.deploy` | Trigger a build and deploy |
| `app.restart` | Restart workloads |
| `app.spec.edit` | Modify the pinned spec |
| `app.secrets.write` | Set or rotate secret values |
| `app.secrets.read` | Read existing secret values |
| `app.exec` | Open a terminal in a workload |
| `app.grants.manage` | Grant and revoke access |
| `app.routing.override` | Deviate from the provider's default routing mode |
| `app.resources.override` | Deviate from host default resource limits |
| `app.egress.tighten` | Change an app's egress rules within the install's (R-182, R-184) |
| `app.egress.loosen` | Loosen the install's egress rules for an app, where policy permits it by verb (R-183, R-184) |
| `app.deploy.approve` | Approve or reject a deploy of this app that needs approval (R-155). In no built-in role |
| `app.delete` | Delete the app |

**R-081 [D]** Eight **immutable** built-in roles ship out of the box. They cannot be edited; Pando may add newly-introduced verbs to them across versions.

| Role | Scope | Verbs |
|---|---|---|
| **Viewer** | app | `app.view`, `app.logs.read` |
| **Operator** | app | Viewer + `app.deploy`, `app.restart`, `app.spec.edit`, `app.secrets.write`, `app.egress.tighten` |
| **Owner** | app | All app-scoped verbs except `app.deploy.approve` (R-155) |
| **Administrator** | install | All install-scoped verbs |
| **Creator** | install | `app.create` — makes apps, and so owns and manages the ones it makes (R-073), and nothing else |
| **App viewer** | install | The counterparts of Viewer's verbs: `install.apps.view`, `install.apps.logs.read` |
| **App manager** | install | The counterparts of Owner's verbs: every `install.apps.*` verb, and not `install.deploys.approve` |
| **Auditor** | install | `install.audit.read`, `install.apps.view`, `install.apps.logs.read` — sees every app and the audit log, changes nothing |

Owner and Administrator partition the catalog but for one verb, and neither contains a verb from the
other's scope. The exception is `app.deploy.approve`, which no built-in role holds: signing off on a
deploy is a trust an installation hands out deliberately, to people it names (R-155). An
Owner of every app in the installation still administers nothing. An Administrator holds every
install verb, `install.apps.*` among them, so it can view and manage **any** app, whoever made it, with
every app verb — approving deploys through `install.deploys.approve` — subject to host policy like
everyone (R-272). It is still not the app's owner of record (R-031), and
managing an app is not using it: opening an app through the proxy still needs a data grant or
ownership (R-072, R-087). App viewer, App manager and Auditor are Viewer and Owner on every
app, and the security reviewer's role; a custom role may hold any one `install.apps.*` verb alone.

**R-082 [D]** Custom roles may be composed from the verb list and assigned to users or groups. A role's name is unique ignoring case and surrounding spaces, and no custom role may take the name of a built-in one.

**R-083 [D]** `app.secrets.write` is deliberately separable from `app.secrets.read` — rotating a credential and reading it are different levels of trust. Secrets are write-only after creation for anyone below owner.

**R-084 [D]** `app.exec` is its own verb, not bundled into app-admin. A custom role may grant deploy and logs without a terminal.

**R-085 [D]** Host policy may **disable exec install-wide**.

**R-086 [D]** Exec is the highest-privilege action in the system. A holder can read the database directly, read injected environment including secrets, and modify a running workload in ways that never appear in the spec. The documentation must state this plainly rather than implying the verb list is a security boundary against someone holding `app.exec`.

**R-087 [D]** Pando does not claim to defend against its own host operator. A host admin has root and can reach any container outside Pando entirely. What Pando guarantees is that the **supported path** requires a grant — so unauthorized access requires deliberately leaving the tool, which is a materially different thing to detect and audit.

**R-088 [D]** **An installation cannot be left with nobody who can administer it.** Removing the last install-wide grant holding `install.users.manage` is refused, and so is removing the last person who holds it through a group — a group with the role and nobody in it administers nothing, and the message names the way out: make someone else an administrator first. The rule is stated in terms of the verb rather than the built-in role, because a custom role (R-082) holding it is just as much an administrator. Recovery from the state this prevents requires shell access to the host and `pando admin`, which is a different and much higher bar than the one click that would otherwise reach it.

---

## 7. Onboarding an App

### 7.1 Input

**R-090 [D]** The user points Pando at a source — a public GitHub repo in v1 — plus routing and hosting choices made in the Pando console. Nothing is read from the repo for permission or policy.

**R-091 [D] [LATER]** Private repos are in scope, supporting the credential mechanisms GitHub offers (PAT, GitHub App installation, deploy keys). **[O-3]** — whether a credential belongs to the app or to the user who supplied it is unresolved; note that user-owned credentials die at offboarding like delegated tokens.

**R-092 [D]** **Source allowlist.** Host policy may restrict deployable sources — to named orgs, named repos, a specific forge, or registry namespaces. Evaluated as admission control **before anything is cloned**, so a blocked source never touches disk. Default is empty, meaning anything.

### 7.2 Detection

**R-093 [D]** Detection is a **detector auction**. Every builder adapter inspects the source and bids with a confidence score and a draft spec. Highest bid wins and produces a **proposal**, not a deployment.

**R-094 [D]** Confidence ladder, highest first:
1. **Already-published image** — check the repo's namespace on ghcr.io and Docker Hub before building anything.
2. **Explicit deployment artifacts** — Dockerfile, compose file, Procfile, `devcontainer.json`, Nix flake, release binaries.
3. **The maintainer's own build commands** — `.github/workflows`, or a command runner's target that
   says how the app is built and run: `Makefile`, `Taskfile.yml`, `justfile`. Ranked among
   themselves by how load-bearing they are: a workflow is what runs on every push, where a runner
   target is what somebody wrote down. Amended 2026-09-14: this tier said "CI workflows" and named
   only `.github/workflows`. The others are the same artifact — the build written down by the person
   who wrote the app — and excluding them sent repositories that have one down to tier 4, where
   convention-matching reads a Go module, plans `go build`, and silently drops the client the
   Makefile would have built. Nothing about the tier's rank or meaning changed.
3b. **Two declarations that imply an ordering** — where one file says where it builds to and another
   says it needs that directory, the build order is stated rather than inferred. `vite.config.ts`
   with `outDir: "../cmd/server/dist"` and a `//go:embed dist` in `cmd/server` are the case this was
   added for. Below tier 3 because it is an ordering and not a command: it knows what must happen
   first, and has to be told by tier 4 what it happens before. Above tier 4 because tier 4 would not
   notice at all. Added 2026-09-14; see
   [the design note](design/notes-declared-builds-without-a-runner.md).
4. **Ecosystem manifests** — `package.json` + lockfile, `go.mod`, `Cargo.toml`, `pyproject.toml` plus framework markers, `pom.xml`, `Gemfile`.
5. **Static** — `index.html` at root, or a known SSG config.

**R-095 [P]** For tier 4, wrap an existing buildpack implementation (Paketo, nixpacks) rather than
reimplementing convention-matching. **nixpacks is the default**, because it *generates a Dockerfile*
and stops — so BuildKit builds it, no container runtime socket is involved anywhere (R-112), the
install needs no extra service, and the plan is readable by the person whose app it is. Paketo is
opt-in and needs a registry service in the install topology: the CNB lifecycle exports the image
itself, to a registry or a daemon, and the daemon is forbidden.

**R-096 [D]** A **compose file is a complete answer**, not a hint. Import it verbatim as a bundle: services, `depends_on` ordering, healthchecks, named volumes, internal network.

**R-097 [D]** A **trial run** in throwaway isolation is part of detection. Port discovery happens by observing what the process binds, not by asking.

**R-098 [D]** The user reviews the proposal, then it **pins**. Detection does not re-run implicitly (R-022).

**R-099 [D]** Compose constructs incompatible with the boundary are rejected or rewritten, with the reason shown: `network_mode: host`, `privileged: true`, bind mounts to host paths, `deploy.replicas`. Host policy governs whether an admin may override (R-190).

**R-099a [D]** A **configuration file** a compose service bind-mounts from the repository — a Caddyfile, an `nginx.conf`, an `init.sql` — is copied into the spec at detection and placed in the workload at every start. It is a snapshot, and the import says so: changing the file in the repository does nothing until the app is read again. This is R-020 rather than an exception to it — the spec remains the sole record of how the app runs. Files are text and capped; anything larger is a build input and is refused with that reason. A runtime that cannot place a file declares so in its capabilities, and the planner refuses before anything is created (R-254).

**R-100 [D]** A user may **promote** a compose-declared service to a Pando-managed one — e.g. binding an ad-hoc Postgres to a real one. Shown as an explicit diff, never automatic.

**R-101 [D]** There is always a bottom escape hatch: supply an image reference and a command, skipping detection.

### 7.3 When detection cannot decide

**R-102 [D]** **Ask, never guess.** If no adapter bids above threshold, or two bid equally, Pando asks a specific question. It does not pick.

**R-103 [D]** **The number of questions is the product metric.** If a normal repo requires six questions to deploy, Pando has failed its premise regardless of how good the questions are.

**R-104 [D]** **Questions are blockers; everything else is configuration.** Pando asks only when it genuinely cannot proceed. Anything with a reasonable default gets the default and is changeable later in settings. Memory limits, restart policy, log retention, auto-deploy are configuration, not questions.

**R-105 [D]** **Every question must be self-contained and pasteable.** It states what is being asked, why, what a valid answer looks like, and enough context that a model which cannot see the repo can answer it. The expected workflow for a non-technical user is to paste the question into the assistant that wrote the app and paste the answer back. This is a hard requirement on question text, and it is what makes AI support useful without making it required.

**R-106 [D]** AI assistance is optional supporting functionality, never required. It may be applied to reading README prose, disambiguating monorepo entrypoints, proposing repairs from a failed build log or a failed detection, and answering the questions detection could not; there it emits the same spec object and passes the same review gate. It may also draft custom roles and groups (R-343), propose host policy changes (R-344), turn a question about the audit log into filters (R-345), and answer how-to questions from the generated reference (R-346). Each of those proposes and a person applies: a draft takes effect only when a person creates or saves it through the ordinary endpoint, under their own authority. With no model configured, or with a function assigned to no adapter (R-259), each tap degrades to a question or to the screen without AI, not a dead end.

**R-107 [D]** The correct failure: a repo needs Postgres and never mentions it anywhere — no compose service, no `DATABASE_URL` in any sample. The trial run crashes. Pando shows the log and stops. That is the right outcome, not a gap to close with inference.

### 7.4 Screening a deployment plan

R-106 says AI assistance is optional supporting functionality and names the places it may be
applied. This section specifies the first built: a screening pass over the proposal the deterministic
pipeline has already produced, run in two cases only — when that proposal failed, to repair it, and
when it asked questions, to answer them (R-336). Its purpose is to raise the share of repositories
that deploy on the first attempt without anyone being asked a question.

**R-330 [D]** **Screening reviews Pando's own answer; it is not a second detector.** An AI adapter
does not bid in the auction (R-093) and does not rank against one. It is given the repository and the
proposal the detectors produced, and reports what that proposal got wrong or left out. The auction
stays a pure function of the source, and is still the whole answer when no screener is configured.

**R-331 [D]** **Screening amends the spec and nothing else.** Amendments land in the draft spec
(R-020: the spec is the sole record of how an app runs), are shown in the review alongside everything
detection worked out, and pin only when a person accepts the proposal (R-098). There is no path from a
model's output to a running app that does not pass the review gate R-106 requires.

**R-332 [D]** **The amendable surface is a closed set.** A screener expresses a change as one of a
fixed list of typed amendments. No amendment in the list changes host policy, isolation class,
adapter selection, routing, resource limits, egress, grants, or a secret's value, so a screener has no
way to request those changes.

**R-333 [D]** **An observation outranks a screening.** Where the trial run established a fact — a port
the process bound, a directory it wrote — a screener may not overwrite it (R-097). It may supply what
was not observed.

**R-334 [D]** **Every amendment carries evidence and is attributed.** Each one names the files in the
repository it rests on and states its reason to the R-105 standard. The review shows which adapter and
which model produced it, and the spec records screened provenance on the fields it touched, the way
`Port.Source` and `EnvEntry.Source` already record observed and human ones. An amendment that cites
no file is refused.

**R-335 [D]** **Screening never blocks and never fails a detection.** No adapter configured, an
unreachable provider, an expired budget, a timeout, an answer that does not parse: each one leaves the
deterministic proposal exactly as it was (R-106). Screening runs after the proposal is complete, so
when it fails the proposal is the one Pando would have produced without it.

**R-336 [P]** **Screening runs only when detection needs it or a person asks, and is on when its
functions are assigned to an AI adapter (R-259).** Detection needs it in two cases. When the proposal failed — the trial run
crashed, or no detector could read the repository — the adapter is asked to repair it. Otherwise, when
detection has questions for a person, the adapter is asked to answer them, and may change nothing else.
A detection that produced a plan without failing or asking anything makes no call at all: no latency,
no cost, and no repository contents leaving the host on the common path. At most one call is made per
detection; a repair is handed the questions too. The third case is a person reviewing the proposal
telling the adapter what is wrong with it; the adapter checks that against the repository and changes
what the repository supports, under the same closed set, evidence and refusals (R-332 – R-334), and
replies. Each such request is one call, made only because the person asked. Configuring an adapter, which means supplying a
credential, and assigning it these functions are the deliberate act; asking a second time would charge the setup
cost twice (R-002). An install that was screening before functions were assigned keeps them assigned.
Host policy may forbid screening install-wide, and an install may turn it off per adapter or per
function.

**R-337 [D]** **Screening sends repository contents to the adapter's provider, and records that it did.**
Every screening writes an audit event naming the adapter, the model, and the files that were read. The
spec half of what is sent is safe by construction — R-020 makes an export safe to hand to someone, so
it carries no secret values — and the repository half is not. The person who configured the adapter is
the person who decided that, which is why R-336 makes configuring it the opt-in.

**R-338 [D]** **A screener may answer Pando's own questions.** An answer is an amendment like any
other: evidenced, attributed, shown in the review, and refused if it does not match an outstanding
question. Each question answered this way is one fewer for a person to answer (R-103, R-105).
Outstanding questions are one of R-336's two triggers on their own, not only something a repair may
settle in passing.

**R-339 [P]** Screening is bounded by files read, bytes read, and wall clock, declared per adapter and
capped by the install. An exhausted budget ends the screening and keeps what it produced, under R-335.

### 7.5 AI assistance outside detection

R-106 also allows four administrative uses. Each is an AI function (R-259), assigned to an adapter
like screening and off until it is, and each is an API endpoint first (R-261). None of them changes
anything: each returns a draft, a filter or an answer, and a person acts on it through the endpoint
that already exists for that action, under their own authority.

**R-343 [D]** **An AI adapter may draft access.** Given a description of who should be able to do
what, `draft_access` returns a custom role, a group with its members, or both. Verbs come only from
what the requester could grant — every app verb, and the install verbs they hold themselves — and core
refuses a verb outside that set, a role mixing install and app verbs (R-080), a role name that already
exists or is built in (R-081, R-082), and a member who is not an account on the install. A person
creates the draft through the ordinary role and group endpoints. Asking requires
`install.users.manage`, the verb that creates them.

**R-344 [D]** **An AI adapter may propose host policy.** `draft_policy` returns the policy document as
it would be saved and each change in it. A change to a field fixed by the startup configuration
(R-271) is declined whatever the model returned, with a reason naming the file and key, or the
variable, that fixes it; core enforces this, not the model. A value that does not read as its field,
or a verb list naming a verb that does not exist, is refused. Nothing is saved until a person saves
the proposal through the ordinary policy endpoint. Asking requires `install.policy.manage`.

**R-345 [D]** **An AI adapter may search the audit log for a person.** `search_audit` turns a
question into one audit filter: actions, app, actor, target, an ID involved, and a time range in UTC.
Core runs the filter and returns it as ordinary audit parameters, so the result can be checked and
changed without AI. The adapter never reads the log (R-027, R-226); it is given the accounts, apps and
action names it needs to write the filter, and afterward the records the filter found, to summarize.
The summary is written from those records alone, and says so when the log does not record what was
asked. Asking requires `install.audit.read`.

**R-346 [D]** **An AI adapter may answer "How can I…" from the reference.** `answer_reference`
answers from the generated API, CLI and MCP reference, cites the endpoints, commands and tools it
relies on, and says so when the reference does not cover the question. A citation that is not in the
reference is dropped. It describes and never acts. Any signed-in user may ask.

---

## 8. Build

**R-110 [D]** Builds never run on the host (R-024).

**R-111 [D] [V1]** The default local builder is **rootless BuildKit in its own container**. Pando starts it, hands it source, receives an image.

**R-112 [D]** **The build path never exposes a container runtime socket to build code.** Mounting the Docker socket into a build is a host compromise and is categorically forbidden.

**R-113 [D]** Build code has no access to Pando's state store, no access to any other app's secrets, and no route to the internal network or other bundles.

**R-114 [D]** **Build isolation class is declared and enforced independently of runtime isolation class.** A strong runtime does not imply a strong build. Host policy may set a floor on each.

**R-115 [P]** Isolation classes, weakest to strongest: `container` (shared kernel), `sandboxed` (gVisor/Kata), `vm` (Firecracker/Incus), `dedicated-host`.

**R-116 [P]** Where a runtime adapter can provision an isolated environment per app (e.g. Incus), building *inside that environment* is preferred, since build isolation then comes free from the same boundary.

**R-117 [P]** Build filesystem is discarded after the build. Layer cache is namespaced per app; no cross-app cache sharing.

**R-118 [P]** Build egress defaults to open, on the grounds that build output is reviewed before it runs. Host policy may restrict it to a package-registry allowlist, accepting that some repos will then fail to build.

**R-119 [P]** Build timeout: 30 minutes, per-app override.

**R-120 [P]** Deploy pins a commit SHA. Auto-deploy triggers (R-141) advance the pin explicitly.

---

## 9. Slots and Services

**R-130 [D]** An environment variable in `.env.example` is **a hole with a type**. `REDIS_URL` tells Pando the app needs a Redis; it does not tell Pando which one. Typing comes from the URL scheme in the sample value, the variable name, or a compose image name.

**R-131 [D]** A slot is resolved exactly three ways, chosen by the user:
- **Provisioned** — Pando stands one up inside the bundle. Hobbyist default.
- **Bound** — point at an external instance, or one already running on this host. Enterprise default.
- **Literal** — paste a value.

**R-132 [D]** Resolution is never silent. **An unfilled required slot blocks deployment** rather than launching something that crashloops on connection refused.

**R-133 [O-4]** Distinguishing required from optional slots is unresolved. A `.env.example` with forty keys where six matter is the common case, and getting this wrong means either blocking on nothing or crashlooping.

**R-134 [P]** Provisioned services live inside the bundle and are not addressable from outside it. Two apps do not share a provisioned service; sharing is done by binding both to one external instance.

**R-135 [P]** A provisioned service's data follows the app's volume rules (§12), including the backup-or-discard prompt at delete.

---

## 10. Runtime and Lifecycle

### 10.1 States

**R-140 [P]** App states: `draft` → `proposed` → `running` | `degraded` | `stopped` | `failed` | `archived`.

### 10.2 Deploy triggers

**R-141 [D]** **Manual deploy is the default.** Auto-deploy is opt-in, with two distinct triggers, chosen separately:
- default branch updated
- new release tagged

**R-142 [P]** Trigger delivery is by polling by default, since inbound connectivity cannot be assumed. Webhook delivery is available where the host is reachable.

**R-143 [P] [LATER]** Watch for new tags on an upstream image, for apps deployed from a published image rather than source.

### 10.3 Deploy strategy

**R-144 [D]** **Recreate is the default strategy.** Stop the old workload, start the new one. Accepts downtime. Correct for anything holding an exclusive lock or a local database file.

**R-145 [D]** **Start-then-swap is available as an explicit opt-in.** Not a bare toggle — the console must state the constraint in plain terms: *two copies of your app run at the same time during a deploy. Do not enable this if your app writes to a local file or runs migrations on startup.*

### 10.4 Failure handling

**R-146 [D]** **Build failure:** nothing is replaced. The running version continues serving. Report and stop.

**R-147 [D]** **Automatic rollback is disabled by default**, available as opt-in. Rationale: not every app has a meaningful health check, and reachability does not imply correctness, so auto-rollback is helpful but not defensible as a default. It is also actively wrong where a schema migration has already run.

**R-148 [D]** **Reconcile when possible; report when not.** If observed state diverges from the spec — a container deleted by hand, a workload that exited — Pando restores it. If it cannot, it reports.

**R-149 [P]** Restart backoff: immediate, then 5s, 15s, 60s, capped at 5 minutes.

**R-150 [P]** Ten failures within 30 minutes marks the app `failed`.

**R-151 [D]** **A `failed` app stays failed until a human intervenes.** Pando does not keep retrying on a long interval. No silent self-healing days later.

**R-152 [P]** Revision history retains the last 10 pinned specs for rollback.

### 10.5 Deploy approval

**R-154 [D]** **A deploy may need somebody's approval before it runs.** It is off by default and adds nothing to an app that does not need it (R-002). A deploy needs approval when any of these is true:
- host policy requires approval for every app;
- host policy requires it for this app. An administrator sets this, and the app's owner cannot turn it off;
- the app requires it of itself (`deploy.require_approval` in its spec). The requirement is read from both the running spec and the one being deployed, so turning it off is itself approved;
- the deploy loosens the install's egress rules and policy says loosening needs approval (R-183).

Approval is a human sign-off on a change. It is not a test gate (R-011).

**R-155 [D]** **Two verbs approve.** Anyone holding `install.deploys.approve` may approve or reject any deploy on any app. The Administrator role holds it. Anyone holding `app.deploy.approve` on an app may approve or reject deploys of that app. No built-in role holds it, so an installation grants it through a custom role (R-082). Either verb may approve its holder's own request. An installation that wants two people keeps the verb from the people who deploy. App manager does not hold `install.deploys.approve`.

**R-156 [D]** **A request is tied to one spec revision**, and so to one commit (R-120). Approving it deploys that revision through the ordinary plan and deploy path. The plan runs again at approval, because policy may have changed while the request waited. A newer request for the same app supersedes an older one that is still waiting. Host policy sets how many approvals a deploy needs (default one) and how long a request waits before it expires (default seven days; zero means never). A rejection by any approver ends the request.

**R-157 [D]** **Rolling back to a revision that was already running does not need approval.** It was approved, or did not need to be, when it first ran, and rollback is how a bad deploy is undone quickly (R-152). Restarts and secret rotations (R-193) change no spec, so they need no approval either.

**R-158 [D]** **Auto-deploy and approval do not combine.** A spec that turns on auto-deploy (R-141) is refused while its app needs approval, and the refusal says why. An app that already auto-deploys stops doing so when policy starts requiring approval for it, and the console says so on the app. Queuing an approval request for every push produces a backlog of requests that nobody wants to read.

**R-159 [D]** Every request, approval, rejection, expiry and supersession is an audit event (R-227). Approvers are told through the notification adapter when a request is waiting.

### 10.6 Scale

**R-153 [D]** One app, one place (R-010). Replica counts from compose are rejected (R-099).

---

## 11. Networking and Routing

**R-160 [D]** Routing is an adapter category. **[V1]** loopback, Traefik and Cloudflare Tunnel.

**R-161 [D]** Each routing adapter advertises which addressing modes it supports: subdomain, path prefix, port.

**R-162 [D]** Each routing adapter declares a **default mode** (e.g. subdomain for Cloudflare). Adding an app uses the default without asking.

**R-163 [D]** Deviating from the default requires `app.routing.override`. Host policy governs who holds it.

**R-164 [D]** **Proxy mode** is a supported topology: one hostname, one certificate, one thing to open on the firewall, all apps reached by logging into Pando first. This is the recommended enterprise topology because it is far easier to get approved than N public hostnames.

**R-165 [D]** In the non-proxy topology, apps have their own hostnames; users bookmark URLs and carry a session. Pando is invisible except at login.

**R-166 [D]** **Subdomain is preferred where a wildcard is available. Path prefix is the fallback.**

**R-167 [D]** Under path routing, Pando strips the prefix before forwarding and sends `X-Forwarded-Prefix`. It does **not** rewrite response bodies. Apps built on frameworks that respect a base path will work; apps that hardcode absolute paths will not.

**R-168 [D]** Where Pando can detect a likely path-routing incompatibility, it shows a **dismissible warning**, not a fix: *"No persistent volume found…"*-style phrasing — e.g. *"Does your app need path prefix routing?"* Consistent with R-028.

**R-169 [D]** TLS issuance is a per-adapter concern: a routing adapter that can issue certificates declares how, and one that cannot says so through `RoutingCapabilities`. **For the edge Pando runs itself (R-174), both ACME challenge types are offered and the install chooses**: HTTP-01 per hostname, which needs nothing but a reachable port 80 and an email address, or DNS-01 for a wildcard, which needs a DNS provider credential and is what R-166's "where a wildcard is available" refers to. Neither is the silent default — an install that picks neither gets `:80` and is told so, because a certificate that quietly did not issue is worse than one that was never promised.

**R-170 [P]** The proxy must support websockets, server-sent events, streaming responses, and large uploads. Body size caps and idle timeouts are configurable per app with permissive defaults.

**R-171 [D]** An app with its own login page is stacked behind Pando's auth by default; the user sees two logins. This is expected and not remediated (R-028), and it is **not warned about** — an app presenting its own login page is that app working correctly. Pando has no basis for treating a working app as a problem, and a warning here would train users to dismiss warnings that do matter.

**R-172 [D]** **Signing in works on an app's own hostname.** Pando reserves one path — `/.pando` — that it answers on every hostname it serves, and the sign-in page lives there. An app keeps every other path including `/login`, which R-171 requires. Without this, subdomain routing has nowhere to sign in: the app's hostname is the app's, so a redirect to `/login` returns to the proxy and redirects again, forever.

**R-173 [D]** **Pando's own credentials never reach an app.** Every cookie in Pando's namespace is removed from a request before it is forwarded, the same rule and for the same reason as the `X-Pando-*` headers in R-053. An app that receives the session cookie does not need to trust anything to impersonate its visitor — it can replay the credential against Pando's API. What an app is given is the assertion (R-054): scoped to that app, signed, and short-lived.

**R-174 [D]** **The edge is Pando's to run, not the operator's.** Where an install wants a component in front of Pando — a reverse proxy terminating `:80` and `:443`, issuing certificates, and giving apps public hostnames — turning it on is a setting in Pando, and Pando creates, configures, reconciles and removes it through a runtime adapter like anything else it runs. Editing a Compose file and running a second service alongside Pando is the setup cost R-002 says is paid once at the host, charged again for every install that wants what R-166 prefers. The exception is Pando's own state store, which has to exist before Pando runs (design 00 §1.1).

---

## 12. Egress and Isolation

**R-180 [D]** Apps are isolated from each other (R-025). If something gets into an app, it cannot get out of that app into another.

**R-181 [D]** **Egress defaults to allow-all.** Host policy may change the install's mode to a **denylist** (anywhere except the listed destinations) or an **allowlist** (only the listed destinations). Blocking private address ranges is a separate switch that works with any mode, including allow-all. *(Amended by issue #79: a denylist mode was added, and blocking private ranges stopped being a mode of its own.)*

**R-182 [D]** **An app's egress rules start from the install's and move only as policy lets them.** An app may:
- **add entries to or remove entries from** the install's list, and
- **keep a list of its own** — an allowlist or a denylist — which is applied **on top of** the install's rules: a destination is reachable only if both allow it. An app on an install with no list can lock itself down this way.

Changes are classed by what they do to the install's rules, not by what they look like. **Tightening** — adding to the install's denylist, removing from its allowlist, keeping a list of the app's own, turning private-range blocking on — is always within policy (R-272). **Loosening** — removing from the install's denylist, adding to its allowlist, turning private-range blocking off when the install has it on — is the only kind host policy gates. *(Amended by issue #79. It used to say an app's allowlist **replaced** the install's.)*

**R-183 [D]** **The install's rules are a floor.** An app list that is layered on top can only narrow them, so the install's list is a security boundary unless policy permits loosening it. Host policy decides whether it may be loosened: **never**, **by someone holding a verb**, or **only after a deploy approval** (R-154). With loosening forbidden, the floor holds for every app. Rules take effect at an app's deploy. A running app is not changed underneath it, as with any other policy change (O-10). When policy stops permitting a loosening that an app runs with, the app's next deploy is refused at plan time until the loosening is removed, and the refusal says which entries. *(Amended by issue #79. It used to say the install list was a default and not a security boundary, which was a consequence of R-182's old replace rule.)*

**R-184 [D]** Egress changes are gated by two verbs. `app.egress.tighten` changes an app's egress within the install's rules. `app.egress.loosen` loosens them, where policy permits loosening by verb. Both belong to Owner, and `app.egress.tighten` also belongs to Operator. *(Amended by issue #79. There was one verb, `app.egress.override`, which became `app.egress.loosen`.)*

**R-185 [D]** **An entry names a destination**: a hostname (`api.example.com`), a wildcard of its subdomains (`*.example.com`), an IP address, or a CIDR range, each optionally with a port (`api.example.com:443`, `[2001:db8::1]:443`). `*` names everywhere. A hostname entry matches the name the app asked for. An address or range entry matches where that name resolved. Blocking private ranges is also checked against resolved addresses, so a public name that resolves to a private address is refused too.

**R-186 [D]** **A runtime adapter either enforces egress rules or says it cannot.** A plan whose rules restrict anything is refused at plan time by a runtime without the capability (R-254). It is never deployed with the rules silently ignored. An app with no restriction in effect runs exactly as it would with no egress controls: nothing is placed in its path.

**R-187 [D]** **Where egress is restricted, only traffic through Pando's egress gateway leaves the app.** On the Docker runtime a restricted app's network has no route out. Its workloads are given `HTTP_PROXY` and `HTTPS_PROXY` naming a gateway that enforces the rules. Traffic that does not use the gateway — raw TCP, or a client that ignores the proxy variables — does not leave, even under a denylist. The plan says so wherever a restriction is in effect, because an app owner choosing a denylist would otherwise expect everything else to keep working.

**R-188 [D]** **The rules an app runs with are shown, merged.** The plan and the console show the effective mode, the effective list, private-range blocking, and where each part came from (the install or the app), along with every loosening and what it needs: forbidden, a verb, or approval.

**R-189 [P]** Build egress (R-118) is a separate setting and does not follow this model. A build runs before anybody has reviewed its output, so what it may reach is a different question.

---

## 13. Secrets

**R-190 [D] [V1]** **Local secret storage:** encrypted at rest with a key on the same disk.

**R-191 [D]** The threat model must be stated, not implied: this protects a leaked backup file or copied volume. **It does not protect against a compromised host** — a Pando that can inject secrets can decrypt them. Installs with real requirements are expected to use an external secrets adapter.

**R-192 [D]** Environment variables are the default injection mechanism, since slot detection keys on them and every app already reads them. File-based injection is available for apps that want it.

**R-193 [D]** On the env path, **rotation implies a restart.** Changing a secret and redeploying are effectively the same operation.

**R-194 [P]** Secret values are redacted in logs, in spec exports, and in the audit log. The audit log records that a secret changed, never its value.

---

## 14. Persistence and Volumes

**R-200 [D]** Persistence declared in a compose file is imported and honored. Nothing special happens.

**R-201 [D]** Where no volume is declared, Pando shows a warning at setup rather than inferring one:
> *No persistent volume found. If your app doesn't store data, you can ignore this. Otherwise define one here.*

**R-202 [P]** The trial run improves this warning: where Pando observed the app writing to a directory outside any declared volume, the warning names that directory.

**R-203 [D]** Rationale for treating this specially: an undeclared **Postgres** fails loudly on first boot. Undeclared **persistence** works perfectly until the second deploy, then silently discards everything while reporting healthy. It is the worst failure mode in the system, so it earns a warning even though inference is otherwise forbidden.

**R-204 [D]** **On delete, Pando asks whether to keep a final backup or discard it.** The kept backup is retained until explicitly discarded; it does not age out.

**R-205 [D]** Non-interactive delete (CLI, API, MCP) **backs up by default.** `--force` skips the backup.

**R-206 [D]** **Restore is in-place only.** A backup restores to an app recreated from the same spec, matched by Pando's own identity for it. Backups are not portable to arbitrary apps — Pando cannot know what is inside a volume, and promising portable restore means promising semantics it cannot verify.

---

## 15. Backup and Disaster Recovery

**R-210 [D]** **Per-app rolling backups** of app data. Scope is "recover from a recent mistake," not a DR product (R-013).

**R-211 [P]** Default: daily, 7 retained.

**R-212 [D]** **Full-host DR bundle.** A complete backup of the Pando install — state database, encryption key, configuration, everything needed to reconstitute the host without pain.

**R-213 [D]** The DR bundle is **encrypted under a separate passphrase or key supplied at backup time**, never one stored on the host. Rationale: bundling the encryption key with the encrypted database makes the bundle plaintext for anyone holding it — every secret for every app in one file. This is also structurally necessary, since a restore onto a fresh machine cannot unwrap keys held by the old machine.

**R-214 [D]** Consequence, accepted: **DR restore is deliberately interactive.** Losing the passphrase makes the bundle useless.

**R-215 [D]** **Restore verifies before applying.** A corrupt or incomplete bundle is detected before it clobbers a running install.

**R-216 [P]** Verification should also be invocable against a bundle without committing it, so a backup can be checked before it is needed rather than at the moment of disaster.

**R-217 [D]** Backup destination is an **adapter category** (R-252). Local disk by default is nearly
useless for the disk-failure case; a remote destination is configured the way every other provider is,
by configuring an adapter. **[V1]** `local` — a filesystem path. Others are ordinary adapters, added
without touching core (O-6 resolved).

---

## 16. Health, Logs, Audit, Notifications

### 16.1 Health

**R-220 [D]** Pando runs health listeners — health endpoints, uptime checks — so you know when an app goes down. This is in scope and distinct from testing (R-011).

**R-221 [P]** Health signal sources, in order: compose healthcheck if declared, HTTP endpoint if configured, TCP connect, process liveness.

### 16.2 Logs

**R-222 [D]** **Log retention is bounded by size, not time**, so a chatty app cannot fill a disk shared with twenty others.

**R-223 [P]** Default cap: 100 MB per app, oldest discarded first.

**R-224 [D]** **Retention must respect total host disk**, in aggregate across all apps. Pando must not be able to brick a host through accumulated logs and backups.

**R-225 [D]** Log masking is out of scope for now. Rationale: a holder of `app.exec` can read the data directly anyway, so masking logs does not create a boundary that otherwise exists. **[LATER]** auto-masking may be revisited.

### 16.3 Audit

**R-226 [D]** The audit log is in core and cannot be written or rewritten by an adapter (R-027).

**R-227 [P]** Auditable events: every spec mutation, every grant change, every deploy and every step of a deploy approval (R-159), every secret write, every token creation and use, every exec session, every policy change, every delete, every use of an app — once per visit, anonymous visitors included unless host policy turns that off — and every call that sends data to an AI adapter's provider, naming the function, the adapter and the model but not what was sent (R-337).

**R-228 [P]** Exec sessions are audited as a distinct event type — principal, app, workload, start and end. Command contents are **not** recorded. **[O-7]**

**R-229 [P]** Actions taken by a delegated token are recorded under the owning user, annotated with the token. Actions by an account-level token are recorded under the token's own name (R-060).

**R-347 [D]** **The live audit log is bounded in time, and nothing leaves it unarchived.** A calendar month past the retention period is written out as a compressed archive with a manifest — row count, first and last event, time range, size and SHA-256 — read back and checked against it, recorded, and only then removed from the live log. The removal is itself an audit event naming the month, the row count and the digest. Archives are kept by Pando under its own data directory or exported to a backup destination (R-217), as host policy says, and can be listed and downloaded through the API (R-261). Turning archiving off archives nothing and so removes nothing. This is R-224 applied to the one log that had no bound; it does not loosen R-027 or R-226: the role serving traffic still cannot update or delete an audit event, and removal is done by a separate role that can do nothing else. Archives carry what the rows carry, which never includes a secret value (R-194).

**R-348 [P]** Default retention is **three months**, which is also the **floor**: host policy may lengthen it and may not shorten it, and the function that removes a month refuses one that ended less than three months ago, whatever policy says, so nothing the running server can write lowers it. Archives are kept by Pando by default. **[O-27]**

### 16.4 Notifications

**R-230 [D]** Notification is an adapter category.

**R-231 [D] [V1]** Default is **console-only**. An installation that configures nothing else gets the
console adapter and nothing else; every other notify adapter is something an administrator adds.

**R-232 [D] [V1]** A built-in adapter for SMTP. *Amended by issue #50, which brought it forward from
LATER:* SendGrid, Mailgun and Amazon SES each offer an SMTP relay, so one SMTP adapter covers them and a
SendGrid-specific adapter would add a second way to do the same thing.

### 16.5 Events and subscriptions *(issue #50)*

**R-364 [D]** **There is one event catalog.** Every event a person can subscribe to has a stable name
(`deploy.failed`), says whether it is about one app or the whole installation, and lists its data
fields. It is defined once, in code, and the API (`GET /events`), the generated `docs/events.md`, the
CLI, MCP and the console's subscription form all read it, so none can describe an event the others do
not. A name or pattern that matches no catalogued event is refused when a subscription is saved.

**R-365 [D]** **Events come from what Pando already records, and from nowhere a person could skip.**
An audited action in the catalog is copied to the event outbox in the same transaction as its audit
row. A state change that has no audited action — an app's state, which includes its health, a deploy's
outcome, a scheduled backup that failed — is written by a database trigger on the row, so every
writer of that row is covered. Core writes the remaining two directly: adapter health changes and test
deliveries. An event carries only the fields the catalog lists, never a secret value (R-194).

**R-366 [D]** **Delivery is at least once and survives a restart.** Events are kept in an outbox table
until every matching subscription has a delivery for them, and a delivery stays pending until it is
sent or plainly fails. A receiver deduplicates by event ID. The outbox is a queue, not a history: an
event with nothing left to deliver is removed after 30 days **[P]**; the audit log is the history.

**R-367 [D]** **A subscription is a filter and a destination.** The filter is a list of event names,
prefixes (`deploy.*`) or `*`. A subscription is about one app or the whole installation. The destination
is a webhook URL or a notification adapter. A subscription hears about events that happen after it is
made. Subscriptions are managed from the API, CLI, MCP and console (R-261); a test delivery can be sent
to one subscription whatever its filter says.

**R-368 [D]** **A subscription is worth no more than its owner's access, checked at every delivery.**
A subscription belongs to a person, or to an account token (R-060), which is bounded by its own grants
like any principal; a delegated token's belongs to the person it acts for (R-058). Subscribing to an app
needs `app.view` on it; subscribing install-wide, and seeing or changing anybody else's subscriptions,
needs `install.events.manage`, held by Administrator. Every delivery is authorized as the owner at the
moment it is sent, so an owner who loses sight of the app, loses the verb, is suspended, or — for a
token — is revoked or expires stops receiving, and the delivery log says why. A token is not a person:
its subscription may send to a webhook or a channel, never to a destination that reaches people.

**R-369 [D]** **A webhook delivery is signed, retried and recorded.** Pando posts the event as JSON with
an HMAC-SHA256 signature over a timestamp and the body, the timestamp, the event's name and ID, and the
delivery's ID. A 2xx answer is a delivery; anything else, a redirect included, is retried on a schedule
of about a day **[P]**. Every attempt is recorded with its status code and error, a person can see recent
deliveries and their attempts, and any delivery can be sent again.

**R-370 [D]** **An endpoint that keeps failing is turned off, and its owner is told.** A subscription
whose every attempt has failed for a day, and at least five times **[P]**, is turned off with the reason
recorded, the turning-off is audited and is itself an event, and the owner receives a
`subscription_disabled` notification. Turning it back on clears the record of failures.

**R-371 [D]** **A webhook's signing key is a secret.** It is kept by the secrets adapter as ciphertext or
an external reference, never in the subscription row (R-190); it is shown once, when the subscription
is made or the key is rotated, and never again. Rotating is not offered over MCP (O-12).

**R-372 [D]** **A webhook cannot reach into Pando's own network by default.** A webhook may not send to a
private, loopback, link-local, unspecified or multicast address — the local network, the host itself, a
cloud metadata service — unless host policy's `allow_private_webhooks` is on. The address is checked when
the subscription is saved, for a readable refusal, and again on the address actually connected to, so a
name that resolves differently later is still refused. Redirects are not followed.

**R-373 [D]** **Pando's own notifications go to people, as each person chooses.** A notify adapter says
whether it reaches **people** (the console, email) or posts to a **channel** (Slack, Teams, Discord,
ntfy), as capabilities data (R-254). Pando's own notifications — a deploy waiting for approval, an app that
failed — go to every adapter that reaches people and never to a channel, because a message meant for one
person must not land in a room. Each person may turn each kind of notification off, or on, per channel;
a kind they have not chosen for follows its default.

**R-374 [D]** **Built-in notify adapters for the platforms people use.** Email (SMTP, R-232), Slack,
Microsoft Teams, Discord and ntfy. Each formats a notification for its platform. A channel adapter's
webhook URL, an ntfy topic and an SMTP password are credentials, stored like any adapter credential
(R-190). Pushover and other platforms are a new adapter each, by pull request (R-253).

**R-375 [D]** **A webhook sends the request its receiver expects.** A webhook subscription may choose its
method (POST, PUT or PATCH) and content type, carry headers of its own, and render its body from a
template given the event and its description; empty sends Pando's envelope. Header values may be
credentials and are kept as the signing key is (R-371): sealed, named but never shown again. A header
Pando or the transport sets — `Pando-*`, `Host`, `Content-Type` and the like — cannot be set, so a
receiver can always trust the signature, which is computed over the body actually sent. A template is
checked against a sample event when it is saved, and a JSON content type needs it to produce JSON.

**R-376 [D]** **A failed deploy and a failed backup tell the people they concern.** Pando's own
`deploy_failed` goes to the app's owner and to whoever started the deploy — the person behind a delegated
token, nobody for Pando itself or an account token; `backup_failed` goes to the app's owner and to
everybody holding `install.backup.manage`. Each person is told once, on the channels R-373 allows.

**R-377 [D]** **The console has an inbox.** What the console notify adapter records is listed to the
person it is for, newest first, with a count of those unread beside Settings on every screen; one or all
can be marked read, by that person alone. A notification links to where in the console to look, when
Pando knows its own address (`external_url`). Every surface reaches it (R-261).

**R-378 [D]** **An app shows its own events.** Anyone who can see an app reads its recent events, as a
webhook receives them and as a person reads them, and manages its subscriptions, from the app's own
Events tab and `GET /apps/{id}/events`. The feed holds what the outbox keeps (R-366).

---

## 17. Resources and Capacity

**R-240 [D]** Default CPU, memory, and disk limits are set at the host. Every new app inherits them.

**R-241 [D]** Per-app override is available, gated by `app.resources.override`.

**R-242 [D]** **Pando tracks total allocation against host capacity** and must refuse a deploy that would oversubscribe, failing at plan time with a readable message rather than letting the kernel resolve it with OOM kills.

**R-243 [D]** **Capacity is adapter-reported, not host-inspected.** The local Docker adapter reports the machine it runs on; a clustered adapter reports what its cluster has. Core does not read `/proc`.

**R-245 [P]** **An app's parts show what they are using now**: CPU, memory and disk for each workload, and each volume's size, beside the limits it runs under — reported by the runtime adapter (R-243) and read on demand. A reading, not a history: Pando keeps no metrics store, graphs no trends and alerts on nothing (R-016). A runtime that cannot report it says so rather than showing zeros.

**R-244 [P] [LATER]** Per-user quotas (max apps, max disk) as a policy knob. The counting required already exists for R-242.

---

## 18. Adapters

**R-250 [D]** **The app declares requirements; adapters translate.** An app never says "Incus config." It says *this is what I need for hosting*, or *this is what I need for routing*. For each provider we write an adapter that normalizes those requirements to that provider's vocabulary. To core, every provider looks the same.

**R-251 [D]** Core never learns a provider's vocabulary. A requirement crossing the interface is expressed in Pando's terms — "2 GB, one persistent volume, one exposed HTTP port" — and the adapter turns it into a VM profile or container arguments.

**R-252 [D]** Adapter categories: identity, routing/ingress, builder, runtime, secrets, services, notification, **backup**. **Scanner** is the ninth (R-317) and **AI** the tenth (R-258).

Backup was added in phase 9, reversing an earlier decision that a backup destination was a byte sink
rather than a category (design 03 §8.1). The earlier reasoning still describes a *destination*
correctly — a thing that takes bytes and gives them back has nothing to negotiate. What it missed is
that the destinations people actually want are not byte sinks: an object store expires objects and
versions them, a mounted share does neither, and whether a destination can enforce retention itself
is exactly the kind of question R-254 says belongs in a capabilities struct rather than in a type
assertion. A `Destination` interface would have had to grow one anyway, under a different name.

**R-253 [D]** **Adapters are compiled in-tree.** Pando ships as a single binary. Third parties contribute adapters by pull request. There is no external plugin protocol and none is planned.

**R-254 [D]** Every adapter **advertises capabilities as data** — `isolation_class`, `supports_persistent_volumes`, `supports_wildcard_tls`, supported addressing modes, and so on. The planner uses these to **fail at plan time with a readable error** rather than halfway through a deploy.

**R-255 [D]** Runtime adapters declare an isolation class. Host policy may require a minimum (R-114).

**R-256 [P]** Multi-machine capability comes entirely from adapters that span machines (e.g. Incus placing VMs across a cluster). Pando remains a single control plane, models no host objects, and performs no placement logic. The scope line (R-010) holds: Pando delegates to something that schedules; it does not schedule. A single control plane may run as several replicas of the Pando process sharing one database: they are one control plane — one state store, one leader for install-wide work, one policy and one audit log — and none of them is a place an app runs. Running several copies of Pando is not scheduling apps (issue #72; topology and prerequisites in `docs/design/notes-multiple-replicas-issue-72.md`).

**R-258 [D]** **AI is the tenth adapter category.** It passes both halves of the test design 03 §8.1
states before a category may be added. The planner's half: whether a screener can read a repository at
all, how much of one it can read, and which functions it performs are questions with consequences
before any work starts, and R-254 says those belong in a capabilities struct rather than in a type
assertion. The vocabulary half: models, context windows, tokens, tool calls and system prompts are a
provider's vocabulary and a large one, and R-251 says core never learns it. Core says "screen this
proposal against this source"; what that costs and how it is asked is the adapter's business.

**R-259 [D]** An AI adapter declares which **functions** it performs as capabilities data. Repairing a
failed detection (`repair_plan`) and answering detection's questions (`answer_questions`) are the
first two, and together they are §7.4's screening; revising a plan when a person asks (`revise_plan`)
is the third, and R-343 – R-346 add four more. Reading README prose, disambiguating monorepo
entrypoints and repairing a failed deploy-time build are the others R-106 names, and an adapter that
cannot do one of them says so rather than failing when asked. Each function is **assigned** to at most
one AI adapter, and one adapter may be assigned any number of functions. The database enforces the
first, and assigning a function that another adapter handles is refused with a message naming that
adapter. An adapter may be assigned only functions it advertises. An install has at most one AI
adapter per provider. When an adapter advertises that it can choose a model, an assignment may name a
model for that one function, and otherwise uses the adapter's own; an assignment naming a model on an
adapter that cannot choose one is refused. A function assigned to no adapter is off, which degrades as
R-106 describes and is not an error.

**R-257 [D]** A runtime adapter may be swapped under an existing app, and it is **neither a migration nor a plain redeploy**: it is a destructive spec change requiring explicit confirmation, with volumes resolved through the keep-or-discard flow (R-204). Pando does not move volume contents between adapters — it cannot know what is inside a volume (R-206), and relocating running workloads is one step from the scheduling R-010 forbids.

---

## 19. Surfaces

**R-260 [D]** Four first-class administrative surfaces, all shipping: **API, CLI, MCP, web console.**

**R-261 [D]** **The API is the product.** The console, CLI, and MCP are clients of it. None may have a capability the API lacks, and anything the API can do is reachable from all three.

**R-262 [D]** MCP is a real deliverable, so an agent can deploy directly. An agent holds a token and is therefore a principal subject to every token rule (§5.5): it acts as its owner, is bounded by their live grants, and its actions land in the audit log under their name.

**R-263 [D]** **End users** — people who were granted use of an app and nothing else — do not need the console. In the per-domain topology they bookmark a URL and carry a session.

**R-264 [D]** **The console is an Okta-style launcher.** Logging in shows tiles for every app you can reach. This is what proxy mode's root looks like, and it is available in the per-domain topology too.

**R-265 [D]** Users holding any administrative verb see an **Admin** entry point from the launcher, exposing the console scoped to whatever privileges they hold.

**R-266 [D]** Sharing an app sends no message **by default**. The app appears in the recipient's launcher tiles (R-264), and that is the notification. A console message would arrive beside the tile that already appeared — and would be invisible to a recipient who has never signed in, which a waiting tile is not. *Amended by issue #50, now that an adapter can reach someone who is not looking at Pando (R-232):* sharing with a person sends an `app_shared` notification that is off on every channel unless that person turns it on (R-373) — for someone who wants an email as well as the tile. Sharing with a group sends nothing; each member's tile appears all the same.

**R-340 [D] [V1]** An app may carry an **image**, shown on its launcher tile (R-264). Anyone who may change the app's spec may set or remove it (`app.spec.edit`); anyone who can open the app can see it. It is presentation, not configuration: it is not part of the spec (R-020) and a rollback does not change it. An app with no image shows a contour map generated from its ID, the same every time and different for every app. The server accepts raster images only — an SVG can carry script and would be served from Pando's origin — so the console converts an SVG to a PNG in the browser before uploading it.

**R-341 [D] [V1]** A person may mark apps they can open as **favorites**, and their launcher shows those first, above the rest. Favorites belong to the account, not the browser. They grant nothing, are visible only to the person who set them, and an app they can no longer open is not shown whether it is a favorite or not.

**R-342 [D] [V1]** A person may make **sections** in their launcher — named, collapsible groupings — and file apps they can open into them, one section per app. Anything not filed is under "Your apps", so a person who never makes one sees the launcher as before. Like favorites, sections belong to the account, grant nothing and are visible only to the person who made them; deleting one returns its apps to "Your apps".

---

## 20. Configuration and Policy

**R-270 [D]** **Pando ships permissive defaults.** Configuration narrows them.

**R-271 [D]** Configuration may be supplied by: a YAML file loaded at startup, environment variables, the CLI, or the console. Host policy fields may be fixed at startup in the file or the environment; adapters and AI function assignments (R-259) may be declared in the file. A declared item overrides what is stored and is read-only through the API, console, CLI and MCP while it is declared: a refusal to change it names the file and key, or the variable, that sets it, and `GET /config` reports each one. Removing the declaration and restarting brings back what was stored, and declared and console-managed adapters may be mixed. A declaration that contradicts itself in any category — two defaults in one category, two AI adapters of one provider, one AI function under two adapters, two services adapters providing the same kind of service — stops startup with an error naming both declarations. A declared credential is a reference to an environment variable or a file, never the value (R-190).

**R-272 [D]** **The general pattern, applied throughout:** a setting has a permissive default; host policy can raise the floor; app-level configuration can only move within what policy allows. This applies to isolation class, egress mode, exec, anonymous grants, routing override, resource limits, token expiry, and data destruction.

**R-273 [D] [LATER]** Premade setting profiles for common postures (hobbyist, hardened, regulated), usable as-is or as a starting point.

**R-274 [D]** Host policy may be applied to an install with running apps. **[O-10]** — behavior when newly-applied policy is violated by an existing app is unresolved: block deploys, force a change, or report.

### Updating Pando itself *(issue #53)*

**R-349 [D]** **Pando checks whether a newer release of itself is published.** It reads the GitHub release list of `trypando/pando` at startup and every six hours **[P]**, sending only its version, in the User-Agent. On by default; host policy `disable_update_check` turns it off, and off sends no request at all, for an install with no internet access or one that may not call out. Like any policy setting it may be fixed at startup (R-271).

**R-350 [D]** Host policy `update_channel` chooses which releases the check offers: `stable`, the default, or `prerelease`, which also offers release candidates (a tag with a suffix, docs/releasing.md).

**R-351 [D]** Every surface shows the version running, the latest on the channel, and each version in between with its CHANGELOG.md section. A version whose Security section names an advisory is marked, and so is one that may break what the version before it did: a MAJOR bump, or before 1.0 a MINOR one. Reading this needs `install.view`. The API serves it at `GET /updates`; the console, `pando updates` and MCP read that. A development build is told the latest release and never that it is behind.

**R-352 [D]** An available update says how to upgrade **this** installation, as a command and a sentence. For a server in a container: the release's Compose file and `docker compose up -d`, or the image to set where infrastructure-as-code deploys Pando, since otherwise the next apply puts the old version back. For the CLI: the command for how it was installed — the Homebrew cask, a Linux package, `go install`, or an archive. Where the in-place upgrade is on and possible (R-355), it is offered beside the command. *(Amended by issue #53's second half, which built the in-place upgrade.)*

**R-353 [D]** The CLI warns, once per run and never as a refusal, when it and the server it reached differ in MAJOR or MINOR version, with how to bring them into step. The server sends its version as `Pando-Version` on responses to signed-in callers only: which version an install runs says which advisories apply to it.

**R-354 [D]** **Pando does not start against a database a newer version migrated.** It refuses before migrating, naming both schema versions, and says to run the newer version or restore the backup taken before the upgrade. Migrations run forward only; going back to an older version is a restore.

**R-355 [D]** **Pando can upgrade itself in place, and only where the deployment allows it.** Host policy `upgrade_in_place`, off by default, turns it on; like any policy field it may be fixed in the deployment's configuration and is then read-only on the Policy screen (R-271). Off by default against R-270 on purpose: a version named in a Compose file or infrastructure-as-code puts the old image back on its next apply, so the person who owns that configuration decides. It is possible only for a server in a container whose image is a moving tag that covers the target version — `latest` for any release, a minor line such as `0.3` for its patches, a major such as `1` for its minor and patch releases. With an exact version, a digest, or a tag that does not cover the target, Pando refuses with the reason and the image to set instead (R-352). A release candidate has no moving tag and is installed by setting its version.

**R-356 [D]** Upgrading is the install verb `install.upgrade`, which the built-in Administrator holds. Starting an upgrade, its outcome, a rollback and a skipped backup are audit events, whether a person or the schedule (R-361) started it.

**R-357 [D]** **The image is verified before anything else happens.** Pando checks the keyless cosign signature of the target version's image digest against the release workflow's identity — the certificate must name `image.yml` in Pando's repository and be issued by GitHub's OIDC provider, as docs/releasing.md documents — and refuses to continue if it does not verify. What is run is that digest, never a tag resolved again later.

**R-358 [D]** A person's upgrade takes a full backup through the backup adapter first, with a passphrase they supply then (R-213), and stops if the backup fails. They may skip it only by saying so explicitly, and the audit event records that they did.

**R-359 [D]** **A failed upgrade puts the previous version back on its own.** A helper container, started from the running image, stops Pando, copies Pando's database, starts the new image with the same configuration and waits for it to report ready **[P: five minutes]**. If it does not, the helper restores the copy and starts the previous image. Down migrations are never used. The copy needs a database account that may create databases; Pando checks before starting and, where it may not — an external database outside the bundled Compose file — refuses the in-place upgrade, naming `CREATEDB` and saying to upgrade by changing the image instead. The copy is deleted after the new version has been healthy for 24 hours **[P]**; going back after that is a restore of a full backup. Apps keep running throughout, but every app is unreachable while Pando restarts, because the proxy is in Pando (R-023), and the confirmation says so.

**R-360 [D]** An upgrade across a version that may break something (R-351) needs its own confirmation, naming the version and showing the Upgrade notes of each such version in between.

**R-361 [D]** Host policy `auto_upgrade_patches`, off by default, upgrades to a new patch release of the running minor line without anyone present, inside `maintenance_window` — weekdays, a start time in UTC and a length in hours. Only stable patch releases qualify, and only where an in-place upgrade is possible (R-355). With nobody to supply a passphrase it takes no full backup; it relies on the database copy of R-359, and the Policy screen says so where it is turned on.

**R-362 [D]** When the update check first sees a newer release, Pando sends an `update_available` notification, once per version, to everyone holding `install.upgrade`, through the notification adapters (R-230) — the console, and email where it is configured (R-373). The console's Updates screen shows it either way. An install-wide subscription to `pando.upgraded` or `pando.upgrade_failed` sends the outcome to a channel or webhook (R-367).

**R-363 [D]** `pando self-update` replaces a CLI installed from a release archive or with `go install` by the latest release for its platform, after checking the release's signed `checksums.txt` against the release workflow's identity and the archive against it. A CLI installed by Homebrew or a Linux package is upgraded by that package manager, and `pando self-update` prints its command instead.

---

## 21. Data Destruction

**R-280 [D]** Losing access to an **app** destroys that user's per-app data (relevant to per-user instances, §22). Default behavior is destroy and reclaim space.

**R-281 [D]** Losing access to **Pando** means losing access to every app the user had.

**R-282 [D]** **Suspended is not deleted** (R-049). Suspension revokes access without triggering destruction.

**R-283 [D]** An option to back up before destroying exists, off by default. **Installs using an external IdP are expected to enable it** — offboarding is exactly when someone needs the data later.

**R-284 [D]** Because the correct value differs by install, this belongs to **host policy**, not per-app configuration. An admin sets "never destroy without backup" once and app owners cannot override downward.

---

## 22. Per-User Instances [LATER]

**R-290 [D]** An app may be configured so that **each user gets their own dedicated instance**, with Pando routing them to it. **Off by default.**

**R-291 [D]** This is the feature that makes "vibe code it and share it with your team" work for apps written single-user, which is most of them.

**R-292 [D]** **Instances are created lazily, on first access.** Not eagerly on grant — sharing with a 200-person department must not create 200 containers.

**R-293 [D] [LATER]** Idle reaping is a per-app option, not a global default. The right interval depends entirely on the app, and a chat UI and a long-running simulation want opposite answers.

**R-294 [D]** Data destruction on revoke follows §21.

**R-295 [P]** The cold-start path needs specification: a first request arrives with nothing running, and must either hold the connection or present a waiting page.

**R-296 [D]** This does not violate R-010. There is still no bin-packing, no autoscaling, and no rescheduling — but it is the first thing Pando does that creates a workload on demand, and the document notes it deliberately.

---

## 23. Security Scanning

**R-310 [D]** **Every app has a security score: a whole number from 0 to 100.** It is Pando's answer
to "is this app safe to run here", in one number a non-technical deployer can act on (R-005), with
the findings behind it available to anyone who can view the app.

**R-311 [D]** The score comes from **scanning what the app actually deploys** — the image that was
built and the source it was built from — not from a questionnaire and not from the repository's
reputation.

**R-312 [D]** **An app is scanned whenever what it runs changes** — on every deploy that changes the
source it is built from (a new commit, a new upload, a new image digest) or the scanner that looks
at it — and **on demand** from the app's settings at any time. A deploy of a source that already has
a successful scan uses that scan and says so; a score depends on what the app is, not on how often
somebody pressed deploy. A score describes a specific spec revision and the source it was built
from. *(Amended by issue #84: "which means on every deploy" assumed every deploy changes what runs,
and a redeploy of an unchanged commit does not.)*

**R-313 [P]** **The score is derived from findings by severity**, starting at 100 and deducting per
finding: critical 25, high 10, medium 3, low 1, floored at 0. The weights are a proposal — the
property that matters is that one critical finding cannot hide behind fifty low ones, and that the
number is stable enough to set a threshold against.

**R-313a [P]** **Host policy may say that a finding with no fix available does not count.** Off by
default, so the score answers "what is wrong with this app" rather than "what could its owner do
about it today" — and an upgrade does not silently move every score. Where it is on, the same filter
drives the number and the list: what is counted is what is shown, because a score that ignored a
finding the list displayed would leave somebody working out why fixing one changed nothing.

**R-313b [D]** **Findings are shown worst first**, and the same scan orders the same way twice. A
list in the scanner's output order changes under the reader for no reason.

**R-314 [D]** **Host policy may set a minimum score, 0 to 100.** Below it, **a deploy is refused at
plan time** with a `PLAN_*` error naming the score, the threshold and the findings that cost the
most — the same contract as every other plan-time refusal (R-024, R-132): fail before anything is
created, and say what to do.

**R-315 [D]** **An app that is already running when it falls below the threshold is not stopped on
the spot.** It is marked insecure, warned about in the console wherever it appears, and its owner is
notified. A running app is somebody's working service, and a policy change or a newly published CVE
is not a reason to take it away without warning.

**R-316 [D]** **Host policy may say that insecure apps are stopped**, with a **grace period** stated
in the policy. The grace starts when the app is first found below the threshold, and the owner is
told at that moment what will happen and when. Stopping is `desired_state = stopped`, which is
reversible and survives a restart — never a delete, and never a change to the app's configuration.

**R-317 [D]** **Scanning is an adapter category** (§18). Pando does not implement a scanner; it
translates one's findings into the score and the policy decision. An installation with no scanner
adapter configured has no scores, and a threshold set on it is inert and says so — a policy that
silently blocks every deploy because a component is missing is worse than one that is visibly off.

**R-318 [P]** **A scanner that fails does not block a deploy.** The previous score stands, the
failure is recorded and shown, and the app is not treated as insecure because Pando could not look.
An app that has *never* been scanned while a threshold is set is refused, with the remedy naming the
scan — the difference is between "we know nothing" and "we know it was fine and cannot check today".

**R-319 [D]** **Scans, score changes, policy-driven warnings and policy-driven stops are audited**
(R-227), with the score, the threshold and the scanner recorded. "Why did my app stop" must be
answerable from the audit log alone.

**R-320 [P]** The score is **not** shown as a grade, a badge, or a color alone. It is a number and a
sentence about what is behind it, in the console's own status vocabulary — a red pill saying "F"
tells a deployer nothing they can act on.

---

## 24. Open Decisions

| ID | Question | Notes |
|---|---|---|
| **O-1** | ~~Identity linking across adapters~~ | **Resolved.** Linking *aliases* and never merges — `users.id` is never retired, because merging would orphan app data keyed on the losing ID (R-054). An administrator links an identity to an account; moving one that already reaches another account leaves that account as a suspended alias. Email-based linking is off by default, per provider, and only on an email the provider vouches for. Design 02 §2.1. |
| **O-2** | ~~Per-adapter session lifetime and revocation~~ | **Resolved.** Each adapter declares it (R-047): local revokes immediately; OIDC and SAML last their configured session length (12h default) unless SCIM is on, when revocation is pushed. Design 03 §5. |
| **O-3** | ~~Private repo credential ownership~~ | **Resolved.** App-owned, with the supplying principal recorded in audit; deletion of that user flags affected apps for rotation rather than breaking their deploys. Design 01 §2.1. |
| **O-4** | Required vs optional slot detection | The forty-key `.env.example` problem (R-133) |
| **O-5** | TLS issuance | Per-adapter; ACME, wildcards, local self-signed |
| **O-6** | Backup destination | Local-only is useless for disk failure. Which destinations ship is open; **that a destination is not an adapter category is settled** (design 03 §8.1). |
| **O-7** | ~~Exec command recording~~ | **Resolved.** The command is recorded at session open; the PTY stream is not. A captured stream is a durable store of every secret typed into it, and cannot be redacted. Design 03 §2.3. |
| **O-8** | ~~Runtime adapter swap under a running app~~ | **Resolved.** Neither: a destructive spec change with the existing keep-or-discard volume flow. R-257, design 01 §4. |
| **O-9** | ~~Share notifications~~ | **Resolved.** No message; the launcher tile is the notification. R-266, design 08 §1.1. |
| **O-10** | ~~Retroactive policy application~~ | **Resolved.** Running apps are untouched; the next deploy fails at plan time with `POLICY_*`. Report now, block on next deploy. Design 05 §3. |
| **O-19** | What "bad code practice" covers | The first scanner reports vulnerable dependencies, leaked secrets and misconfiguration. Static analysis of the app's own code — a different class of tool, per-language, and noisy — is not in the score yet. R-311, design 09 §2. |
| **O-20** | Whether a score ages | A scan from three weeks ago describes three-week-old vulnerability data, and nothing rescans an app that has not been deployed since. A scheduled rescan is the obvious answer and needs a decision about what it costs on a small host. It also decides how old a scan a deploy may reuse (R-312, issue #84): today there is no limit, and whether a scanner database update invalidates reuse is part of the same question. Design 09 §4.1, §5. |
| **O-27** | What happens to audit archives once written | Retention is decided (R-347, R-348). Still open: whether archives kept by Pando are themselves aged out or counted against a disk budget, whether the DR bundle includes them, whether archived events stay searchable in the console rather than only downloadable, and whether events are hash-chained so an exported archive is provably complete. Design 02 §2.6. |

---

## 25. v1 Scope

Confirmed for the first release:

- Core: state, authorization, audit, identity assertion path, reconciler
- **Identity:** local users (username/password); OpenID Connect and SAML 2.0 providers; SCIM 2.0 provisioning
- **Routing:** loopback, Traefik and Cloudflare Tunnel
- **Secrets:** local encrypted storage
- **Source:** public GitHub repos
- **Builder:** rootless BuildKit in a container
- **Runtime:** local (Docker)
- **Notification:** console by default; email (SMTP), Slack, Microsoft Teams, Discord and ntfy when configured; event subscriptions and signed webhooks (issue #50)
- All four surfaces: API, CLI, MCP, console
- Detection pipeline, trial run, compose import, slots
- Recreate deploy, reconcile loop, health monitoring
- Volumes with the undeclared-persistence warning
- Rolling backups + full-host DR bundle

Explicitly deferred: per-user instances, GitHub OAuth sign-in, private repos, cloud routing adapters other than Cloudflare Tunnel, external secrets adapters, VM runtime adapters, setting profiles, per-user quotas, log masking.

---

## 26. Licensing and Governance

**R-300 [D]** **AGPL, dual-licensed with commercial exceptions available.** All functionality is available to everyone under the AGPL; nothing is paywalled. Companies that cannot accept AGPL terms purchase an exception. What is sold is a license, never a feature.

**R-301 [D]** A **CLA is required from the first outside contribution**, implemented with CLA Assistant as a GitHub Action. A DCO is insufficient — it certifies provenance but grants no relicensing rights, so it cannot support dual licensing.

**R-302 [D]** Rationale for starting here: AGPL → MIT is reversible; MIT → AGPL is not. Code released permissively stays permissive forever and can be forked from that commit.

**R-303 [D]** Accepted cost: some enterprises decline AGPL on blanket policy rather than analysis, which cuts against R-003. Retrofitting a CLA later means chasing every prior contributor, which is why it must be in place from the start.

**R-304 [D] [LATER]** MIT may be reconsidered if adoption proves more valuable than the revenue path.
