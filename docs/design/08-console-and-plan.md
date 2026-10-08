# 08 — Console and Build Plan

---

## 1. Console

React + TypeScript + Vite. Built to static assets, embedded in the Go binary via `embed.FS`, served by chi. One binary (R-253) includes the UI.

### 1.1 Two audiences, one app

**[D]** R-264/265. Root is the launcher — tiles for every app the user holds a **data-plane** grant on, from `GET /me/apps`. Users holding any administrative verb see an **Admin** entry that reveals the management console scoped to what they hold.

**[D]** The launcher is not a separate build. A user with no admin verbs simply never sees the admin routes. This matters because it means a non-technical user's first experience is a page of tiles, not a dashboard.

**[D] Resolved (O-9): the launcher is the notification.** Being granted access to an app does not send
a message. The app appears in the recipient's tiles the next time they load the launcher (R-264), and
that is the whole mechanism for v1.

A separate notification would be delivered by the notify adapter, which is console-only in v1 (R-231)
— so it would arrive as a message in the console, next to the tile that already appeared. For a
recipient who has never signed in, a console-only notification is invisible in a way a tile is not:
the tile is waiting for them whenever they arrive. Reconsider when an SMTP adapter exists and a share
can reach someone who is not already looking at Pando; the interface for it is already there (R-232).

**[D] Tiles.** A tile is a small square card holding the app's picture with its name beneath it,
inside the card. The picture is the app's image (R-340), or when it has none a patch of terrain
generated from its ID: the console background's generator on a small grid, zoomed so that four to six
large contours fill it — a mark, not a map — printed on one of five sheets from the palette's terrain
colors (vegetation and water, each light-on-dark and dark-on-light; sand). Red is left out so the
marker stays rare; grey because a grey tile means an app that will not open; paper because the card
is paper and the picture would have no edge. This puts the contour map in a place the brand spec
does not list: decided by the product owner, on the reasoning that a tile is content — a picture of
the app — not decoration.

A tile carries **no status line**: an app that is running or degraded and has an address is a link,
and anything else is greyed out and is not. This is a deliberate exception to the design system's
"status is a symbol plus a word" rule, decided by the product owner: the launcher is for opening
apps, "running" is the normal case, and a word on every tile saying so is noise. The state is still
in each unreachable tile's accessible name and hover title, so the difference is never carried by
appearance alone.

**[D] Arranging the launcher.** R-341, R-342. Everything that arranges the page is in one place: a
three-dot menu on each tile, shown on hover or focus (always, on a device that cannot hover). It
offers *Launch*, *Open in admin* (only for an app the person also administers — it is on `GET /apps`,
the control-plane list, not merely on the launcher), *Add to / Remove from favorites*, and *Move to
section…*, whose second page lists *Your apps*, the person's sections and *New section…*, which makes
one with the app already in it. A quiet *New section* at the foot of the page makes an empty one.
There is no screen for managing sections. The
page shows *Favorites* (when there are any), then the person's sections in the order made, then
*Your apps* for everything else; a favorite appears once, in *Favorites*. Every group collapses from
its heading, remembered per browser like the theme. A person's own section has a menu of its own on
its heading: *Rename* and *Delete section* — no confirmation, because deleting one loses nothing:
its apps go back to *Your apps*. Favoriting and moving are optimistic and roll back on refusal.
Someone who never opens a menu sees *Your apps* and nothing else.

Tiles also **drag** between groups — native HTML drag and drop, carrying the app's ID under a type of
its own (`application/x-pando-app`) so a group ignores anything else dragged over it. Dropping on
*Favorites* favorites the app; dropping anywhere else files it there and un-favorites it, since a
favorite only shows in *Favorites* and a drop out of it would otherwise change nothing visible. While a
tile is being dragged, *Your apps* shows even when empty, at the foot of the page where appearing moves
nothing. An empty *Favorites* does not appear: at the top, it pushed the page down under the pointer as
the drag began, so the first favorite comes from the menu. The group under the pointer takes a dashed
outline. Dragging is a shortcut, not the only way:
the menu does all of it, which is the path for a keyboard and for touch screens, where HTML drag and
drop is unreliable.

The design system has no menu component; `ui/Menu.tsx` is one built from its popover rules
(paper-raised, 1px rule, the one popover shadow, 6px corners), with the arrow keys, Escape and
focus return. It should move into the design project.

**[D] Search.** The launcher has a search field in its header ("/" focuses it), and the admin
console has one beside the heading of Apps, Accounts, Groups and roles, and Policy. Each filters the
list the page already has, in the browser: one installation's lists are small (R-015), the API
already returns them whole, so nothing here is a capability the API lacks (R-261). Matching is
case-insensitive and needs every word somewhere in the row (`ui/search.ts`). The launcher hides
groups with no match and opens collapsed ones while searching. Policy searches the rendered text of
each section — headings, notes, labels, descriptions, options — rather than a keyword list kept
beside it that would drift the first time a setting was added. Escape clears any of them.

**[D] Filters.** Tables filter by column, from a button in the column's header (`ui/Table.tsx`): a
"contains" field for free text, a checklist of the values present, with counts, for a column of a few
kinds of value. Filters on several columns combine, with the page's search, in the browser. Apps
filters on name, status and security; Accounts on username, name, status and installation role. The
audit log filters on the server instead, because it pages and the console never holds all of it:
action (prefix), actor (typed: username, email or ID, with suggestions — an installation has too many accounts for a dropdown), target type, target ID and time range — all `GET /audit` parameters, which
combine, and which the CLI (`pando audit`) and MCP (`pando_list_audit`) take too. Its labels use the
audit vocabulary as is; whoever reads an audit log knows what an actor and a target are.

**[D] AI functions are chosen on the adapter.** Opening an AI adapter from the Adapters screen
shows *What this adapter handles*: a checkbox per AI function (design 10 §9), and for each ticked
one an optional model for that function alone. Open the Anthropic adapter and tick what it does,
then the OpenAI adapter and tick what it does; there is no separate list of functions to manage.
A function another adapter handles is shown unticked and unavailable, with *Handled by ai_openai.
Remove it there to handle it here.* — the API refuses a second adapter for a function (R-259), so
the console states the fix rather than offering a box that would fail. A function the config file
assigns says where it is set and cannot be changed (R-271). Function choices take effect on save
without a restart; the adapter's own settings are saved, and need a restart, only when they
changed. A new AI adapter is not running until Pando restarts, so its dialog says to open it again
then to choose what it handles. An AI adapter has no default checkbox.

**[D] Asking AI on an admin screen.** Out of the way until wanted. Four screens carry one
secondary button with the AI mark, *Ask AI*, in the screen's header — never its primary action, and
shown only when that screen's function is on (*API and tools* shows it unless reference help is
known to be off, since most people cannot read which functions are on). Everything else happens in a
dialog the button opens (`ui/AskAI.tsx`): the mark and one line on what AI will do, a field with an
*Ask AI* button, and the result.

- *Groups and roles* (R-343): a preview of the drafted role — its name, what it applies to, and a
  checkbox per permission of that scope — and group, with its members. Change it by hand, or say what
  to change, which re-drafts from what is on screen, hand edits included (`current` in the request).
  *Accept* creates it through `POST /roles`, `POST /groups` and, for an installation role,
  `PUT /groups/{id}/role`; *Reject* discards it.
- *Policy* (R-344): each proposed change with a checkbox to keep it, what it was and what it would be,
  the changes declined because the startup configuration fixes them, with where, and which apps the
  kept changes would refuse at their next deploy (`POST /policy/preview`, asked as soon as there is a
  proposal). Asking again refines from what is kept (`proposed` in the request). *Accept* saves the
  policy; *Reject* discards the proposal. The button is hidden while the screen has unsaved edits of
  its own, so the two never race.
- *Audit log* (R-345): the summary, AI's note on what the log cannot answer, and how many events
  matched. A search creates nothing, so there is no Accept: *Show these events* puts the filters on
  the log as ordinary filters and closes the dialog.
- *API and tools* (R-346): the answer and what it cites. *Close*.

Every answer carries the AI mark and names the adapter and model that wrote it. The AI mark
(`ui/AiStar.tsx`) is the one used everywhere AI is indicated and nowhere else, so a person can always
tell a request to AI from an ordinary control.

**[D] Phone width.** At 48em and below (`ui/narrow.css`, `ui/narrow.ts`): the page padding token
drops to `--space-4`; headings and their actions wrap; the admin sidebar becomes a menu behind a
button in a top bar that keeps the way home and settings; the launcher's search takes its own line;
and a table keeps its columns and scrolls sideways inside itself (`ui/Table.tsx` adds the class),
rather than squeezing every column to an ellipsis.

**[D] Settings.** The signed-in person's own settings — theme and signing out — are a page of their
own at `/admin/settings`, opened by a gear in the launcher header and in the admin sidebar header,
with a back arrow to wherever it was opened from. Not a section of the admin console: nothing on it
is administration. Under `/admin` only because that prefix is already reserved against app slugs
(R-023). The launcher does not link to *API and tools*; it is in the admin console.

### 1.2 Stack [P]

| Concern | Choice |
|---|---|
| Router | TanStack Router — typed routes |
| Server state | TanStack Query |
| Client state | Zustand, sparingly |
| Forms | React Hook Form + Zod |
| Styling | Tailwind |
| Components | **The `pando-design` system** (`.claude/skills/pando-design/`). Radix only for behavior it does not implement, styled from its tokens. |
| API types | Generated from the OpenAPI spec — never hand-written |
| Streaming | Native `EventSource` for logs, `WebSocket` for exec |
| Terminal | xterm.js |

**[D]** API types are generated. Hand-written types drift from the server and R-261 depends on the API being authoritative.

**[D] A design system exists and supersedes the original "own layer on top" line above.** It is
imported into `.claude/skills/pando-design/` from a Claude Design project and carries tokens, 24
components, brand rules and voice guidance, plus a lint config that catches raw hex values, raw `px`
values and non-brand fonts. Building a parallel component layer beside it is how an install ends up
with two design systems and neither maintained.

**[D]** Its voice rules and §00 3.2's error standard are one standard, not two. R-105 asks for an
error a person can act on or paste into an assistant; the design system asks for no apology, no
`Error:` prefix and no exclamation mark. An API message and a console message reach the same person.

**[D] The console carries generated topographic terrain in its background.** This departs from the
design system, which confines the contour map to four places (marketing hero, docs home header,
empty states, 404) and says "never as wallpaper, never behind text". A console built strictly to
that rule carried no trace of the brand, and the product owner chose to put the terrain behind it.

It covers the whole background, so what keeps it from reading as wallpaper is how quiet it is:

- **Extremely faint, and even.** One colour (`--contour-line`), one line width, one opacity (0.35)
  across the whole thing. No index contours and no fading in or out — a darker line or a patch
  that fades draws the eye, which is exactly what a background must not do.
- **Part of the page.** `console/src/ui/TopoBackground.tsx` fills its page root and scrolls with
  the content; pinned to the window it looked like a layer floating over the product. It is behind
  the admin console and the launcher.
- **In colour, beside the form, on sign-in.** The sign-in and first-run password screens show the
  same terrain as a picture (`TopoMap`): index contours in `--contour`, the rest in
  `--contour-line`, and the marker-red summit triangle on the top of the central hill, as on the
  brand's hero figure. On a wide window the land rises on the right and falls away before the form
  on the left; below 60em it rises at the top and falls away above the form. There is no panel
  edge: the map ends where its lowest contour does, an irregular line made by the terrain, not a
  crop and not a fade. The form is never drawn over the map.
- **Seamless at any length.** It is a tile, and the terrain is periodic — hills wrap round the
  tile's edges and the warp uses whole periods — so contours meet across every seam. The tile is
  1600 × 1200, so a repeat is rarely in view at once.
- **Different ground per page.** Hills and warp come from a seed: the section, and the app when
  there is one. Each screen has its own map, the tabs of one app share that app's map, and the same
  seed always gives the same map.
- **Real terrain, not a pattern.** A height field of irregular hills, contoured by marching squares
  and smoothed into curves. Lines are level sets of one surface, so they never cross.
- **Tokens only.** Nothing new enters the palette, and the night-survey theme follows.
- **No frame.** Pages are not bordered or boxed; `ui/Sheet.tsx` is only the shared heading-and-
  content layout, uncapped to match `ui/layout.ts`.

Where the contour figure itself appears, it is by the system's existing recipe: 120px inside empty
states. The 404 figure (320px, collared, summit mark absent) is **not**
used for an app whose record fails to load: that figure means "the thing you came for is not here",
and such an app is in the list — the screen shows the server's own reason (R-105) and offers to
delete it instead.

### 1.3 Screens that carry requirement weight

Most screens are ordinary CRUD. These four are where requirements are either honored or lost.

**Detection review** — R-102, R-105, R-103.
Shows the winning bid with its evidence, the runners-up, and every outstanding question. **Each question has a copy button**, because the intended workflow is pasting it into the assistant that wrote the app. Question text is rendered verbatim from the API; the console does not paraphrase it, or the R-105 guarantee is lost in the UI layer.

**[D] Onboarding a new app** — the detection review for an app with no configuration yet is its own
page, not a one-tab app screen (`AppOnboarding.tsx`, from the repo-discovery design handoff, issue
#69). One page in three phases that morphs rather than cuts:

1. **Discovering.** A terrain profile draws across the top as detection advances. The headline is
   what Pando is doing ("Reading the code"), with a working line under it: a ripple, a phrase that
   turns over every 1.5s, and elapsed seconds. A segmented bar, four tallies (processes, services,
   variables, questions for you) and a step list, newest on top, each step with what it found.
2. **Ready.** The headline becomes "Plan ready" at display size and the red summit mark lands on the
   terrain. Held 2.4s, and only when the page watched detection finish.
3. **Done.** The column widens from 45 to 55rem, the steps fold behind "How Pando got here", and the
   review rises: notices, questions, variables, *Notes from AI* (only when an adapter ran and left
   notes), the plan, and a sticky bar with **Reject plan**,
   **Accept plan** and **Accept and deploy** (disabled while a question is unanswered). Opening an
   app whose detection already finished lands here directly.

**[D] Steps are real events, not a script.** The handoff's prototype ran on canned steps; the page
derives them from the detection response (`discovery.ts`): *Read the repo* (source, branch, commit),
*Work out how it's built* (strategy and evidence), *Find processes and services* and *Collect
variables* (the draft's workloads, slots, mounts and env), then *Try running it* only if a trial ran,
and an AI step only if an adapter was called (R-336) — *Review the failed plan* or *Answer what it
can*. No per-step durations are shown, because detection does not record them.

**[D] Paced, so a fast detection does not teleport.** A small repository is read in a second or two
and the three auction steps finish on one server event, so the list, tallies and terrain used to jump
to the end at once. While the page is watching, steps are revealed one at a time, each held as under
way for 1.4–3.6s (longer the more it found) while its findings rise in, and "Plan ready" waits for
the last. A step is never shown done before the server says it is, only later. Tallies fill in as
the step that found them is shown. The terrain eases through the current step and never draws faster
than about a fifth of its width per second, so a step completing moves the ridge on rather than
snapping it.

**[D] The security scan is a step of the plan** (R-310). Detection reports a `scanning` stage while
the source scan runs, and the step shows the score, the counts by severity and the worst findings.
Beside "Plan ready" sit the score badge (`ScoreBadge`, number first, color second — R-320) and a link
to the findings, which open the overview's security panel in a dialog, because a draft app has no
overview to send anybody to. An install with no scanner shows neither.

**[D] Tallies count what a person thinks of as the app's parts:** *Services* is everything that runs
(the app's own services and the ones Pando runs beside it — app, proxy and PostgreSQL are three),
then *Variables*, *Storage*, and *Questions* — every question detection asked, with how many AI
answered, because an answered question is still one Pando could not settle alone.

**[D] Every slot-filled variable is a value somebody can set here.** A key named with no value in
`.env.example` is a slot the deploy is refused without (R-132); a value typed for it during review is
stored as the slot's secret at accept (`spec.FillSlotLiteral`), the same place `PUT /slots/{key}`
writes, so the slot is filled rather than the variable overwritten and the slot left empty. A
database's URL cannot be prefilled — its host and password are generated at the first deploy — so its
row says Pando fills it then, and a value typed there points the app at a database the person already
runs instead. *Accept and deploy* waits for required values; *Accept plan* does not.

**[D] Ask AI about this plan.** When an AI adapter is available for the app, the review ends with a
conversation: the person says what is wrong, AI checks the repository and changes what it can
(design 10 §4.3), and each reply lists what changed and what Pando would not do. Without an adapter the
section does not exist — not disabled, absent. Someone who may read but not change the plan sees the
conversation and no box to type in.

**[D] Compose rewrites are one notice**, "Pando adapted N settings from the compose file", opening to
one line each — service, construct, the first sentence of why — with the importer's full paragraph on
hover. Eight paragraphs of it was a wall nobody read.

A value Pando fills (a created database's URL) shows *Filled in by Pando* and no field until the person
chooses *Use your own*. A required value carries a *Required* tag, in marker red while it is empty,
with *Not required?* beside it: detection marks slots required from a template or a crash and can be
wrong, so the person may say the app runs without one, after a confirmation that a wrong call breaks
the deploy (`optional` on accept, `spec.MarkSlotOptional`; an optional slot left empty leaves its
variable unset at deploy). *Secret* is a default on every value, never a lock: a service's address or
a key-like name starts secret, and a value left plain is stored as the slot's target where it can be
read back (`spec.FillSlotValue`). Notices sit after the variables, just above AI's notes. The action bar is two columns so
its buttons stay put as the status beside them changes length. The repository tag opens the repository
in a new tab. The security dialog lists every finding and scrolls.

No back button: the sidebar is the way back. *Reject plan* is a ghost button in `--marker-deep`, the
destructive text color.

**[D] The plan shows what runs, and only services as services.** The variables, tallies and plan all
describe the spec accepting would pin — the reading the build-method answer picks — not the winner's
draft. A workload with no command that builds from the Dockerfile the Dockerfile reading parsed shows
that Dockerfile's CMD. A slot of type `unknown` (a key named with no value in `.env.example`) is a value
to set after accepting, listed under Variables as *Needs a value*; only typed slots are services, shown
by what they are, the image they run, and how Pando provides them.

**[D] AI appears only where it did something.** An AI step is tinted `--status-info-tint` with a water
border; questions it answered are grouped under *Check what AI filled in* with its reason and files,
and changing one offers *Use suggestion* back (R-338 — AI answers are suggestions on the question,
design 10 §4.2); anything else it changed carries the AI mark in the plan; what Pando refused of it is
a notice with its reasons (R-334). A plain successful detection shows none of this. A failed trial
run's output stays reachable from its notice whatever a repair did (R-107).

**[D] Two brand exceptions, approved in the handoff and confined to this page:** the AI mark (Lucide
`sparkle` in `--water` with a small solid four-point star at its lower right — the readme's
avoid-list otherwise rules out sparkle icons) and the animated terrain strip, outside the contour
system's usual placements. The console's `TopoBackground` still sits behind the page, so the strip
has a solid `--paper` fill: one terrain in view, not two.

**[P] Kept from the previous page though the handoff omits them:** the copy button on each question
(R-105), the *Secret* checkbox on variables, adding a variable, and the reject confirmation dialog.
Accepting leaves the page for the configured app screen, so the handoff's post-decision bar states
and toasts are not built. It is the second orchestrated moment the design system allows motion for,
and under `prefers-reduced-motion` every phase shows its final state.

**Warnings** — R-201, R-168, R-028.
Rendered inline where they apply, dismissible, never blocking. The persistence warning uses the observed directory when available: *"Your app wrote to `/app/data` during setup. That data won't survive a redeploy unless you add a volume here."* Warnings and blockers are visually distinct — a warning must never look like an error, or people learn to ignore both.

**[D]** Nothing warns about stacked logins (R-171). An app that presents its own login page behind
Pando's is that app working correctly, and Pando has no basis for calling a working app a problem.
This is the general rule the warning set is held to: a warning describes something that will bite the
user later — data that will not survive a redeploy, routing that will break — not something that
merely looks unusual. Warnings that fire on correct behavior are how users learn to dismiss the ones
that matter.

**Sharing** — R-076, R-077.
The anonymous grant never stands on the bare word "public." **[P] R-077 overridden in the heading:**
the action is called *Make it public*, because that is what it is called everywhere else and a
heading nobody recognizes is unclear in its own way. What the requirement protects is kept — the
consequence, *anyone on the internet can open this, without signing in*, sits directly beneath the
heading and again in the confirmation, so the word never does the work alone. The confirmation step
stays. When host policy forbids it (R-076), the option is visible but disabled with an explanation of who to ask — not hidden, because a hidden option produces a support ticket instead of understanding.

**Deploy settings** — R-145, R-147.
Start-then-swap shows its constraint in body text at the point of enabling, not a tooltip: *two copies of your app run at the same time during a deploy. Do not enable this if your app writes to a local file or runs migrations on startup.* Auto-rollback likewise explains why it is off by default.

**Egress** — R-182, R-187, R-188.
The plan and the app's egress settings show the rules **merged**, from the plan's `egress`: the
effective mode, the effective list with each entry marked as the installation's or the app's, the
app's own list on top, private-range blocking and where it came from, and every loosening with what it
needs: forbidden, `app.egress.loosen`, or approval. A forbidden
loosening is a blocker; the rest are notes. Wherever a restriction is in effect the R-187 note is in
body text, not a tooltip: only HTTP and HTTPS through Pando's gateway leave the app, even under a
denylist. The settings read the installation's rules from `GET /apps/{id}/egress`, so an owner without
`install.view` edits against what they can see; the controls follow the caller's `verbs` (tighten,
loosen) like every other control. As the draft changes the editor asks the server with a dry-run save
(design 04 §2.3) and shows its answer — the refusal as written, or what the draft newly loosens and
whether deploying it would need approval — and Save stays off while the server would refuse.

**Deploy approval** — R-154 – R-159.
A deploy that waits says so where it was started, with the reasons and how many approvals it needs,
and the app shows it among its deploys as *awaiting approval* — never as *deploying*, because the app
has not changed. `rejected`, `expired` and `superseded` read as outcomes of the request, not as
failures of the app. An **Approvals** screen lists `GET /approvals`: each request's app, revision,
requester, reasons, approvals so far and expiry, with *Approve* and *Reject* (an optional comment)
where `can_decide` is true and read-only otherwise. An app whose auto-deploy is paused by approval
(`auto_deploy_paused`) says so on the app, and the deploy settings explain why auto-deploy cannot be
turned on while approval is required (R-158). (Implementation: see `console/src`.)

**[D]** The deploy settings choose what deploys automatically (R-141): each new commit on a branch
(the default, with the branch defaulting to the one the app was deployed from) or each new release
tag (with an optional tag pattern), saved in the same revision as the other deploy settings. Once the
deployed spec auto-deploys, a **Checks** section reads `GET /auto-deploy`: when Pando last checked,
what it found or why it found nothing, and the webhook URL with *Make webhook secret*, which shows the
secret once (R-142). An app from an image or uploaded files explains that automatic deploys watch a
repository.

### 1.4 Design principle

**[D]** R-005 and R-104. The default path shows almost nothing — name, source, deploy. Everything with a sane default is behind **Advanced** and never surfaced during setup. If a new setting is added to the primary flow, someone must justify why it is a blocker rather than configuration.

---

## 2. Build order

Sequenced so each phase produces something runnable and the riskiest work happens while it is still cheap to change.

### Phase 0 — Skeleton
Repo layout, Postgres + migrations, sqlc, zap, chi, config loading, ID generation, the error envelope, `secret.Value`, audit table with `REVOKE UPDATE, DELETE`, the import-lint CI rule (§03 9).

*Done when:* the server starts, `/healthz` responds, an audit event can be written and provably not modified.

### Phase 1 — Identity and authorization
Local identity adapter, sessions, tokens (both kinds), roles seeded by migration with the immutability trigger, the authorizer, the verb catalog.

*Done when:* §06 evaluation order is fully covered by unit tests, including a delegated token orphaned by its owner's deletion.

### Phase 2 — Spec and state
`AppSpec` types, validation, the classified differ, `spec_revisions` with the append-only trigger, apps and grants, the app CRUD endpoints.

*Done when:* an app can be created and a spec hand-written, validated, and pinned via the API. Nothing runs yet.

### Phase 3 — Adapters and the planner
Interface definitions, the registry, the Docker runtime adapter, the loopback routing adapter, the local secrets adapter. The planner with every plan-time error path.

*Done when:* `POST /apps/{id}:plan` returns each of `PLAN_SLOT_UNFILLED`, `PLAN_CAPABILITY_UNSUPPORTED`, `CAPACITY_WOULD_OVERSUBSCRIBE`, and `PLAN_NO_ADAPTER_MEETS_POLICY` on the right inputs.

### Phase 4 — Build and deploy
BuildKit adapter, rootless, containerized, no socket. The deployment pipeline. Recreate strategy.

*Done when:* **Sequence B** passes with a hand-written spec, including the assertion that the build container has no runtime socket.

### Phase 5 — Proxy
The identity-aware proxy, assertion minting, JWKS, header stripping, streaming.

*Done when:* **Sequence C** passes, including the forged-header test and the cross-app `aud` rejection.

### Phase 6 — Detection
Detector auction, the confidence ladder, compose import, trial run, question generation, warnings.

*Done when:* **Sequence A** passes against a set of real public repos — a Dockerfile app, a compose stack, a static site, a Node app with no deployment artifacts, and a monorepo.

### Phase 7 — Reconciler
The loop, the state machine, drift classification, backoff, the give-up threshold.

*Done when:* killing a container by hand restores it; killing it repeatedly reaches `failed` and **stays** there.

### Phase 8 — Console
Launcher, admin, the four weight-bearing screens.

### Phase 9 — Backup and DR
Rolling backups, the DR bundle, verify-then-restore, GC with the aggregate disk budget.

*Done when:* **Sequence D** passes, including rejection of a tampered bundle with the target untouched.

### Phase 10 — CLI, MCP, Traefik
The remaining surfaces and the second routing adapter.

**[D]** Traefik lands last deliberately. It is the second implementation of the routing interface, and building it is the test of whether §03 4's abstraction actually holds. If Traefik requires changing the interface, the interface was wrong — better to learn that in phase 10 than to have assumed it was right in phase 3.

---

## 3. Risk register

| Risk | Phase | Mitigation |
|---|---|---|
| Detection quality below the R-103 bar | 6 | Build a corpus of 30 real repos early. Track questions-per-deploy as a tracked metric, not a vibe. |
| Docker socket leaks into a build | 4 | Integration test asserting the build container's mount list. Non-negotiable. |
| Header spoofing through the proxy | 5 | Explicit test with forged headers. |
| Postgres prerequisite undermines the hobbyist install | 0 | **Resolved.** Compose supplies Postgres beside Pando; no prerequisite beyond the container runtime v1 already requires. |
| Routing abstraction is Docker/Traefik-shaped | 10 | Sketch the Cloudflare adapter on paper during phase 3, before the interface is fixed. |
| Required-vs-optional slots (O-4) | 6 | Trial-run promotion fallback (§01 2.5). Measure false-block rate against the corpus. |
| The two planes get conflated again | 1 | The comment in `CheckData` explaining that this was reversed once, plus a test asserting an operator on someone else's app is denied use. |

---

## 4. Open decisions added during design

These extend §24 of the requirements document.

| ID | Question | Where |
|---|---|---|
| **O-11** | ~~How Postgres is supplied~~ — **resolved:** the install topology supplies it (Compose), with an external-database override | §00 1.1 |
| **O-12** | ~~Whether the MCP exclusion list is hard or policy-controlled~~ — **resolved:** policy-controlled, default-closed, expressed as host policy per verb rather than a second mechanism | §04 3 |
| **O-13** | ~~Session revocation mid-websocket~~ — **resolved:** re-authorize on the assertion lifetime, close on failure; falls out of the single revocation window in §06 3.1 | §06 4.2 |
| **O-14** | ~~DR restore bootstrap ordering~~ — **largely dissolved** by O-11; confirm sequencing in phase 9 | §07 D |

**All four are resolved.** Of the ten in requirements §24, three remain open: O-4 (slot detection,
awaiting measurement rather than decision), O-5 (TLS issuance, genuinely per-adapter), and O-6 (which
backup destinations ship — provider-shaped; that a destination is *not* an adapter category is settled
in §03 8.1). None blocks any phase.

A second review also closed three fragilities that were not on any list: the unreconciled revocation
window (§06 3.1), `markUnobservable` having no field to write to (§02 2.3), and R-193's rotation
restart having no detection mechanism (§02 2.4). See `../plan/design-gaps.md`.
