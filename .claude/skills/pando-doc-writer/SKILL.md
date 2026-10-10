---
name: pando-doc-writer
description: Write and update user-facing Pando documents (security papers, release notes, how-to guides, quickstarts, written references, policies, white papers) as a branded document that reads as one long page on the web, follows the reader's light or dark theme, and exports to paged PDF with a running header and footer. Only for documents published to people who use or evaluate Pando. Not for the repository's own docs (requirements, design docs, plans, open decisions, READMEs, CLAUDE.md, Markdown under docs/) and not for generated docs (API, CLI and MCP reference, OpenAPI/Swagger, changelogs from commits).
---

# Pando document

## When to use
Use only for user-facing documents a person writes and Pando publishes to the people who use or evaluate it: security papers, release notes, how-to guides, quickstarts, policies, white papers, written references.

Don't use for:
- **The repository's own documentation.** Requirements (`docs/requirements.md`), design docs, plans, open decisions, the traceability index, READMEs, `CONTRIBUTING.md`, `CLAUDE.md` and other Markdown that lives in the repo for the people building Pando. Those stay Markdown in their existing format.
- **Generated documentation.** API, CLI and MCP reference (`docs/api.md`, `docs/cli.md`, `docs/mcp.md`, written by `make reference`), OpenAPI/Swagger specs, SDK docs, changelogs generated from commits. Those come from the code and have their own format.

If it isn't clear whether a document is user-facing, ask before using this template.

## Before writing
- The request may be unstructured. Turn it into a coherent document, but don't invent facts, numbers, behavior, commands, limits or timelines.
- If anything about the subject isn't 100% clear, ask the user before writing. List the specific gaps as short questions.
- If an image or screenshot is needed to explain something properly, ask the user for it. Don't draw or fake one.
- **Check every fact against the code, on the branch or release the document describes.** The README and other prose docs can be out of date. Ask which branch or release the document is for if it isn't stated. If it describes anything not in the latest release (see `CHANGELOG.md`'s Unreleased section), say so to the user: the document can't be published before that release.
- **Name console screens by their navigation path, as the console labels it,** and check the labels in `console/src`: **System > Updates**, not "the Updates screen". Use the exact labels of buttons and fields (**Add app**, **Accept and deploy**).

## Writing rules
These are docs, not a website. The job is to provide information, not to tell a story.
- Keep it simple and to the point. Every sentence must carry information the reader needs. Cut anything that doesn't.
- No catchy phrases, slogans, taglines or quips (for example "Your hosts, your bill."). They read as filler and lose the reader's trust.
- No marketing language, hype, rhetorical questions or decorative intros and outros. Don't open with "In this guide we'll…" or end with a recap.
- State facts plainly: "Secrets are encrypted at rest", not "Your secrets are always safe with us".
- Show, don't list. When the reader must produce something (a report, a config, a request), give a filled-in template or example instead of a list of what to include.
- Pando voice: sentence case, second person, "Pando" rather than "we", contractions are fine, no exclamation marks, no emoji. US spelling.
- Don't make Pando the subject of every sentence. "Pando does…" and "Pando can…" are fine occasionally but grating in excess. Prefer the thing itself as the subject ("Each build starts from a clean image", "Secrets are encrypted at rest"), or the reader ("You can see…", "Run `pando inspect`…").
- Commands, URLs, file paths, variable names and log lines appear verbatim in mono, never paraphrased.
- Use tables for comparisons, numbered steps for procedures, and code blocks for anything the reader copies.

### Instructions only
In a guide, every sentence either tells the reader what to do or gives a fact they act on. A true sentence that does neither is cut. Each of these was cut from a draft:

| Cut | Why |
|---|---|
| A "Before you start" section describing the containers Pando runs as | Describes the system. The reader needs only the requirement, if anything. |
| "Run these on the host. They set Pando up to install new releases itself…" | Explains the steps before giving them. |
| "The downloaded file pins a version. Compose merges `docker-compose.override.yml` into it, so changes go there:" | Explains why before a step. A fact the reader needs goes where it applies: a comment in the copied file (`# UTC`), or the step itself. |
| "Pando stops, copies its database and starts the new version." | Narrates what the software does after the reader acts. |
| "If the log no longer has the token, for example because the container was recreated, …" | An aside. |
| A caption under `mkdir pando && cd pando` saying "Make a directory for Pando…" | Restates the command. Captions are optional; add one only when it says something the code doesn't. |
| A table explaining each line of a config file whose variable names already say what they do | Restates the config. |
| "The app's address is on its page. Apps are private until you share them." | Tells the reader nothing to do. |
| "New releases appear… with their changelogs, within six hours of release." | Detail the reader doesn't act on. "New releases appear in **System > Updates**." is enough. |

Read every sentence against this before showing a draft. If the user has to find these, the draft wasn't ready.

### Requirements, defaults and versions
- State a requirement for the method the document shows, not for Pando. "You need Docker" is false when Pando also installs on Kubernetes. Say which method the document uses only if it isn't obvious, and link the alternatives in a "Where to go next" list.
- If a default can be changed, show how: the variable and an example value. Ports, passwords and paths usually can.
- State a default as a default: "If you don't set one, it defaults to `pando`.", not "If you don't, it's `pando`."
- Don't write a version number that goes stale. Use moving tags (`trypando/pando:latest`), `releases/latest/download/` URLs, or a command that looks the version up:
  ```sh
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/trypando/pando/releases/latest | sed 's|.*/v||')
  ```

## Accuracy and updates
Inaccurate or partly outdated documentation breaks trust. On every update:
1. Bump `version` (patch for corrections, minor for new or changed content, major for restructures) and set `date` to today. A document that hasn't been published yet stays at 1.0, "First published".
2. Add a revision history row at the top. One line saying what changed and where, by section number.
3. Make sure the contents list matches the section headings exactly: titles, numbers and order.
4. Check for drift. If a change affects anything elsewhere (cross-references like "see section 4", numbers, names, commands, tables and their numbers, the summary, earlier revision notes that refer to sections by number), fix it everywhere in the document.
5. Re-read the whole document once before finishing.

## Template
File: `Pando Document.dc.html`. Duplicate it for each new document. Replace the content and keep the structure, styling and scripts.

Dependencies (keep alongside the file):
- `support.js`: the runtime
- `doc-page.js`: the page shell. One long sheet on screen, Letter pages when printed or exported to PDF, with the header and footer repeated on every page
- `_ds/pando-…/`: Pando design tokens, fonts and components (Banner, CodeBlock)

**Don't edit the dependencies.** `doc-page.js` and `support.js` are a copied starter that is overwritten when it's re-copied, and `_ds/` is regenerated from the Claude Design project. Fixes go in the template, which every document copies:
- **Light and dark.** A script in `<head>` sets `data-theme` on `<html>` from the reader's choice on trypando.ai (`localStorage` key `pando-theme`), or else the system setting, and switches to light while printing. The colors come from the design tokens, which define both themes. `doc-page` hard-codes a white sheet inside its shadow root, so the template takes the sheet's color from `--doc-sheet` (white, or `--paper-raised` in dark).
- **Inline code.** `<code>` is styled as the design system's `InlineCode`: tinted background, `--radius-xs`, `--type-code`. Mono alone is too close to the body text to tell apart.
- **Phone screens.** Below 920px the sheet uses the full width with 16px margins, section numbers sit inline, the contents list is one column, wide tables scroll inside themselves, and long commands wrap. Print keeps the Letter layout.
- **Copy buttons.** The design system's `CodeBlock` shows "Copied" without writing to the clipboard. A click handler in the template copies the block's lines without the `$` prompt or the title. Remove it once `CodeBlock` is fixed in the Claude Design project.

Settings (Tweaks):
- `docType`: Guide · Reference · Security paper · White paper · Policy · Release notes
- `version`, `date`: shown in the running header, the footer and the latest revision row
- `showContents`: show or hide the contents block. Hide it for short documents (under about 3 sections)
- `heroStyle`: Plain (default) · Topo beside title · Topo strip

Structure:
1. Running header: `pando.` wordmark, document title, `v{version}`
2. Running footer: `Pando · {docType} · {date}`
3. Hero: document type, title (h1, Newsreader 48), one factual sentence on what the document covers
4. Contents: two-column numbered list, always in sync with the headings. Link each entry to its section's `id`
5. Numbered sections: h2 with the number hanging in the margin, optional h3 `n.n` subsections
6. Revision history: the final section, newest first

Content elements:
- Body: Newsreader 16/1.65, maximum width 40em
- Inline code: `<code>`, for commands, values, variables and paths inside a sentence
- Callout: `Pando_275832.Banner` with tone `info`, for one fact the reader must not miss. At most one or two per document, and often none
- Code: `Pando_275832.CodeBlock`, with `prompt` for commands and `title` for a file's name. A Public Sans 12px figcaption only when it adds something the code doesn't. Put lines the reader copies in `renderVals()` and pass them as `lines`
- Example or template block: a bordered `pre` in IBM Plex Mono 13, as in "Reporting a vulnerability"
- Tables: Public Sans 14, 1px rules, caption below ("Table n. …"), numbered in order
- Spacing: 32px above each section, 48px above the revision history

## Publishing on trypando.ai
A document is shown at `/<slug>` between the site's nav and footer, in a frame, so its runtime and styles stay apart from the site's. `/quickstart` is the worked example: `src/pages/Quickstart.tsx` and `public/guides/quickstart/`.
1. Put the document at `public/guides/<slug>/index.html` in the trypando.ai repository, with `support.js`, `doc-page.js` and `_ds/` beside it. Vite copies `public/` as is. Opened on its own, `/guides/<slug>` is the printable document.
2. Make every script and stylesheet path absolute (`/guides/<slug>/support.js`, `/guides/<slug>/_ds/…`). Cloudflare Pages serves a page at `/<path>` without a trailing slash, where relative paths resolve to the site root.
3. In `<head>`, add a `<title>`, a description, a canonical link to `https://trypando.ai/<slug>` and the site's favicons. Copy the quickstart's `<base target="_top">` and frame script: they report the document's height to the page, send section links to the page's address, open other links in the whole window, and follow the site's theme toggle.
4. Add the site page: a component like `Quickstart.tsx` pointing at `/guides/<slug>`, an entry in `PAGES` and `pageFor` (`src/site.ts`), in `VIEWS` (`src/App.tsx`) and in `routes` (`src/entry-server.tsx`), which also puts it in the sitemap.
5. Preview with `npm run build`, then a server that serves `/<path>` from `<path>/index.html` as Cloudflare Pages does. `npx vite preview` doesn't, so a link to `/<slug>` lands on the 404 page there.
6. Check, in a browser: no console errors or failed requests; the frame is as tall as the document, with one scrollbar; a contents link scrolls the page and sets its address; each Copy button puts its lines on the clipboard; the site's theme toggle changes the document; nothing scrolls sideways at 390px wide.

## Export
Use the browser's print, or the PDF export. Pages break automatically. Figures, tables, code and example blocks are kept from splitting across pages. Printing always uses the light theme.
