# 04 — API

R-261: the API is the product. Console, CLI, and MCP are clients of it. None may have a capability the API lacks.

**[D]** Enforcement: handlers contain no business logic. Everything lives in a service layer under `internal/core`, and `httpapi` and `mcp` both call it. A capability that exists in one and not the other means someone put logic in a handler.

---

## 1. Conventions

**[P]** Base path `/api/v1`. JSON in and out. chi router.

**Authentication**, in precedence order:
1. `Authorization: Bearer tok_...` — a token principal (§02 2.1)
2. Session cookie `pando_session=ses_...`

**Errors** use the envelope from §00 3.2. Every response carries `X-Request-Id`.

**Pagination [P]:** cursor-based. `?limit=50&cursor=...`, response carries `next_cursor`.

**Idempotency [P]:** `POST` endpoints that create infrastructure accept `Idempotency-Key`. Required for MCP, where an agent retry must not deploy twice.

---

## 2. Resources

### 2.1 Apps

```
GET    /api/v1/apps                      list (filtered by app.view)
POST   /api/v1/apps                      create — begins onboarding; app.create (install-scoped)
GET    /api/v1/apps/{id}
PATCH  /api/v1/apps/{id}                 name, owner
DELETE /api/v1/apps/{id}                 R-204/205 — see below
GET    /api/v1/apps/{id}/icon            the launcher tile's image — data plane (R-340)
PUT    /api/v1/apps/{id}/icon            body is the image; app.spec.edit
DELETE /api/v1/apps/{id}/icon            app.spec.edit
POST   /api/v1/apps/{id}:start
POST   /api/v1/apps/{id}:stop
POST   /api/v1/apps/{id}:restart
```

**Icon** (R-340). The body of `PUT` is the image bytes, not JSON. The type is sniffed from the bytes, never taken from `Content-Type`; PNG, JPEG, WebP and GIF are accepted and SVG is not, because it is a document that can carry script and would be served from Pando's origin. 256 KB at most. `GET` is gated on the data plane (`CheckData`), the same as the tile it is drawn on, and answers not-found to anyone who cannot open the app. It is served with `nosniff`, a `sandbox` CSP and `Cache-Control: private`. Every app representation carries `icon_updated_at`, absent when there is no image, so a client can put it in the image URL and never show a stale picture.

**Create** takes a source and, optionally, routing and runtime choices. It does not deploy. It returns an app in `draft` with a detection job started.

```json
POST /api/v1/apps
{
  "name": "team-notes",
  "source": { "type": "git", "url": "https://github.com/acme/notes", "ref": "main" },
  "routing": { "adapter_ref": "rte_traefik" },
  "runtime": { "adapter_ref": "rt_docker" }
}
```

**[D]** The source allowlist (R-092) is evaluated **here**, before any clone. A blocked source returns `POLICY_SOURCE_NOT_ALLOWED` and nothing touches disk.

**Delete** implements R-204/205:

```
DELETE /api/v1/apps/{id}?backup=true|false&force=true
```

- `backup` omitted, `force` absent → `409 STATE_BACKUP_DECISION_REQUIRED`. The console uses this to raise the prompt.
- `force=true` → delete without backup (R-205).
- `backup=true` → snapshot volumes and provisioned services first, retained until explicitly discarded (R-204).

**[D]** The 409-on-ambiguity design is what makes the interactive prompt and the non-interactive default coexist without two code paths.

### 2.2 Detection and proposals

```
GET  /api/v1/apps/{id}/detection          current auction result
POST /api/v1/apps/{id}/detection:rerun    explicit re-detection (R-022)
GET  /api/v1/apps/{id}/detection/diff     against the pinned spec
POST /api/v1/apps/{id}/detection/answers  answer outstanding questions
POST /api/v1/apps/{id}/detection:accept   pin the proposal → spec revision 1
```

```json
GET /api/v1/apps/{id}/detection
{
  "status": "needs_answers",
  "winning_bid": {
    "adapter": "buildkit",
    "strategy": "dockerfile",
    "confidence": 0.92,
    "evidence": ["Dockerfile at repository root", "EXPOSE 3000"]
  },
  "runners_up": [ { "adapter": "buildkit", "strategy": "buildpack", "confidence": 0.4 } ],
  "questions": [
    {
      "key": "primary_port",
      "prompt": "This repository builds two services. Pando could not determine which one serves the app's web interface. Valid answer: the name of one service, either 'web' or 'admin'.",
      "why": "Pando needs to know which service to route your app's URL to.",
      "kind": "choice",
      "options": ["web", "admin"]
    }
  ],
  "draft_spec": { }
}
```

**[D]** `runners_up` is returned so the review UI can show what else bid, satisfying "ask, never guess" (R-102) transparently — the user can see the auction rather than being handed a verdict.

**[P]** While `status` is `needs_answers`, the response also carries `unanswered`: the keys of the questions that still have no answer. Answers are applied at accept, so `status` stays `needs_answers` until then; an empty `unanswered` is how a client tells "answered, ready to accept" from "waiting for answers" (issue #55).

**[P]** While `status` is `running`, the response carries `stage` (fetching, detecting, trying, scanning, screening) and `elapsed_seconds`, by the server's clock. Elapsed time rather than an estimate: the trial run (R-097) dominates and varies too widely for an estimate to be honest (issue #80).

**[P]** `?wait=` (seconds, or a duration such as `30s`; at most 60) makes the read a long poll: while detection is running, the answer is held until it reaches a new stage or finishes, or the wait runs out. A finished detection is answered at once. A long poll rather than a stream, because it works through every proxy and fits MCP's request/response model; and one mechanism for every surface (R-261) — the console waits this way, `pando app detection --wait` does, and `pando_get_detection` takes `wait_seconds`. `GET /apps` and `GET /apps/{id}` carry the same `status` and `stage` as `detection` on each app, so a draft says why it is still a draft (issue #80).

**[P]** `pando app detection --wait` exits 0 when the proposal is ready to accept, 2 when there are questions a person has to answer, and 1 when detection failed or was blocked — so a script can tell "someone has to act" from "it broke" without reading the output.

**[D]** `questions[].prompt` is held to R-105. The console shows a copy button on it, because the expected workflow is pasting it into the assistant that wrote the app.

### 2.3 Specs and deployments

```
GET  /api/v1/apps/{id}/specs                     revision list
GET  /api/v1/apps/{id}/specs/{rev}
POST /api/v1/apps/{id}/specs                     create a new revision (edit); ?dry_run=true writes nothing
GET  /api/v1/apps/{id}/specs/{a}/diff/{b}        classified diff (§01 4)

POST /api/v1/apps/{id}/deployments                deploy a revision
GET  /api/v1/apps/{id}/deployments
GET  /api/v1/apps/{id}/deployments/{did}
GET  /api/v1/apps/{id}/deployments/{did}/logs     SSE stream of build output
POST /api/v1/apps/{id}/deployments:rollback       to a prior revision (R-152)
POST /api/v1/apps/{id}/deployments/{did}/approve  {comment?}; app.deploy.approve or install.deploys.approve (R-155)
POST /api/v1/apps/{id}/deployments/{did}/reject   {comment?}; same verbs
POST /api/v1/apps/{id}:plan                       dry run — plan without applying
GET  /api/v1/apps/{id}/egress                     install rules + effective rules; app.view (R-188)
GET  /api/v1/approvals                            deploys awaiting approval on apps the caller can view
```

**[D] A spec save can be a dry run.** `POST /specs?dry_run=true` runs everything a save runs —
defaults, validation, the routing-override check, the egress gate, the auto-deploy refusal — and
writes nothing: no revision, no audit event, and no audited denial, because nothing was attempted
(`Authorizer.PreviewControl`, `specgate.Quiet`). A spec it would refuse is refused with the save's own
code and words. One it would accept answers 200 with `egress` (the merged rules it would run with),
`egress_changed`, `new_loosenings`, and `approval: {required, reasons}`, measured as a deploy measures
it, against what the app last ran. The console's egress editor asks it as the draft changes, so what
it shows is the server's decision rather than its own reading of the rules.

**[D] Deploy approval (R-154 – R-159).** `POST /deployments` takes the same request whether or not
approval is needed. When it is, the answer is still 202, with the deployment in status
`awaiting_approval` — the app's state does **not** move to `deploying`, and nothing is built. A
deployment's statuses are `awaiting_approval`, `pending`, `building`, `applying`, `succeeded`, `failed`,
`superseded`, `rejected`, `expired`. A deployment gains, all omitted when empty:
`approvals_required`, `approval_expires_at`, `approval_reasons` (`[{reason, message}]`, reason one of
`install | app_policy | app_spec | egress_loosening`), `approvals`
(`[{principal_id, principal_name, decision, comment, decided_at}]`), and `can_decide` — whether the
caller may approve or reject it, on `awaiting_approval` ones only, so the console never offers a button
that will be refused.

**[D]** `approve` returns 200 with the deployment: still `awaiting_approval` while more approvals are
needed, `pending` or later once it has enough and has started. It re-runs the plan first, because
policy may have changed while the request waited (R-156); a plan-time refusal comes back as the plan's
own error. `reject` returns 200 with the deployment `rejected`; one rejection ends a request. Either
verb may decide its holder's own request (R-155). The App manager role does not hold
`install.deploys.approve` (design 06 §5). A rollback to a revision that previously ran is never held for
approval, and restarts and secret rotations are not deploys (R-157).

**[D]** `GET /approvals` returns `{"approvals": [deployment + app_name, app_slug]}` for
`awaiting_approval` deployments on apps the caller can view, each with `can_decide`. It is the
approver's inbox; approvers are also told through the notification adapter (R-159).

**[D]** `GET /apps/{id}/status` gains `auto_deploy_paused`: the pinned spec has auto-deploy on, but the
app now needs approval, so the auto-deploy job skips it (R-158).

**[D] Egress.** An app's egress is changed the ordinary way: a new spec revision with `egress` changed.
The save checks, against the rules the app runs with now: any change needs `app.egress.tighten` or
`app.egress.loosen`; a **new** loosening under `egress_loosening: forbidden` is refused with
`PLAN_EGRESS_LOOSENING_FORBIDDEN`, under `verb` needs `app.egress.loosen` (else `PERM_VERB_REQUIRED`),
and under `approval` is saved and its deploy waits (R-183, R-184). A loosening the running spec already
carries is not asked about again. `GET /apps/{id}/egress` returns
`{install: {mode, list, block_private, loosening}, effective, spec}` — the installation's rules, the
merged rules for the pinned spec, and the spec's own egress object — so an owner without
`install.view` can see the rules they are editing against.

**[D]** The plan response gains `egress` (the merged `EffectiveEgress`: mode, list with each entry's
origin, the app's own list, private-range blocking and its origin, every loosening with the gate it
needs, and whether anything is restricted — R-188), `notes` (never blockers: the R-187 proxy-only note
wherever a restriction is in effect, additions and removals that change nothing), and
`approval: {required, reasons}` compared against the pinned spec.

**[D]** `:plan` exists as its own endpoint because every plan-time failure in the requirements (R-024, R-132, R-242, R-254) is more useful before a user commits than during a deploy. The console calls it on every spec edit.

```json
POST /api/v1/apps/{id}:plan
→ 409
{
  "code": "PLAN_SLOT_UNFILLED",
  "message": "This app needs a Redis, and one hasn't been chosen yet.",
  "remedy": "Choose how to fill the REDIS_URL slot: provision one inside this app, connect to an existing Redis, or paste a connection string.",
  "details": { "slots": [ { "key": "REDIS_URL", "type": "redis" } ] },
  "request_id": "req_..."
}
```

### 2.4 Slots, secrets, volumes

```
GET   /api/v1/apps/{id}/slots
PUT   /api/v1/apps/{id}/slots/{key}          set resolution (R-131)

GET   /api/v1/apps/{id}/secrets              keys and metadata only — never values
PUT   /api/v1/apps/{id}/secrets/{key}        requires app.secrets.write
GET   /api/v1/apps/{id}/secrets/{key}/value  requires app.secrets.read; audited
DELETE /api/v1/apps/{id}/secrets/{key}

GET   /api/v1/apps/{id}/volumes
POST  /api/v1/apps/{id}/volumes              add one after the R-201 warning
```

**[D]** Reading a secret value is a **separate endpoint** from listing secrets, so R-083's split between write and read is enforced by routing rather than by a field-level check that someone will forget.

### 2.5 Access

```
GET    /api/v1/apps/{id}/grants
POST   /api/v1/apps/{id}/grants
DELETE /api/v1/apps/{id}/grants/{gid}
```

```json
POST /api/v1/apps/{id}/grants
{ "plane": "data",    "principal_kind": "group", "principal_id": "grp_..." }
{ "plane": "control", "principal_kind": "user",  "principal_id": "usr_...", "role_id": "role_operator" }
{ "plane": "data",    "principal_kind": "anonymous" }
```

**[D]** The anonymous grant is the same endpoint, not a special toggle (R-075). Host policy may reject it with `POLICY_ANONYMOUS_GRANT_FORBIDDEN` (R-076). The console renders this grant with the R-077 wording — *anyone on the internet, without signing in* — never the word "public" alone.

### 2.6 Runtime access

```
GET  /api/v1/apps/{id}/logs?follow=true      SSE
GET  /api/v1/apps/{id}/exec                  WebSocket upgrade; requires app.exec
GET  /api/v1/apps/{id}/status                observed state, health, restarts
```

**[D]** `/exec` checks `app.exec`, then host policy (R-085, returning `POLICY_EXEC_DISABLED`), then writes the audit event, **then** opens the session. Audit before access, so an aborted session is still recorded.

**[D]** It is a `GET`, not the `POST` this line said until phase 8. A WebSocket handshake is a GET by protocol — RFC 6455 requires it and a browser's `new WebSocket()` cannot issue anything else — so `POST` was not implementable from the console the endpoint exists for. Nothing else about the ordering or the checks changes.

### 2.7 Identity and principals

```
GET    /api/v1/users                      install.view
POST   /api/v1/users                      local adapter only; install.users.manage
GET    /api/v1/users/{id}                 self, or install.view
PATCH  /api/v1/users/{id}                 status: active | suspended (R-049); self, or install.users.manage
DELETE /api/v1/users/{id}                 triggers §21 destruction rules; install.users.manage

PUT    /api/v1/users/{id}/role            grant an install-scoped role; install.users.manage
DELETE /api/v1/users/{id}/role            revoke it; install.users.manage

GET    /api/v1/groups
POST   /api/v1/groups
PUT    /api/v1/groups/{id}/members

GET    /api/v1/tokens
POST   /api/v1/tokens                     secret returned once (R-063)
DELETE /api/v1/tokens/{id}

GET    /api/v1/roles
POST   /api/v1/roles                      custom roles (R-082)
GET    /api/v1/verbs                      the verb catalog, for building custom roles
```

**[D]** `PATCH /users/{id}` with `status: suspended` must not trigger data destruction. `DELETE` does. The API shape makes R-049/R-282 explicit rather than a flag on one endpoint.

**[D]** The two `/users/{id}` routes are **self or verb**. Reading or changing your own account is
self-service; doing either to someone else is administration and needs an install-scoped verb
(design 06 §2.1). Both halves are load-bearing: without the first, an install with one administrator
cannot let anyone manage their own account; without the second, any signed-in account can suspend the
administrator, which is what these endpoints allowed until O-17 was resolved. A delegated token acts
as its owner here as everywhere else (R-058), so an agent may act on its owner's account and no
other.

**[D]** Promotion is its own route, not a field on `PATCH /users/{id}`. Changing someone's status and
changing their power are different acts with different verbs everywhere else in this system, and
folding them into one body is how a status update quietly becomes a promotion.

**[D]** `DELETE /users/{id}/role` refuses to remove the last principal holding `install.users.manage`,
transactionally. An install that cannot be administered has no recovery path inside the product — the
way back is a psql prompt, which is the same lockout O-17 allowed by accident, reachable on purpose.
The rule names the *verb* rather than the administrator role, so a custom role (R-082) holding it
counts.

**[D]** `GET /users/{id}` is not public to signed-in callers. An account carries an email address and
a display name, and "every user can enumerate every user" is a disclosure nobody asked for.

### 2.7a External identity (issue #51)

```
GET    /api/v1/auth/options                        public: password sign-in on?, providers turned on
GET    /api/v1/auth/providers/{id}/start           public, a navigation: redirects to the provider
GET    /api/v1/auth/providers/{id}/callback        public: OIDC redirect URI
POST   /api/v1/auth/providers/{id}/callback        public: SAML ACS (HTTP-POST binding)
GET    /api/v1/auth/providers/{id}/metadata        public: SAML SP metadata; its URL is the entity ID
GET    /api/v1/auth/complete?code=                 public: finishes a sign-in in the browser that began it
GET    /api/v1/auth/failures/{flowID}              public: why a sign-in failed, for the sign-in page

GET    /api/v1/identity-providers                  install.view; with the kinds and their presets
POST   /api/v1/identity-providers                  install.adapters.manage; starts turned off
GET    /api/v1/identity-providers/{id}             install.view
PATCH  /api/v1/identity-providers/{id}             install.adapters.manage; no restart
DELETE /api/v1/identity-providers/{id}             install.adapters.manage; refused once used
POST   /api/v1/identity-providers/{id}/check       install.adapters.manage
POST   /api/v1/identity-providers/{id}/scim-token  install.adapters.manage; token returned once
DELETE /api/v1/identity-providers/{id}/scim-token  install.adapters.manage
GET    /api/v1/identity-providers/{id}/test        install.adapters.manage, a navigation: test sign-in
GET    /api/v1/identity-providers/{id}/tests/{fid} the report, to the administrator who ran it

GET    /api/v1/users/{id}/identities               self, or install.view
POST   /api/v1/users/{id}/identities               install.users.manage; replace_account moves one
DELETE /api/v1/users/{id}/identities?adapter_id=&external_id=
PUT    /api/v1/groups/{id}/links/{syncedID}        install.users.manage (R-078)
DELETE /api/v1/groups/{id}/links/{syncedID}

/api/v1/scim/v2/{ServiceProviderConfig,ResourceTypes,Schemas,Users[/{id}],Groups[/{id}]}
                                                   SCIM 2.0; the provider's own bearer token
```

**[D]** Identity providers are adapters (R-040), so the verbs are the adapters' verbs: read with
`install.view`, changed with `install.adapters.manage`. Linking an identity to an account is account
administration, `install.users.manage`.

**[D]** The sign-in endpoints are browser navigations and answer with redirects, never JSON a person
would see. A failure goes to the sign-in page with a flow ID or a fixed key, and the page asks
`/auth/failures/{id}` for the message: nothing in a URL is ever shown as text, so a link cannot put
words on Pando's sign-in page.

**[D] SCIM authenticates with the provider's token, not a Pando token.** The Authenticate middleware
lets `/api/v1/scim/v2/` through untouched and the SCIM handlers resolve the bearer token to its
provider by digest. A SCIM token is not a principal: it reaches these endpoints and nothing else, and
a Pando token does not reach them. Errors use SCIM's own error schema (RFC 7644 §3.12), not the
envelope, because SCIM clients parse that. PATCH accepts what Okta and Entra actually send —
`replace` with a value object and no path, capitalized ops, `"False"` as a string, and filtered paths
such as `emails[type eq "work"].value` — and is tested against those shapes. Filters are
`attribute eq "value"` only: every provisioning client needs "does this person exist", and nothing
more is kept for nobody. `DELETE /Users` suspends (R-049).

### 2.8 Platform

```
GET  /api/v1/adapters                     configured instances + live capabilities; install.view
POST /api/v1/adapters                     install.adapters.manage
GET  /api/v1/capacity                     aggregated from adapters (R-243); install.view
GET  /api/v1/policy                       install.view
PUT  /api/v1/policy                       R-274; see O-10; install.policy.manage
GET  /api/v1/config                       startup settings and their sources; fixed policy fields (R-271); install.view
POST /api/v1/policy:preview               what this policy would block, unsaved; install.policy.manage
GET  /api/v1/audit                        ?action= (prefix, repeatable: any matches) &principal_id= &principal_kind= &app_id= &target_kind= &target_id= &involving= &since= &until= (RFC 3339) &before= ; install.audit.read
GET  /api/v1/audit/archives               months past retention, each with its manifest (R-347); install.audit.read
GET  /api/v1/audit/archives/{id}          one archived month, gzipped JSON lines; Repr-Digest carries its SHA-256; install.audit.read
GET  /api/v1/ai/functions                 each AI function, its adapter and model, on or off, and its source (R-259); install.view
PUT  /api/v1/ai/functions/{function}      {adapter_id, model?}; refused while another adapter holds it (R-259); install.adapters.manage
DELETE /api/v1/ai/functions/{function}    turn a function off; install.adapters.manage
POST /api/v1/ai/access/draft              {description} → a draft role and group, not created (R-343); install.users.manage
POST /api/v1/ai/policy/draft              {description} → a proposed policy, not saved (R-344); install.policy.manage
POST /api/v1/ai/audit/search              {question} → audit filters, run, with a summary (R-345); install.audit.read
POST /api/v1/ai/reference/answer          {question} → an answer from the reference, with citations (R-346); any signed-in user
GET  /api/v1/roles                        ?scope=install (default) | app | all (R-082); install.view
GET  /api/v1/backups
POST /api/v1/backups                      trigger; kind = rolling | dr_bundle
POST /api/v1/apps/{id}/restore            put one app's data back (R-206); app.deploy
POST /api/v1/backups/{id}:verify          R-216
POST /api/v1/backups/{id}:restore         verifies first (R-215)
GET  /api/v1/.well-known/jwks.json        assertion keys (R-057)
```

**[D]** `GET /adapters` returns live capabilities, not stored config, so the console can grey out routing modes an adapter doesn't support instead of offering choices that fail at plan time.

**[D]** An adapter declared in the config file is listed with `declared: true` and its `source`, and a stored adapter it overrides with `status: "overridden"`. `POST /adapters` refuses a declared adapter, or an AI adapter of a declared provider, with `STATE_SET_AT_STARTUP` naming the file and key; `PUT` and `DELETE /ai/functions/{function}` refuse a declared function the same way. `GET /config` lists the declared adapters (R-271, design 10 §7.1).

**[D]** The `/ai/*` drafts, search and answer each propose and change nothing (R-106). Each is gated by the verb its ordinary endpoint needs, is audited as `ai.<function>` naming the adapter and model but not the text, and returns `ADAPTER_UNAVAILABLE` with the remedy when its function is not assigned. Applying a result is the ordinary request: `POST /roles` and `POST /groups`, `PUT /policy`, `GET /audit` with the returned filter. Design 10 §10.

**[D]** The two backup kinds are **two objects on one route, authorized differently**, and the
difference is scope rather than size. `dr_bundle` is the whole installation, needs
`install.backup.manage`, and is encrypted under a passphrase Pando never stores (R-213). `rolling` is
one app's data, names that app in `app_id`, and needs `app.deploy` **on that app** — the same verb as
restoring it, because taking a copy and putting it back are two halves of one operation and an owner
who may do the destructive half should not need an administrator for the safe one. It is checked with
`CheckControl`, so an administrator reaches it through `install.apps.deploy` like any other app verb
(R-081); it is not `install.backup.manage` with a filter, which is a different question.

**[P]** A rolling backup is encrypted under the install's own secrets key, not a typed passphrase.
R-213 governs the bundle that has to survive the machine; applying it here would mean an app backup
nobody schedules, and R-210's scope — "recover from a recent mistake" — is served by a copy that
exists, on a host whose key the attacker would already have if they had the copy.

**[P]** A rolling backup of an app with no volumes is **refused**, not taken empty. An app with no
storage has nothing a copy would hold that its spec revisions do not, and a bundle that restores
nothing is worse than a refusal: it is a recovery somebody believes in (R-105).

**[D]** Policy is read with `install.view` and written with `install.policy.manage`. Seeing the rules
you work under is not the same privilege as changing them — the same split as `app.secrets.read` and
`app.secrets.write` (R-083).

**[D]** `PUT /policy` rejects a `disabled_verbs` entry that is not in the catalog. Policy can only
deny (R-272), so a typo denies nothing and looks exactly like a rule that works, which is the worst
failure mode a security control has.

**[D]** `POST /policy:preview` takes the **same body** as `PUT` and saves nothing, returning the apps
whose next deploy the policy would block, each with the error code and message that deploy will
actually fail with. Design 05 §3 requires this: O-10 resolved policy application to "report now, block
on next deploy", and an admin tightening a policy is entitled to know it will block four apps before
they save. Finding out one deploy at a time is how a policy gets rolled back in anger.

**[P]** Preview is behind `install.policy.manage`, not `install.view`. The body is a policy someone is
composing, and answering "which apps does this break" for anyone who can read policy hands them a
probe for the install's shape — one arbitrary query at a time, without writing anything. An empty
list returns 200: "nothing breaks" is the answer an admin most wants, and it must not be
indistinguishable from a failure. Preview is not audited (R-229): it reads, changes nothing, and a log
with an entry per keystroke of a form is a log nobody reads.

**[D]** `GET /audit` pages on a **cursor** (`before=<id>`), not an offset. The log is append-only with
monotonic IDs; with an offset, events arriving between requests shift every later page. `action`
matches a prefix rather than a substring, because actions are dotted namespaces and a substring match
would make `grant.delete` a result for a search for "delete". `action` may be given more than once, and
any of the prefixes matches — "apps someone created or deleted" is two prefixes — while different
parameters still combine with AND.

### 2.9 End-user surface

```
GET  /api/v1/me                           profile, groups, install-scoped verbs
GET  /api/v1/me/apps                      the launcher tiles (R-264)
POST /api/v1/me/password                  change your own password (R-046)
PUT    /api/v1/me/favorites/{id}          pin an app to the top of your launcher (R-341)
DELETE /api/v1/me/favorites/{id}          unpin it
POST   /api/v1/me/sections                make a launcher section (R-342)
PATCH  /api/v1/me/sections/{id}           rename it
DELETE /api/v1/me/sections/{id}           delete it; its apps go back to Your apps
PUT    /api/v1/me/sections/{id}/apps/{app}   file an app into it, out of any other
DELETE /api/v1/me/sections/{id}/apps/{app}   take it back out
```

**[D]** Sections (R-342) follow favorites: self only, no verb, a user required, every store call keyed on
the caller so someone else's section is not-found. Filing an app needs `CheckData` on it; taking one out
does not. An app is in at most one of a person's sections (primary key on placements), and the composite
foreign key from placement to `(section id, user id)` makes filing into someone else's section
unrepresentable. `GET /me/apps` returns `sections` alongside `apps`, and each app's `section_id`.

**[D]** Favorites (R-341) are self-only and carry no verb. `PUT` needs a user — a service token has no
launcher and is refused — and answers not-found for an app the caller cannot open (`CheckData`), so a
favorite cannot be used to probe for apps. `DELETE` checks nothing beyond the caller: removing your own
row is always allowed, including for an app you have since lost. Both are idempotent and answer `204`.
`GET /me/apps` carries `favorite` on each app; there is no separate list, because the favorites are a
subset of that list and a second endpoint could disagree with it.

**[D]** `POST /me/password` takes the current password as well as the new one, even though the caller
is already authenticated. A session cookie is a bearer credential; without the check, anyone holding
a borrowed one could lock the owner out of their own account. It is self-only and carries no verb:
changing your own password is not administration, and changing somebody else's is a *reset* — a
different action with different consequences, `POST /users/{id}/password`, which did not arrive by
relaxing this route.

**[D] A new installation is set up in the console, not from a log line (R-046).** Without
`PANDO_ADMIN_PASSWORD`, first run creates no account and prints nothing. `GET /setup` says whether
the installation is waiting; `POST /setup` creates the first account with the username and password
the person chose, grants it Administrator, and signs it in. Both are public, because nobody can sign
in yet, and `POST /setup` is refused once any account exists — a transaction under an advisory lock,
so two people submitting at once cannot both win. The account is not flagged must-change: its holder
chose the password. The exposure is the window before setup, when whoever reaches the console first
becomes the administrator; the README says to set up before exposing Pando, and startup logs a warning
until it is done. A generated password printed to the log had its own exposure — anyone who could
read logs — and was lost when a container was recreated before anyone read it.

**[P] The first password can still be supplied, as `PANDO_ADMIN_PASSWORD`**, for an unattended
install. It must be changed at first sign-in, because an environment variable is not a safe place for
one — it is in the Compose file, in `docker inspect`, and inherited by every child process. Set on an
install that already has accounts it is ignored, and says so.

**[D] Passwords an administrator sets are generated.** `POST /passwords/generate` returns one, 18 to
22 characters of upper and lower case, digits and symbols with at least one of each, from the same
generator `pando admin reset-password` uses. The console shows it with a way to draw another and to
copy it; the administrator hands it over out of band. `POST /users` and `POST /users/{id}/password`
take it with `must_change_password`, true by default. A reset ends every session the account holds,
and is not for your own account — that is `POST /me/password`, which asks for the current one.

**[P] Ten characters, and that is the only rule.** No composition classes: a class requirement pushes
people toward `Passw0rd!`, which has less real entropy than three words and is the password the rule
reliably produces. Ten rather than a DR bundle's sixteen (§07) because the threats differ — a password
is guessed against a server that rate-limits and can lock the account, and a passphrase protects a
file an attacker already holds and can grind offline as fast as their hardware allows.

**[D]** Success clears `must_change_password` and revokes the caller's **other** sessions. The
current one survives, because being signed out by your own password change teaches people that
changing it is risky. R-046 promised "must be changed on first login" from the beginning; until this
endpoint existed that flag was something nothing could clear.

**[D]** `GET /me` carries `verbs`: the install-scoped verbs the caller holds, always present and
usually empty. The console reads it to decide whether to show the Admin entry and what to put in it
(R-265) rather than inferring administration from another response. It is not enforcement — every
install-level endpoint checks its verb itself — it is what keeps the console a client of the API
rather than a second opinion about authorization (R-261).

**[D]** `/me/apps` returns apps where the caller holds a **data-plane** grant. It is not the same list as `GET /apps`, which is control-plane scoped. Two planes, two endpoints (R-070/071).

---

## 3. MCP surface

**[D]** R-262. The MCP server exposes the same service layer. Tools map to endpoints:

| Tool | Endpoint |
|---|---|
| `pando_list_apps` | `GET /apps` |
| `pando_get_app` | `GET /apps/{id}` |
| `pando_create_app` | `POST /apps` |
| `pando_get_detection` | `GET /apps/{id}/detection` |
| `pando_answer_detection` | `POST /apps/{id}/detection/answers` |
| `pando_accept_proposal` | `POST /apps/{id}/detection:accept` |
| `pando_plan` | `POST /apps/{id}:plan` |
| `pando_deploy` | `POST /apps/{id}/deployments` |
| `pando_get_logs` | `GET /apps/{id}/logs` |
| `pando_get_status` | `GET /apps/{id}/status` |
| `pando_list_approvals` | `GET /approvals` |
| `pando_approve_deploy` | `POST /apps/{id}/deployments/{did}/approve` |
| `pando_reject_deploy` | `POST /apps/{id}/deployments/{did}/reject` |
| `pando_list_ai_functions` | `GET /ai/functions` |
| `pando_assign_ai_function` | `PUT /ai/functions/{function}` |
| `pando_unassign_ai_function` | `DELETE /ai/functions/{function}` |
| `pando_ai_draft_access` | `POST /ai/access/draft` |
| `pando_ai_draft_host_rules` | `POST /ai/policy/draft` |
| `pando_ai_search_audit` | `POST /ai/audit/search` |
| `pando_list_audit_archives` | `GET /audit/archives` |
| `pando_ai_ask_reference` | `POST /ai/reference/answer` |

**[D]** `GET /audit/archives/{id}` has no tool. A tool's result is JSON text, and an archive is a gzip
file; base64 inside a tool result would be a capability the CLI and console do not need. The list tool
names the endpoint and `pando audit archives download`, which an agent holding a token can use.

**[D]** The AI drafting tools are listed because a draft changes nothing: applying it is a separate
call the agent may not be able to make. The policy draft is `pando_ai_draft_host_rules`, not a name
containing "policy", because the O-12 test refuses any tool named for policy and this one is not the
policy mutation that test keeps out.

**[D]** An agent holds a token and is a principal like any other (R-262). No MCP tool bypasses authorization, and every action lands in the audit log under the token's owner.

**[D] Not exposed via MCP:** exec, secret value reads, grant mutation, policy mutation, user deletion. Rationale — these are the highest-consequence actions in the system and R-086 already concedes exec is not bounded by the verb list. An agent should not hold the most dangerous capabilities by default.

**[D]** Approving a deploy is excluded by the same default, for a different reason: approval is a
human sign-off on a change (R-154), and an agent approving the deploy it — or another agent — just
requested is the check signing itself. `policy.Default()` puts `install.deploys.approve` and
`app.deploy.approve` in `agent_disabled_verbs`. The approval tools exist so an install that lifts that
rule does not have to reach for the REST API; out of the box they list, and refuse to decide.

**[D] Resolved (O-12): policy-controlled, default-closed, and expressed as host policy — not as an MCP
list.** The exclusions above are the shipped default and an install can lift them, but the knob is the
existing per-verb host policy rather than a second mechanism that happens to gate the same actions.

A dedicated MCP exclusion list would be the second place in the system that answers "may this
principal exec," and the two would disagree the first time someone edited one. Host policy is already
evaluated before grants for every principal (§06 2, step 5) and an agent is a principal like any other
(R-262) — so the honest expression of "agents may not exec here" is a policy scoped to token
principals, which also covers the CLI token an agent could otherwise use to route around an
MCP-specific block.

**[D]** That last point is the reason this cannot stay a hard-coded list: an agent holding a token can
call the REST API directly. An MCP-layer exclusion is a speed bump, not a boundary. Enforcement has to
live where every surface passes through it.

---

## 4. CLI shape [P]

```
pando login
pando app list
pando app add <url> [--routing=...] [--runtime=...]
pando app show <app>
pando deploy <app|path>
pando plan <app>
pando logs <app> [-f]
pando exec <app> [--workload <name>] [-- <cmd>...]
pando secret set <app> <key>
pando slot set <app> <key> --provision|--bind=<target>|--literal
pando grant add <app> --user=<u> --plane=data
pando rollback <app> [--to=<rev>]
pando approvals list
pando deploy approve|reject <app> <deployment> [--comment=...]
pando export <app>
pando backup create|verify|restore
pando policy show|set

pando admin reset-password [user]          server-side; needs the host, not a token
```

**[D]** `pando deploy ./` must work from a local path, since R-262's agent workflow depends on it — a generated app cannot drop a config file, but an agent can invoke a command.

**[D]** Everything above `pando admin` is a client of this API and nothing else. `internal/cli` imports
no core package, so a command that needed something the API cannot do fails to compile rather than
quietly growing a shortcut (R-261).

**[D]** `pando admin` is the exception, and lives in `cmd/pando` beside `serve` and `migrate` rather
than in `internal/cli`. Its commands run against the database directly because they exist for the case
where **there is no account to authenticate as** — there is no request they could make. Keeping them
out of the client package is what stops that exception spreading.

**[P] Host shell access is the authorization for `pando admin`, and that is a boundary rather than the
absence of one.** Whoever can run it can already read the configuration naming the database and change
the row by hand; the command exists so that doing it correctly — a real argon2id digest, sessions
ended, an audit event written — is easier than doing it by hand. Every `pando admin` action writes to
the audit log as `system` with the reason it ran outside any session, and a failure to record it fails
the command: a credential reset that leaves no trace is a backdoor.

**[P] `reset-password` ends every session the account has.** A reset that leaves a live cookie working
has taken nothing back (R-048), and the case it exists for is the one where somebody else may be
holding it.
