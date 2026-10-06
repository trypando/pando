# 02 — Data Model

PostgreSQL. `sqlc` for typed queries, `golang-migrate` for versioning. No ORM.

R-020 makes this store the sole record of how every app runs, which sets the bar: every mutation is audited, every object is exportable, and nothing important lives only in memory.

---

## 1. Conventions

- IDs are `text`, prefixed ULIDs (§00 3.1). Not `uuid` — the prefix is load-bearing for readability.
- Timestamps are `timestamptz`, always UTC.
- Soft delete only where an object must survive its own deletion for audit purposes. Apps and users soft-delete; grants and sessions hard-delete.
- Every table with a mutable row carries `created_at`, `updated_at`.
- Specs are append-only. There is no `UPDATE` on `spec_revisions`.

---

## 2. Schema

### 2.1 Principals

```sql
CREATE TABLE identity_adapters (
    id            text PRIMARY KEY,          -- idp_...
    kind          text NOT NULL,             -- local | oidc | saml | github
    name          text NOT NULL,             -- unique, ignoring case: the sign-in page's button
    config        jsonb NOT NULL DEFAULT '{}',   -- never a secret: CHECK refuses credentials/client_secret/scim_token
    enabled       boolean NOT NULL DEFAULT true,
    jit_provisioning boolean NOT NULL DEFAULT false,  -- make an account at a first sign-in
    link_by_email    boolean NOT NULL DEFAULT false,  -- link a first sign-in by verified email (O-1)
    scim_token_hash  text,                   -- SHA-256 of the SCIM bearer token; NULL = SCIM off
    scim_token_created_at timestamptz,
    scim_identity_attribute text,            -- externalId | userName; NULL = the kind's default
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity_adapter_credentials (   -- an OIDC client secret, sealed (R-190)
    adapter_id    text NOT NULL REFERENCES identity_adapters(id) ON DELETE CASCADE,
    field         text NOT NULL,
    adapter_ref   text NOT NULL,             -- the secrets adapter that sealed it
    ciphertext    bytea,
    external_ref  text,
    PRIMARY KEY (adapter_id, field)
);

CREATE TABLE users (
    id            text PRIMARY KEY,          -- usr_...
    adapter_id    text NOT NULL REFERENCES identity_adapters(id),   -- the originating adapter (R-045)
    external_id   text NOT NULL,             -- subject as the adapter knows them
    email         text,
    display_name  text,
    status        text NOT NULL,             -- active | suspended | deleted  (R-049)
    suspended_by  text,                      -- admin | scim:<idp> | alias
    alias_of      text REFERENCES users(id), -- identities moved to another account (O-1)
    password_hash text,                      -- local adapter only, argon2id
    must_change_password boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    UNIQUE (adapter_id, external_id),
    CHECK (alias_of IS NULL OR status <> 'active')
);

CREATE TABLE user_identities (
    adapter_id    text NOT NULL REFERENCES identity_adapters(id),
    external_id   text NOT NULL,             -- the provider's stable ID for the person
    user_id       text NOT NULL REFERENCES users(id),
    scim_user_name text, scim_external_id text, scim_resource jsonb,  -- what a SCIM client said
    last_sign_in_at timestamptz,
    created_by    text NOT NULL,
    PRIMARY KEY (adapter_id, external_id)    -- one identity reaches one account
);
```

**[D]** `users.id` is what goes in the assertion `sub` claim (R-054). It is stable across email change and independent of the adapter's own identifiers. This is the single most important stability guarantee in the schema — apps key their data on it.

**[D]** `status` is three-valued because suspended is not deleted (R-049, R-282). Destruction rules (R-280) fire on `deleted`, never on `suspended`.

**[D]** `suspended_by` says who suspended an account, because a SCIM client that re-sends
`active: true` on every cycle — Entra does, every forty minutes — must lift only a suspension it made,
never one an administrator made.

**[D] Resolved (O-1): linking aliases, it never merges.** Shipped with issue #51. Every identity an
external provider vouches for — the one an account came from and any linked to it — is a row in
`user_identities`, and an external sign-in resolves through that table alone; its primary key is what
makes one identity reach exactly one account. `users.adapter_id`/`external_id` stay as the record of
where the account came from (R-045). Local accounts are not listed there: their identity is their
username, which a person may change, and the local adapter resolves it as it always has.

Linking a second identity to a user attaches an alias. It does **not** merge two `users` rows, and
`users.id` never changes and is never retired. Merging is the obvious implementation and it breaks
R-054: `users.id` is the assertion `sub` claim, apps key their data on it, and Pando has no way to
reach into an app and rewrite the rows it stored under the losing ID. A merge would silently orphan a
person's data inside every app they had ever used.

So: an administrator linking `alice@corp` (OIDC) to an existing local `alice` moves the identity row to
her account, and from then on that identity authenticates *to* it and assertions carry her ID. If the
identity already reached another account (a just-in-time account, say) and that was its only way in,
that account becomes an alias — `alias_of` set, suspended, sessions ended — and is never deleted: a
deletion would free its `external_id` for reuse by a different human, and apps may hold data under its
ID. Linking by email is per provider, off by default, and only on an email the provider vouches for.

```sql
CREATE TABLE groups (
    id            text PRIMARY KEY,          -- grp_...
    adapter_id    text REFERENCES identity_adapters(id),  -- NULL = Pando-native
    external_id   text,
    name          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (adapter_id, external_id)
);

CREATE TABLE group_members (
    group_id      text NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id       text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE group_links (                 -- a provider's group feeding a Pando group (R-078)
    synced_group_id text NOT NULL REFERENCES groups(id) ON DELETE CASCADE,  -- adapter_id NOT NULL
    group_id        text NOT NULL REFERENCES groups(id) ON DELETE CASCADE,  -- adapter_id IS NULL
    PRIMARY KEY (synced_group_id, group_id)
);

CREATE VIEW effective_group_members AS     -- what authorization reads
    SELECT group_id, user_id FROM group_members
    UNION
    SELECT l.group_id, m.user_id FROM group_links l JOIN group_members m ON m.group_id = l.synced_group_id;
```

**[D]** Membership is read live at authorization time (R-079). It is never denormalized into grants.

**[D]** A group with an `adapter_id` is **synced**: the provider owns its membership, through the
groups claim of each sign-in or through SCIM (when SCIM is on for the provider, it alone). Pando owns
what the group can do. `group_links` lets a provider's group stand in for a Pando-made one: its
members count as that group's members, through the `effective_group_members` view, which every query
asking "which groups is this person in" reads — the authorizer, the apps lists and the R-088 lockout
checks — so linking cannot give a different answer in one place than another. A trigger holds the
direction: synced to Pando-made, never the reverse.

```sql
CREATE TABLE tokens (
    id            text PRIMARY KEY,          -- tok_...
    kind          text NOT NULL,             -- delegated | account   (R-058, R-060)
    name          text NOT NULL,
    hash          text NOT NULL,             -- argon2id of the secret; secret shown once (R-063)
    owner_user_id text REFERENCES users(id), -- delegated: required. account: NULL (R-060)
    created_by    text NOT NULL,             -- principal that minted it
    expires_at    timestamptz,               -- NULL = never; policy may forbid (R-061)
    last_used_at  timestamptz,               -- R-062
    revoked_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON tokens (owner_user_id) WHERE revoked_at IS NULL;
```

**[D]** A delegated token has no grants of its own. Authorization resolves through `owner_user_id` live (R-059), so an owner's revocation is the token's revocation with no cascade to write.

**[D]** An account token is its own principal and therefore appears directly in `grants.principal_id` (R-060).

### 2.2 Roles and grants

```sql
CREATE TABLE roles (
    id            text PRIMARY KEY,          -- role_...
    name          text NOT NULL UNIQUE,
    builtin       boolean NOT NULL DEFAULT false,   -- R-081, immutable
    scope         text NOT NULL DEFAULT 'app',      -- app | install  (R-080)
    verbs         text[] NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, scope)                              -- for the FK below
);
```

**[D]** Built-in rows (`viewer`, `operator`, `owner`, `administrator`, `creator`, `app viewer`, `app manager`, `auditor`) are seeded by migration and protected by a trigger against `UPDATE`/`DELETE` (R-081). New verbs added in a later Pando version are added to built-in roles **by migration**, which is the mechanism R-081 promises.

**[D]** Migration 000039 is the worked example (issues #79, #39). It renames `app.egress.override` to
`app.egress.loosen` in **every** role that held it, custom roles included, and in host policy's
`disabled_verbs` and `agent_disabled_verbs` — the meaning carried over, and silently dropping a grant
would take away something an administrator gave on purpose. It adds `app.egress.tighten` to Owner and
Operator and `install.deploys.approve` to Administrator. `app.deploy.approve` joins the catalog in no
built-in role (R-155). The trigger is disabled for the length of the migration and re-enabled in it.
Migration 000042 does the same for `install.upgrade` (R-356), the Administrator's alone, and adds it to
a stored policy's `agent_disabled_verbs` as `policy.Default()` does. Migration 000043 adds
`install.events.manage` (R-368) to Administrator alone; agents may hold it.

**[D]** A role is scoped. A role carrying install verbs granted on a single app is nonsense, and a role carrying app verbs granted install-wide is worse. `administrator` is the only install-scoped built-in; custom roles (R-082) are composed within one scope.

**[D]** The `UNIQUE (id, scope)` index is redundant as a uniqueness constraint — `id` is already the primary key — and exists solely so `grants` can reference the pair. It is the cheapest way to make the correspondence a foreign key instead of a convention.

```sql
CREATE TABLE grants (
    id            text PRIMARY KEY,          -- gr_...
    app_id        text REFERENCES apps(id) ON DELETE CASCADE,  -- NULL = install-scoped
    plane         text NOT NULL,             -- control | data   (R-070, R-071)
    role_scope    text NOT NULL DEFAULT 'app',                 -- app | install
    principal_kind text NOT NULL,            -- user | group | token | anonymous
    principal_id  text,                      -- NULL when kind = anonymous (R-074)
    role_id       text REFERENCES roles(id), -- control plane only
    created_by    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_id, plane, principal_kind, principal_id),      -- NULLS NOT DISTINCT

    FOREIGN KEY (role_id, role_scope) REFERENCES roles (id, scope),
    CHECK ((role_scope = 'install' AND app_id IS NULL) OR
           (role_scope = 'app'     AND app_id IS NOT NULL)),
    CHECK (plane = 'control' OR app_id IS NOT NULL)
);
```

**[D]** A grant with **no app** is install-scoped (R-080, O-17). An administrator is a principal holding a grant, the same as everyone else; the grant simply has no app. There is no admin flag on a user, so the power is revocable and grantable like any other.

**[D]** `app_id NOT NULL` used to be what kept an app-scoped grant from becoming global. Its replacement is the composite foreign key plus the first CHECK: a role carries its scope, a grant carries the scope it was made at, and the two must agree. So "no app" and "carries install verbs" cannot come apart, whatever the application does.

**[D]** The second CHECK says a data grant always names an app. Data-plane use is per-app and binary (R-070) — there is no install-wide "use" — and that was previously implied by the NOT NULL.

**[D]** The unique index is `NULLS NOT DISTINCT`, which already existed so the anonymous grant (whose `principal_id` is NULL) could not be inserted twice. With a NULL `app_id` it does a second job for free: NULL compares equal to itself, so the index reads "one control grant per principal, install-wide". A combination of privileges is a custom role composed from the verb list (R-082), not two grants.

**[D]** Data-plane grants have no role — use is binary (R-070).

**[D]** App creation writes **two rows**, one per plane (R-073). They are independently revocable.

**[D]** `principal_kind = 'anonymous'` is a real row, not a flag on the app (R-075).

### 2.3 Apps and specs

```sql
CREATE TABLE apps (
    id             text PRIMARY KEY,          -- app_...
    name           text NOT NULL,
    slug           text NOT NULL UNIQUE,      -- used in routing
    owner_user_id  text REFERENCES users(id), -- R-031
    state          text NOT NULL,             -- see 05-reconciler
    pinned_spec_id text REFERENCES spec_revisions(id),
    desired_state  text NOT NULL,             -- running | stopped
    unobservable_since timestamptz,           -- adapter unreachable; NOT an app state
    applied_env_fingerprint text,             -- see 2.4, secret rotation
    address_hostname text,                    -- the pinned spec's hostname, unique among live apps
    address_path     text,                    -- the pinned spec's path, unique among live apps (design 03 §4.1)
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);

CREATE TABLE spec_revisions (
    id           text PRIMARY KEY,            -- spec_...
    app_id       text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    revision     integer NOT NULL,
    origin       text NOT NULL,               -- detected | edited | redetected | imported | manual
    body         jsonb NOT NULL,              -- the AppSpec (01-spec-schema)
    created_by   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_id, revision)
);
CREATE INDEX ON spec_revisions (app_id, revision DESC);
```

**[D]** Append-only. Enforced by a trigger rejecting `UPDATE` and `DELETE`, so R-152's rollback is always to something that provably existed.

**[D]** `unobservable_since` is a third field alongside `state` and `desired_state`, for the same
reason those two are separate: it answers a different question. `state` is what is true of the app;
`desired_state` is what a human asked for; `unobservable_since` is whether Pando currently knows
either. An adapter being unreachable is a platform problem, not an app state (§05 2), and folding it
into `state` would mean either lying — reporting `running` for an app nobody can see — or inventing an
`unknown` state that every consumer of the state machine then has to handle. The console renders it as
a banner over the app's last known state, not as a replacement for it.

**[P]** Pruning past `Retention.SpecRevisions` is a background job that deletes only revisions never pinned. A revision that was ever live is kept.

```sql
CREATE TABLE deployments (
    id           text PRIMARY KEY,            -- dep_...
    app_id       text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    spec_id      text NOT NULL REFERENCES spec_revisions(id),
    trigger      text NOT NULL,               -- manual | branch_updated | release_tagged | rollback
    status       text NOT NULL,               -- awaiting_approval|pending|building|applying|
                                              -- succeeded|failed|superseded|rejected|expired (CHECK)
    error_code   text,
    error_detail jsonb,
    egress_rules jsonb,                       -- egress.Rules the deploy ran with (R-183, O-10)
    approvals_required  integer,              -- R-156; fixed when the request is made
    approval_expires_at timestamptz,          -- NULL: waits until answered
    approval_reasons    text[],               -- install | app_policy | app_spec | egress_loosening
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    created_by   text NOT NULL
);
CREATE INDEX deployments_awaiting_idx ON deployments (approval_expires_at)
    WHERE status = 'awaiting_approval';

CREATE TABLE deployment_approvals (
    deployment_id text NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    principal_id  text NOT NULL,
    decision      text NOT NULL CHECK (decision IN ('approve', 'reject')),
    comment       text,
    decided_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (deployment_id, principal_id)
);
```

**[D]** `egress_rules` is the merged rules (`policy.EffectiveEgress.Rules`) resolved when the deploy
ran, and the reconciler restores **these**, not a fresh resolution against today's policy. A policy
edit takes effect at an app's next deploy and never changes a running app underneath it (R-183, O-10).
NULL is a deployment from before egress was enforced, which ran unrestricted.

**[D]** A deploy that needs approval is a deployment row that waits, not a separate request object
(R-156). It is tied to one `spec_id` and so one commit, listed with the app's other deploys, and moves
to `pending` — the ordinary path — once it has `approvals_required` approvals. The count and the expiry
are copied from host policy when the request is made, so a policy edit does not move a waiting
request's goalposts. `approval_reasons` records why it waited, for the approver and the audit log.

**[D]** One `deployment_approvals` row per principal per request: approving twice does not count twice.
A single `reject` row ends the request (R-156). The rows are what the audit log's `deploy.approve` and
`deploy.reject` events describe (R-159); the audit log remains the record (R-027).

### 2.4 Volumes, secrets, services

```sql
CREATE TABLE volumes (
    id          text NOT NULL,                -- the spec's volume ID: unique within its app
    app_id      text NOT NULL REFERENCES apps(id) ON DELETE RESTRICT,
    name        text NOT NULL,
    adapter_ref text NOT NULL,
    handle      text,                         -- adapter's own identifier
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, id),
    UNIQUE (app_id, name)
);
```

**[D]** Keyed by app and ID together. The ID is the spec's volume ID, and a spec names volumes after
what they hold — two apps that each keep one called `data` are ordinary. Keyed by ID alone, the second
app's deploy rewrote the first app's row and recorded nothing for its own, so it was never backed up
and lost R-203's protection (issue #87, migration 000036). The reconciler records what the runtime
reports holding for an app when Pando's rows disagree, so rows a deploy could not write come back
without a redeploy. A whole-installation bundle names each volume `volumes/<app>/<volume>.tar` for the
same reason.

**[D]** `ON DELETE RESTRICT`, deliberately. An app cannot be deleted out from under its volumes; the delete flow must resolve them explicitly through the keep-or-discard prompt (R-204). Once it has — discarded, or backed up first — the rows go and `apps.discard_storage` is set, and the GC's teardown destroys the volumes with the bundle. Nothing else sets it, so an app whose storage no delete settled keeps its volumes.

```sql
CREATE TABLE secrets (
    id          text PRIMARY KEY,             -- sec_...
    app_id      text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key         text NOT NULL,
    adapter_ref text NOT NULL,                -- which secrets adapter holds it
    ciphertext  bytea,                        -- local adapter only
    external_ref text,                        -- external adapter: a pointer, not a value
    version     integer NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_id, key)
);
```

**[D]** No plaintext column exists anywhere. The local adapter stores ciphertext; external adapters store only a reference (R-190, R-191).

**[D]** `version` increments on rotation so the reconciler can detect that a restart is required
(R-193). **The detection is state-side, not observed.** `Observe` returns no environment — see
§03 2.2 — so there is no way to see that a running workload holds a stale secret by looking at it. The
reconciler instead compares `apps.applied_env_fingerprint`, written at apply time, against the
fingerprint of the currently-resolved environment. A mismatch is reconcilable drift and the workload is
recreated.

**[D]** The fingerprint is a hash over `(key, version)` pairs and literal env values — **never over
secret values.** It has to be comparable without decrypting anything and must not become a place a
secret can leak into (R-194). Hashing the resolved values would put a verifier for every secret in the
state store, which is a worse position than not having the feature.

```sql
CREATE TABLE provisioned_services (
    id          text PRIMARY KEY,             -- svc_...
    app_id      text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    slot_key    text NOT NULL,
    type        text NOT NULL,                -- postgres | redis | ...
    adapter_ref text NOT NULL,
    handle      text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
```

**[D]** Scoped to one app (R-134). No sharing — sharing is expressed as two apps binding to one external target.

```sql
CREATE TABLE registry_credentials (
    app_id       text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    field        text NOT NULL,               -- kind | username | password | access_key_id | ...
    adapter_ref  text NOT NULL,
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, field)
);
```

**[D]** The credential an image app's private image is pulled with (issue #41, O-30). App-owned as O-3
decided for source credentials, with the supplier in the `app.registry_credential.write` audit event
rather than here. **Not a row in `secrets`**: an app secret can be named by an env entry and so reach the
app, and a credential that fetches the app's image is never the app's to read. Sealed by the secrets
adapter under the scope `registry:`, which no app ID can collide with, so a ciphertext cannot be
replayed as an app secret (the same arrangement as `adapter_credentials`, §2.5). Apps soft-delete, so the
cascade does not fire on deletion; the GC's teardown removes the rows.

### 2.5 Policy and adapters

```sql
CREATE TABLE adapter_configs (
    id          text PRIMARY KEY,             -- rt_..., rte_..., bld_..., sec_..., ntf_...
    category    text NOT NULL,                -- runtime|routing|builder|secrets|services|identity|notify|backup|ai
    kind        text NOT NULL,                -- docker | traefik | buildkit | local | ...
    name        text NOT NULL,
    config      jsonb NOT NULL DEFAULT '{}',
    is_default  boolean NOT NULL DEFAULT false,
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON adapter_configs (category) WHERE is_default;
-- config is never a credential store (O-20).
ALTER TABLE adapter_configs ADD CHECK (NOT (config ? 'credentials'));

-- An adapter's credentials, sealed by the secrets adapter (R-190, O-20). The same
-- shape as `secrets`, one scope up: no plaintext column.
CREATE TABLE adapter_credentials (
    adapter_id   text NOT NULL REFERENCES adapter_configs(id) ON DELETE CASCADE,
    field        text NOT NULL,               -- api_key
    adapter_ref  text NOT NULL,               -- the secrets adapter that sealed it
    ciphertext   bytea,
    external_ref text,
    version      integer NOT NULL DEFAULT 1,
    PRIMARY KEY (adapter_id, field)
);

-- One AI adapter per provider (R-259).
CREATE UNIQUE INDEX adapter_configs_one_ai_adapter_per_kind
    ON adapter_configs (kind) WHERE category = 'ai';

-- Which AI adapter performs each AI function (R-259). The primary key is the
-- rule: a function has at most one adapter; an adapter may hold any number.
-- No FK to adapter_configs: an adapter declared in the config file has no row.
CREATE TABLE ai_assignments (
    function    text PRIMARY KEY CHECK (function ~ '^[a-z][a-z_]*$'),  -- repair_plan, search_audit, ...
    adapter_id  text NOT NULL CHECK (adapter_id <> ''),
    model       text NOT NULL DEFAULT '',     -- empty: the adapter's own model
    updated_by  text NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE host_policy (
    id          integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),  -- singleton, R-015
    body        jsonb NOT NULL,
    updated_by  text NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
```

**[D]** Singleton by constraint. One install, one org (R-015) — encode it so nobody accidentally builds multi-tenancy in.

**[D]** Policy is a single versioned document, not scattered columns, so R-274's "apply policy to a running install" is one transaction and one audit event.

**[D]** Egress and deploy approval are `body` fields (`policy.Document`):

| Field | Meaning |
|---|---|
| `egress_mode` | `allow_all` (unset) \| `denylist` \| `allowlist` (R-181) |
| `egress_list` | the list `egress_mode` reads (R-185) |
| `egress_block_private` | block private ranges; works with any mode, allow-all included |
| `egress_loosening` | `verb` (unset, R-270) \| `approval` \| `forbidden` (R-183) |
| `egress_allowlist` | before issue #79; read as `allowlist` mode when `egress_mode` is unset, never written |
| `deploy_approval_required` | every app's deploys need approval (R-154) |
| `deploy_approval_apps` | these app IDs' deploys do, whatever their owners' specs say |
| `deploy_approval_count` | approvals a deploy needs; 0 is 1 (R-156) |
| `deploy_approval_expiry_hours` | how long a request waits; 0 is forever; `Default()` ships 168 |

`PUT /policy` refuses a mode, loosening rule or entry that does not parse, and a negative count or
expiry (`Document.ValidateRules`). The requirement an administrator places on one app lives here rather
than in the app's spec so that the app's owner cannot remove it (R-154).

**[D]** Startup configuration can fix any policy field (R-271): a `policy:` section in the config file or `PANDO_POLICY_<FIELD>`, the environment winning. A fixed field is laid over the stored document by the policy store itself (`policy.Overlay.Wrap`), so every reader — evaluator, handlers, the security pass — sees it; `PUT /policy` refuses to change it and the store never writes it into `body`, so removing it from the config and restarting restores what was stored. An unknown field or a value of the wrong type stops startup, because a policy that silently does not apply is worse than one that refuses to start. `GET /config` reports every non-secret startup setting and each fixed field with its source (env var, or file and key), and the console shows fixed fields disabled with that source on hover.

**[D]** Adapters and AI function assignments are overlaid the same way (R-271). An `adapters:` section of the config file declares adapters by ID, and the AI functions each handles; credentials are `{env: …}` or `{file: …}` references, never values (R-190). At startup a declared adapter replaces a stored one with the same ID, a stored AI adapter of the same kind, and a stored default in its category, and a declared assignment replaces the `ai_assignments` row for its function. Nothing declared is written to `adapter_configs` or `ai_assignments`, so removing a declaration and restarting brings back what was stored; the API reports a stored row the file replaces as overridden, and refuses to change a declared item with `STATE_SET_AT_STARTUP`. A declaration that contradicts itself — two defaults in a category, two AI adapters of one kind, one function under two adapters, two services adapters for one slot type — stops startup, naming both keys. Design 10 §7.1.

**[D]** `ai_assignments.adapter_id` has no foreign key, deliberately: a declared adapter can be assigned a function from the console and has no `adapter_configs` row to reference. An assignment whose adapter is not running leaves the function off. The primary key makes a second adapter for one function impossible rather than a handler's check; `PUT /ai/functions/{function}` updates a row only when it already names the same adapter, and otherwise returns `STATE_AI_FUNCTION_ASSIGNED`.

### 2.6 Audit

```sql
CREATE TABLE audit_events (
    id            bigint NOT NULL DEFAULT nextval('audit_events_id_seq'),
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    principal_kind text NOT NULL,             -- user | token | system | anonymous
    principal_id  text,
    on_behalf_of  text,                       -- delegated token: the owning user (R-229)
    action        text NOT NULL,              -- e.g. app.deploy, grant.create, exec.session
    app_id        text,
    target_kind   text,
    target_id     text,
    request_id    text,
    detail        jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (id, occurred_at)             -- a partitioned table's key includes the partition key
) PARTITION BY RANGE (occurred_at);           -- one partition per UTC month: audit_events_2026_09
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;
CREATE INDEX ON audit_events (app_id, occurred_at DESC);
CREATE INDEX ON audit_events (principal_id, occurred_at DESC);

CREATE TABLE audit_archives (                -- one archived month (R-347)
    id          text PRIMARY KEY,             -- aar_...
    month       date NOT NULL,                -- first day of the month
    adapter_ref text,                         -- NULL: kept by Pando; else the backup adapter
    object_name text NOT NULL,
    row_count   bigint NOT NULL, first_id bigint NOT NULL, last_id bigint NOT NULL,
    first_at    timestamptz NOT NULL, last_at timestamptz NOT NULL,
    size_bytes  bigint NOT NULL, sha256 text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Owner-defined, SECURITY DEFINER, executable by pando_audit_archiver only.
audit_ensure_partition(month date) RETURNS text
audit_drop_month(month date, expected_rows bigint, archive_sha256 text) RETURNS bigint
```

**[D] `app.use`** is the highest-volume action: one row per visit to an app (design 06 §6), with
`detail` holding the first path, the visit ID, and for an anonymous visitor the address Pando saw.
About 470 bytes a row with its indexes.

**[D]** Append-only. `REVOKE UPDATE, DELETE` from the application role at the database level. R-027 says no adapter can rewrite the audit log; the enforcement should be a database grant, not a code review.

**[D] The revoke is only meaningful if the application role owns nothing.** This was established
empirically in phase 0 and is easy to get backwards. `REVOKE` *does* take effect against a table's
owner — after revoking, `has_table_privilege` reports false even for the owner. What an owner retains
is **grant option**, implicitly, so it can restore the privilege to itself in a single statement:

```sql
GRANT UPDATE ON audit_events TO pando_app;   -- succeeds when run as the owner
UPDATE audit_events SET action = 'something else';
```

Against an owning role the revoke is a speed bump, not a boundary, and the statement that undoes it is
available to exactly the process an attacker would be running inside. **Ownership is the property that
must be denied, not the privilege.** Pando therefore runs migrations as a schema-owning role and
serves traffic as a separate `pando_app` that owns nothing, and it verifies both at startup —
refusing to run if the audit table is owned by the role serving traffic.

**[D]** `detail` never contains a secret value. The `secret.Value` type from §00 3.3 makes this structural.

**[D] Retention removes whole months, and only through one function (R-347, issue #60).** The log is
partitioned by UTC calendar month, so a month past retention leaves as one `DROP TABLE` rather than as
row deletes, and leaves no partial month. The daily pass (`audit.Archiver`, as `pando_audit_archiver`):

1. Makes the partitions for this month and the next two (`audit_ensure_partition`). A month with no
   partition still takes writes, in `audit_events_default` — an archiver that had not run must not
   make every audit write fail, because the write comes before the privileged action (§04 2.6). Rows
   already there move into the new partition unchanged, in the same transaction.
2. For each month that ended at least `audit_retention_months` ago, writes the archive — gzipped JSON
   lines, one `row_to_json` per event in `id` order, times in UTC — and a manifest beside it
   (`<name>.manifest.json`), from one repeatable-read snapshot.
3. Reads the archive back and checks it against the manifest: size, SHA-256, that it decompresses,
   and that it holds `row_count` events in increasing `id` from `first_id` to `last_id`.
4. Records it in `audit_archives`.
5. Calls `audit_drop_month`, which locks the log against writes, counts the month again, and drops it
   only if the count, the id range and the digest match a recorded archive — writing `audit.archive`
   with the month, rows and digest in the same transaction.

A failure anywhere before step 5 leaves the month in the live log, and the next pass picks up an
archive already written if it still verifies. An event that lands in an old month after it was
archived — a restore, a backdated insert — is a count mismatch the drop refuses, and the next pass
writes a new archive holding it.

**[D] Who may remove a month.** Three roles, not two. `pando_app` keeps `INSERT` and `SELECT` and
nothing else — on the table **and on every partition**, because a partition is a table and `DELETE` on
one is `DELETE` on the audit log; `state.applyGrants` revokes it from each partition on every start, and
`audit_ensure_partition` revokes it from a new one before it is visible. `pando_audit_archiver` owns
nothing, reads the log, inserts into `audit_archives`, and may execute the two functions; it holds no
`UPDATE` or `DELETE` either. The functions belong to the schema owner and enforce, whoever calls them:

- **the floor** — a month that ended less than three months ago is refused (R-348). It is a literal in
  the function, so changing it is a migration, like a built-in role's verbs (R-081);
- **the archive** — a month with events is refused unless `audit_archives` holds one with that many
  rows, that id range and that digest. Only the archiver can write that table, so the role serving
  traffic cannot vouch for an archive that was never written.

Startup refuses to serve (`verifyRetentionIsTheArchiversAlone`) if `pando_app` owns or can modify any
partition, owns or can execute either function, is a member of the archiver role, or can write
`audit_archives`. A DR restore re-applies the grants straight after `pg_restore` rather than at the
next start (`backup.Service.Regrant`), because restored tables arrive with the owner's default
privileges.

**[P] Retention is host policy** (`policy.Document`):

| Field | Meaning |
|---|---|
| `audit_retention_months` | months the live log keeps; 0 is 3, and fewer than 3 is refused (R-348) |
| `audit_archive` | `keep` (unset) under `server.audit_archive_dir`, default `/var/lib/pando/audit-archives`; `export` to a backup destination; `off` archives nothing, so removes nothing |
| `audit_archive_destination` | the backup adapter an export goes to; empty is the default one. Refused unless `audit_archive` is `export` |

An archive is resolved by its recorded `adapter_ref` and never looked for elsewhere, like a backup
(§2.8). `GET /audit/archives` lists them and `GET /audit/archives/{id}` serves one, behind
`install.audit.read`. What an archive is owed after it is written — ageing out, the DR bundle,
search, hash chaining — is O-27.

### 2.7 Sessions

```sql
CREATE TABLE sessions (
    id           text PRIMARY KEY,            -- ses_...
    user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    adapter_id   text NOT NULL REFERENCES identity_adapters(id),
    issued_at    timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    user_agent   text,
    ip           inet
);
CREATE INDEX ON sessions (user_id) WHERE revoked_at IS NULL;
```

**[D]** Sessions are server-side rows, not stateless cookies. R-047 defers session lifetime to each adapter, but revocation has to be immediate when an adapter *can* push (R-048), and that is impossible with a stateless cookie. The cookie carries only `ses_...`.

**[P]** The proxy checks session validity on every request. At single-host scale this is one indexed lookup; cache with a short TTL if it ever matters, accepting that the TTL becomes the revocation window.

**[D]** `sessions.adapter_id` is the provider the session was signed in through, which is not
necessarily the account's originating adapter once identities are linked; its lifetime is that
provider's (R-047).

```sql
CREATE TABLE sso_flows (                    -- a redirect sign-in in progress (design 06 §3.2)
    id            text PRIMARY KEY,         -- the state: 256 random bits
    adapter_id    text NOT NULL REFERENCES identity_adapters(id) ON DELETE CASCADE,
    purpose       text NOT NULL,            -- sign_in | test
    bind_hash     text,                     -- SHA-256 of the starting browser's cookie
    return_origin text NOT NULL,            -- the hostname the flow started on (R-172)
    next_path     text NOT NULL,
    callback_url  text NOT NULL, entity_id text NOT NULL,
    flow          bytea,                    -- the adapter's PKCE verifier and nonce, or SAML request ID
    initiated_by  text,                     -- a test sign-in's administrator
    user_id       text REFERENCES users(id),
    handoff_hash  text,                     -- SHA-256 of the one-time code that finishes it
    result        jsonb, failed boolean NOT NULL DEFAULT false,
    expires_at    timestamptz NOT NULL, consumed_at timestamptz
);

CREATE TABLE sso_replay (                   -- one-time IDs a provider promised: SAML assertion IDs
    adapter_id text NOT NULL, one_time_id text NOT NULL, expires_at timestamptz NOT NULL,
    PRIMARY KEY (adapter_id, one_time_id)
);
```

**[D]** A flow is a row, not a signed cookie, because it crosses hostnames: it may start on an app's
own hostname and the provider returns to the one callback registered with it. `flow` holds the
adapter's own secrets for that sign-in for at most ten minutes, is cleared when the callback takes it,
and is worthless without the provider's response. Rows are kept a day after expiry so a sign-in page
can still say why one failed.

### 2.8 Backups

```sql
CREATE TABLE backups (
    id           text PRIMARY KEY,            -- bkp_...
    app_id       text REFERENCES apps(id) ON DELETE SET NULL,  -- NULL for DR bundles
    kind         text NOT NULL CHECK (kind IN ('rolling','on_delete','dr_bundle')),
    adapter_ref  text NOT NULL,               -- which backup adapter holds it (R-217)
    object_name  text NOT NULL,
    size_bytes   bigint,
    manifest     jsonb NOT NULL,              -- what's inside; drives restore verification (R-215)
    retain_until timestamptz,                 -- NULL for on_delete: kept until discarded (R-204)
    created_by   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT backups_on_delete_is_never_aged_out CHECK (
        kind <> 'on_delete' OR retain_until IS NULL),
    CONSTRAINT backups_scope_matches_kind CHECK (
        (kind =  'dr_bundle' AND app_id IS     NULL) OR
        (kind <> 'dr_bundle' AND app_id IS NOT NULL)),
    CONSTRAINT backups_object_is_unique UNIQUE (adapter_ref, object_name)
);
```

```sql
CREATE TABLE backup_attempts (
    app_id       text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    attempted_at timestamptz NOT NULL,
    outcome      text NOT NULL CHECK (outcome IN ('taken', 'skipped', 'failed')),
    backup_id    text,                        -- when taken; not a FK, expiry removes backups
    message      text NOT NULL DEFAULT '',    -- why skipped or failed (R-105)
    remedy       text NOT NULL DEFAULT ''
);
```

**[D]** The last scheduled rolling backup of each app, whatever came of it — one row per app, replaced
on every attempt. `backups` says what exists; this says what was tried and did not happen, which
`backups` cannot: an app missing from it looks the same whether it was never due or failed every hour
for a week (issue #87). Shown on the app and on the Backups screen.

**[D]** `kind = 'on_delete'` rows have `retain_until IS NULL` — R-204 says these are kept until explicitly discarded, not aged out. The CHECK makes that structural rather than a convention the pruning query has to remember.

**[D]** `app_id` is `ON DELETE SET NULL`, **not** `CASCADE`. R-204 keeps a final backup after the app is gone; cascading would delete the record of that backup at exactly the moment it starts mattering. The row outlives its app on purpose, and `backups_scope_matches_kind` is therefore written against `kind`, which does not change, rather than against the app still existing.

**[D]** The destination is stored as an adapter reference plus an object name (R-217, R-252). Resolved at write time and recorded, never re-resolved at restore: a bundle written to one destination is not findable in another, and quietly looking elsewhere is how a restore reports "not found" for a bundle that exists.

**[D]** The `manifest` is what restore verifies against before applying anything (R-215). Pando keeps this copy; the copy inside the bundle is the one being checked against it.

**[D]** A per-app backup is encrypted under the **install's own secrets key**, not a supplied passphrase. R-213 governs the DR bundle and its reasoning does not carry over: it exists because a restore onto a fresh machine cannot unwrap keys held by the machine that died, and because shipping the key inside the bundle makes it plaintext for anyone holding the file. An app backup is restored in place, onto this install (R-206), so the machine that can read it is the machine that wrote it. The alternative — prompting for a passphrase on every app deletion — is a prompt people learn to type "password" into, which is weaker than the key already protecting every secret in the install. Pando keeps this copy; the copy inside the bundle is the one being checked against it.

---

### 2.9 Events and subscriptions (issue #50)

Migration 000043. Design in [11-events-and-subscriptions.md](11-events-and-subscriptions.md).

| Table | Holds | Constraints that matter |
|---|---|---|
| `events` | The outbox: name, app, actor, catalogued `data`, `routed_at` | No FK on `app_id` — `app.deleted` outlives the app. A CHECK on the name's shape. IDs from `pando_ulid()`, the shape `internal/id` makes |
| `subscriptions` | Owner, app (NULL = install-wide), filter, destination, a webhook's method, content type, header names and body template | `owner_user_id` or `owner_token_id`, exactly one, each cascading. `webhook` ⇔ `url`, `notify` ⇔ `adapter_id`, as CHECKs. **No column a signing key could go in** (R-371) |
| `subscription_secrets` | A webhook's signing key and each custom header's value, as the secrets adapter's ciphertext or external reference | Same shape as `adapter_credentials`; `field` is `signing_key` or `header:<Name>`; a CHECK that one of ciphertext or reference is set |
| `event_deliveries` | One event to one subscription; status, attempts, next attempt | `UNIQUE (subscription_id, event_id)` — routing twice queues once |
| `delivery_attempts` | Every attempt: when, status code, error, duration | Cascades with its delivery |
| `notification_preferences` | A person's choice per kind and channel | A missing row is the default |
| `notifications` (amended) | Gains `link` and `event_id`, for the inbox (R-377) | — |

**[D] Four triggers write events**, because each column has more than one writer and a trigger is the one
place they all pass through: `apps.state` → `app.state_changed`; `deployments.status` reaching
`succeeded`/`failed` → `deploy.*`; a `backups` insert → `backup.created`; a `backup_attempts` row with
outcome `failed` → `backup.failed`. The audit writer adds a fifth path for catalogued actions, in the same
transaction as the audit row (R-365).

## 3. Things deliberately not in the schema

- **Hosts.** R-256: multi-machine capability lives entirely in adapters. Adding a `hosts` table would be the first step toward the scheduler R-010 forbids.
- **Tenants/orgs.** R-015.
- **Per-user instances.** R-290 is `LATER`. When it lands it is an `app_instances` table keyed on `(app_id, user_id)` with its own volume rows; nothing above needs restructuring to accommodate it.
- **Log storage.** Logs are streamed from the runtime adapter and retained on disk under a size cap (R-222), not in Postgres. Putting them in Postgres makes R-224's disk accounting harder, not easier.
