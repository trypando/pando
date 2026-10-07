# Open decisions

Twenty-eight questions. O-1 through O-10 come from requirements §23; O-11 through O-14 were added during
design; O-15 through O-17 were found while implementing phases 6, 7 and 8; O-18 was found while
setting up the release build; O-19 was found by turning `gosec` on; O-20 was found while building the
first AI adapter; O-21 and O-22 came from issue #74, AI functions beyond detection; O-23 came from
building the Cloudflare Tunnel adapter; O-24 came from issue #87; O-25 from issue #79, egress rules;
O-26 from issue #39, deploy approval; O-28 through O-31 from issue #41, deploying prebuilt images
and uploaded files; O-32 and O-33 from issue #72, running Pando as several replicas; O-34 through O-47 from the design notes for PRs 5–7 of the same issue (an image registry, a Kubernetes runtime adapter, Docker on several hosts). **Twenty-five are resolved; the rest are listed below.** O-32 (a deploy and detection work queue) and O-33 (a Kubernetes runtime adapter) were both answered yes, alongside a multi-host Docker adapter and a registry Pando runs by default; `docs/design/notes-multiple-replicas-issue-72.md` has the decisions and the PRs that carry them out. O-4 needs a
measurement, O-18 needs somebody to pick a host and pay for it, O-23 is kept open deliberately so
it is revisited, and O-24 needs a product call on stopped apps.

O-5 was the other long-standing one and is now resolved: "per-adapter" answered it until R-174 made
Pando run the edge and write its configuration, at which point Pando became the thing choosing.

**These are not TODOs to resolve at your discretion.** An agent hitting an open one should raise it,
state which options the docs already identify, and stop — not pick quietly and move on. Record any
resolution both here and in the requirements or design doc that owns it.

## Still open

| ID | Question | Why it stays open | Needed by |
|---|---|---|---|
| **O-4** | Required vs optional slot detection — the forty-key `.env.example` problem | Has a `[P]` answer that needs measuring, not deciding | Phase 6 |
| **O-18** | Where a signed apt repository is hosted, so `apt install pando` works without downloading a file first | Costs money or custody of a signing key; neither is an engineering call | Not blocking — the `.deb` is already published |
| **O-23** | Whether the Cloudflare adapter configures Cloudflare Access in front of an app | A product decision about a second gate Pando does not control; the adapter ships without it | Not blocking — design 03 §4.5 |
| **O-24** | Whether a stopped app keeps a daily backup, and whether an app that has gone unbacked-up is notified | R-211 says "daily, 7 retained" and does not say what happens while an app is stopped; the sweep backs up only running and degraded apps | Not blocking — issue #87 |
| **O-27** | What happens to audit archives once written (issue #60) | Retention itself is decided (R-347, R-348); what an archive is owed after that is a product call about custody, disk and evidence, and none of it blocks retention | Not blocking — design 02 §2.6 |
| **O-28** | Whether an ECR pull may use Pando's own AWS identity (an instance role) rather than access keys an app supplies (issue #41) | Lends every app whatever Pando's role can read — the same trade as O-30's resolution, with a cloud IAM boundary behind it. `[P]` explicit keys only | Not blocking — design 01 §2.1 |
| **O-29** | Whether a non-empty source allowlist admits uploaded files (issue #41) | R-092 says the allowlist restricts "deployable sources" and does not mention uploads, which have no host for an entry to name. `[P]` refused unless the list contains `upload` | Not blocking — design 01 §2.1 |
| **O-31** | Whether an upload may be a `docker save` tarball as well as source files (issue #41) | The runtime can already import an image from a stream (`SupportsImageImport`); accepting one is a third kind of upload with its own scanning and pinning story, not an extension of this one | Not blocking — design 04 §4 |
| **O-34** | Whether the single-VM install runs an image registry too (issue #72, PR 5) | `[P]` no: single-host Docker keeps `ImportImage` and pins built images by image ID | Issue #72 PR 5 — `notes-image-registry-issue-72.md` |
| **O-35** | How a Pando-run registry gets a certificate every node trusts | `[P]` operator-supplied certificate; plain HTTP accepted by explicit setting | Issue #72 PR 5 — `notes-image-registry-issue-72.md` |
| **O-36** | Whether one full-access registry credential is acceptable, or Pando issues pull-only tokens per app | `[P]` one `htpasswd` credential now; pull by digest limits what a leak can change | Issue #72 PR 5 — `notes-image-registry-issue-72.md` |
| **O-37** | Whether the DR bundle includes the registry's images (R-212) | `[P]` never; rebuild after restore. Uploads, which are not in the bundle today, go in | Issue #72 PR 5 — `notes-image-registry-issue-72.md` |
| **O-38** | Whether a weekly read-only window for registry blob GC is acceptable | `[P]` yes, Distribution with a scheduled GC; Zot if not | Issue #72 PR 5 — `notes-image-registry-issue-72.md` |
| **O-39** | What replaces the shared `/var/lib/pando` in a cluster | `[P]` a ReadWriteMany volume first, then uploads and build cache moved out | Issue #72 PR 6 — `notes-kubernetes-runtime-issue-72.md` |
| **O-40** | Namespace per app on Kubernetes, given 20,000 apps exceeds the tested namespace count | `[P]` namespace per app, proven with the load harness | Issue #72 PR 6 — `notes-kubernetes-runtime-issue-72.md` |
| **O-41** | How a pod pulls an app's private image | `[P]` an `imagePullSecret` per app namespace, rewritten at every apply | Issue #72 PR 6 — `notes-kubernetes-runtime-issue-72.md` |
| **O-42** | How R-174's edge works on Kubernetes | `[P]` edges without a shared mount (cloudflared); Traefik unmanaged; a Gateway API routing adapter later | Issue #72 PR 6 — `notes-kubernetes-runtime-issue-72.md` |
| **O-43** | Whether the Kubernetes adapter requires a NetworkPolicy-enforcing CNI and a dedicated cluster | `[P]` require the CNI (verified by a canary), recommend dedication | Issue #72 PR 6 — `notes-kubernetes-runtime-issue-72.md` |
| **O-44** | Whether a workload recreated on another node after node failure is consistent with R-010 | `[P]` yes, with R-010 amended to say Pando itself does not reschedule. A requirement change | Issue #72 PR 6 — `notes-kubernetes-runtime-issue-72.md` |
| **O-45** | How the proxy reaches an app on another Docker host (R-023) | `[P]` a forwarding agent per host, mTLS with a certificate only Pando holds; changes design 06 §4's mechanism | Issue #72 PR 7 — `notes-multi-host-docker-issue-72.md` |
| **O-46** | Whether moving an app between Docker hosts is an action Pando offers | `[P]` not in the first release; later an administrator's action, needing a new install verb | Issue #72 PR 7 — `notes-multi-host-docker-issue-72.md` |
| **O-47** | Whether Pando's replicas may run on more than one Docker host | `[P]` control host only until O-39 removes the shared volume | Issue #72 PR 7 — `notes-multi-host-docker-issue-72.md` |

**O-4** has a `[P]` fallback that preserves R-103: default `Required: false` for anything the file
gives a sample value for, and let the trial run settle it — a slot whose absence crashes the trial run
is promoted to required with the crash log as evidence. This turns an unanswerable question into an
observation. It is open because it needs a false-block rate measured against the detection corpus, not
because nobody has decided.

The question is narrower than "forty keys" now, and the reason is R-130 rather than a decision about
O-4. A variable is a hole *with a type*, and only the typed ones are dependencies: `REDIS_URL` names a
Redis, `VAPID_PRIVATE_KEY` names a value with nothing to connect it to. Every key used to become a
slot, so a `.env.example` with eleven variables produced eleven "dependencies" of type unknown, each
offering to connect to something that already exists. Untyped keys are variables now, declared empty
on the app, and O-4 applies to what is left: of the keys that really are dependencies, which are
required.

**O-27** is what issue #60 left open once retention shipped. Two of its questions were answered
with a `[P]` default so retention could land, and can be overridden: **three months is a floor as well
as a default** (R-348) — held by the database function that removes a month, which is what makes "the
running server cannot erase recent history" true rather than a policy setting — and **archived events
are downloadable, not searchable**: the console lists archived months on the Audit log screen with a
download for each. Four remain:

- **Do archives kept by Pando age out?** Today they are kept indefinitely. They are compressed, about
  a tenth of the rows' size, but unbounded, and R-224 is about exactly that. The options are a second
  retention period for archives, counting them against a disk budget, or leaving long-term custody to
  `export`. Deleting an archive deletes audit history, so it should not be a default anybody slides
  into.
- **Does the DR bundle include them?** The bundle has the database, which records every archive, but
  not the archive files. A restored install lists archives it cannot serve until the directory is
  copied across.
- **Are archived months searchable?** That means loading an archive back into something queryable,
  which is either a second store or a temporary table, and a reason to keep the archives small.
- **Should events be hash-chained?** An archive's digest proves it was not changed after it was
  written. It does not prove the month was complete when it was written; a chain over the live log
  would, and would matter most once archives leave the host.

**O-18** exists because a `.deb` attached to a release and an apt repository are different products.
The release build publishes `.deb`, `.rpm` and `.apk` packages, which install with
`sudo apt install ./pando_<version>_linux_amd64.deb` and never upgrade themselves. `apt install pando`
and `apt upgrade` need a repository that apt trusts, and the options differ in who holds the signing
key:

- **A hosted repository** — Cloudsmith has an open-source tier and Gemfury hosts public packages for
  free. Both sign the repository and give users a key to install. GoReleaser publishes to either. The
  cost is a dependency on a vendor for the install path, and an account somebody has to own.
- **Self-hosted on GitHub Pages**, generated with `aptly` or `apt-ftparchive` and signed in CI. Free,
  and the key is ours — which is also the problem, because the key then has to live somewhere, be
  rotated, and survive the person who made it.
- **Neither**, and the `.deb` on the release page stays the answer. Debian and Ubuntu users download
  a file and upgrade by downloading another one.

The choice matters more than it looks: an unsigned repository, or one added with `[trusted=yes]`,
tells every user of a product that argues for provenance to skip checking ours.

**O-20 — where an adapter's credential lives. Resolved:** encrypted by the install's secrets adapter,
in its own table. The Anthropic adapter (design 10 §6) was the first adapter to hold a credential, and
`adapter_configs.config` is plain JSON, stored unencrypted and exported readably into every DR bundle.
Keeping a key there is inconsistent with R-190, so it is not allowed.

- `POST /adapters` takes a separate, write-only `credentials` object. Each value is sealed by the
  install's secrets adapter and stored in `adapter_credentials` as ciphertext, bound to the adapter's
  ID as authenticated data, exactly as an app secret is stored in `secrets`. There is no plaintext
  column.
- The database refuses a `credentials` key in `adapter_configs.config` (a check constraint), and the
  handler refuses the field names credentials usually go by, with a message saying where to send them.
- At startup core configures the secrets adapter first, decrypts each adapter's credentials, and hands
  them to `Configure` in memory under `credentials`. An adapter that finds a key at the top level of
  its stored configuration refuses to start.
- `GET /adapters` lists which credentials are set by name. Nothing returns a value, and the audit
  event names the fields that changed, never their values.
- A secrets adapter cannot be given credentials, since it is what would encrypt them.

`api_key_env` remains for an operator who keeps credentials in the environment; it stores a variable
name. The two alternatives considered were encrypting all of `adapter_configs.config`, which ties every
adapter's non-secret settings to the secrets key, and environment variables only, which the console
cannot set (R-002).

**O-19 — the session cookie's `Secure` attribute. Resolved:** option 3, an explicit
`PANDO_SERVER_EXTERNAL_URL`. The operator states the scheme browsers reach the installation on, and
Pando believes the operator rather than the request. Unset falls back to `r.TLS != nil`, which is
correct for the two topologies where the request tells the truth — Pando terminating its own TLS, and
the plain-HTTP localhost install the README documents. Implemented in
`internal/httpapi.Server.secureCookie`, with the decision table in
`cookie_secure_internal_test.go`; the value is parsed and rejected at startup rather than at the
first sign-in.

Option 2, a trusted-proxy list, was the other real candidate and is a fine answer; it loses on
having two things to get wrong instead of one, and on a wrong trusted-proxy list failing quietly.
Option 1 was never viable and option 4 breaks the README as written. The rest of this section is the
original write-up, kept because the reasoning is why the answer is what it is.

**It was a live weakness, not a tidiness question.** `handleLogin` sets the session cookie with
`Secure: r.TLS != nil` (`internal/httpapi/session_handlers.go`). That is right for the documented
default install — plain HTTP on `localhost`, where an unconditional `Secure` would stop sign-in
working, and where `localtest.me` is not a browser-trustworthy origin either. It is **wrong** for the
other documented topology. `SECURITY.md` says Pando does not terminate TLS and expects a reverse
proxy in front of it; behind one, `r.TLS` is nil on every request, so the session cookie is sent with
no `Secure` attribute even though the browser's connection is encrypted. A single plaintext request
to the Pando hostname — a typed URL, a stale bookmark, an `http://` link — then puts the cookie on
the wire in the clear.

What has to be decided is not *whether* to fix it but **which signal Pando trusts, and when**:

1. **Trust `X-Forwarded-Proto` unconditionally.** One line, and wrong in the same way forged headers
   are always wrong: any client that can reach Pando directly can also set it, and R-053 exists
   precisely because Pando does not treat an inbound header as a fact. Rejected on the same grounds
   Pando strips inbound `X-Pando-*`.
2. **Trust `X-Forwarded-Proto` only from a configured set of trusted proxy addresses.** Correct, and
   the standard answer. Costs a new configuration value, and an install that sets it wrong gets a
   subtle failure rather than a loud one.
3. **An explicit `PANDO_EXTERNAL_URL` (or `PANDO_SECURE_COOKIES`).** The operator states the scheme
   the installation is reached on, and Pando believes the operator rather than the request. No
   trusted-proxy list, one value to get wrong, and it doubles as the base for absolute URLs in
   notifications and redirects — which the system needs anyway. The `[P]` favorite.
4. **Always `Secure`, with an explicit opt-out for local development.** Safe by default, but it
   breaks the install instructions in the README as written, which is the one thing this project
   should not do quietly.

Whichever is chosen, the cookie set in `handleLogout` has to follow it: a clearing cookie whose
attributes differ from the original may not clear it at all.

`gosec`'s G124 finding on both call sites is still suppressed, because `Secure` is computed rather
than a literal `true` — but it now names the function that decides it instead of an open question.

**O-15** was found by asking whether a detected app could actually deploy. It could not: the spec had
no routing, and filling that in surfaced the question nobody had answered. **It is now resolved** —
see design 03 §4.2; the rest of this section is why.

R-166 prefers subdomain and falls back to path. The `loopback` adapter that ships as the laptop
default supports **neither** — it is port mode only (design 03 §4.1). So on a default install every
app needs a host port, and no requirement says where one comes from. R-005 rules out asking the user:
someone who may not know what a port is cannot pick a free one.

`[P]` implemented: the lowest free port in a configured range, default `9000-9999`, held in a
`port_allocations` row keyed `(adapter_ref, port)`. Exhausting the range returns
`CAPACITY_NO_FREE_PORT` naming the setting to widen.

**Half of this question is now answered, by being got wrong.** The first version derived the port —
"the lowest number no pinned spec is using" — and that is not an allocation, it is a guess about one.
It raced immediately, and in the ordinary case rather than an exotic one: adding five apps at once
runs five background detections, two computed the same answer before either had written anything
down, and both were handed port 9001 with nothing anywhere noticing. So a port **is** a durable
allocation with its own table, and the unique constraint is what makes a collision impossible rather
than unlikely.

Lowest-free rather than random so an app tends to keep its port across a rebuild and a bookmark keeps
working; reused rather than ever-increasing so a deleted app's port comes back.

**The other half is answered by phase 10 shipping.** What remained was whether lowest-free is the rule
and whether `9000-9999` is the right range, to be revisited once there was a second routing adapter to
compare against. There is: Traefik does subdomain and path, so port mode is the laptop default's path
rather than the only one, and an install that outgrows a thousand ports has a better answer available
than a wider range. The `[P]` stands as the `[D]`.

**O-16** was found implementing phase 7's garbage collection. R-222 bounds log retention by size,
R-223 sets 100 MB per app, and R-224 says the aggregate must respect total host disk. The GC job was
meant to enforce all three. It cannot, because **Pando does not hold app logs** — it streams them from
the runtime through `RuntimeAdapter.Logs`, and the bytes live wherever that runtime put them.

There is no `TrimLogs` on the adapter interface, and adding one is not obviously right either. On
Docker the honest mechanism is the log driver's own `max-size` / `max-file`, set when the container is
created — which makes the per-app cap (R-223) easy and the aggregate (R-224) hard, because scaling
every app's cap down proportionally when the total exceeds the disk budget would mean **recreating
every container**. The reconciler may not do that: it is destruction of something a person may have
wanted, on a schedule, triggered by an unrelated app being chatty.

Options, none free:

1. **Per-app cap at creation, aggregate as a warning only.** Honest and cheap; R-224 becomes a
   notification rather than a guarantee, which is a real weakening of a `[D]` requirement.
2. **A `LogRetention` capability on the runtime adapter**, applied without recreating where the
   runtime allows it. Docker does not allow it for an existing container; another runtime might.
3. **Pando collects logs itself** into storage it controls, which makes both requirements trivially
   enforceable and adds a durable, secret-bearing store R-225 currently reasons about not needing.

Recorded rather than decided. Spec revision pruning (R-152) is implemented — it is the part of GC
that operates on data Pando actually owns.

**O-5 is resolved** — see R-169 and design 03 §4.3. Deferring to each adapter was always most of the
answer (R-047's shape): an adapter that issues certificates declares how, one that cannot says so
through `RoutingCapabilities`. What that left unanswered arrived with R-174, which makes Pando run the
edge and therefore write its static configuration — so Pando is the thing choosing a challenge type,
and "per-adapter" stopped being an answer for the adapter Pando ships.

**Both, chosen in the edge settings.** HTTP-01 per hostname needs only a reachable `:80` and an email
address, and is the one an install can turn on without understanding its own DNS. DNS-01 needs a
provider credential and yields the wildcard R-166 prefers, covering an app's hostname before the app
is deployed. Picking one for everybody would be wrong in opposite directions: HTTP-01 alone leaves
R-166's preferred topology permanently unavailable, DNS-01 alone makes TLS conditional on credentials
many installs do not have.

Neither is a silent default. An install that configures neither gets `:80` and is told that is what it
has — a certificate that quietly failed to issue is worse than one nobody promised, because the
failure surfaces as a browser warning to a user rather than as a message to an operator.

## Resolved

| ID | Question | Resolution | Where |
|---|---|---|---|
| **O-1** | Identity linking across adapters | Linking **aliases and never merges** — `users.id` is never retired. Shipped with issue #51: admin linking, a moved identity leaves a suspended alias, email linking opt-in per provider on verified email only | design 02 §2.1 |
| **O-2** | Per-adapter session lifetime and revocation | Deferring to each adapter's `SessionPolicy` *is* the answer (R-047) | design 03 §5 |
| **O-3** | Private repo credential ownership | App-owned, attributed to the supplier in audit; offboarding flags rather than breaks | design 01 §2.1 |
| **O-7** | Exec command recording | Command recorded at open; PTY stream not captured | design 03 §2.3 |
| **O-8** | Runtime adapter swap under a running app | Neither migration nor plain redeploy — a `destructive` spec change | design 01 §4, R-257 |
| **O-9** | Share notifications | No message; the launcher tile is the notification | design 08 §1.1, R-266 |
| **O-10** | Retroactive policy application | Running apps untouched; next deploy fails at plan time | design 05 §3 |
| **O-11** | How Postgres is supplied | The install topology supplies it — Compose, with an external-database override | design 00 §1.1 |
| **O-12** | MCP exclusion list hard or policy-controlled | Policy-controlled, default-closed, expressed as host policy — not a second mechanism | design 04 §3 |
| **O-5** | TLS issuance — ACME, wildcards, self-signed local | Per-adapter, and for Pando's own edge both challenge types offered and chosen per install | R-169, design 03 §4.3 |
| **O-13** | Session revocation mid-websocket | Re-authorize on the assertion lifetime; close on failure | design 06 §4.2 |
| **O-15** | How a host port is chosen in port-mode routing | Lowest free port in a configured range, held as a durable allocation; revisit closed by Traefik shipping | design 03 §4.2 |
| **O-14** | DR restore bootstrap ordering | Largely dissolved by O-11; confirm sequencing in phase 9 | design 07 D |
| **O-6** | Which backup destinations ship | Backup is an adapter category; destinations are adapters, and `local` ships in v1 | R-217, R-252, design 03 §8.1 |
| **O-16** | How log retention is enforced | Per-app cap applied at workload creation; the aggregate enforced at plan time against the **sum of committed caps**, not measured usage | R-222–R-224, design 03 §2 |
| **O-17** | What an "administrative verb" is (R-265) | Install-scoped verbs, held as a grant with no app; a fourth built-in role | design 06 §2.1, R-080/R-081 |
| **O-22** | Whether successful use of an app is audited, so audit search can answer "who accessed this app" | Yes: `app.use`, once per visit, anonymous visitors included by default and turned off by host policy (`disable_anonymous_use_audit`) | R-227, design 06 §4, §6 |
| **O-21** | How AI functions are assigned, named and gated, and how the config file declares them (issue #74) | The config file wins over a stored assignment or adapter, and the stored one is shown as overridden; declared and console-managed adapters mix; plan chat stays `revise_plan`; access drafting is one function; each function is gated by the verb its ordinary endpoint needs | R-259, R-271, R-343 – R-346, design 10 §7.1, §9, §10 |
| **O-25** | Egress rules: install allow/denylist with app-level overrides (issue #79) | The install picks allow-all, a denylist or an allowlist, plus a separate private-range switch. An app adds or removes entries and may keep its own list **on top** — it never replaces. Tightening is always allowed (`app.egress.tighten`); loosening is gated by `egress_loosening`: forbidden, a verb (`app.egress.loosen`), or deploy approval | R-181 – R-189, design 01 §2.7, 03 §2.1, 06 §5 |
| **O-30** | Whether a registry credential belongs to the app or to the install (issue #41) | To the app, as O-3 decided for source credentials, and kept apart from the app's secrets so no env entry can hand it to the app. An install may also opt in (`apps.docker_credentials`, off by default) to pulling with the Docker login on the Pando server for apps with no credential of their own; the app's credential always wins | design 01 §2.1, 02 §2.4 |
| **O-26** | Require approval before deployments (issue #39) | Off by default. Required by host policy for every app or named apps, by the app's own spec, or by a new egress loosening. Two verbs: `install.deploys.approve` (Administrator) and `app.deploy.approve` (no built-in role); self-approval allowed. Count and expiry configurable (1, seven days). Rollback to a revision that ran, restarts and rotations are free. Auto-deploy and approval do not combine | R-154 – R-159, design 02 §2.3, 05 §3.3, 06 §5, 07 B |

### O-25 — egress: layered, and only loosening is gated

Issue #79 asked to replace R-182's rule that an app's allowlist **replaces** the install's, which made
the install's list a default rather than a boundary. Its questions, and the answers:

- **Can an app remove entries from an install denylist, or add to an install allowlist?** Yes, and
  those are two of the three **loosenings** (`denylist_remove`, `allowlist_add`). Whether they are
  allowed is host policy's `egress_loosening`: `forbidden` (refused at plan time,
  `PLAN_EGRESS_LOOSENING_FORBIDDEN`), `verb` (whoever holds `app.egress.loosen`; the default, R-270),
  or `approval` (the deploy waits for an approver, O-26). Tightening — adding to a denylist, removing
  from an allowlist — is always allowed (R-272).
- **Can an app under an install denylist switch itself to an allowlist?** Yes, ungated beyond
  `app.egress.tighten`. It does not switch: its own list is a second layer, and a destination must pass
  both. That is also how an app on an open install locks itself down.
- **How does blocking private ranges combine with a list?** It is a separate switch beside any mode,
  allow-all included, checked against resolved addresses. An app may turn it on; turning it off when
  the install has it on is the third loosening (`block_private_off`).
- **Should build egress follow the same model?** No. It stays separate (R-118, R-189): a build runs
  before anybody has reviewed its output, so what it may reach is a different question.
- **Entry format?** Hostname, `*.` wildcard of subdomains, IP address or CIDR, each optionally with a
  port, and `*` (R-185). Hostname entries match the name asked for; address entries match where it
  resolved. The Docker runtime enforces all of them through its gateway; a runtime that cannot enforce
  a restriction says so and the plan is refused (R-186).
- **Should `app.egress.override` split?** Yes: `app.egress.tighten` (Owner and Operator) and
  `app.egress.loosen` (Owner; the old verb, renamed by migration in every role and policy list).
- **How are the effective rules shown?** Merged, in the plan and the console: the effective mode and
  list with where each entry came from, the app's own list, private-range blocking and its origin, and
  every loosening with what it needs (R-188). The plan also says, wherever a restriction is in effect,
  that only HTTP and HTTPS through the gateway leave the app (R-187).

The issue's "install-level switch for whether overrides are allowed at all" became the loosening gate
rather than an override switch, because an override that only tightens never needed one. Rules take
effect at deploy, are recorded on the deployment, and a policy edit does not change a running app (O-10).

### O-26 — deploy approval

Issue #39 asked for a request-then-approve flow for change control. Its questions, and the answers:

1. **Scope?** All of them: host policy for every app (`deploy_approval_required`), host policy for
   named apps (`deploy_approval_apps`, which the app's owner cannot turn off), and the app's own spec
   (`deploy.require_approval`, read from the running spec and the next, so turning it off is approved).
   A new egress loosening under `egress_loosening: approval` is a fourth reason (R-154).
2. **Auto-deploy?** Refuses to combine. A spec that turns auto-deploy on is refused while approval is
   required; an app that already auto-deploys is skipped when policy starts requiring it, and the
   console says so (R-158). A request per push is a backlog nobody reads.
3. **Rollbacks, restarts, secret rotations?** None needs approval. A rollback to a revision that ran
   was approved, or did not need it, when it first ran; restarts and rotations change no spec (R-157).
4. **Can a requester approve their own deploy?** Yes. The issue proposed no by default; the decision
   was to keep the rule simple and put the control in who holds the verb. `install.deploys.approve`
   (Administrator) approves anything; `app.deploy.approve` (no built-in role, granted through a custom
   role) approves one app's. An install that wants two people keeps the verb from the people who
   deploy (R-155). Owner does not get `app.deploy.approve`, and App manager does not hold
   `install.deploys.approve`, its install-wide counterpart. Agents' tokens are denied both by default (`agent_disabled_verbs`): approval is a human
   sign-off.
5. **Do requests expire?** Yes, after `deploy_approval_expiry_hours` (default 168, seven days; 0 means
   never). A newer request for the same app supersedes an older waiting one (R-156).
6. **One approval or a count?** A count, `deploy_approval_count`, default one. Any rejection ends the
   request.

The issue's other proposals stand as written: tied to one spec revision and commit (R-120); approvers
notified through the notification adapter; every step audited (R-159); and no path to `failed`
(R-151). One changed: the waiting state is not *before* planning. The plan runs when the deploy is
requested, so a deploy that cannot succeed is refused before anybody is asked to approve it, and runs
again at approval because policy may have changed meanwhile.

### O-21 — AI functions: assignment, naming and gating

Issue #74 left five questions open. What was decided:

- **Config and database disagree about the same function or adapter: the config file wins, and the
  stored row is shown as overridden.** This is the policy overlay's behavior (design 02 §2.5), for the
  same reason: the file is what the operator wrote most deliberately, and failing startup over a row
  somebody saved in the console would make the console able to stop the server. `GET /ai/functions`
  reports the stored assignment under `overridden`, `GET /adapters` lists an overridden adapter with
  status `overridden`, and removing the declaration and restarting brings the stored one back.
  Contradictions *within* the file still stop startup (R-271).
- **Declared and console-managed adapters may be mixed.** The policy overlay mixes field by field,
  and nothing here needs it to be all or nothing. An adapter declared in the file can be assigned a
  function from the console if the file does not assign that function.
- **Chat on a plan is `revise_plan`,** the function issue #69 introduced. There is no `chat_app`: no
  conversation about a deployed app exists yet, and a function name with nothing behind it would be
  listed and assignable and do nothing. It is added when the feature is.
- **Access drafting is one function, `draft_access`,** covering roles and groups. The request that
  produces a role usually produces the group that holds it, and splitting it would mean two
  assignments for one question.
- **Each function is gated by the verb its ordinary endpoint needs:** `draft_access` by
  `install.users.manage`, `draft_policy` by `install.policy.manage`, `search_audit` by
  `install.audit.read`, and `answer_reference` by any signed-in user, like the reference itself.
  Assigning functions is `install.adapters.manage`, and listing them `install.view`.

The sixth question, whether to audit successful app use, is O-22.

### O-22 — who used an app is recorded

**Resolved:** the proxy writes `app.use` when a use is allowed, once per visit rather than per
request. Issue #74's example question, "apps Ben Meeker added, deleted or accessed in the last month",
could not be answered: the proxy recorded refusals (`app.use.denied`) and sessions recorded sign-ins,
and nothing recorded which app was opened. After a leak or a misuse that is the first question, and
the proxy is the one place every use passes through.

- **A visit, not a request.** A browser carries a visit cookie in Pando's namespace (stripped before
  the app sees a request, R-173); a token is its own visit, remembered for twelve hours. A page load is
  dozens of requests, and a row for each would be a log nobody reads.
- **Anonymous visitors are recorded by default**, with the address Pando saw, because "who used this"
  includes people nobody knew by name. An install that does not want those rows turns them off with
  `disable_anonymous_use_audit`. A client that never keeps cookies is capped per app per minute, and
  what went unrecorded is counted onto the next record.
- **Size.** About 470 bytes per row with its indexes, measured on a running install: one visit a day by
  200 people across 10 apps is roughly 340 MB a year. The log is still unbounded, which issue #60
  addresses for every event; this makes it grow faster, not a new problem.

### O-16 — bound what is promised, not what accumulates

The three options were: cap per app and let the aggregate be a warning; put a capability on the
runtime adapter; or have Pando collect logs itself. The second, with a specific shape.

A runtime declares what it can do about logs (`LogRetentionCapability`). Docker answers: it can cap a
workload at creation, it cannot change that cap without recreating the container, and it cannot
report how much log space an app is using. All three answers matter.

**The aggregate is a plan-time bound on the sum of caps, not an observation of usage.** That is the
part worth arguing for. Bounding what is committed is the stronger guarantee — if every app's logs
are capped and the caps sum under the budget, the total cannot exceed it, and nothing has to be
watched. Measuring usage would mean acting *after* the disk was already filling, and the only remedy
at that point is recreating containers, which the reconciler may not do because an unrelated app
turned chatty: that is destruction on a schedule.

So R-224 does not become a notification, which is what option 1 would have cost. It becomes a refusal
at the point where a refusal is cheap and reversible: `CAPACITY_WOULD_OVERSUBSCRIBE` at plan time,
naming what would be committed and what is allowed.

Two things fell out of implementing it. Nothing carried `retention.log_bytes` into a container at
all, so every app's logs were unbounded regardless of what its spec said. And `Defaults.Apply` ran
only on the detection path — so a hand-written spec, which is the API's own documented way to
configure an app, got no retention defaults whatsoever. Every spec the acceptance suite writes is
hand-written, which is exactly why nothing noticed.

### O-6, and the `[D]` it reversed

**O-6 — resolved by making backup a category, which reversed a `[D]`.** The question was "which
destinations ship"; the answer changes the shape rather than the list. Destinations are adapters, so
which ones ship is the same kind of question as which runtimes ship, and it stops being an open
decision — `local` is v1, anything else is a pull request that touches no core code.

The reversal is the interesting part and it is recorded in full in design 03 §8.1, including what the
old argument got right. Short version: it described a byte sink correctly, then assumed the
destinations people want are byte sinks. An object store expires and versions objects on its own
schedule and a filesystem path does not, so R-211's retention has two possible owners and picking the
wrong one silently breaks either pruning or restoring. That is a capabilities question (R-254), and a
`Destination` interface would have grown a capabilities struct one provider later, consulted through
the type assertion R-254 exists to forbid.

### O-17 in full, because it was a live escalation

**O-17 — resolved: option 1, install-level verbs as grants with no app.**

It was not only a console gap. Six endpoints were gated by "are you signed in" and nothing else,
because there was no install-level authority to gate them with, and three of those mutated. It was
demonstrated end to end on the shipped stack: create an ordinary user, sign in as them, `PATCH` the
administrator to `suspended`, and the administrator's next login returns 401. Two calls, no grants
needed. `GET /apps` correctly returned nothing for that user the whole time — the per-app
authorization worked exactly as designed, which is what made the gap so easy to miss.

What shipped:

| Piece | Where |
|---|---|
| Six install verbs — `install.view`, `install.users.manage`, `install.policy.manage`, `install.adapters.manage`, `install.audit.read`, `app.create` | R-080, `internal/core/authz/verbs.go` |
| A fourth built-in role, **Administrator**, install-scoped, holding all six and no app verb | R-081, migration `000009` |
| `grants.app_id` nullable, with the scope correspondence enforced structurally | design 06 §2.1, migration `000009` |
| `CheckInstall`, a third check function beside `CheckControl` and `CheckData` | design 06 §2.1 |
| Bootstrap grants the first account the Administrator role | R-046 |
| The six endpoints gated; `/users/{id}` **self or verb** | design 04 §2.7, §2.8 |
| `GET /me` returns the caller's install verbs; the console reads them | design 04 §2.9, R-265 |

Two things about the resolution are worth keeping in mind:

**The `app_id NOT NULL` guarantee was replaced, not dropped.** That column was doing real work — it is
why an app-scoped grant could not accidentally become global. In its place: `roles.scope`,
`grants.role_scope`, a composite foreign key between them, and a CHECK tying `role_scope` to whether
`app_id` is null. So "no app" and "carries install verbs" cannot come apart, whatever the application
does. A third CHECK says a data grant always names an app, because R-070's binary use has no
install-wide form and that was previously implied by the NOT NULL.

**`PATCH /users/{id}` is self-or-verb, not verb-only.** Your own account is self-service; anyone
else's needs `install.users.manage`. Both halves are load-bearing, and the first is what keeps an
install with one administrator from being an install where nobody can manage their own account.

What is still not built is install-level **screens** — users, host policy, adapters, the audit log.
They have verbs and gated endpoints now; they have no UI. That is phase-9 work and it is recorded as
such in `phase-08-console.md` rather than as an open decision, because nothing is undecided about it.

Related and now unblocked: design 05 §3's promise that the console lists policy-violating apps before
a policy is saved, and R-085's install-wide exec disable, both of which needed this concept.

### The ones worth understanding before you touch that area

**O-1 — linking aliases, it never merges.** Merging two `users` rows is the obvious implementation and
it breaks R-054: `users.id` is the assertion `sub` claim, apps key their data on it, and Pando cannot
reach into an app to rewrite rows stored under the losing ID. A merge silently orphans a person's data
inside every app they ever used. Decided now because getting it wrong later is unrecoverable. When
it shipped (issue #51) the rule held: identities live in `user_identities`, and moving one that already
reaches another account leaves that account as a suspended alias (`users.alias_of`) rather than
deleting or merging it.

**GitHub OAuth (R-043) — a follow-up, not part of issue #51.** GitHub is OAuth 2 without OpenID
Connect: no ID token, and organization and team membership come from the REST API rather than claims.
It fits the same `IdentityAdapter` interface (organizations and teams map to synced groups) and needs
its own adapter.

**O-7 — the command, not the stream.** A captured PTY stream is a durable, searchable store of every
secret an operator ever typed, sitting in the one table deliberately readable by anyone with audit
access. It cannot be redacted, because `secret.Value` protects values Pando *handles* and a stream is
bytes Pando never parses. Recording the command answers what an investigation asks first; the
documentation must not imply exec is fully audited (R-086).

**O-12 — an MCP-layer block is a speed bump, not a boundary.** An agent holding a token can call the
REST API directly, so enforcement has to live where every surface passes through it. That is host
policy, which is already evaluated before grants for every principal.

**O-17 — an administrator is a principal with a grant.** Not a flag on a user, which was the cheap
option and the one that would have to be decided again the first time somebody asked for "manages
users but not policy". The cost is that `grants.app_id` is nullable, and the whole design of the
migration is about paying that cost in the schema rather than in `CheckControl`'s branching: a role
carries its scope, a grant carries the scope it was made at, and a composite foreign key makes the two
agree. Two check functions that each refuse the other's verbs keep the call sites honest — and the
asymmetry matters, because an app verb evaluated install-wide looks for a grant that *can* exist.

**O-23** was raised while building the Cloudflare Tunnel adapter, and is open on purpose so it is
revisited rather than forgotten. Cloudflare Access can require a sign-in at Cloudflare's edge before a
request reaches the tunnel. R-023 settles what it cannot be: the only check. What is open is whether
Pando should configure it at all. The options:

1. **Leave it to the operator.** Access is configured in Cloudflare's dashboard against hostnames
   Pando created. Nothing new in Pando; the operator maintains two lists of who may use what.
2. **Mirror grants into Access policies.** One source of truth, and a request refused at Cloudflare's
   edge never reaches the host. It makes Pando's authorization depend on a second system staying in
   step, and a policy that drifts open is invisible to Pando.
3. **A per-app switch that puts Access in front with an operator-chosen policy**, reported in the plan
   as an addition and never as a replacement for the proxy.

**O-24** was raised by issue #87. The rolling backup sweep (design 05 §6) takes a copy only of apps
that are `running` or `degraded`. A stopped app's data does not change, so a new copy would hold
nothing the last one did not — but retention keeps going, and seven days after an app is stopped its
last rolling backup expires and nothing replaces it. The options:

1. **Leave it.** A stopped app is one somebody chose to stop, and its volume is still on disk (R-204).
   The cost is that "7 retained" quietly becomes "none" for an app that has been stopped a week.
2. **Stop expiring the newest rolling backup of an app that is not running**, so there is always one.
   Nothing extra is taken; one copy outlives its retention on purpose.
3. **Back up stopped apps too.** Simplest to state, and seven identical copies of data nothing is
   writing to.

Separately, the issue asks whether an app with storage that has had no backup in more than 48 hours
should notify its owner. Every attempt is now recorded and shown on the app and on the Backups screen,
so the question is only whether it should also reach somebody who is not looking. That is a notification
policy, and R-231 leaves which events notify to be decided.

**O-13 — one clock, not two.** The re-authorization interval is *exactly* the assertion lifetime
rather than an independently chosen value. Two clocks measuring the same thing drift apart the first
time someone tunes one. See design 06 §3.1.

## Adding one

If you hit a question the docs do not answer, add a row here rather than deciding in code. Include the
question, what depends on it, the options you can see, and which requirement or design section would
need to change for each. A question recorded with its options is most of the work of answering it.
