<!--
The full version of this is CONTRIBUTING.md. This is the short form, and the
boxes are the parts people most often find out about after review rather than
before it.
-->

## What this changes

<!-- What it does and why. Link the issue if there is one. -->

## Requirements

<!--
Cite the R-IDs this implements or touches (docs/requirements.md). If a
requirement it touches has no acceptance test, say so here and say whether that
is a gap, deferred work, or philosophy — `make requirements-coverage` reports
which IDs have one.
-->

## Checklist

- [ ] `make check` passes — build, vet, golangci-lint (including the R-027 adapter import rule and gosec), unit tests.
- [ ] There is an acceptance test named for the requirement, in the `TestR132_UnfilledRequiredSlotBlocksDeploy` form — or the box below is ticked instead.
- [ ] No test applies, and the reason is stated above.
- [ ] Any `[P]` default overridden is noted in the design document with the reason, not only in a code comment.
- [ ] Console changes use the design system's tokens — no raw hex, no raw `px`, no font outside Newsreader / Public Sans / IBM Plex Mono. `npm run check` in `console/` enforces this.
- [ ] User-facing error text says what happened and what to do, with no apology and no `Error:` prefix (R-105).
- [ ] `CHANGELOG.md` has an entry under Unreleased, if an operator would notice this change.
- [ ] I have signed the [Contributor License Agreement](https://github.com/trypando/pando/blob/main/CONTRIBUTOR_AGREEMENT.md), or will when the **Contributor agreement** check asks. It is one comment, once.

## Anything that touches authorization, the proxy, or the audit log

<!--
Delete this section if it does not apply. If it does, say which of the
invariants in CLAUDE.md §2 you checked against, and confirm that no mechanism
listed there was weakened to make a test pass. That code has been broken once
already.
-->
