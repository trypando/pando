---
name: pando-doc-writer
description: Write and update user-facing Pando documents (security papers, release notes, how-to guides, written references, policies, white papers) as a branded document that reads as one long page on the web and exports to paged PDF with a running header and footer. Only for documents published to people who use or evaluate Pando. Not for the repository's own docs (requirements, design docs, plans, open decisions, READMEs, CLAUDE.md, Markdown under docs/) and not for generated docs (API, CLI and MCP reference, OpenAPI/Swagger, changelogs from commits).
---

# Pando document

## When to use
Use only for user-facing documents a person writes and Pando publishes to the people who use or evaluate it: security papers, release notes, how-to guides, policies, white papers, written references.

Don't use for:
- **The repository's own documentation.** Requirements (`docs/requirements.md`), design docs, plans, open decisions, the traceability index, READMEs, `CONTRIBUTING.md`, `CLAUDE.md` and other Markdown that lives in the repo for the people building Pando. Those stay Markdown in their existing format.
- **Generated documentation.** API, CLI and MCP reference (`docs/api.md`, `docs/cli.md`, `docs/mcp.md`, written by `make reference`), OpenAPI/Swagger specs, SDK docs, changelogs generated from commits. Those come from the code and have their own format.

If it isn't clear whether a document is user-facing, ask before using this template.

## Before writing
- The request may be unstructured. Turn it into a coherent document, but don't invent facts, numbers, behaviour, commands, limits or timelines.
- If anything about the subject isn't 100% clear, ask the user before writing. List the specific gaps as short questions.
- If an image or screenshot is needed to explain something properly, ask the user for it. Don't draw or fake one.

## Writing rules
These are docs, not a website. The job is to provide information, not to tell a story.
- Keep it simple and to the point. Every sentence must carry information the reader needs. Cut anything that doesn't.
- No catchy phrases, slogans, taglines or quips (for example "Your hosts, your bill."). They read as filler and lose the reader's trust.
- No marketing language, hype, rhetorical questions or decorative intros and outros. Don't open with "In this guide we'll…" or end with a recap.
- State facts plainly: "Secrets are encrypted at rest", not "Your secrets are always safe with us".
- Show, don't list. When the reader must produce something (a report, a config, a request), give a filled-in template or example instead of a list of what to include.
- Pando voice: sentence case, second person, "Pando" rather than "we", contractions are fine, no exclamation marks, no emoji.
- Don't make Pando the subject of every sentence. "Pando does…" and "Pando can…" are fine occasionally but grating in excess. Prefer the thing itself as the subject ("Each build starts from a clean image", "Secrets are encrypted at rest"), or the reader ("You can see…", "Run `pando inspect`…").
- Commands, URLs, file paths, variable names and log lines appear verbatim in mono, never paraphrased.
- Use tables for comparisons, numbered steps for procedures, and code blocks for anything the reader copies.

## Accuracy and updates
Inaccurate or partly outdated documentation breaks trust. On every update:
1. Bump `version` (patch for corrections, minor for new or changed content, major for restructures) and set `date` to today.
2. Add a revision history row at the top. One line saying what changed and where, by section number.
3. Make sure the contents list matches the section headings exactly: titles, numbers and order.
4. Check for drift. If a change affects anything elsewhere (cross-references like "see section 4", numbers, names, commands, tables, the summary, earlier revision notes that refer to sections by number), fix it everywhere in the document.
5. Re-read the whole document once before finishing.

## Template
File: `Pando Document.dc.html`. Duplicate it for each new document. Replace the content and keep the structure and styling.

Dependencies (keep alongside the file):
- `support.js`: the runtime
- `doc-page.js`: the page shell. One long sheet on screen, Letter pages when printed or exported to PDF, with the header and footer repeated on every page
- `_ds/pando-…/`: Pando design tokens, fonts and components (Banner, CodeBlock)

Settings (Tweaks):
- `docType`: Guide · Reference · Security paper · White paper · Policy · Release notes
- `version`, `date`: shown in the running header, the footer and the latest revision row
- `showContents`: show or hide the contents block. Hide it for short documents (under about 3 sections)
- `heroStyle`: Plain (default) · Topo beside title · Topo strip

Structure:
1. Running header: `pando.` wordmark, document title, `v{version}`
2. Running footer: `Pando · {docType} · {date}`
3. Hero: document type, title (h1, Newsreader 48), one factual sentence on what the document covers
4. Contents: two-column numbered list, always in sync with the headings
5. Numbered sections: h2 with the number hanging in the margin, optional h3 `n.n` subsections
6. Revision history: the final section, newest first

Content elements:
- Body: Newsreader 16/1.65, maximum width 40em
- Callout: `Pando_275832.Banner` with tone `info`, for one fact the reader must not miss. At most one or two per document
- Code: `Pando_275832.CodeBlock` with a Public Sans 12px figcaption
- Example or template block: a bordered `pre` in IBM Plex Mono 13, as in "Reporting a vulnerability"
- Tables: Public Sans 14, 1px rules, caption below ("Table n. …")
- Spacing: 32px above each section, 48px above the revision history

## Export
Use the browser's print, or the PDF export. Pages break automatically. Figures, tables, code and example blocks are kept from splitting across pages.
