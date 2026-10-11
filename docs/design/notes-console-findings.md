# What building the console turned up

Phase 8 in one sentence: the launcher, the management console, and the four
screens that carry requirement weight. Most of it went the way the design said
it would. These did not.

## R-265 has no administrative verb to hold

> **R-265** Users holding any administrative verb see an **Admin** entry point
> from the launcher, exposing the console scoped to whatever privileges they
> hold.

There is no such verb, anywhere:

- **R-080's catalog is entirely `app.*`** — thirteen verbs, every one scoped to
  a single app. Nothing install-level.
- **Grants are per-app.** `grants.app_id` is `NOT NULL`, so an install-level
  grant cannot be written.
- **Users carry no admin flag.** The first-run "administrator" (R-046) is an
  ordinary local user named `admin`.
- **`app.create` is referenced but does not exist.** Sequence A step 1 says
  "authz: user holds `app.create` (install-level)". It is not in the catalog and
  nothing checks it — `handleCreateApp` checks only that the caller is not
  anonymous.

Verified empirically: `GET /me` returns `principal_kind`, `id`, `groups`,
`user_id`, `email`, `display_name` and `must_change_password`. No verbs.

**What was built.** The half of R-265 that is implementable, which is also the
half the screens that exist need: the management console opens for someone
holding a **control-plane** grant on at least one app. `GET /apps` is
control-plane scoped — a different list from `GET /me/apps`, which is data-plane
scoped (R-070, R-071) — so a non-empty result means "there is something here you
can administer", and what opens is scoped to exactly those apps. The scoping is
the server's, not the console's.

**What is still missing** is install-level administration: users, hosts, host
policy, the audit log. Those need verbs that do not exist — and none of those
screens exist either, so nothing is being hidden from anyone. Recorded as
**O-17**.

Worth noting how much else depends on this. Design 05 §3 says "the console lists
violating apps when a policy is saved, before it is saved"; there is no policy
endpoint and no verb to gate one. R-085's "host policy may disable exec
install-wide" is the same shape.

**Resolved within this phase**, once the privilege escalation this gap allowed was
demonstrated on the shipped stack:
option 1 from `open-decisions.md` — install-scoped verbs held as a grant with no
app, plus a fourth built-in role. `GET /me` now carries `verbs`, and
`app/principal.ts` reads them rather than inferring administration from
`GET /apps`. The six endpoints are gated. The paragraphs above describe what was
true when this note was written, and are left as written; design 06 §2.1 is the
current account. What is still absent is the install-level *screens* — the verbs
and endpoints they would use exist now.

## The design system's lint config does not run

`_adherence.oxlintrc.json` is the phase's stated mechanism: *"Wire
`_adherence.oxlintrc.json` into the console's lint step, so a raw hex or px
fails CI the way the adapter import rule does for R-027."*

It uses exactly two rules, and **oxlint implements neither**:

- `no-restricted-syntax` — absent from `oxlint --rules` entirely, and dropped
  from the resolved config without a word. `--print-config` confirms it.
- `no-restricted-imports` — listed in `oxlint --rules`, with no checkmark in the
  implemented column.

A file containing `{ color: "#ff0000", padding: "12px" }` lints clean. Every
brand rule in the file — hex, px, fonts, and all 24 components' props — is
inert.

The file's *contents* are an ESLint config: `no-restricted-syntax` with esquery
selectors is an ESLint core rule. Only its name says otherwise. So the console
reads the rules out of the vendored JSON and hands them to ESLint, which
implements both, and raises them from `warn` to `error` — upstream ships
everything as a warning, and a warning fails nothing. Read rather than
restated: the design project is the source of truth, and a second copy of forty
selectors would be a second opinion about the brand.

It then caught real violations in this console's own first draft: two raw px
values and one raw hex.

## And its per-component prop rules are wrong

Under ESLint the prop selectors fire — 21 errors, all false. Each enumerates
only its interface's *own* members:

```
<Button> doesn't accept that prop. Declared props: variant, size, icon,
disabled, fullWidth, children
```

But `ButtonProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>,
'size'>`. So the rule rejects `onClick` on a button. It rejects the design
project's own console prototype, which is written
`<Button variant="primary" onClick={onAdd}>Add app</Button>`.

TypeScript checks the same thing correctly from the same 24 `.d.ts` files,
inherited attributes included: `variant="nonsense"` is caught, `onClick` is not.
That is exactly the checking the phase asked the declaration files to provide,
so it is the one kept, and the prop selectors are filtered out with the reason
recorded in `eslint.config.mjs`. Reported upstream rather than corrected here —
PROVENANCE.md makes the design project the source of truth.

## `degraded` has no status symbol

`StatusIndicator` takes `running | building | failed | stopped | info`. Pando's
state machine has eight states, and `degraded` is the reconciler's most
load-bearing distinction: degraded is recoverable and still being worked, failed
is terminal and needs a person (R-151).

Mapping it to `failed` would be wrong twice — marker red on an app that is being
recovered, breaking both that distinction and the under-3%-red rule. It maps to
`building`'s hollow ring, which already means "in flux, being worked on", and
the word beside it says "Degraded". Status is a symbol *plus a word*; here the
word carries the difference. The system should grow a sixth symbol, in the
design project.

## There is no warning tone, and that turned out to be right

`Banner` takes `info | running | building | failed`. Design 08 §1.3 requires
warnings and blockers to be visually distinct, "a warning that looks like an
error teaches people to ignore both" — and the palette deliberately has no
amber: marker red is reserved for three things, one of which is a failed app.

`Banner.prompt.md` also says never to stack banners, and a proposal can carry
several warnings at once.

Both constraints point the same way, and design 08 already said it: warnings go
**inline, where they apply**, not stacked at the top. `ui/InlineWarning.tsx` is
sunken paper, a rule, an `info` symbol and the text — and **no red at all**. An
error is red; a warning is quiet. That is a larger difference than any two tints
would give, and it needs no new color.

## Two build failures from embedding

`//go:embed all:dist` fails the build outright when its pattern matches nothing.
That broke the build twice, in different places:

- `internal/console/dist/` was gitignored, so a fresh clone could not compile the
  Go code at all until someone ran npm. A committed `dist/README.md` keeps the
  pattern satisfied; the binary reports the console is absent and serves the API
  regardless.
- `.dockerignore` excluded `internal/console/dist`, so the image build failed
  inside Docker even with the assets present locally. Excluding the built
  console from the image defeats the point of embedding it (R-253).

## The console needed the internet — fixed

Two substitutions flagged upstream, both of which matter more for Pando than for
most products, because Pando is software someone installs on their own host. An
installation on a private network is a normal way to run it, and there both of
these fail silently — the product renders, just wrong.

**Icons** were fetched from the unpkg CDN *at runtime*, per glyph, on first
render. Offline that meant no icons at all; online it meant every icon appearing
a moment after the rest of the interface. Ten Lucide SVGs (ISC) are vendored in
`assets/icons/` and compiled to a plain JS module by `assets/icons/build.mjs`,
which `Icon` imports synchronously. A module rather than `import.meta.glob`
because the design system has no build step and no npm dependencies — bundler
magic would give that up.

**Fonts** were a `@import` from Google Fonts. The `.woff2` files are now in
`assets/fonts/`: 16 files, 355 KiB, five subsets, so an app named in Cyrillic or
Vietnamese still renders in the real face. `tokens/fonts.css` is Google's own CSS
with the URLs swapped, because the unicode-ranges and subset split are theirs and
re-deriving them is how a subset quietly stops loading.

That second point is not hypothetical — the first attempt *did* re-derive the
names and broke nine of twenty-five faces, because Newsreader and Public Sans are
variable fonts where several `@font-face` blocks share one file while IBM Plex
Mono ships one file per weight. Naming by family and subset alone made the two
Plex files collide and pointed the 400 face at the 500 glyphs.

Verified at the artifact level: `grep` for `fonts.googleapis`, `fonts.gstatic` or
`unpkg.com` across the built CSS and JS returns nothing.

## Exec: the endpoint did not exist

Phase 8's task list says "exec via WebSocket + xterm.js", which reads like console
work. The server side was missing too — no route, no handler — and no phase file
claimed it. Design 04 specified it, so both halves landed here.

One correction to that spec: it wrote `POST /api/v1/apps/{id}/exec`. A WebSocket
handshake is a **GET** by protocol; RFC 6455 requires it and a browser's
`new WebSocket()` cannot issue anything else, so `POST` was not implementable
from the console the endpoint exists for. Design 04 now says GET, and nothing
about the ordering or the checks changed.

The ordering is the requirement, and it is what the tests assert: check
`app.exec`, then host policy (R-085 → `POLICY_EXEC_DISABLED`, which denies the
owner too), then **write the audit event**, and only then open the session. A
session authorized and abandoned without a byte sent is still recorded — there is
a test for exactly that.

R-086 also decides what the screen says. It requires the documentation to state
plainly that the verb list is not a security boundary against someone holding
`app.exec`, so the console says so before opening anything: a terminal can read
the database directly, read the values Pando passed the app including its
secrets, and change the running app in ways that will not appear in its
configuration. And it says what is recorded — the command, not the session (O-7)
— because someone about to type a password is entitled to know which.

## The OpenAPI document is generated, and the console's types do not come from it

The phase asks for "API types generated from the OpenAPI spec — never
hand-written (R-261)". There was no OpenAPI document when the console was built,
and there is no hand-written one now.

`cmd/gen-api-types` reflects over the Go types the handlers serialize instead.
Writing an OpenAPI document by hand to generate from would reintroduce exactly
the drift the requirement forbids, one level removed: the spec would be the
thing that fell out of step. The generated file is committed and CI fails on a
diff, the same way the traceability index works, so a change to the API's
surface shows up in review.

`docs/openapi.json` now exists, and it is generated the same way, from the
code. `make reference` writes it with `internal/openapi`, which reads
`internal/httpapi` as source: the routes from `(*Server).Routes`, each request
body from the type its handler decodes into, each response from what its
handler passes to `JSON` and with which status — a map literal's keys read from
the literal — and summaries, groups and verbs from `routeDocs`. What the code
cannot say (a body written as bytes, a type with its own `MarshalJSON`, SCIM's
resources built in `core/idp`) is in `internal/openapi/overrides.go`, and
generation fails on any answer it could not read rather than guessing.
`make reference-check` diffs it like the rest of the reference.

The console's types still come from `cmd/gen-api-types`, not from the
document: both are generated from the same Go types, so neither is a second
opinion of the other.

## The console could not delete an app, and the content column was capped

Two gaps found by using the console on a wide display.

**Delete.** `DELETE /apps/{id}` has existed since phase 2, with the CLI calling
it and tests covering both answers to R-204's question. The console had no
button for it, so the one surface where R-204's *asks* is literally an ask was
the one surface that could not delete anything.

It sits in the app's header, beside the name, and not on the settings tab. The
app most likely to be deleted is the one with no settings tab: an app whose
source could not be fetched fails detection, never pins a spec, and shows a
single Configuration tab carrying the reason. A delete on the settings tab is a
delete that app cannot reach. The same reasoning covers the app whose record
will not load at all — that screen was a back button and nothing else, and now
carries the error, the name from the list row, and the delete.

The dialog is R-204's question: two radios, keep a final backup or discard the
storage, with keeping selected — the default R-205 gives the CLI, so the two
surfaces do not disagree about what happens when nobody thinks about it. An app
known to keep nothing gets a plain confirmation instead. *Known* is load-bearing:
an unanswered volumes request is not evidence that an app keeps nothing, so
unknown asks the question rather than assuming the empty case and discarding
data.

The dialog shows the server's message when a delete fails and not its remedy.
The remedy names `force=true`, which is the CLI's way of answering the question
the radios already ask; the console answers it with the second radio and says
so.

**[P] The brand spec's 1280px console column is overridden in the admin
console.** The page is not capped and stays left-aligned: a table's rows and
their rules run to the edge of the window. Its *content* is capped, which is a
different thing and turned out to be the whole of the problem. Three attempts,
in order:

1. 1280px, left-aligned — the spec as written. On a 2560px display that is a
   console in the left third of the window and two thirds of empty paper.
2. 1280px, centered. The emptiness moves to both sides, which is worse: now
   nothing is anchored.
3. Uncapped. The rows look right and the content inside them does not — Status
   and Updated end up two thousand pixels from the name they describe, and Add
   app sits in the far corner.

What is actually wanted is rules to the window and columns at a measure. So
every table sizes its columns in `ch` rather than `fr`, summing to 84ch, and
`ui/layout.ts` carries that measure plus the Table's own gaps and padding for
the page headers, so a header's action stops where the last column does. A
header carrying the measure also carries `--type-body-ui`, because `ch` is
measured in the font of the element it is written on and the tables' font is
what the number was counted in.

Prose keeps its own caps where they are written — 68ch, the audit filter at
40ch. Sharing, deploy settings and the terminal take the same measure as
everything else. The launcher is unchanged.

## Logs: one box, showing one deploy, growing without limit

`GET /apps/{id}/logs` — the running app's own output — existed and nothing
called it. The console's only log was the newest deployment's build output, on
Overview, in a box with no height limit: a few thousand lines of build output
pushed the rest of the screen out of reach, and there was no way back to the
deploy before it.

There is now a Logs tab holding both, because both are what somebody means by
"the logs": the app's output, tailed at 500 lines and re-asked every five
seconds while the app runs, and every deploy with its build log. Overview keeps
the current deploy's log, since that is the screen a deploy is watched from,
and every earlier one is a history that belongs with the other history.

Both boxes stop at 60vh and scroll, and follow the end of the stream until the
reader scrolls up — a log that jumps back to the bottom while somebody is
reading the failure three screens above cannot be read at all.

**Deploy logs do not survive a restart, and the console now says so.**
`deploy.LogStore` is in memory, bounded at 2000 lines, and written nowhere. A
deploy from before the last restart has no log, and `Follow` on an unknown
deployment ID opens an empty stream rather than reporting that there is
nothing — so the console would have shown an empty black box, which reads as a
deploy that printed nothing. It says what is true instead. Logs on disk under a
size cap are R-222/R-223 and belong to the reconciler's garbage collection;
they are not implemented.

## A warning that says "define one here", on a screen with no here

R-201's persistence warning ends *"Otherwise define one here"*, and it renders
on Overview, where nothing can be defined. Storage is on the settings tab, four
sections down.

Warning text is rendered verbatim (R-105, design 08 §1.3) so the console does
not get to fix that sentence by rewriting it. It carries a way to the place
instead: `InlineWarning` takes an action, and Overview maps the warning codes it
has a screen for — `WARN_NO_PERSISTENT_VOLUME` to storage,
`WARN_COMPOSE_CONSTRUCT_REWRITTEN` to the configuration,
`WARN_UNDECLARED_DEPENDENCY_SUSPECTED` to dependencies. The storage one opens
the settings tab scrolled to Storage rather than at its top, which is what
"here" was promising. `WARN_PATH_ROUTING_INCOMPATIBLE` has no action, because
the console has no routing screen to send anyone to; a code with no entry
renders as text, with nothing claiming to be actionable.

## Deploy settings are unreachable

`DeploySettings.tsx` implements R-145 and R-147 — start-then-swap, auto-rollback
and deploy-on-push, each explaining itself at the point of enabling, as design
08 §1.3 requires. Nothing imports it. The component exists, is typed, passes
lint, and no route or tab renders it, so neither setting can be changed from the
console. Not fixed here; it needs a decision about where it belongs, and the
settings tab is already four sections long.

## The app-logs endpoint returned Docker's stream framing

`GET /apps/{id}/logs` copies the runtime adapter's reader into the response
body. The Docker adapter handed back `ContainerLogs` unchanged, and a container
with no TTY — which is every workload Pando creates — has its output framed: an
8-byte header before each chunk saying which stream it came from and how long it
is. So the endpoint answered with control bytes at the start of every line.

Nothing had read it. The console had no screen for an app's own output, the CLI
has no `logs` command, and the one place in the codebase that already reads
container output — the trial run's crash capture — strips the framing itself,
in `trial.go`, which is where the knowledge stayed. The adapter now unpicks the
frames as they arrive rather than from a buffer, so a followed log stays live,
and `TestR071_LogsArriveWithoutDockerFraming` asserts that what comes out is
what the app printed.

## Rolling backups were failing for the whole installation

Found while watching the log of a restart, not through the console: the
reconciler logs *"could not list apps for rolling backups: cannot get array
length of a scalar (SQLSTATE 22023)"* on every pass, and the sweep returns
before taking any (R-210).

`AppSpec.Volumes` is tagged `json:"volumes"` with no omitempty, so an app that
declares no storage — most of them — stores `"volumes": null`. The query read it
with `->`, which answers with that JSON null and not SQL NULL, so the
`coalesce(…, '[]')` guarding the call never fired and `jsonb_array_length` was
handed a scalar. One such app failed the query, and the query is the whole
sweep, so no app on the installation was backed up.

The first fix — a `jsonb_typeof(...) = 'array'` test in front of the length
call — still failed. `AND` promises no evaluation order and the planner reached
the length call first. The predicate is now a comparison against `'[]'`, which
is defined for every value that column can hold, and
`TestR210_AnAppWithNoVolumesDoesNotStopTheRollingBackupSweep` asserts it against
a real Postgres.

## Light and dark, and who decides

The design system has shipped both themes from the start — `data-theme="dark"`
on the root, a full second set of color tokens — and nothing in the console ever
set the attribute, so an installation was paper-white at three in the morning
whatever the machine was set to.

`ui/theme.ts` applies it, with three states rather than two. "System" is the
default and is not a third appearance: it is the absence of a choice, and it
keeps following the machine when the machine changes. Choosing light or dark
records that choice in `localStorage` and stops following — per browser, because
the same account on a bright desk and in a dark room wants different answers and
neither belongs in the database. It is applied at the root of the app, above the
sign-in page, which is the screen most likely to be met in the dark.

The control is a ghost button naming what it will do — "Dark" in the light
theme, "Light" in the dark one — because a toggle labelled with its current
state reads as a statement and gets clicked by people who wanted the opposite.
It sits in the launcher's header and the console's sidebar footer. Nothing else
needed changing: every color in the console already comes from a token, and the
logo is drawn from `--ink` and `--marker` rather than loaded as artwork.

## Smaller things the same session turned up

**A screen's action belongs beside its heading.** Add app was opposite the
heading, at the far end of the content measure, which on a wide window is a
long way from the word it belongs to. Both page headers — the apps list and the
shared install screen — now put the action directly after the title.

**An empty terminal is not a log.** A finished deploy whose output Pando no
longer holds rendered on Overview as a black box saying "Deploy log ·
succeeded", with nothing in it and nothing to scroll. It now shows the deploy's
result and a way to the Logs tab, and the box is drawn only when there is
something in it.

**An app opens in its own tab.** The launcher's tiles and the address on an
app's Overview replaced the console with the app, leaving the browser's back
button as the way back to what you were doing.

**The console does not explain itself.** "This is the running app's own output,
kept by the runtime — not by Pando — so it goes when the app is redeployed" was
written for a reader who asked, and the reader of a log has not asked anything.
Both section preambles on the Logs tab are gone; what is on the screen says what
it is.

## The manual is now built from the thing it describes

Pando ships three surfaces — the API, the CLI, MCP — and documented none of
them in a way that could stay true. `GET /api/v1/logs` existed with no client;
the README listed the MCP tool names by hand; `docs/reference.md` pointed at a
design document, which describes intent rather than what the binary serves. All
three are the same failure: a description maintained by remembering.

`internal/reference` assembles a document from the things themselves. The CLI
half is walked out of the cobra tree `pando` actually runs, so a new command or
flag appears with no further step. The MCP half is the tool list the MCP server
actually announces. The error codes are `errs.Catalog()`, which
`TestEveryCodeIsInTheCatalog` checks against this package's own parsed source —
Go cannot enumerate constants at runtime, so a new code with no documented
meaning fails the build.

The API half cannot be read the same way: chi knows a route's method and pattern
and nothing about what it is for. So `internal/httpapi/reference.go` keeps a
table of summaries beside the router, and `TestR261_EveryRouteIsDocumented`
walks the real router and fails **in both directions** — an endpoint with no row,
or a row with no endpoint. It caught its first drift immediately: the reference
endpoint was documented before it was routed.

Three readers, one document:

- `GET /api/v1/reference`, which the console's **API and tools** screen renders.
- `make reference`, which writes `docs/api.md`, `docs/cli.md` and `docs/mcp.md`.
- `make check` and CI, which fail on a stale one — the same shape as the
  generated API types and the requirements index.

**The screen is open to anyone signed in, and the endpoint to anyone at all.**
The document holds nothing about the installation — no app, no account, no
policy — and the same text is in the repository. A client that must authenticate
before it can read how to authenticate is a product with one client.

**One thing the generator caught in the code rather than in the docs.** The
first draft of `docs/cli.md` said to set `PANDO_SERVER` and `PANDO_TOKEN`, which
is the obvious way to give CI a token — and the CLI read neither. It read a file
under the user's home directory and nothing else, so a token minted for a
container could only be used by writing that file by hand. Rather than document
the gap, `cli.New` now reads both, the environment beating the stored file and
`--server` beating both, with the names exported as constants so the generated
page quotes what the code reads.

## Service tokens had a schema, a store and no endpoint

R-060 has been implemented everywhere except where somebody could reach it:
`state.TokenAccount` exists, the tokens table takes it, `grants.principal_kind`
includes `token`, and `authz` resolves one as its own principal. There was no
route, so an installation could not have a service token at all.

`POST /tokens/service` mints one, behind `install.users.manage`. The verb is the
one that creates and deletes accounts, because a service token is something that
acts like one — its own principal, in the audit log under its own name,
outliving whoever created it. No new verb: a verb that only ever travels with
another is a list to maintain and nothing to enforce.

A token still cannot mint a token, the same rule delegated tokens have and for
the same reason — a stolen one that can renew itself makes revoking the original
pointless. And a fresh service token holds nothing at all; it reaches an app when
somebody shares one with it, which is the R-060 shape rather than an
administrator's second credential.

## Controls in a table row need their own padding

A 36px `Select` in a 40px row leaves two pixels above and below it, so a column
of them reads as one solid block with hairlines through it, and a wrapped row of
permission tags had nothing between it and the rule underneath. The Table sets a
minimum row height rather than a height, so the fix is padding on the cell and
the row grows to fit.

## Installing the CLI, without a second copy of the truth

The reference told people how to connect the CLI and not how to get it. The
facts are in `.goreleaser.yaml` — a Homebrew cask in `bemeek-io/homebrew-tap`,
`.deb`, `.rpm` and `.apk` packages, tarballs for four platforms, the module path
for `go install` — and the server has no release config beside it at runtime, so
it cannot read them when it serves the page.

They are constants in `internal/reference/install.go`, and
`TestInstallInstructionsMatchWhatATagPublishes` reads `.goreleaser.yaml` and
`go.mod` and fails when the two disagree: a renamed tap, a dropped package
format, an added architecture, a moved module. A second test does the same for
the README's install section, which is prose and stays prose but contains four
strings a release can invalidate.

Install instructions are the first thing anybody reads and the last thing anybody
checks. They are correct the day they are written and nobody notices when they
stop being, because the person they are wrong for is not on the team.

## A log stream that never ends, twice

The overview's deploy log rendered as a black box with a title and nothing in
it. It was fixed once, by showing the deploy's result instead when the stream
ended with no lines — and it came back, because for the case that produces it
the stream does not end.

`deploy.LogStore.Follow` on a deployment ID it has never heard of does not say
so: it creates an empty stream, returns an open channel, and the handler holds
the connection waiting for lines that will never arrive. That is precisely the
state of every deploy from before the last restart, which is the state that
shows the empty box. The first fix listened for `end` and `error`, and neither
fires.

Two changes. The console treats four seconds of silence on a *finished* deploy
as the end, which is the only signal available to it. And the overview no longer
carries a log at all: the security score is in that column, the deploy's result
is a row on the card beside it with a way to the Logs tab, and the log itself
lives where the app's other histories are.

The server side is worth fixing too — `Follow` could report that it holds
nothing rather than opening an empty stream — and is not fixed here.

## A style object whose keys were not CSS

The score badge rendered as plain text. Its tones were written
`{ fg, bg, border }` and spread onto the element: React drops `fg` and `bg`
because they are not properties, and `border` — spread after the `border:
"1px solid"` shorthand — replaced it with a bare color, which is not a valid
shorthand, so the browser dropped the border too. Three colors, none of them
applied, and nothing anywhere reports it: an unknown style key is silently
ignored, which is the one thing about inline styles that makes them worse than a
stylesheet.

They are `color`, `background` and `borderColor` now, with `borderWidth` and
`borderStyle` separate so a spread cannot take the shorthand with it. Verified by
reading the computed style out of a live page rather than by looking at it.

**And the shield is drawn in the console.** The design system's vendored icon
set has ten glyphs and none of them is a shield, so `ScoreBadge` carries the
path at the brand's 1.5px stroke, filled with its own status tint. When a shield
is added upstream this becomes an `<Icon name="shield">` and nothing else
changes — the stroke weight and both colors are the system's already.

## The policy screen is sections now, and covers the whole document

A flat column of switches was fine at four settings and stopped being fine at
nine. It is five sections separated by rules — where apps come from, who can
reach them, isolation and egress, tokens and agents, security scanning, data and
destruction — each with a line saying which question its controls answer. Rules
rather than cards, which is the system's own answer for settings on one page.

Grouping it turned up the larger problem: the screen carried nine of the
document's fourteen fields. An installation could not set its isolation floors,
its egress allowlist, its token lifetime cap, the verbs an agent's token may not
use, or its log disk budget without calling the API by hand — which is R-261's
gap in the other direction, a capability the API has and a client does not. All
five are there now.

One bug fell out of it: `min_build_isolation` and `min_runtime_isolation` were
typed `string` in the console and are ordered integers on the wire (R-114, gaps
of ten so a class can be inserted without a migration). Nothing had rendered
them, so nothing had noticed.

## A deleted app kept its name

Delete `crewmate`, add `crewmate` again, and Pando answered "an app named
crewmate already exists" — pointing at a row the person had just deleted and
can no longer see anywhere in the console.

`apps.slug` was `UNIQUE` across every row, and a deleted app keeps its row on
purpose: the archive is what a backup taken on delete refers to (R-204), what an
audit event's target resolves against (R-227), and what the reconciler tears the
bundle down from. None of that needs the name.

The constraint is a partial unique index on live rows now. Two live apps still
cannot share a slug — a slug is an address (design 03 §4.2), and two apps at one
address is what it was for — and an archived app is at no address at all: its
routing is removed, its bundle destroyed, and the proxy's own lookup has always
filtered `deleted_at IS NULL`, so a reused slug cannot reach the old app even in
the window before the janitor runs.

## Deploy was under everything on the page

The overview grew — a status card, a security score with its findings, the
warnings — and Deploy stayed at the bottom of it, so deploying meant scrolling
past everything to reach the one thing somebody came to do.

It is in the app's header now, beside Delete: an action on the app rather than
a part of the overview, and reachable from every tab rather than from one. It
is the view's single primary button, which is the brand's rule, and it keeps its
own query of the app's revisions rather than taking one as a prop — passing the
revision down is how a button ends up shipping a spec that changed while
somebody was reading the page. It appears only once an app has a configuration
to deploy; before that the thing to do is accept one.

## An app that could be accepted and could not be deployed

crewmate imported a compose file with two services, was accepted, and then
refused every deploy: "none of this app's workloads is marked as the primary
one". The message was correct and there was nothing anywhere to act on it —
Deploy raised it, the decision belongs to the configuration tab, and no screen
has a control for marking a workload primary.

Three things were wrong, and each is fixed where it went wrong.

**Nobody elected one.** Design 01 says the detector asks rather than picking,
and it does — but the question rides on the *compose candidate*, and this
repository had a Dockerfile as well, so the tie-break was the question a person
saw. Answering it adopts the compose reading, whose own question was never put.
`WithAnswers` now elects a primary when no answer names one: the workload that
serves HTTP and that nothing depends on, which is the shape of a web service in
every ordinary compose file. **[P] overriding that [D]** — it still asks, and
now it also picks, and it says which it picked in a warning (R-102, R-104).

**Accept pinned it anyway.** The spec was never validated before being written,
so a spec that could not deploy was accepted quietly and refused later, by a
different button, on a different screen. Accept validates now, and the refusal
arrives where the answer is.

**And the refusal moved the header around.** The deploy error rendered inside
the header's action cluster; it is three sentences, so it widened the row and
shoved Deploy and Delete sideways. It is a banner under the header now, where
the app's other banners are — the button reports the refusal upward rather than
drawing it.

## The compose file answered the question Pando then asked

crewmate's compose file runs `db: image: postgres:16-alpine`. The importer read
that, correctly created a required `DB_URL` slot of type postgres — and left it
unfilled, so an app that worked under `docker compose up` was accepted and then
refused at deploy: "this app needs a PostgreSQL database, and one hasn't been
chosen yet."

The answer was in the file being imported. `services: db: image: postgres` says
the app runs a database beside itself, and "Pando runs one inside this app" is
that same sentence in Pando's vocabulary (R-131). Compose-derived slots now
arrive resolved to `provisioned`, with the evidence still naming the service, and
somebody who wants a different answer changes it on the settings tab.

Slots read from a `.env.example` are deliberately not filled this way: a variable
named `DATABASE_URL` is evidence that the app wants a database, not that its
author meant Pando to run one (R-133, O-4).

**And the review screen now says what an app needs before anybody accepts it.**
The first anybody heard of a required slot was the deploy refusal — after
accepting, from a button on another screen. It belongs on the page where
somebody reads how their app will run: "it needs a PostgreSQL database, and Pando
will run one inside it" is part of how it will run.

**Refusals carry the way to their fix**, the same as the warnings on the
overview: `PLAN_SLOT_UNFILLED` offers dependencies, `VALID_PRIMARY_WORKLOAD`
offers the configuration, `PLAN_SECURITY_BELOW_THRESHOLD` offers the findings. A
code with no screen keeps a plain dismissal — nothing claims to be actionable
when it is not.

## A mount the daemon refused, at the last step, without saying why

The deploy log ended:

```
=> Starting the app
!! apply failed: Could not create "proxy".
```

Two separate failures, one after the other.

**The log did not say why.** The build and scan steps print the cause of a
failure; every step after them printed only the adapter's message, and an
adapter's message names what it could not do, not why. The reason was in the
error that message wrapped and went to the server's log, where the person
deploying cannot see it. All three steps share one writer now — headline, cause,
remedy — so the last line of a deploy is never a dead end (R-105).

**And the cause was a mount that could never have worked.** crewmate's compose
file has `./Caddyfile:/etc/caddy/Caddyfile`. The importer turned every relative
bind mount into a volume Pando manages, which is right for `./pgdata` and wrong
for a single file, twice over: nothing is read from the repository at deploy time
(R-020), so the volume comes up empty, and Docker will not mount a directory over
a file that exists in the image at all — `source /var/lib/docker/rootfs/…/etc/
caddy/Caddyfile is not directory`.

A file bind mount is now an R-099 rejection, named at discovery with the line to
change and what to do instead: `COPY Caddyfile /etc/caddy/Caddyfile` in the
service's Dockerfile does what the mount was doing. A path the repository does
not hold is still read as a directory — compose creates those on first run, and
refusing on the shape of the path alone would refuse most real stacks.

The Docker adapter names the mount too, for the apps that already carry one and
for a mount typed in by hand. The daemon's path is the container's rootfs with
the mount on the end, so the mount that appears in it is the one that failed.

**And the same app's environment held `${APP_DOMAIN:-localhost}` verbatim.**
Mount sources were interpolated at import; environment values were not, so a
compose substitution arrived at the container as its own twenty-four characters.
They are resolved the same way now. One with no default has no value to resolve
to: it keeps its spelling and raises a warning naming the variable, because an
empty string would be an app misconfigured with nothing to show for it (R-102).

## Eleven dependencies, none of which were dependencies

An app's dependencies listed `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` and
`POSTGRES_PASSWORD`, and filling one offered "connect to one that already
exists" with a box for a connection string. A push-notification key is a value.
There is nothing to connect it to.

`slotsFromEnvExample` made a slot of every assignment in `.env.example`, typed by
substring: anything matching a service word got that type, everything else got
`unknown`. So `POSTGRES_PASSWORD` was a database — it contains the word postgres
— and a password was offered a connection string. `UPGRADE_URL` would have been
one too: "UPGRADE" contains "PG".

R-130 already says what to do. A variable is a hole *with a type*, and the type
comes from the URL scheme in the sample value, the variable name, or a compose
image name. Read that way it sorts itself: a name types a variable only when the
name says *connection* — `DATABASE_URL`, `REDIS_URI`, `PG_DSN` — and a value
types it whenever it carries a scheme Pando knows, so `CACHE=redis://…` is a
Redis whatever it is called. Words are matched whole rather than as substrings.

Everything else is a variable, and lands on the app's variables declared with
nothing in it. The sample value is deliberately not carried across:
`POSTGRES_PASSWORD=changeme` filled in is worse than empty, because it looks
answered.

`HOST` is not in the connection list, though `POSTGRES_HOST` is a connection
target in the ordinary sense. An app spelling it that way pairs it with a port, a
user and a password, and no slot resolution fills four variables from one answer.

Apps pinned before this still carry unknown-typed slots. The console reads one as
what it is — "a value" — and its dialog says nothing about connecting.

## A list of everything the app reads, and no way to say what it reads

Moving untyped variables off the dependencies list put them on the environment
list, where the only action on one was Remove. Eight names, no values, and a
delete button.

**A variable can be given a value now**, through the same dialog that adds one:
the name fixed, the kind — an ordinary setting or a secret — chosen, the value
typed. The kind defaults to secret when the name reads like a credential, and
says so when it does not: a plain value lives in the spec, and the spec is
exportable and meant to be safe to hand to somebody (R-190, R-191). PUBLIC wins
over KEY, because a VAPID public key is published to browsers by design. It is a
default and a sentence, never a refusal — which variables are credentials is the
person's knowledge, not Pando's.

**And nothing had said they were there.** Accepting a configuration produced the
list and no mention of it. The review screen lists them before anybody accepts,
under "Variables with no value", and the overview carries a warning naming the
first few with the way to the screen that fills them. Counted from the spec each
time rather than stored in it: a warning written at detection would still be
there after they were filled.

**An unfilled one no longer reaches the container as an empty string.** Setting a
variable to nothing is a different thing from leaving it unset, and the
difference decides what an app does — `if "APP_BASE_URL" in os.environ` is true
for an empty one. A variable Pando has no value for is not set at all. An empty
value somebody typed is kept: that is an answer.

Never a blocker. Which of an app's forty variables actually matter is what the
trial run is for (R-133, O-4), and a warning that blocks is an error wearing a
warning's clothes.

## Two databases, and an app connected to neither

`crewmate: DATABASE_URL is required`, on repeat, from an app whose compose file
set `DATABASE_URL` and whose deploy log said it had provisioned a PostgreSQL.
Three separate faults, stacked.

**The compose import ran the database and asked Pando for a second one.** A
`db: image: postgres:16` service became a workload *and* a slot, and the slot
was resolved `provisioned` — so the deploy started the compose container and
provisioned a Pando-managed PostgreSQL beside it. Nothing connected the app to
either: the slot filled no variable, and the app's own `DATABASE_URL` still read
`postgres://crewmate:${POSTGRES_PASSWORD}@db:5432/crewmate`, interpolated from a
shell that does not exist here.

A recognized backing service is now the slot and not a workload. Pando
provisions it, manages its storage, backs it up with the app — and the variables
whose value pointed at that service are rewired to the slot, so the app is wired
the way its author wired it. The slot takes the variable's name: `DATABASE_URL`,
not the invented `DB_URL`, because a name somebody reading the app will
recognize beats one Pando made up. The compose volume that belonged to the
dropped container goes with it; the provisioned service brings its own.

**And the app Pando actually deployed was not the compose file at all.** The
repository has a Dockerfile too. A Dockerfile that declares a port bids 0.95,
compose bids 0.88, so when the compose file was refused for its single-file bind
mount, the refusal lost the auction quietly and the app was imported as the
Dockerfile alone — no database, no second service, and nothing anywhere saying a
compose file had been read and rejected. A refused compose file now wins the
auction it would otherwise have lost: it is the author's complete statement of
what the app is (R-096), and R-099 says the reason is shown, not filed away
under a reading that discards it.

**The refusal also names a proxy as one.** A mounted config file usually belongs
to a reverse proxy in front of the app, and under Pando that service has no work
left to do (R-023). When the image is a known proxy the refusal says so, which
turns a file somebody has to relocate into a service they can delete.

## "Seems like it should be able to run"

It should, and refusing it was the wrong call.

The reasoning behind the refusal was sound as far as it went: storage Pando
manages is a directory, Docker will not mount a directory over a file in the
image, and R-020 forbids reading the repository at deploy time. What it missed
is that R-020 says the *spec* is the sole record of how an app runs — and a file
read once at detection and stored in the spec satisfies that completely.
`Build.GeneratedFiles` already works exactly this way, with a comment making the
same argument: "they live in the spec because R-020 makes the state store the
sole record of how an app runs."

So a single-file bind mount is carried now. `spec.File` holds the path, the
content and the mode; the runtime places it between create and start, which is
the only moment available — the container's filesystem exists and nothing has
read it yet. Directories above it are created, so an image without `/etc/caddy`
is an ordinary image rather than a failure. The Docker adapter reports
`SupportsCarriedFiles`, and a runtime that cannot do it is a plan-time refusal
rather than an app that starts without its configuration (R-254).

Two things that are easy to get wrong and are tested:

**A changed file has to recreate the container.** Content copied in after create
leaves no trace in the container's configuration, so `matchesPlan` would have
said "already running" and an edited Caddyfile would never ship. The container
carries a digest of its files as a label.

**A carried file is a snapshot.** Editing the repository changes nothing until
the app is read again, and the import warning says so in those words. That is
R-020 working rather than a gap: a file fetched from a branch at deploy time
would make the same revision deploy differently tomorrow.

What is still refused is what cannot travel in a spec: larger than 64 KB, or not
text. Both say which of the two it is, and both point at the same alternative —
a `COPY` line, because a file that size is a build input.

## The reconciler replaced an app with a second copy of its own proxy

crewmate came up, said degraded, and served nothing. Both of its containers were
running Caddy — the proxy, and the application, which had been built correctly
and then thrown away.

Three things lined up, and each is worth fixing on its own.

**One image per app.** A deployment records `image_ref`, and for a compose app
that is whatever `primaryImage` returned: the primary workload's, which here was
the proxy. The reconciler plans from that single reference, so every workload
whose own image the plan does not name took the proxy's.

**One digest per app.** `ExpectedDigest` was compared against every workload, so
the application container — running its own image, correctly — was permanently
"running the wrong image". That is classified reconcilable, so the loop
corrected it: recreated from the plan, which had Caddy in it. Fifteen seconds
after a good deploy, the app was gone.

**And the digest was not even the primary's.** `primaryDigest` returned the
first running workload the runtime happened to list.

So deployments record what each part ran — `workload_images` as
`{"app": {"ref": …, "digest": …}}` — the reconciler plans from that, and each
workload is compared against its own digest. A workload with no record is not
compared at all, because unknown is not drift.

Two guards for what is left over. The app-level image is no longer substituted
for a workload that builds its own: empty is the honest answer, and nothing
starts a workload from an image Pando cannot name. And a missing workload the
plan cannot name an image for is reported rather than recreated (R-148,
"reconcile when possible, report when not") with "deploy this app again" as the
remedy — which is exactly what an app deployed before this change needs.

## An app made of parts had one state, one log, and no way to ask about either

crewmate said degraded. Its application container was restarting every two
seconds on a missing variable, and the only place that was visible was
`docker ps` on the host — which is the thing Pando exists so nobody has to run.

**`env_file:` was read by nothing.** crewmate's compose file says
`env_file: .env`, and the importer had no such field: the app got the two
variables spelled out under `environment:` and none of the other eight. The file
itself is almost never committed, because it holds the app's own passwords, so
the import reads the template beside it — the `.env.example` the README tells you
to copy — takes the names, leaves the values empty (R-130), and says so in a
warning naming them. `environment:` still wins over `env_file:`, which is
compose's own precedence.

**Status is per part now.** `GET /apps/{id}/status` reported the adapter's own
struct, serialized with Go field names in an API that is snake_case everywhere
else. It returns a typed list: name, whether it is the part the address resolves
to, running, **restarting and how many times**, health, exit code. Restarting is
the field that matters — a container the runtime keeps restarting is up at
almost every instant anybody looks at it, which is the reading that makes a
broken app look fine.

**And logs are per part.** The endpoint has always taken `?workload=`; nothing
passed one, so an app made of three containers showed one log and the container
that was actually crash-looping had no screen at all. The console has a picker,
`pando logs` has `--workload`, and `pando_get_logs` takes one.

All four surfaces move together, because R-261 says they must: the console's
Parts table, `pando app status`, `pando_get_status`, and the API they all read.

## Stopping an app, and a deploy history that says what happened

**The only way to take an app down was to delete it.** `POST /apps/{id}/start`,
`/stop` and `/restart` have existed since the lifecycle handlers were written
and no surface called any of them — so the control somebody reaches for when an
app is misbehaving was the one that also asks what to do with its data and
cannot be undone. The console has Stop and Start in the app header beside
Deploy, the CLI has `pando app stop|start|restart`, and MCP has the three tools.
Stop is desired state rather than an act, so a stopped app stays stopped across
a restart of Pando itself (design 05).

The confirmation says the thing that makes it different from Delete: nothing is
removed, the storage and the address are kept, and starting brings back the
version that was running.

**And "Deployed" was a true answer to a question nobody asked.** crewmate's
history read Deployed, Deployed, Deployed for an app that had never once served
a request. The deploy's status says whether Pando did the work — built, applied,
routed — and it did; what the reader wants to know is whether the app came up.
The deploy already knows: it waits for health and pins the app as `degraded`
when it does not arrive. That state is recorded on the deployment now, and the
list reads "Deployed, not healthy" with the same symbol a degraded app carries
everywhere else. A deploy from before the column is unchanged, because repainting
an app's whole history on an upgrade would be its own lie.

## "Pando has stopped trying to start this app", and the restart count climbing

Both true at once, which means one of them was a lie.

**The runtime was doing the restarting, not Pando.** Every workload was created
with Docker's `unless-stopped` policy, so a container that exited came back
immediately, forever, with no backoff and no end. That is the seat R-149's
backoff and R-150's give-up rule are meant to occupy: an app that cannot start
was restarted every two seconds while Pando counted ten failures, gave up, and
said so — and the loop carried on underneath the message.

It also hid itself. A container restarted that often reports Running at almost
every instant the reconciler looks at it, which is why the app read healthy for
so long before it read degraded.

No restart policy now. A workload that exits is started again by the next tick,
which is fifteen seconds rather than instant, and that is the trade: a restart
Pando paces and counts beats one it never sees.

**And giving up stops the app.** The sentence has to be true of the host as well
as of the state machine, so reaching the threshold stops the bundle. Nothing is
destroyed — the containers, the storage and the address stay, and the app is one
deploy away — what stops is the churn.

**A failed app can be stopped and started by a person.** The reconciler has no
code path that touches one, which is how R-151 is enforced, and that left both
lifecycle endpoints converging to nothing for exactly the app somebody most
wants to act on: Stop recorded a desired state nothing honored, Start recorded
one nothing acted on. R-151 says a failed app stays failed "until a human
intervenes" — so Stop now stops it directly, and Start clears the failure count
and hands it back to the loop. Neither is Pando retrying on its own, which is
the thing the requirement forbids.
