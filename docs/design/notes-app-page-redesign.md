# The app page redesign

The app page had grown one setting at a time until nobody could tell what it was for: the Overview
mixed editing the app, its live status and a scatter of warnings, and two tabs held what was really
one screen of settings. This note records what replaced it, decided with the maintainer over a series
of mockups in October 2026. Design 08 §1.3 still governs the screens that carry requirement weight;
this is the layout and wording those screens now use.

Copy throughout follows one rule the maintainer applied to every round: a short title, at most one
short line under it, no stacked caveats. "Deployments", never "deploys", in anything a person reads.

## Tabs

**Overview · Events · Sharing · Settings.** The Logs and Terminal tabs are gone (a side panel replaces
them, below), and Configuration is folded into Settings. A link to an old tab (`?tab=logs`,
`terminal`, `detection`, `resources`) lands on its new home rather than on nothing.

## Overview

Top to bottom:

1. **Header — what the app is.** The launcher image as a tile with a pencil badge (change image), the
   name with a **Rename** button, the status, and the security score as the shield badge — shield and
   number, never "/ 100" (R-320). Under the name, the address with **Copy** and **Change address**.
   Deploy, Stop and Delete stay where they are. Nothing else: no "who can open it" line.
2. **Needs your attention** — shown only when it has something in it. One list replaces every banner
   and inline warning that used to sit above and below everything else: unshipped configuration,
   variables with no value, a security score below the host's minimum, a backup that was not taken,
   spec warnings. Each item is one sentence, the action that fixes it, and **Ignore**. Ignored items
   leave the Overview entirely (R-412); when everything is ignored the section is not drawn at all.
   A failed app still gets its banner — that is not a to-do, it is the state.
3. **Source and deploys** — one compact card. A source line (repository and branch, image and tag, or
   uploaded files) with what is running and **Deployment log · History**; an update strip only when
   something newer exists (a newer commit to deploy; a newer build of the tag to **Upgrade**, R-143a;
   for an upload, a drop target for new files); then the four deploy settings as switches in one row
   with a few words each — Deploy automatically (repositories only), Require approval, Start new version
   first, Roll back failed deploys. Turning on "Start new version first" shows R-145's sentence in full
   and asks before it saves. **Change source** sits beside the heading.
4. **Workloads** — one card per workload: status, restarts, and memory, CPU and disk as sparklines with
   the current value beside them (R-245). Memory and CPU carry a dashed line at the limit; a workload
   near its memory limit is drawn in the warning brown. A **Refresh** button and "Since you opened this
   page" above the cards. Each card ends with three labeled icon buttons, always bottom-left:
   **Logs**, **Terminal** and **Resources**. Terminal is offered only to holders of `app.exec` and not
   on a workload that is not running; Resources only to holders of `app.resources.override`.
5. **Security** — score, when it was scanned, the host's minimum, the top findings, and a link to all.

### Changing a workload's resources

The **Resources** button opens a dialog with the workload's memory and CPU, each showing its default and
what it uses now, and how much the host has left unallocated (R-242). Raising a workload above the
app's limit raises the app's limit to match, and the dialog says so before it saves; every other
workload keeps the limit it had, written into the spec as its own, so one change never quietly grows
the rest (R-241a). Saving writes a revision; the Overview's attention list then offers to deploy it.

## The side panel: Output, Deployments, Terminal

Logs and Terminal are not tabs. A panel slides over the app page at three quarters of the window, with
**Full screen** and **Close**, and its own address (`?panel=output&workload=worker`), so a reload and
any link still land on it. Three views:

- **Output** — what a workload is printing: a workload picker, **Follow new lines**, **Copy**.
- **Deployments** — the history on the left; the chosen deployment's build log on the right with
  **Copy** and **Roll back to this**. The list starts with **Not deployed yet** when a saved revision is
  newer than the running one, with **Deploy** and **Compare**. This replaces the old Revisions list:
  rollback lives on each deployment, and a revision saved and replaced before anyone deployed it is in
  Events and the audit log.
- **Terminal** — the workload picker, then before connecting one line of warning, R-086's: *A terminal
  has everything the app has, including its database and its secrets. Changes made here don't appear in
  its configuration.* — and **Open terminal**.

## Events

A plain table, **Time · Event · Actor** (the audit log's own column names where they overlap): one
line per event, a symbol only on successes and failures, the detail after a dash, day divider rows.
All thirty days the outbox keeps (R-366) in one scrolling box with the header pinned; no filters and no
"show older". Under it: *Anything older than 30 days is in the audit log*, linked for those who may read
it. A row opens its details in the side panel. Subscriptions are a column of cards on the right:
destination, the events it sends, on or turned off (with the reason and **Turn on**, R-370), and
**Send a test · Edit · Deliveries**.

## Sharing

**Public access** is a card with two switches — **Make public** (*Accessible to anyone on the
internet*) and, under it while on, **Require passcode** (*Visitors need the passcode to open it*). Making
an app public adds to the people it is shared with; it never replaces them (R-075, R-264), which is why
it is a switch and not a choice among three. The passcode is shown, with a reveal toggle and **Copy**,
only while it is being set or changed: Pando keeps its digest and nothing else (R-075a). A host that
forbids either shows the switch disabled with a few words of why (R-076).

**People and groups**: a share bar (person or group, the access to give, **Share**), then a table with
**Opens it** and **Manages it** as separate columns, because they are separate planes (R-070, R-071).
A note under it says Administrators and App managers can manage every app.

## Settings

A list on the left that is real navigation — one page at a time, each with its own address:

- **General** — Build (what detection found, **View build plan**, **Run detection again**), Inactivity
  (**Stop when unused**, **Delete when unused**, R-393 – R-397), and Ignored notices with **Restore**.
- **Variables** — one card per workload, because the spec keeps variables per workload
  (`Workload.Env`). Each row: the name, the value with a lock inside its right edge (closed is a secret,
  R-190), **Generate**, remove. A value a dependency supplies is read-only and says where it comes from;
  an empty one is outlined in brown. **Add variable** inside each card; one **Save changes** bar.
  The plan screen uses the same editor, adding a source line under each name and the Required tag
  (R-132) — and with it a variable added there now belongs to the workload whose card it was added in.
- **Dependencies** — one card per slot: the service and its variable, a status ("Pando runs it",
  "Connected", "Optional, not set up", or "Needed to deploy" in brown), and R-131's three ways as one
  control: **Pando runs one · Use an existing one · Enter a value**.
- **Storage** — disk use against the app's limit (R-403), then each volume with its workload, path and
  size. A volume a Pando-run service keeps is tagged with that service and cannot be removed from here.
- **Egress** — a line saying where the host stands, then one list of addresses. What the host sets is
  shown with a lock and *Set by this host*, read-only here; what this app adds can be removed, and an
  addition that loosens the host's rules says *Needs approval to deploy* (R-183). With nothing set
  anywhere: *Unrestricted. This app can connect anywhere.* The console no longer offers to exclude a host
  address for one app; the API still does (R-182).

## Contour background

The terrain behind every console page — the app pages, the launcher, account settings and sign-in —
is drawn one step darker than the paper instead of in contour brown: `--contour-background` is
`#0F110F` in the dark theme and `#E7E3D8` in the light, at full strength. The brown contour line stays
where the brand wants the map to stand out: empty states, the 404, generated app tiles.
