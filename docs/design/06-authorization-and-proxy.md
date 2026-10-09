# 06 — Authorization and the Proxy

Two planes, evaluated separately, never conflated. This is the most security-sensitive code in the system and the part most likely to be quietly broken by a later refactor.

---

## 1. Principals

```go
type Principal struct {
    Kind        PrincipalKind  // user | token | anonymous | system
    ID          string
    UserID      string   // delegated token: the owner (R-059). Same as ID for users.
    TokenID     string
    Groups      []string // resolved live (R-079)
    AdapterID   string
}
```

**[D]** A delegated token resolves to a `Principal` with `Kind: token` and `UserID` set to its owner. Authorization then runs against `UserID` exactly as if the user made the request (R-058, R-059). No grant is ever written for a delegated token.

**[D]** An account token resolves to `Kind: token`, `UserID` empty, and is looked up in grants under its own ID (R-060).

**[D]** `system` exists for the reconciler and background jobs. It bypasses grant checks but **still writes audit events**, attributed to `system`. Anything a background job does must be as visible as anything a person does.

---

## 2. Evaluation order

**[D]** Fixed order. Each step can only deny; none can restore access denied by an earlier step.

```
1. Authenticate            → AUTH_* on failure
2. Principal status check  → suspended/deleted principals denied (R-049)
3. Token validity          → expired, revoked → AUTH_TOKEN_INVALID
4. Token derivation        → owner suspended/deleted → AUTH_TOKEN_ORPHANED (R-059)
5. Host policy             → POLICY_* (e.g. exec disabled install-wide, R-085)
6. Grant lookup            → PERM_* if no matching grant
7. Verb check              → PERM_VERB_REQUIRED (control plane only)
```

**[D]** Policy is evaluated **before** grants (step 5). A policy that disables exec install-wide denies the owner too. Policy is a floor, not an override (R-272).

**[D]** Step 4 is what makes R-059 real. It is a live lookup on every request, not a cascade run at revocation time. Slower, and correct — a cascade means a missed cascade is a permanent security hole.

```go
func (a *Authorizer) CheckControl(ctx context.Context, p Principal, appID string, verb Verb) error {
    if err := a.checkPrincipal(ctx, p); err != nil { return err }
    if err := a.policy.Allows(ctx, verb, appID); err != nil { return err }

    grants := a.state.ControlGrantsFor(ctx, appID, p)  // user, groups, token
    for _, g := range grants {
        if a.roles.Has(g.RoleID, verb) {
            return nil
        }
    }
    return ErrPermission(verb)
}

func (a *Authorizer) CheckData(ctx context.Context, p Principal, appID string) error {
    if err := a.checkPrincipal(ctx, p); err != nil { return err }

    if a.state.IsOwner(ctx, appID, p.UserID) {
        return nil   // R-072: the sole implication between planes
    }
    if a.state.HasDataGrant(ctx, appID, p) {
        return nil
    }
    granted, passcode := a.state.AnonymousAccess(ctx, appID)
    if granted && !passcode {
        return nil   // R-075
    }
    if granted && a.state.PasscodeUnlocked(ctx, appID, p.Passcodes[appID]) {
        return nil   // R-075a
    }
    if granted {
        return ErrPasscodeRequired   // the proxy sends a browser to the passcode page
    }
    return ErrPermission("app.use")
}
```

### 2.1 Install scope

**[D]** There are three check functions, not two, and they take different arguments on purpose:
`CheckControl(p, appID, verb)`, `CheckInstall(p, verb)`, `CheckData(p, appID)`. An install-scoped verb
(R-080) is held through a grant with no app, so there is nothing to pass as `appID` and no correct
value to invent.

```go
func (a *Authorizer) CheckInstall(ctx context.Context, p Principal, verb Verb) error {
    if !InstallScoped(verb) { return ErrInternal(verb) }   // see below
    if err := a.checkPrincipal(ctx, p); err != nil { return err }
    if err := a.policy.Allows(ctx, verb, ""); err != nil { return err }

    for _, g := range a.state.InstallGrantsFor(ctx, p) {   // user, groups, token
        if a.roles.Has(g.RoleID, verb) {
            return nil
        }
    }
    return ErrPermission(verb)
}
```

**[D]** Same seven steps, in the same order. Policy is still a floor (R-272): an install that has
disabled a verb has disabled it for administrators too.

**[D]** Each function **refuses a verb from the other's scope**, with an internal error rather than a
denial. The asymmetry is the reason. An install verb evaluated against an app looks for a grant that
cannot exist and denies — wrong but safe. An app verb evaluated install-wide looks for a grant that
*can* exist and could allow. Refusing both is what keeps the safe direction from teaching anyone that
the unsafe one is also fine.

**[D]** The scope correspondence is enforced by the schema, not by this code (design 02 §2.2):
`roles.scope`, `grants.role_scope`, a composite foreign key between them, and a CHECK tying
`role_scope` to whether `app_id` is null. So `app_id IS NULL` and "carries install verbs" cannot come
apart, and `InstallGrantsFor` cannot return a grant carrying app verbs however the row was written.

**[D]** One install-scoped grant per principal. `grants_unique_principal` is
`(app_id, plane, principal_kind, principal_id) NULLS NOT DISTINCT`, and NULL comparing equal to itself
means that index already reads "one control grant per principal, install-wide". A combination of
privileges is a custom role composed from the verb list (R-082), not two grants.

**[D]** There is no install-wide data plane. Data-plane use is per-app and binary (R-070), enforced by
`grants_data_plane_is_app_scoped`.

**[D]** `CheckData` contains exactly one cross-plane implication — ownership (R-072). No other control-plane role appears in it. A reviewer seeing another control-plane check added to this function should reject the change; that is R-029 and it was reversed once already during design, so it needs a comment saying so in the code.

**[D] Public with a passcode (R-075a) is data-plane only.** The principal carries the unlock tokens
its request brought (`pando_pass_<app>` cookies), and `CheckData` asks the store whether the one for
this app is live — unexpired, and made under the app's current anonymous grant, which a new passcode
or making the app private clears. A live lookup per request, like every other step, so there is no
cached "unlocked" to outlive a revocation. `PERM_PASSCODE_REQUIRED` is not audited as a denial: it is
every first visit to a passcode app. The owner and anyone holding their own grant never meet it.

**[P] Sharing looks people up by the app, not the directory.** Choosing whom to share with needs a
list of people and groups, and an app owner who is not an administrator cannot list accounts
(`install.view`). `GET /apps/{id}/principals?q=` answers for whoever holds `app.grants.manage` on that
app, searched and capped at twenty of each: enough to find Dana, not a way to take the directory.

**[D]** Being a Pando admin does not appear in `CheckData` at all. R-087: an admin has root and can reach a container outside Pando, but the supported path requires a grant.

---

## 3. Group resolution

**[D]** Groups are resolved live per request (R-079), never denormalized into grants.

**[P]** Cached per session with a short TTL (60s), invalidated immediately on a SCIM push (R-048). The TTL is the effective propagation delay for a group removal on adapters without push, and must be documented as such rather than implied to be instant.

**[D] As built: read live, and on the proxy's path kept until anything changes (issue #93).** The
60s per-session cache above was never built. Membership is read from `effective_group_members` with the
session, in one query. The API reads it afresh on every request. The proxy, which every request to
every app passes through, keeps what a request reads — the session's principal and groups, the app a
hostname names, and the facts `CheckData` decides on — in `proxy.Cache`, and empties it on every
replica whenever anything it may hold changes: triggers on sessions, users, group membership, grants,
unlocks and apps send a NOTIFY when the change commits (migration 68). So a SCIM push, an
administrator's change or a revoked session takes effect on the proxy's next request after the
notification arrives, which is milliseconds. A replica that is not listening keeps nothing, and no
entry lives longer than 30 seconds, which is what a notification lost in flight can cost. What is kept
is facts, never a verdict: `CheckData` still runs on every request, so every denial is still audited.

**[D] Where membership comes from (issue #51).** A group with an identity provider as its source is
**synced**: its members are set by that provider — from the groups claim at each sign-in, or, when SCIM
is on for the provider, by SCIM alone — and never by hand. `group_links` lets a provider's group count
as members of a Pando-made group. Authorization reads both through one view, so a linked group's
members hold the Pando group's grants exactly as direct members do, live (R-079), and the R-088 lockout
checks count them the same way. What any group can do is Pando's (R-078): a claim or a push can put a
person in a group, never give the group a role.

### 3.1 The revocation window

**[D]** Access does not stop the instant it is revoked, and the design contains four separate delays
that were each chosen locally and never added up:

| Source | Delay | Effect |
|---|---|---|
| Session validity check | none by default — one indexed lookup per request (§02 2.7) | 0 |
| Proxy cache (issue #93) | until the next change's NOTIFY arrives; 30s if one is lost; nothing kept while not listening | milliseconds, up to 30s |
| Assertion lifetime | 120s (R-055) | up to 120s, if the app caches it for its full life |
| Long-lived connections | re-authorized on an interval (§4.2) | up to that interval |

**The effective revocation window is the largest of these, not the smallest.** Revoking a grant while
an app holds a 120-second assertion means the app may honor that assertion for its remaining life.
Nothing in the system was computing this number, and each component's documentation implied its own
delay was the answer.

**[D] One number: 120 seconds.** Everything above is set to that or below it, and the long-lived
connection interval is set to *exactly* the assertion lifetime rather than to an independently chosen
value — two clocks measuring the same thing will drift apart the first time someone tunes one of them.
The proxy cache's 30 seconds stays under the window, so it does not widen it; if it is ever raised, it
must not be raised past 120.

**[D]** The console displays this window wherever access is revoked — removing a grant, suspending a
user, removing someone from a group — as a plain statement that access stops within two minutes.
Implying revocation is instant is the failure mode here, and §03 5's per-adapter `SessionPolicy`
display must show the effective window rather than only the adapter's own lifetime.

**[D]** The window is a floor on Pando's side, not a promise about the app. An app that caches
identity from an assertion for longer than the assertion's life has extended the window itself, which
is one more reason the app-developer documentation states that assertions are per-request and short.

**[D]** The session check is cached on the proxy's path for throughput (issue #93), and its bound is in
the table above rather than a hidden fifth delay. The API's session check is not cached.

### 3.2 Redirect sign-in (issue #51)

An external provider signs someone in through two redirects, and the second arrives at one callback
address registered with the provider — on Pando's external URL — while the person may have started on
an app's own hostname (R-172), where the session cookie must end up. So a sign-in is three steps:

```
1. GET {any hostname}/.pando/api/v1/auth/providers/{id}/start?next=/path
     core makes a flow: state = 256 random bits, return_origin = this hostname,
     the adapter's Flow (PKCE verifier + nonce, or SAML request ID) stored with it;
     browser gets pando_sso_bind = random, and the flow keeps its SHA-256
     → 302 to the provider, with state / RelayState
2. GET|POST {external URL}/api/v1/auth/providers/{id}/callback
     the flow is taken once (replays find it spent); the adapter verifies the response;
     one-time IDs recorded (SAML assertion replay); core decides the account (below);
     a one-time code is issued, its digest kept, valid two minutes
     → 303 to {return_origin}/.pando/api/v1/auth/complete?code=…
3. GET {return_origin}/.pando/api/v1/auth/complete
     the code is spent once; pando_sso_bind must hash to the flow's bind_hash;
     the request's origin must be the flow's; the session cookie is set on this hostname
     → 302 to next (a path on this host, never a URL)
```

**[D] The bind cookie is the login-CSRF defence.** Without it, someone could start a sign-in as
themselves, stop at the code, and send the link to a victim, who would then be signed in as the
attacker — and whatever they typed into an app would be the attacker's to read. The code only works in
the browser that started the flow. It is SameSite=Lax, which a top-level GET navigation carries even
after a cross-site SAML POST, and in Pando's cookie namespace, so the proxy strips it from every
request an app sees (R-173).

**[D]** `return_origin` is a hostname Pando serves — its own, or an app's (`AppHosts`) — checked when the
flow starts; a flow cannot be made to finish anywhere else. The callback never chooses where the
browser goes from anything the provider sent.

**[D] Which account.** In order: the account the identity already reaches (`user_identities`); a
suspended, deleted or aliased one is refused, never replaced. Else, if the provider allows it and the
provider vouches for the email, the one active account with that email — linked, as an alias (O-1).
Else, if the provider creates accounts and host policy does not refuse it, a new account with no
access. Else refused, in a sentence that says whom to ask and gives the ID they need. Group
memberships at that provider are then set from the claims unless SCIM owns them.

**[D] Test sign-in** runs steps 1–2 with `purpose = test`: it decides the account without creating or
linking anything, and stores a report — what the provider sent, what Pando read from it, and what a
real sign-in would have done — for the administrator who started it. It works on a provider that is
off, which is when it is needed.

---

## 4. The proxy

The single enforcement point (R-023). One path for every request to every app — authenticated, anonymous, proxy mode, per-domain mode.

```
1.  Resolve app from hostname or path prefix
2.  App exists and is running?             → 404 / 503
3.  Extract session cookie, or a bearer token shaped like Pando's (tok_…)
4.  Authenticate → Principal, or anonymous
5.  CheckData(principal, app)
      passcode required   → redirect to the passcode page (R-075a)
      denied + anonymous  → redirect to login
      denied + authed     → 403 page
5a. Record use: app.use, once per visit (R-227, §6)
6.  Mint assertion (R-051)
7.  Strip inbound X-Pando-* headers        ← critical, see below
8.  Set assertion + convenience headers
9.  Strip path prefix, set X-Forwarded-Prefix (R-167)
10. Strip every cookie in Pando's namespace, and a Pando API token (R-173)  ← credentials; see below
11. Forward to the workload
12. Stream response
```

**Step 5, "redirect to login", goes to `/.pando/login` (R-172).** Not `/login`: on an app's own
hostname every path belongs to the app, so the router hands `/login` to the proxy, which redirects to
it again — an infinite loop, and subdomain routing unusable for any app that is not public. `/.pando`
is the one path Pando answers on every hostname it serves; a slug cannot contain a dot, so no app can
claim it. The console's assets are served from there too, because a sign-in page whose HTML asks for
`/assets/app.js` on an app's hostname is asking the app for it.

**Step 10 is the cookie half of step 7.** Headers are stripped because an app that trusts
`X-Pando-User` is trusting the network boundary. Cookies are stripped because an app that *receives*
`pando_session` has to trust nothing at all — it can replay the credential against Pando's own API as
whoever visited it. Under path routing the browser sends it on every request, because the app shares
Pando's origin. A namespace prefix rather than one cookie name, so the rule covers the cookie nobody
has added yet. What an app is entitled to is the assertion from step 6: scoped to that app (R-054),
signed, and short-lived.

**[D] Nor may an app's page use Pando's credentials (issue #78).** Stripping the cookie keeps it from
the app's server. A browser still sends it with any request the app's script makes to a hostname the
cookie was set on, so two more rules, in `internal/httpapi/origin.go`:

- On an app's hostname or port, `/.pando` answers the sign-in page, its assets and the endpoints that
  page calls, listed in `signInRoutes`, and nothing else. Signing in there sets the cookie on the
  app's origin, and before this the whole API was behind `/.pando`, so the app's script could mint a
  token or open exec as its visitor. A hostname that cannot be looked up counts as an app's. On a
  port-mode listener this matters even without signing in there: browsers do not scope cookies by
  port.
- On the API, a write carried by the session cookie is refused with `PERM_CROSS_ORIGIN` when
  `Sec-Fetch-Site` says another origin, or, without it, when `Origin` is neither the request's host
  nor the external URL's. `notes.example.com` and `pando.example.com` are one site, so SameSite=Lax
  sends the cookie with a request from one to the other. A bearer token is not ambient and is let
  through; so is the SAML callback, which a provider posts from its own site and which the flow's
  bind cookie defends.

**[D] Under path routing, neither rule can help, and that is accepted (R-166).** An app at a path is
on Pando's own origin, where its script is indistinguishable from the console's to the browser and to
Pando. The console says so in the address dialog when path is chosen. No routing adapter defaults to
it.

**[D] In front of an app, only a Pando-shaped bearer is Pando's (issue #93).** Step 3 used to read
every `Authorization` header as a Pando token. An app with its own login sends its own from the
browser — a Supabase or Firebase JWT, a Basic header — so Pando failed to parse it and treated a
visitor with a good session cookie as signed out, and a private app's API calls were sent to sign in.
Now step 3 shows the authenticator `Bearer tok_<ULID>.<secret>` and nothing else, so another value
leaves the cookie to decide and is forwarded untouched. A Pando-shaped token that fails is anonymous
and does not fall back to the cookie: two credentials with one quietly winning is how a revoked token
keeps working. And step 10 removes every Pando-shaped bearer, valid or not, for R-173's reason: an
app holding one could replay it against the API. The API itself is unchanged; there, any
`Authorization` header is Pando's and one it cannot read is refused. `internal/proxy/credentials.go`.

**[D] Step 7 is a security requirement, not hygiene.** Any inbound header in Pando's namespace must be stripped unconditionally before step 8. Without it, a client sets `X-Pando-User: admin@corp.com` and an app trusting the convenience headers (R-053) is trivially spoofed. This is the single most likely serious bug in the proxy, and it needs a test asserting that a request with forged headers arrives with them replaced.

**[D]** The proxy never routes around itself. Routing adapters place traffic in front of it (§00 1.3, §03 4). There is no bypass for public apps (R-075), no bypass for performance, no bypass for websockets.

**[D] How that is made structural, discovered while building it.** Every app sits on its own private
network so no app can reach another (R-025), and no workload publishes a host port (R-026). That
leaves no address at which an app can be reached — except from inside its own network. The runtime
adapter therefore attaches *Pando's own container* to each bundle network as it creates it.

So the set of networks Pando belongs to **is** the set of apps it can reach, and nothing else is
joined to any of them. R-023 stops being a promise the proxy keeps and becomes a property of the
topology: there is no route to an app that does not pass through enforcement, because there is no
route to an app at all.

Attaching is the adapter's job rather than core's — how a workload becomes reachable is exactly the
provider vocabulary core must never learn (R-251). So is the address step 11 forwards to: the proxy
picks the primary workload's port from the spec and asks the app's runtime adapter for the rest
(`RuntimeAdapter.Upstream`, §03 2). Docker answers with the container's name, which is unique across
every network Pando is joined to where the workload's alias is not.

**[D] On several Docker hosts, a forwarding agent stands where Pando's container stood (O-45).** A
bridge network exists on one host, so a Pando on the control host cannot join the network of an app on
another. Each app host runs one Pando-owned container, `pando-agent` (Pando's binary, `pando
host-agent`), and the multi-host runtime adapter joins *it* to every app network on that host, with the
same call that joins Pando's container on one host. The agent publishes one port and accepts only
mutual TLS with a client certificate that only Pando's replicas hold. The proxy completes steps 1–10
as above, then opens a connection to the agent on the app's host through the `Dial` that
`RuntimeAdapter.Upstream` returns, and the agent carries that connection to the named container and
port and nowhere else.

The statement above then reads per host: the set of networks each host's agent belongs to is the set
of that host's apps that can be reached, and the agent forwards only for a holder of Pando's client
certificate. There is still no route to an app that does not pass through enforcement; the route now
includes a certificate check. What that adds:

- **The agent decides nothing.** Authentication, `CheckData`, assertion minting, and header and cookie
  stripping stay in the proxy, in core (R-027). The agent is a transport.
- **The client key is the key to every app.** Anyone holding it can open a connection to a workload
  without passing the proxy. As built, what is stored is the certificate authority it is issued from:
  `pando host-agent new-authority` generates it, and it is the multi-host runtime's `agent_authority`
  credential, sealed by the secrets adapter in Postgres like every adapter credential (R-190), so every
  replica has it and a database dump alone does not. Each replica issues its own client certificate
  from it in memory at start; each agent's certificate is for server authentication only and cannot
  open another agent. It is replaced by overlap: the credential may hold several authorities, the
  first issues and all are trusted. Its exposure is the secrets key's, under R-191's threat model: it
  protects against a copied disk or dump, not a compromised Pando.
- **The agent forwards only into Pando's app networks.** It carries a connection to a container name
  of Pando's form, resolved on the networks it is joined to, and only to an address inside the app
  network range on one of those networks — never the bridge address (the host), the network or
  broadcast address, or itself (`hostagent.Permitted`). Its own published port is on a network outside
  the range.
- **The agent's port is a published host port.** R-026 is about app workloads; this port reaches no app
  without the certificate, and the operator documentation says to restrict it to the control host's
  address with the host firewall.
- **Routing adapters do not change.** They still point at Pando's proxy (§00 1.3); any replica serves
  any app, and nothing in front of Pando knows which host an app is on.

Design and tests: `notes-multi-host-docker-issue-72.md`. On Kubernetes the same property is held by a
NetworkPolicy in each app namespace that admits connections only from Pando's server pods
(`notes-kubernetes-runtime-issue-72.md`).

**[P]** The cost is a private network per app, and a container runtime has a finite supply. Docker's
default pool holds about thirty, so an install past that size needs `default-address-pools` widened
before it can start another app. The adapter turns that refusal into a `CAPACITY_*` error naming the
setting, because the daemon's own message talks about subnets and tells an operator nothing.

### 4.1 Assertion minting

```go
type Assertion struct {
    Sub    string   `json:"sub"`     // users.id, stable (R-054)
    Email  string   `json:"email,omitempty"`
    Name   string   `json:"name,omitempty"`
    Groups []string `json:"groups,omitempty"`
    Aud    string   `json:"aud"`     // app ID — prevents cross-app replay
    Iat    int64    `json:"iat"`
    Exp    int64    `json:"exp"`
    Iss    string   `json:"iss"`
}
```

**[P]** Header: `X-Pando-Assertion`. Lifetime 120 seconds (R-055), minted per request.

**[D]** Anonymous requests get an assertion with `sub: "anonymous"`, a constant (R-056). Consequence, which the app-developer documentation must state explicitly: **absence of the assertion header means the request did not come through Pando**, and an app may reject on that basis.

**[D]** Convenience headers `X-Pando-User`, `X-Pando-Email`, `X-Pando-Groups` are sent alongside (R-053) and documented as **unverified**. An app trusting them is trusting the network boundary, which is legitimate but must be a choice the developer knows they are making.

**[P]** Keys: Ed25519, published at `/.well-known/jwks.json` with key IDs, rotated on overlap (R-057). Rotation generates a new key, publishes both, signs with the new one after a propagation window, retires the old.

### 4.2 Streaming

**[D]** R-170: websockets, SSE, and large uploads must work. Concretely — no response buffering, `Flush()` on every write for SSE, hijack for websocket upgrade, and no default body size limit.

**[D] Resolved (O-13).** A long-lived connection is re-authorized on a timer and closed when
authorization fails. The interval is the assertion lifetime — 120s, the same number as §3.1 — because
a long-lived connection is the one case where the per-request check that normally enforces `CheckData`
never fires again. Accepting the alternative (leave it open, document the gap) would have made a
websocket the one way to hold access indefinitely after revocation, which is precisely the property an
attacker would look for.

Re-authorization runs the same `CheckData` as a fresh request, on credentials authenticated again
from the request that opened the connection — its session cookie or bearer token, and any passcode
unlock — not on the principal they resolved to then. That principal is a snapshot of who the user
was at connect time; reusing it meant a session revoked, a token revoked, or a user suspended
(R-048, R-049) left an open websocket open. The loop also ends when the connection closes, rather
than running for the life of the process. On failure the connection closes with a
normal WebSocket close frame carrying a policy-violation status, not an abrupt reset, so a client can
tell revocation from a network fault.

---

## 5. Verb catalog

```go
var Verbs = []Verb{
    // Install-scoped: held through a grant with no app (§2.1).
    "install.view", "install.users.manage", "install.policy.manage",
    "install.adapters.manage", "install.audit.read", "install.backup.manage",
    "install.tokens.manage", "install.deploys.approve", "install.upgrade",
    "install.events.manage", "app.create",

    // Install-scoped: each one app verb on every app (issue #81). See below.
    "install.apps.view", "install.apps.logs.read", "install.apps.deploy",
    "install.apps.restart", "install.apps.spec.edit", "install.apps.secrets.write",
    "install.apps.secrets.read", "install.apps.exec", "install.apps.grants.manage",
    "install.apps.routing.override", "install.apps.resources.override",
    "install.apps.egress.tighten", "install.apps.egress.loosen", "install.apps.delete",

    // App-scoped.
    "app.view", "app.logs.read", "app.deploy", "app.restart",
    "app.spec.edit", "app.secrets.write", "app.secrets.read",
    "app.exec", "app.grants.manage", "app.routing.override",
    "app.resources.override", "app.egress.tighten", "app.egress.loosen",
    "app.deploy.approve", "app.delete",
}
```

**[D]** `app.create` is install-scoped despite its prefix. There is no app yet when it is checked —
Sequence A step 1 has always called it install-level — and renaming it to `install.apps.create` would
churn the string in the seeded role for nothing. `InstallScoped(verb)` is the predicate; the prefix is
a naming convention, not the rule.

**[D]** The install verbs gate six endpoints that were previously gated by authentication alone:
`POST /users`, `PATCH /users/{id}`, `GET /users/{id}`, `POST /apps`, `GET /capacity`,
`GET /adapters`. The two `/users/{id}` routes are **self or verb**: your own account is self-service,
anyone else's needs `install.users.manage` (write) or `install.view` (read). Without the first half an
install with one administrator could not let anyone manage their own account; without the second, any
signed-in account could suspend the administrator — which it could, until O-17 was resolved.

**[D]** The two `*.override` verbs, `app.routing.override` and `app.resources.override`, each permit
deviating from a default the host operator chose, which is why neither is in Operator and both sit
with Owner.

**[D] Egress is split into tighten and loosen, by what a change does to the install's rules** (R-182 –
R-184; amended by issue #79, O-25). There used to be a third override, `app.egress.override`, because an
app's allowlist **replaced** the install's and so defining one was an escalation. An app's own list now
sits on top of the install's and can only narrow, so most egress changes cannot escalate, and gating
them like one kept Operators from locking an app down. The split follows the risk:

- `app.egress.tighten` — any change that stays within the install's rules: the app's own list, adding
  to a denylist, removing from an allowlist, turning private-range blocking on. Always within policy
  (R-272), so Owner **and Operator** hold it.
- `app.egress.loosen` — `app.egress.override` renamed by migration 000039 in every role and policy list
  that named it. Needed for a loosening (`allowlist_add`, `denylist_remove`, `block_private_off`) when
  host policy's `egress_loosening` is `verb`. Holding it covers every egress change, tightening
  included. Owner only.

The spec save asks: did egress change at all → `tighten` **or** `loosen`. Is there a loosening the
running spec does not already carry (`policy.NewLoosenings`)? Then `forbidden` refuses it
(`PLAN_EGRESS_LOOSENING_FORBIDDEN`), `verb` needs `app.egress.loosen`, and `approval` lets anyone who may
change egress propose it, because the deploy will wait for an approver (R-154). Policy, not the verb, is
what makes the install's rules a floor: with loosening `forbidden`, no grant gets an app past them.

**[D] Deploy approval has two verbs** (R-155). `install.deploys.approve` approves or rejects any
deploy on any app; Administrator holds it. `app.deploy.approve` does the same for one app's deploys and
is in **no** built-in role — signing off is a trust an installation hands to people it names, through a
custom role (R-082). Owner does not get it, because an owner approving their own app's deploys is the
case approval was turned on to prevent. Either verb may approve its holder's own request: Pando does
not enforce two people, an install that wants two keeps the verb from the people who deploy. By default
both are in `agent_disabled_verbs` (design 04 §3): approval is a human sign-off.

**[D] Every app verb has one install-wide counterpart** (issue #81, R-080). `install.apps.deploy` is
`app.deploy` on every app, including apps created after the grant; `install.apps.secrets.read` is
`app.secrets.read` on every app; and so on, one for one, with `install.deploys.approve` as
`app.deploy.approve`'s (it is older than the rest and kept its name). So a security group can see every
app, read its logs and its secrets, and change nothing, with one grant and no list to keep current.
This is an implication, the one this design allows between verbs, and it lives in exactly one place —
`CheckControl`, step 6b, reading the table in `authz.everyApp` — after the app's own grants and after
host policy. Policy is asked about the *app* verb, so a verb it disables stays disabled for whoever
holds it install-wide, administrators included (R-272). `CheckData` does not read it: managing an app
is not using it, and R-087's line holds on the data plane — the supported path to *use* an app is a
data grant or ownership.

Approach A of the issue, chosen over an "every app" grant target: the counterparts are install verbs
in install roles, so the R-080 mechanism (`grants (role_id, role_scope) → roles (id, scope)` and its
CHECKs) is unchanged, at the cost of fourteen more verbs in the catalog. Every app verb gets one,
`app.secrets.read`, `app.exec` and `app.grants.manage` included, with no warning on assigning them: an
installation that hands out exec everywhere has decided to, and R-086 already says what exec means.

`everyApp` is a table, not a rule: one install verb to one app verb, never a bundle. A combination is a
role. The two bundles that came before, `install.apps.view` (Viewer's two verbs on every app) and
`install.apps.manage` (Owner's on every app), were replaced by migration 000040 with built-in roles:
**App viewer** and **App manager**. `install.apps.view` remains, meaning `app.view` alone; every role
that held it gained `install.apps.logs.read`, and every role holding `install.apps.manage` holds its
thirteen verbs instead. A third, **Auditor**, holds `install.audit.read` with App viewer's two — the
issue's security reviewer — and not `install.view`, because the accounts and adapters are not what it
audits. A new app verb has no counterpart until one is written into the table, and
`TestR080_EveryAppVerbHasOneInstallCounterpart` fails until it is.

**[D]** Administrator holds every install verb and no app verb; Owner holds every app verb but
`app.deploy.approve`. Through the counterparts, an administrator holds every app verb on every app,
approval included.

**[D]** App manager leaves out `install.deploys.approve` as Owner leaves out `app.deploy.approve`
(R-155). Managing every app is not a mandate to sign off on every deploy; an installation hands that
verb out on purpose, through Administrator or a custom role.

**[D]** Of the counterparts, only `install.apps.view` opens up the app list. `GET /apps` shows
every app to a principal holding it, and otherwise the apps they hold a grant on: holding
`install.apps.restart` alone restarts any app whose ID you have and does not reveal the others, because
there is no implication graph, not even to `app.view`.
The approval service accepts either: `install.deploys.approve` install-wide, or `app.deploy.approve` on
the app. It asks the install verb first with `Authorizer.AllowsInstall`, which answers without auditing
a denial, so an approver holding only the app verb does not leave a spurious refusal in the log; only a
caller holding neither is audited as denied.

**[D]** What the caller may do on an app is on the wire: `GET /apps/{id}` returns `verbs`, computed by
`Authorizer.AppVerbs`, which asks `CheckControl`'s own question for each app verb without auditing a
denial. The console shows a control it cannot use as read-only instead of letting it fail, and it
cannot drift from the API because it is the API's answer. The rules are `CheckControl`'s, verb by verb, but one
call reads the principal's status, the policy document, the grants (each with its role, joined in
the same query) and the install grants once rather than once per verb; nothing it read outlives the
call (R-274). `GET /users/{id}/apps` returns
`can_manage` per app the same way, and lists only apps the caller could see.

**[D]** Creator holds `app.create` and nothing else. It is the built-in answer to "may make and run
their own apps, and touch no other setting": creating an app writes the creator an owner grant on it
(R-073), so what a Creator can manage is exactly what they made, through the ordinary grant path. No
verb in the role mentions apps they did not make, and none reaches users, policy, adapters, backups or
the audit log. In the console, a Creator's admin sidebar has Apps and the API screen everyone gets,
and nothing else, because the sidebar is drawn per verb.

**[D] `install.events.manage` subscribes install-wide and manages anybody's subscriptions** (R-368,
issue #50). Administrator holds it. It is not an `everyApp` counterpart — there is no app verb it stands
for: an app subscription needs `app.view`, which counts its install counterpart like any `CheckControl`
question, while an install-wide subscription also hears sign-ins, policy changes and adapter health, which
seeing every app does not cover. A delivery is authorized as the subscription's owner when it is sent, with
`Allows` / `AllowsInstall`, so it audits nothing and a revoked grant stops the next delivery. Design 11 §4.

**[D]** Built-in roles (R-081) are seeded by migration and trigger-protected. When a new verb is introduced in a later Pando version, a migration adds it to the appropriate built-in roles. That is the upgrade mechanism R-081 promises, and it is the only sanctioned way built-in role contents change.

**[D]** Deleting a custom role deletes every grant of it in the same transaction, and deleting a group
deletes every grant to it; whoever held them loses that access and keeps anything held another way.
Both are refused when they would leave no installation-wide grant holding `install.users.manage`
(R-088) — the same lockout as revoking that grant directly, reached through a different door. The
console confirms each with its own wording, naming whose access goes and whose stays.

**[D]** Custom roles (R-082) are arbitrary subsets. There is no verb implication graph — holding `app.delete` does not imply `app.view`. Implication graphs are where authorization bugs live; the console can suggest sensible combinations instead.

---

## 6. Audit integration

**[D]** Every authorization **denial** is audited, not only successes. A denial pattern is the signal that matters for detecting misuse, and it is the thing most commonly left out.

**[D] Except, if host policy says so, an anonymous one on the data plane** (R-227, issue #72). A
visitor who is not signed in reaching an app not shared with them is refused in `CheckData` and
audited as `authz.denied`, one row per request — and anyone can make those requests, without an
account. Host policy's `disable_anonymous_denial_audit` (default off, so they are recorded) stops
writing them; `PANDO_POLICY_DISABLE_ANONYMOUS_DENIAL_AUDIT` sets it at startup like any other policy
field. A denial of a signed-in person or a token is always written, and so is every control-plane and
install denial. The setting is read per request, like all policy (R-274).

**[D] Reaching an app through an install grant is audited** (issue #81). When `CheckControl` allows
an app verb through an install-wide counterpart rather than a grant on the app, it writes
`authz.install_wide`: the app, the verb, the install verb that stood for it, and the grant, role and
holder that carried it. So the log says the access came from everywhere and not from this app.
`AppVerbs`, `Allows` and `PreviewControl` decide what to show and write nothing, like denials.

**[P]** `app.view` and `app.logs.read` are not recorded this way. The console asks `app.view` on every
app screen and `app.logs.read` every few seconds while logs are open; a row for each would bury the
changes the record is for, and neither read is audited for anyone holding it on the app either.

**[D] Use is audited, once per visit (R-227, O-22).** An allowed request writes `app.use` the first time
its visit is seen: a browser's visit is a cookie in Pando's namespace (`pando_visit_<app>`, set on the
app's response and stripped from every request before the app sees it, like every Pando cookie); a
token's visit is the token, for twelve hours. Recorded after `CheckData` allows and before the request
is forwarded. Anonymous visitors are recorded with the address Pando saw unless host policy's
`disable_anonymous_use_audit` is set, and are capped per app per minute so a client that keeps no
cookies cannot write a row per request. A write that fails is logged and does not refuse the use.
Visits are remembered in memory, so a restart may record a visit twice and never records one too few.

**[D]** Audit is written before the privileged action, not after (§04 2.6). An exec session that fails to open is still recorded as attempted.

**[D]** Database-level enforcement: the application role has `INSERT` on `audit_events` and no `UPDATE` or `DELETE`. R-027 says no adapter can rewrite the audit log; this makes it true of core as well, which is stronger and costs nothing.

**[D] Retention does not loosen this (R-347, issue #60).** The log is bounded by removing whole
months past retention, and the application role still cannot remove anything: it holds no `UPDATE`,
`DELETE` or `TRUNCATE` on the table or on any monthly partition of it, cannot execute the functions
that make and drop a month, and cannot write the record of archives those functions trust. Removal is
done by a separate role, `pando_audit_archiver`, through one owner-defined function that refuses a
month younger than three months (R-348) or without a recorded, verified archive of every row it holds,
and that records the removal in the log itself. So "the running server cannot rewrite history" now
reads: it cannot change any event, cannot remove one younger than the floor, and cannot remove an
older one without an archive holding it. Startup verifies all of this, as it verifies ownership.
Design 02 §2.6.

## 7. Metrics (issue #126)

**[D] Pando's metrics are about Pando, and never name a person, a token or an app** (R-399, R-400).
Whoever runs the collector Pando pushes to is outside every grant in this document: they hold no
verb and pass no check, so what reaches them must be something any operator of the host may see.
How busy Pando is qualifies. Who uses which app does not, and per-app traffic is not part of this
either — an app's numbers are about the app, and reading them would need `app.view`, which a push to
a collector has no way to ask.

So the proxy counts every request after `CheckData` (`pando.proxy.requests`), allowed or denied, with
the kind of principal — `user`, `token`, `anonymous` — and nothing else: not the app, not the
principal, not the path. The API's request metric carries the route pattern, never the path, which
holds IDs. Requests the router hands to the proxy are left out of it, so app traffic is counted once
and without its app.

The mechanism is the shape of `internal/telemetry`: every recording takes typed, bounded values and
there is no free-form attribute parameter, so a caller has nowhere to put an ID.
`TestR400_NoMetricNamesAPersonTokenOrApp` checks every attribute key against the allowed list and
every value against the shape of an ID. Exporting per-app metrics would need its own requirement
first, decided against this document.
