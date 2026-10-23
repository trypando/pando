# Pando design system — "Topo map"

> **Imported verbatim from the Claude Design project.** The Index and file paths below
> describe *that project*, not this directory: the guideline cards, UI kits, per-component
> `.d.ts` and `.prompt.md` sidecars and specimen pages were deliberately not vendored.
> `PROVENANCE.md` lists what is here, what is not, and how to fetch anything missing.
> This file is otherwise unedited so it cannot drift from upstream.

Pando turns any repo into a running, shareable app: point it at a repository and it
builds, runs and hands back a URL, with no pipelines and no per-app setup. The people
who use it include developers, but also the people who just *made* an app with an AI
tool and now need it online — so the product explains itself in plain words and asks
only what it genuinely can't work out.

Pando is named after the aspen in Utah that grew into a forest from a single root
system: one organism, thousands of trunks. The visual language borrows from the printed
survey quadrangle — the folded topo map hikers and foresters have trusted for a century.
It should feel long-trusted, never like a period costume: layouts are spacious and
current, and the heritage lives in color, type and one illustration system.

**The one bold element is the contour map. Everything else stays quiet: paper, ink, thin
rules and plain type.** If a screen already has a contour illustration, it gets no other
decoration.

## Sources this system was built from

| Source | Status |
| --- | --- |
| Company description ("Set up once. Deploy everything. Pando turns any repo into a running, shareable app, with no pipelines or per-app setup.") | Provided in chat. |
| **Pando design system: Topo map** — the written brand spec (principles, color with measured contrast, typography, layout, shape, contour illustration system, logo, components, icons, motion, copy, avoid list, CSS variables) | Provided in chat, 12 sections. **This document is the ground truth for everything here.** |
| Codebase / repository | **Not provided.** No GitHub repo, local folder or code was attached. |
| Figma file | **Not provided.** No file or link was given. |
| Screenshots, slide decks, marketing copy | **Not provided.** |
| Logo files, illustrations, photography, icon assets, font binaries | **Not provided.** See *Assets* below. |

Because no code or design file exists for the product, the two UI kits in this project
are **built from the spec's own wireframes and rules, not recreated from a shipped
product**. They are faithful to the spec and internally consistent; they are not
evidence of what Pando's UI actually looks like today. Each kit's README repeats this.

## Index

| Path | What's there |
| --- | --- |
| `styles.css` | The single entry point consumers link. `@import` lines only. |
| `tokens/` | `fonts.css`, `colors.css` (light + `[data-theme="dark"]`), `typography.css`, `spacing.css`, `radius.css`, `elevation.css`, `motion.css`, `base.css`. |
| `components/` | React primitives, one directory per concern, each with `.jsx`, `.d.ts`, `.prompt.md` and one `@dsCard` specimen page. |
| `guidelines/` | 21 specimen cards: Colors, Type, Spacing, Brand. |
| `ui_kits/console/` | The authenticated console: apps list, app detail, build log, variables, settings, add-app flow, night-survey dark theme. |
| `ui_kits/site/` | Marketing home, docs area, 404 page. |
| `thumbnail.html` | Project tile. |
| `SKILL.md` | Agent-skill wrapper for use outside this project. |

## Components

Grouped by concern. Every component reads its values from the CSS custom properties —
no CSS-in-JS, no npm dependencies, React only.

**`components/brand/`** — `ContourMap`, `Logo`, `MapCollar`
**`components/core/`** — `Button`, `IconButton`, `Icon`, `Tag`, `Badge`, `Card`
**`components/forms/`** — `Input`, `Select`, `Checkbox`, `Radio`, `Switch`
**`components/data/`** — `Table`, `StatusIndicator` (plus `StatusSymbol`)
**`components/code/`** — `CodeBlock`, `InlineCode`
**`components/feedback/`** — `Banner`, `EmptyState`, `Dialog`, `Toast`, `Tooltip`
**`components/navigation/`** — `SidebarNav`, `Tabs`

### Intentional additions

The brand spec describes buttons, inputs, focus, status indicators, tables, code and
terminals, navigation, banners and empty states. These exist because the product needs
them and the spec's rules extend cleanly to them — each is noted in its `.d.ts`:

- `IconButton` — the spec requires a copy button on every code block, and icon-only
  row actions; ghost styling, 4px radius, same press behavior as `Button`.
- `Tabs` — sections within one screen (Overview / Deploys / Variables / Settings),
  marked with a 1px ink underline, which is the same structural logic as every other rule.
- `Toast` — past-tense confirmation of something that already happened, following the
  banner's symbol-plus-one-sentence rule on `--paper-raised`.
- `Card` — the spec's "bounded object" container, named conventionally. Its docs repeat
  the spec's warning: prefer sections separated by rules over grids of identical cards.
- `Badge` — counts only (sidebar, tabs). Status never goes in a pill.
- `Switch`, `Radio`, `Select` — form controls the console needs; styled from the
  spec's input and focus rules.
- `StatusSymbol` — the bare 8px symbol, for dense tables and banners.
- `MapCollar` — the neatline, corner ticks and marginal data of a printed sheet,
  lifted out of `ContourMap`'s hero mode, which is where the spec first describes
  them. The collar and the contour are different ideas and the split is the point:
  the contour is terrain and is confined to four places, while the collar is the
  sheet it prints on, and a page can be a sheet without being terrain. `ContourMap
  collar` composes it, so the tick geometry has one definition rather than one per
  surface that wants to look like a map. **Added in phase 8.**

## Content fundamentals

**Voice.** Plain, literal, unhurried. Sentence case everywhere, including buttons and
table headers; nothing is set in all caps. Contractions are welcome. Active voice, and
buttons lead with a verb.

**Name things by what the person is doing**, not by how the system works: "Share app",
not "Grant data-plane access". Pando's deployers may not know what a port is.

**An action keeps its name through the whole flow.** "Deploy" produces "Deployed".
The button, the toast and the log line use the same word.

**Errors say what happened and what to do.** No apology, no "Error:" prefix, no
exclamation marks:

> Pando couldn't find a start command. Add one in app settings.

not

> Oops! Something went wrong.

**Setup questions stand on their own**, so a person can paste one into the tool that
wrote their app and get a usable answer: "What command starts this app?"

**Second person for the user, first person never.** "Your hosts, your bill." Pando
refers to itself by name — "Pando couldn't find…" — rather than "we".

**No emoji. No hype.** "Set up once. Deploy everything." and "Point Pando at a repo and
it builds, runs, and shares the app." are the register. "Supercharge your deployment
workflow" is not. No exclamation points, no "seamless", "effortless", "unlock", "magic".

**Machine text stays machine text.** Commit hashes, commands, URLs, variable names and
log lines are shown verbatim, in mono, and never prettified.

**Numbers and time.** Relative timestamps in tables ("2 min ago", "Just now",
"Yesterday"), with the full UTC time in a tooltip. Durations in the shortest honest
unit ("18s", "9.1s").

## Visual foundations

**Printed, not glowing.** Surfaces are flat paper and ink. No gradients, no glows, no
frosted glass, no drop shadows under content. Depth comes from rules and tonal shifts,
the way it does on a printed map.

**Color.** Warm paper (`#F0EDE4`) with raised and sunken variants; near-black ink
(`#1A1C1B`) for text and primary buttons; three rule weights (`--rule`,
`--rule-strong`, `--field-border`); contour browns for the illustration system and
elevation text; vegetation green and water blue for quiet tints and two statuses; and
marker red (`#B23A2C`) for exactly three things — the summit mark, failed apps and
destructive actions. Proportion: roughly 85% paper and ink, 12% rules, tints and browns,
under 3% marker red. If a screen shows red in more than one or two places, something is
wrong with the screen. A dark theme ("night survey") swaps paper and ink; the terminal
surface is identical in both.
`--contour-background` is the one line color for the terrain the console draws behind
its pages: one step darker than the paper (`#E7E3D8` light, `#0F110F` dark), at full
strength. The contour browns stay with the illustration itself.

**Type.** Newsreader (variable optical size) for display and headings, Public Sans for
interface and body, IBM Plex Mono for code, commands and IDs — and nothing else.
Classical scale: 12, 14, 16, 18, 21, 24, 36, 48, 60, 72. Headlines take -0.015em at 36px
and above; everything else sits at 0. One weight and one color per headline — never an
accented word. No all-caps tracked eyebrow labels above headings. Body text runs no wider
than 68 characters. Docs body is Newsreader 18/32. Text aligns left everywhere, including
the hero.

**Spacing and layout.** 4px base scale (4, 8, 12, 16, 24, 32, 48, 64, 96, 128).
Marketing: 12 columns, 24px gutters, 1120px max content width, 96px between sections on
desktop and 64px on mobile. Console: a fixed 232px sidebar, content up to 1280px, 32px
page padding. Prefer sections separated by rules over grids of identical cards; use a
bordered container only when the thing inside is a single object.

**Backgrounds.** Plain paper. No imagery, no photography, no textures, no wood grain or
paper grain, no repeating patterns, no full-bleed art. The only illustration in the
system is the contour map, and it appears in five defined places (hero, docs home header,
empty states, 404, logo mark) at defined sizes — never as wallpaper, never behind text,
never inside a button.

**The contour system.** Closed, irregular, roughly concentric paths that never cross:
5–9 rings at hero scale with 12–18px spacing, 3–5 rings small with 6–10px spacing, varied
slightly so the terrain looks real. Every fifth ring is an index contour — 1.5px in
`--contour`; ordinary rings are 1px in `--contour-line`. Always unfilled strokes: no
gradients, no shading, no 3D. Exactly one summit mark per figure in marker red — an 8px
filled triangle (the survey benchmark symbol) at hero scale, a 6px circle small — standing
for the thing the person cares about. The 404 page is the one deliberate exception: its
summit is missing. At hero scale the figure gets a map collar: 1px `--rule-strong`
frame, 8px corner ticks, coordinates in `code-sm` `--ink-secondary` at the top corners
(Pando's actual location, 38°31′30″N 111°45′00″W), a four-segment alternating scale bar at
bottom-left, and optionally an elevation label set along an index contour with the line
broken around it, printed-map style.

**Corners.** Radius signals hierarchy, so nothing uses one radius for everything: 2px
tags and inline code, 4px buttons, inputs and banners, 6px bounded objects and code
blocks, 8px dialogs. Pills are reserved for status dots and the switch track.

**Borders and shadows.** Borders are always 1px. Content has no shadow, ever. Popovers,
menus, toasts and dialogs get exactly one: `0 8px 24px rgba(26, 28, 27, 0.08)`, plus a
1px rule border. Dialogs sit on a scrim of `rgba(26, 28, 27, 0.4)`. There are no inner
shadows and no bevels. No protection gradients over imagery — there is no imagery.

**Transparency and blur.** Effectively unused. The dialog scrim is the only alpha surface
in the product; there is no backdrop blur anywhere, and glassy chrome is explicitly out.

**Cards.** `--paper-raised` fill, 1px `--rule` border, 6px radius, no shadow. Hover
darkens the border to `--ink-secondary` only when the whole card is a link.

**Hover, press and focus.** Hover is a tonal shift, never an opacity change: primary
buttons go to `#2B2E2C`, secondary borders darken to `--ink-secondary`, ghost buttons
pick up a `--paper-sunken` fill, table rows lift to `--paper-raised`. Press scales to
0.98 over 80ms — no other press treatment. Focus is a 2px ink outline at 2px offset,
never removed and never replaced with a glow.

**Motion.** Motion responds to what a person does. Hover and press transitions take
120ms, panels 200ms, everything on `cubic-bezier(0.2, 0, 0, 1)`, no bounce and no
scale-in. There are two orchestrated moments. On first load of the marketing hero the
contours draw in from the outermost ring inward over 900ms, then the summit mark
appears. Onboarding a new app is the second: the contour map draws in ring by ring as
detection advances, and each finding rises into place as it is found. Nowhere else does
anything animate on load. Building status shows a hollow ring rather than a
spinner, so a table stays calm while a deploy runs. Under `prefers-reduced-motion` the
map simply appears.

**Fixed elements.** The marketing nav is a plain 1px-ruled bar that does not change on
scroll and is not sticky. The console sidebar is fixed at 232px; the docs sub-nav is the
one sticky element in the system.

**Status.** Every status pairs a color with a distinct shape, so it never depends on
color alone: filled circle running, hollow ring building, filled triangle failed, short
dash stopped, filled circle info. Status is always a symbol plus a word, never a filled
pill.

## Iconography

**Set.** Lucide (Tabler outline is an acceptable alternative) at **1.5px stroke**, 16px
in the console and 20px on marketing, colored `--ink-secondary` by default. The
`Icon` component fetches the Lucide glyph and rewrites its stroke width to 1.5 so the
brand weight holds; it renders `currentColor`, so tinting the parent tints the icon.

**Vendored, as of phase 8.** No icon assets came with the brief, and `Icon` originally
fetched each glyph from the unpkg CDN at runtime. That cost an installation on a private
network every icon in the product, and cost even an online one a visible pop-in. The set
actually used now lives in `assets/icons/` as Lucide SVGs (ISC), compiled into
`assets/icons/index.js` by `assets/icons/build.mjs` and imported synchronously.

Add one by dropping its SVG there and re-running the script. An unknown name renders
nothing and warns, rather than throwing — a typo should cost a gap in the chrome, not a
blank screen.

**Never** hand-roll SVG paths; the only bespoke drawing in this system is `ContourMap`,
which is generated procedurally to the spec's construction rules. **Never** use emoji.
**Never** use unicode characters as icons — the one exception is the status symbol set,
which is drawn as real SVG shapes in `StatusSymbol`. Avoid sparkle, magic wand and robot
icons entirely.

**Working set used across the kits:** `search`, `copy`, `arrow-left`, `user`,
`ellipsis`, `git-branch`, `x`.

## Assets

`assets/` holds the logo artwork (supplied 2026-09-12), the icon set and the webfonts.
Nothing else was supplied with the brief — no illustration or photograph, and there is
none by design.

- **Logo — supplied, and it is not what the spec described.** The official mark is
  **"pando." with the full stop in marker red**: a wordmark, no contour ring, no summit
  dot. The spec's written description (three nested contours with a red summit dot) was a
  different mark, and `components/brand/Logo.jsx` has been rebuilt from the artwork.

  Its colors are already tokens — the exports hard-code `#1A1C1B`/`#ECE8DE` for the
  wordmark and `#B23A2C`/`#E0654F` for the full stop, which are exactly `--ink` and
  `--marker` in the two themes — so the component draws from tokens and follows the theme
  rather than needing a light file and a dark file. The exported SVGs carry **live**
  Newsreader text, not outlines, so they depend on the same font the component does.

  The icon is "p." in `--paper` on an `--ink` tile at radius 112 on a 512 grid.
  `wordmark={false}` renders it.

  **One consequence for the contour system:** the spec listed the logo mark as one of the
  five places the contour figure appears. It is no longer one of them. The other four —
  hero, docs home header, empty states, 404 — are unchanged, and `ContourMap` is
  untouched.
- **Fonts — self-hosted, as of phase 8.** Newsreader, Public Sans and IBM Plex Mono are
  all free under the SIL Open Font License, and their `.woff2` files are in
  `assets/fonts/`: 16 files, 355 KiB, covering latin, latin-ext, cyrillic, cyrillic-ext
  and vietnamese, so an app named in any of those scripts renders in the real face.
  `tokens/fonts.css` is Google's own CSS with the URLs swapped for local paths — the
  unicode-ranges and the subset split are theirs, because re-deriving them is how a
  subset quietly stops loading. Refresh with `assets/fonts/build.mjs`.

  The CDN `@import` it replaced rendered the entire product in fallback faces on any host
  without internet access, which is a normal way to run software someone installs
  themselves.
- **Imagery.** There is none, by design. The brand has no photography or illustration
  besides the contour system.

## Using this system

Consumers link one file:

```html
<link rel="stylesheet" href="styles.css">
```

Dark theme is a single attribute on the root: `<html data-theme="dark">`.

Read a component's `.prompt.md` before using it — each states what it is for, when not
to use it, and the brand rule it enforces.
