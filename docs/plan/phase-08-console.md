# Phase 8 — Console

**Goal:** two audiences, one app. A non-technical user's first experience is a page of tiles, not a
dashboard.

**Prerequisites:** phase 5.

**Design:** `../design/08-console-and-plan.md` §1; `../design/04-api.md` §2.9.

## Before anything else: there is already a design system

`.claude/skills/pando-design/` holds the Pando design system — "Topo map". Invoke the `pando-design`
skill and read its `readme.md` **before** writing a component or a line of CSS. It carries the tokens,
24 React components, the brand rules, the voice guidance and a lint config that catches raw hex
values, raw `px` values and non-brand fonts.

Design 08 §1.2 predates it and says "Radix primitives, own layer on top". **That is superseded**: the
components exist, they have no npm dependencies and no CSS-in-JS, and building a second layer beside
them is how an install ends up with two design systems and neither maintained. Use Radix only for
behavior the system does not implement — focus trapping, a combobox — and style it from the tokens.

Three things the system settles that this phase would otherwise re-decide:

- **Status is a symbol plus a word, never a colored pill** — so it never depends on color alone. That
  is the same distinction the reconciler draws between `running`, `degraded` and `failed`.
- **Marker red appears in under 3% of any screen**: the summit mark, failed apps, destructive
  actions. If a screen shows red in more than one or two places, something is wrong with the screen.
  This is the visual form of "a warning must never look like an error".
- **An action keeps its name through the whole flow.** "Deploy" produces "Deployed" — the button, the
  toast and the log line use the same word.

The design project also holds a clickable console prototype (`ui_kits/console/`) built from the
brand spec's own wireframes — the apps table, app detail, build log, variables, settings, add-app
flow and the dark theme. It is not vendored here. **Read it before designing these screens again**;
see `.claude/skills/pando-design/PROVENANCE.md` for how to pull it.

## Tasks

- [x] Pull the per-component `.d.ts` and `.prompt.md` sidecars from the design project — the console
      is TypeScript and needs the types, and each `.prompt.md` states when *not* to use a component
- [x] Vite + React + TypeScript in `console/`, built to static assets embedded via `embed.FS`
- [x] Link `styles.css` from the design system; dark theme is `<html data-theme="dark">`
- [x] Wire `_adherence.oxlintrc.json` into the console's lint step, so a raw hex or px fails CI the
      way the adapter import rule does for R-027 — **it does not run under oxlint**, which implements
      neither rule the file uses. Run under ESLint instead, and its per-component prop rules are
      filtered out because they reject `onClick` on a `<Button>`
      ([note](../design/notes-console-findings.md))
- [x] **API types generated, never hand-written** (R-261) — `cmd/gen-api-types` reflects the Go
      types the handlers serialize. Committed and diff-checked in CI, like the traceability index.
      `docs/openapi.json` is generated from the same code by `make reference`, and is not hand-written
      either
- [x] Launcher at root: tiles from `GET /me/apps`, data-plane grants only (R-264)
- [x] Admin entry (R-265) — the entry appears for someone holding an install-scoped verb **or** a
      control-plane grant on at least one app, because "any administrative verb" is two scopes.
      `GET /me` returns the install verbs and the console reads them; `GET /apps` is control-plane
      scoped and the server filters it. O-17 is resolved — the install verbs and the fourth built-in
      role landed with this phase, along with the six endpoints they gate
- [x] Install-level **screens** — Accounts, Installation, Policy, Audit log, each in the sidebar on
      the verb it needs (`useInstallVerb`) rather than on "is an administrator". There is no
      implication graph between verbs (R-082), so a sidebar that assumed one would offer a screen
      whose every request comes back 403
- [x] **Grant and revoke an install-scoped role** — `PUT`/`DELETE /users/{id}/role`. Until this
      existed, the only install grant was the one bootstrap writes on first run, so an install had
      exactly one administrator forever and no way to hand over. Its own route rather than a field on
      `PATCH /users/{id}`, and `POST /apps/{id}/grants` still cannot reach install scope — a sharing
      request that accepted an empty app ID would be a way to make an administrator
- [x] `GET /users`, `GET /roles`, `GET`/`PUT /policy`, `GET /audit` — three of the six install verbs
      gated endpoints that did not exist. A verb nobody can exercise is not authorization, it is a
      string in a table
- [x] **A login screen, and a password anyone can change.** `/login` rendered the launcher, which
      rendered a link to `/login`: there was no form anywhere and the only way into a fresh install
      was to call the API by hand. R-046's "must be changed on first login" was worse — the flag was
      set at bootstrap, surfaced on `GET /me`, and **nothing could clear it**, because no
      password-change endpoint existed. Both halves are here: `POST /me/password` and the two screens
      in `console/src/auth/`
- [x] The **policy preview** design 05 §3 promises. `POST /policy:preview` runs each live app's
      pinned spec through the policy-derived plan checks against a policy that is not saved yet, and
      the screen lists what each app's next deploy will say — in the deploy's own words, so the
      administrator and the developer read the same sentence. Asked for rather than automatic: it is
      the expensive check, and running it on every keystroke makes a form that stutters, which
      teaches people to ignore the panel it is stuttering to fill
- [x] **Dependencies and storage** on the app — R-030 makes Slot and Volume first-class objects and
      both had a full API and no screen. A slot names how it is filled in words rather than showing
      the stored value (R-083 keeps a pasted database URL off a screen `app.view` can read), and
      says plainly which kinds Pando will stand up and which it will not (R-010). Adding storage
      leads with R-201's warning, because an app that loses its data at the next deploy is what the
      endpoint exists to prevent
- [x] **Groups and roles** (R-078, R-082) — two sections, not one screen with two tabs pretending
      they are the same idea. A group is *who*; a role is *what*. A group synced from an identity
      provider is shown with its source and is not editable here, because the provider owns
      membership and an edit would be overwritten at the next sign-in. A custom role offers only its
      chosen scope's verbs: the server refuses a mixed-scope role (R-080), and offering a choice
      that will be refused is worse than not offering it
- [x] **Adding an app** (R-002, R-005, R-101). The console could administer apps and not make one —
      the only ways in were `pando app add` or a POST by hand, and a person who may not know what a
      port is will not be running curl. One input: design 08's principle is "the default path shows
      almost nothing", so the name is derived from the source and shown as a field somebody may
      correct, rather than asked for. R-101's escape hatch — an image, skipping detection — is second
      in the list and never the default, because the product is the first option working. Adding
      opens the app, since detection is already running and the next thing to do is look at it
- [x] **Environment variables** (R-102, R-103). Detection asks rather than guesses, which implies the
      answer can be corrected — and there was no way to add a variable it missed without the API. A
      plain value goes in the spec, which is exportable; a secret goes through the secrets adapter and
      the spec carries only a reference (R-190, R-191), so pasting a key into the wrong box cannot put
      it in every export of the app forever. The form asks which and defaults to the safe one
- [x] **Edits can actually ship.** Every spec edit the console made — a slot, a volume, a deploy
      setting — wrote a revision and left the pinned one alone, correctly (pinning is what the
      reconciler converges to, and a dropdown must not restart an app). Nothing then offered to deploy
      that revision, so every one of those edits was inert: saved, and then a deploy shipped the old
      spec without saying so. Overview says when the configuration has moved ahead of what is running,
      and Deploy ships it
- [x] **Choosing which part of the app to open a terminal in.** The exec endpoint has always taken a
      workload and the CLI has always had `--workload`; the console sent neither, so a compose app
      with several services could only ever be reached at the one its URL points at. Shown even for a
      single service: it answers *where am I* as well as offering a choice, and a shell prompt does
      not say which container it belongs to
- [x] Detection review screen (R-102, R-103, R-105)
- [x] **Detection failure** shown, with the reason and a way back. The server records why precisely so
      somebody returning later can find out; nothing read it, so a failed detection left the screen
      saying "Reading the repository." for ever. An app stuck on a progress message is worse than an
      error: there is nothing to act on and no reason to stop waiting
- [x] Warnings, rendered inline where they apply (R-201, R-168, R-028) — carrying no red at all,
      which is what makes them distinct from an error in a palette that has no amber
- [x] Sharing screen (R-076, R-077)
- [x] Deploy settings (R-145, R-147)
- [x] Log streaming via `EventSource`; exec via WebSocket + xterm.js — the server endpoint did not
      exist either and no phase claimed it, so both halves are here
- [x] Vendor the icons and self-host the fonts, so an installation with no internet renders the
      product rather than a fallback of it — flagged upstream as production work and assigned to no
      phase

## Requirements in scope

R-005, R-076, R-077, R-102–R-105, R-145, R-147, R-168, R-201, R-261, R-264, R-265.

## Done when

A user with no admin verbs sees only tiles; a user with admin verbs sees the management console scoped
to what they hold; the four screens below meet their stated requirements.

**Verified in a browser against the shipped binary**, with two apps from the bemeek-io org: the
launcher renders tiles with symbol-plus-word status and no Admin entry for a user with no
control-plane grant; the Admin entry appears for one who has; detection review shows the winning bid
with its evidence, the question **verbatim** with a working copy button, the runners-up behind a
disclosure, and the path-routing warning inline and quiet; answering the question enabled Accept, and
accepting pinned revision 1 and left the app `proposed` — it did not deploy.

Verified in a browser against the shipped binary, from a cold `docker compose down -v`: the sign-in
form appears at the root, the generated first-run credential is accepted, the change-password screen
follows it and cannot be navigated around, and the launcher appears after it. The Admin entry opens a
five-item sidebar; setting the only administrator's role to None is refused inline with the server's
sentence; the audit log shows `user.password.change`, `grant.create`, `grant.delete`, `policy.update`
and `authz.denied` from that session.

**The second clause is met for both scopes.** "Scoped to what they hold" is the server's scoping of
`GET /apps` for app administration, and the verb list from `GET /me` for install administration.
Verified against the shipped binary: an account with no grants sees no Admin entry, an empty `verbs`
list, and a 403 naming the missing verb on each of the six endpoints — including
`PATCH /users/{admin}`, which returned 204 before this phase and locked the install out.

Exec is verified the same way: a live shell inside a running nginx container from the Terminal tab,
with the R-086 warning stated before anything opens. Six acceptance tests cover the ordering that
matters — the audit event is written **before** the session, so one authorized and abandoned without
a byte sent is still recorded, and the stream is confirmed absent from the log.

The console now ships with no CDN dependency at all: `grep` for `fonts.googleapis`, `fonts.gstatic`
or `unpkg.com` in the built assets returns nothing.

## The four screens that carry requirement weight

Most screens are ordinary CRUD. These four are where requirements are honored or lost.

**Detection review.** Shows the winning bid with its evidence, the runners-up, and every outstanding
question. **Each question has a copy button** — the intended workflow is pasting it into the assistant
that wrote the app. Question text is rendered **verbatim** from the API; the console does not
paraphrase, or the R-105 guarantee is lost in the UI layer.

**Warnings.** Inline, dismissible, never blocking. The persistence warning uses the observed directory
when available: *"Your app wrote to `/app/data` during setup. That data won't survive a redeploy
unless you add a volume here."* Warnings and blockers must be **visually distinct** — a warning that
looks like an error teaches people to ignore both.

**Sharing.** The anonymous grant is **never labeled "public."** It reads *anyone on the internet,
without signing in*, with a confirmation step. When host policy forbids it, the option is **visible
but disabled with an explanation of who to ask** — not hidden. A hidden option produces a support
ticket instead of understanding.

**Deploy settings.** Start-then-swap shows its constraint in **body text at the point of enabling, not
a tooltip**: *two copies of your app run at the same time during a deploy. Do not enable this if your
app writes to a local file or runs migrations on startup.* Auto-rollback likewise explains why it is
off by default.

## Design principle

R-005 and R-104: **the default path shows almost nothing** — name, source, deploy. Everything with a
sane default lives behind **Advanced** and is never surfaced during setup. If a new setting appears in
the primary flow, someone must justify why it is a blocker rather than configuration.
