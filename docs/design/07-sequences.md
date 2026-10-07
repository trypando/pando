# 07 — End-to-End Sequences

Four flows that between them exercise every subsystem. These are the integration-level acceptance criteria: if all four pass against real Postgres and real Docker, v1 works.

---

## A. Add an app

Non-technical user, public GitHub repo, Traefik routing, Docker runtime.

```
User → POST /api/v1/apps { source: git url }

  1. authz: user holds app.create (install-level)
  2. policy: source allowlist (R-092)
       ✗ → POLICY_SOURCE_NOT_ALLOWED. Nothing cloned. STOP.
  3. create app row, state = draft
  4. audit: app.create
  5. enqueue detection job
  → 202 { app_id, state: "draft" }

Detection job:
  6.  clone to a read-only SourceView (R-020)
  7.  registry check: ghcr.io/acme/notes, docker.io/acme/notes  (R-094 tier 1)
  8.  every builder adapter Bid()  (R-093)
  9.  rank; winner emits draft spec + evidence + questions
  10. trial run in throwaway isolation  (R-097)
        - observe bound ports        → Port.Source = "observed"
        - observe writes outside volumes → ObservedWrites
        - crash → capture log; promote suspected slots to Required (§01 2.5 fallback)
  11. attach warnings:
        WARN_NO_PERSISTENT_VOLUME if ObservedWrites non-empty  (R-201, R-202)
        WARN_PATH_ROUTING_INCOMPATIBLE if applicable            (R-168)
  12. status = ready | needs_answers

User → GET /apps/{id}/detection
  → { winning_bid, runners_up, questions[], draft_spec, warnings[] }

  Console shows each question with a copy button.  (R-105)
  User pastes into the assistant that wrote the app, pastes answer back.

User → POST /apps/{id}/detection/answers
User → PUT  /apps/{id}/slots/REDIS_URL { mode: "provisioned" }   (R-131)
User → POST /apps/{id}/detection:accept

  13. validate spec (§01 3)
  14. write spec_revisions rev 1, origin = detected
  15. app.state = proposed, pinned_spec_id set
  16. write TWO grants: control/owner + data  (R-073)
  17. audit: spec.pin, grant.create ×2
```

**Assertions:**
- A blocked source produces zero disk writes. `git clone` is never invoked.
- Accepting does not deploy.
- Exactly two grant rows exist afterward.
- Ports discovered by observation carry `Source: "observed"`, not `"framework"`.

---

## B. Deploy

```
User → POST /apps/{id}/deployments { spec_revision: 1 }

  authz: app.deploy

  ── PLAN (no side effects) ──
  1. validate spec                       → VALID_*
  2. host policy                         → POLICY_*
  3. resolve adapters + HealthCheck()    → ADAPTER_UNAVAILABLE
  4. capability check                    → PLAN_CAPABILITY_UNSUPPORTED
       routing mode advertised? isolation floors met? (R-024, R-114, R-254)
  4b. egress: policy.EgressFor(spec.egress) → merged rules + loosenings (R-182, R-188)
       loosening, egress_loosening = forbidden → PLAN_EGRESS_LOOSENING_FORBIDDEN (R-183)
       restricted, runtime can't enforce       → PLAN_CAPABILITY_UNSUPPORTED (R-186)
  5. every Required slot resolved?       → PLAN_SLOT_UNFILLED  (R-132)
  6. capacity: sum(allocated) + requested ≤ adapter-reported total
                                         → CAPACITY_WOULD_OVERSUBSCRIBE (R-242)
  ── plan boundary ──

  6b. approval.Reasons(policy, app, running spec, this spec)   (R-154)
        none, or rollback to a revision that ran (R-157)
          → create deployment row, status = pending; app.state = deploying
        any → create deployment row, status = awaiting_approval,
              approvals_required + approval_expires_at from policy, reasons recorded;
              an older awaiting request for the app → superseded;
              audit: deploy.request; notify approvers (R-159)
              → 202. app.state UNCHANGED. Stop.

      Approver → POST /apps/{id}/deployments/{did}/approve
        authz: install.deploys.approve, or app.deploy.approve on the app (R-155)
        record decision; audit: deploy.approve
        fewer than approvals_required → stays awaiting_approval
        enough → re-run steps 1–6 against policy as it is now (R-156)
                 ✗ → the plan's own error; the request does not run
                 ✓ → status = pending; app.state = deploying; continue at 7
      reject → status = rejected; audit: deploy.reject. app.state UNCHANGED.
      expiry passes → status = expired; audit: deploy.expire.

  7.  clone, resolve ref → commit SHA; write it into the spec  (R-120)
  8.  Build:
        isolated, no runtime socket      (R-024, R-112)
        cache namespace = app id         (R-117)
        egress per policy                (R-118)
        logs streamed to SSE
        ✗ → deployment failed, app.state UNCHANGED, old version serving (R-146)
  9.  provision slots marked provisioned → workloads joined to bundle (R-134)
  10. secrets.Get() → materialize WorkloadPlan.Env (fully resolved, §03 2.1)
  11. runtime.CreateVolume() for any missing
  12. runtime.Apply(), with NetworkPlan.Egress = the rules from 4b,
      recorded on the deployment for the reconciler (O-10):
        restricted → internal network + egress gateway, HTTP(S)_PROXY set (R-187)
        unrestricted → nothing in the app's path (R-186)
        recreate (R-144): stop old → start new
        start_then_swap (R-145): only if capability + explicit opt-in
  13. routing.Ensure() → points at the PANDO PROXY, never the workload (§00 1.3)
  14. wait for health (R-221)
        pass → running
        fail → degraded; rollback only if opted in (R-147)
  15. audit: app.deploy
```

**Assertions:**
- A build container has no Docker socket mounted. Assert on the container's mounts.
- A failed build leaves the previous container running and the app's state unchanged.
- Traefik's generated config points at Pando's proxy address, not at the workload's.
- Secrets never appear in build logs, deployment records, or audit detail.
- Every plan-stage rejection happens before any clone, build, or container creation.
- A deploy that needs approval clones, builds and creates nothing, and leaves the app's state as it
  was until it is approved; a rejected or expired one never touches it.
- An app with no egress restriction in effect gets no gateway, no proxy variables, and an ordinary
  network. A restricted one reaches an allowed destination through the gateway and nothing else.

---

## C. A request through the proxy

The hottest path, and the one where a mistake is worst.

```
Browser → GET https://notes.corp.com/dashboard

 1. resolve app from Host header (or path prefix in proxy mode)
 2. app running? → 503 if not
 3. read session cookie → sessions row → user
      no cookie → Principal{anonymous}
 4. principal status: suspended/deleted → deny  (R-049)
 5. CheckData(principal, app)                    (§06 2)
      owner?                → allow             (R-072)
      data grant (direct)?  → allow
      data grant (group)?   → resolve live      (R-079)
      anonymous grant?      → allow             (R-075)
      else: anonymous → 302 login; authed → 403
 5a. record app.use, first request of a visit only  (R-227)
 6. mint assertion: sub, email, name, groups, aud=app_id, exp=+120s  (R-054)
 7. STRIP all inbound X-Pando-* headers          ← R-053 spoofing defense
 8. set X-Pando-Assertion + convenience headers
 9. path mode: strip prefix, set X-Forwarded-Prefix  (R-167)
10. forward; stream response unbuffered           (R-170)
```

**Assertions:**
- A request arriving with a forged `X-Pando-User` header reaches the app with that header **replaced**, never passed through.
- An anonymous request to a public app still passes through every step and receives `sub: "anonymous"` (R-056). No bypass path exists — assert by confirming the audit/metrics counter increments.
- An assertion minted for app A is rejected by app B's verification because `aud` mismatches.
- Removing a user from a group revokes access within the documented cache TTL, without a redeploy.
- A websocket upgrade succeeds and streams bidirectionally.
- SSE responses are not buffered.

---

## D. Disaster recovery

The flow nobody tests until they need it, which is why it is one of the four.

### Backup

```
Admin → POST /api/v1/backups { kind: "dr_bundle", passphrase }

 1. authz: install admin
 2. pg_dump the state store
 3. export the local secrets encryption key           (R-212)
 4. export adapter configs and host policy
 5. snapshot app volumes + provisioned services
 5a. copy uploaded source archives (uploads/, O-37) — the registry's
     images are not in the bundle; an image is rebuilt after a restore
 6. build a manifest: object counts, checksums, versions
 7. encrypt the whole bundle under the SUPPLIED PASSPHRASE  (R-213)
      — never a key stored on the host
 8. write to destination                              (O-6)
 9. audit: backup.create
```

**[D]** The passphrase is never persisted. If the operator loses it the bundle is unusable (R-214) — an accepted cost, and the console must say so at creation, not in documentation.

**[P] Sixteen characters, and that is the only rule.** Longer than a password's minimum (ten, §06)
because the threat is different in both directions: a password is rate-limited by a server that can
lock an account, and a passphrase protects a file an attacker holds and can grind offline at whatever
rate their hardware allows. No composition rules in either place — a class requirement pushes people
toward `Passw0rd!`, which is shorter in real entropy than four words, and the KDF is what actually
buys the margin here (argon2id, 256MiB, which is the number to raise if the margin needs raising).

**[P] Step 5 asks the adapter whether it needs asking.** A services adapter reports
`DataInAppVolumes` (§03 7): true means its data is in app volumes and step 5's volume snapshot already
holds it, so calling `Snapshot` would put the same bytes in twice and double the size of the one file
an operator has to store offsite. The manifest counts `services` and `services_snapshotted`
separately, so "5 services, 0 snapshotted" reads as the in-bundle provisioner working rather than as
five databases silently missing.

### Restore

```
Admin → POST /api/v1/backups/{id}:restore { passphrase }

 1. decrypt                          → BACKUP_DECRYPT_FAILED
 2. VERIFY against manifest          → BACKUP_INCOMPLETE   (R-215)
       checksums, object counts, schema version compatibility
       ✗ → STOP. Nothing on the target is touched.
 3. confirm destructive intent (target install is replaced)
 4. restore Postgres
 5. restore secrets key
 6. restore adapter configs + policy
 7. restore volumes
 8. reconciler picks up every app and converges to pinned specs
 9. audit: backup.restore
```

**Assertions:**
- A truncated or tampered bundle is rejected at step 2 with the target install untouched.
- A wrong passphrase fails at step 1 and reveals nothing about bundle contents.
- After restore, every app returns to its pinned spec without human intervention beyond the restore itself.
- The restored install's assertion signing key is the original, so apps that cached JWKS still verify.

**[D] Bootstrap ordering, largely dissolved (O-14).** The problem existed only under the
bundled-container option: if Pando managed its own Postgres container, restore would have had to start
that container before it had a state store telling it how. O-11 resolved to the install topology
supplying Postgres (§00 1.1), so the database is already up when restore runs and restore writes into
it.

What remains is ordinary sequencing, not a paradox: confirm during phase 9 that nothing else in the
restore path needs adapter configuration before the database is available. If something does, it reads
that configuration from the bundle — a normal ordering fix rather than a bootstrap mode.

---

## Coverage

| Subsystem | A | B | C | D |
|---|:-:|:-:|:-:|:-:|
| Source allowlist / policy | ● | ● | | |
| Detection auction | ● | | | |
| Trial run | ● | | | |
| Questions (R-105) | ● | | | |
| Spec validation | ● | ● | | ● |
| Planner / plan-time errors | | ● | | |
| Builder isolation | | ● | | |
| Slots / provisioned services | ● | ● | | ● |
| Secrets | | ● | | ● |
| Runtime adapter | | ● | | ● |
| Routing adapter | | ● | ● | |
| Proxy / assertion | | | ● | ● |
| Authorization, both planes | ● | ● | ● | |
| Groups, live resolution | | | ● | |
| Reconciler | | ● | | ● |
| Volumes | ● | ● | | ● |
| Audit | ● | ● | ● | ● |
| Backup / restore | | | | ● |
