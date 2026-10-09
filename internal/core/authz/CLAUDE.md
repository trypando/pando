# Authorization — read this before changing anything here

Design: `docs/design/06-authorization-and-proxy.md` §1–3, §5. Read it in full. Then Sequence C in
`docs/design/07-sequences.md`.

This is the most security-sensitive code in the system and the part most likely to be quietly broken
by a later refactor.

## Two planes, never conflated

- **Control plane** — managing an app: deploy, edit spec, read logs, manage grants. Role-scoped.
- **Data plane** — *using* an app: reaching it through the proxy. Binary, no role (R-070).

App creation writes **two grant rows**, one per plane, independently revocable (R-073).

`CheckData` contains **exactly one** cross-plane implication: ownership (R-072). No other
control-plane role appears in it, and being a Pando admin does not appear in it at all (R-087 — an
admin has root and can reach a container outside Pando, but the supported path requires a grant).

**This was reversed once during design.** If a change adds a control-plane check to `CheckData`,
reject it. The code needs a comment saying so, and a test asserting an operator on someone else's app
is denied *use*.

## Evaluation order is fixed

Each step can only deny. None can restore access denied by an earlier step.

```
1. Authenticate             → AUTH_*
2. Principal status         → suspended/deleted denied (R-049)
3. Token validity           → expired, revoked → AUTH_TOKEN_INVALID
4. Token derivation         → owner suspended/deleted → AUTH_TOKEN_ORPHANED (R-059)
5. Host policy              → POLICY_*
6. Grant lookup             → PERM_*
7. Verb check               → PERM_VERB_REQUIRED (control plane only)
```

**Policy is evaluated before grants.** Policy is a floor, not an override (R-272) — a policy that
disables exec install-wide denies the owner too.

**Step 4 is a live lookup on every request, not a cascade run at revocation time.** Slower, and
correct: a cascade means a missed cascade is a permanent security hole. Do not "optimize" this into a
denormalized flag.

## Principals

- **Delegated token** → `Kind: token`, `UserID` = owner. Authorization runs against the owner exactly
  as if they made the request (R-058/059). **No grant is ever written for a delegated token.**
- **Account token** → `Kind: token`, `UserID` empty, appears directly in `grants.principal_id` (R-060).
- **Anonymous** → a real grant row with `principal_kind = 'anonymous'`, never a flag on the app (R-075).
- **System** → the reconciler and background jobs. Bypasses grant checks but **still writes audit
  events**, attributed to `system`. Anything a background job does must be as visible as anything a
  person does.

## Groups

Resolved **live** per request (R-079), never denormalized into grants. The API reads membership
afresh every time. The proxy keeps what a request read in `proxy.Cache`, emptied on every replica by a
NOTIFY whenever membership, a grant, a session, a person or an app changes, and never kept longer than
30 seconds (issue #93, design 06 §3.1). It keeps the facts `CheckData` reads (`DataFacts`), never its
verdict, so a denial is audited every time.

## Verbs

The catalog is in design 06 §5. **There is no verb implication graph** — holding `app.delete` does
not imply `app.view`. Implication graphs are where authorization bugs live. The console suggests
sensible combinations instead.

The single exception is deliberate and lives in one table, `everyApp`, read in one place,
`CheckControl` step 6b: each app verb has one install-wide counterpart — `install.apps.deploy` is
`app.deploy` on every app, `install.deploys.approve` is `app.deploy.approve` (R-080, R-081, issue #81).
One verb per verb, never a bundle; a bundle is a role (App viewer, App manager, Auditor). A new app
verb has no counterpart until one is written into the table, and
`TestR080_EveryAppVerbHasOneInstallCounterpart` fails until then. Do not read it anywhere else, and never in
`CheckData`. `AppVerbs` and `Allows` go through the same `control` function, so what the console
shows as editable cannot disagree with what `CheckControl` allows.

Built-in roles (`viewer`, `operator`, `owner`, and the install-scoped `administrator`, `creator`,
`app viewer`, `app manager`, `auditor`) are seeded by migration and trigger-protected (R-081).
A new verb in a later Pando version is added to built-in roles **by migration**. That is the only
sanctioned way built-in role contents change.

## Audit

**Every denial is audited, not only successes.** And an app verb allowed through an install-wide grant
rather than one on the app is audited too (`ThroughInstall`, `authz.install_wide`), except
`app.view` and `app.logs.read`, which the console asks constantly. Only `CheckControl` records it;
`AppVerbs`, `Allows` and `PreviewControl` decide what to show and record nothing.

A denial pattern is the signal that matters for detecting
misuse, and it is the thing most commonly left out.

Audit is written **before** the privileged action, not after. An exec session that fails to open is
still recorded as attempted (R-228).

## Tests this package owes

- Full coverage of the evaluation order above, including a delegated token orphaned by its owner's
  deletion.
- An operator on someone else's app is denied *use* of it.
- A Pando admin with no data grant is denied *use*.
- Policy denies the owner when it disables a verb install-wide.
