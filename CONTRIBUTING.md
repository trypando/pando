# Contributing

[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) applies everywhere this project happens.

**Found a security vulnerability?** Do not open an issue or a pull request for it — a pull request
that fixes a security bug describes the bug in public before anyone can upgrade. Use
[the advisory form](https://github.com/trypando/pando/security/advisories/new), and see
[`SECURITY.md`](SECURITY.md) for what to include and what response to expect.

## Getting set up

```bash
git clone https://github.com/trypando/pando.git
cd pando
make check        # vet, lint, unit tests — what CI runs on a pull request
```

Go 1.27 and Docker. Node 22 as well if you are touching the console.

To run the server you are changing, build it from this checkout rather than pulling the published
image: `docker-compose.yml` in the repository builds the `pando` service from source, where the file a
release ships runs `trypando/pando` instead (`scripts/release-compose.sh` writes one from the other).
The image is built on [Docker Hardened Images](https://docs.docker.com/dhi/), which `dhi.io` serves
only to a signed-in Docker account (a free one is enough), so sign in once first. The deploy QA run
(`test/deploy-qa`) builds the same image and needs the same.

```bash
docker login dhi.io
docker compose up -d --build
```

Or check the image the way CI's `image` job does. The smoke test starts its own stack from the
compose file, deploys an app, reaches it through Pando and removes everything it made. Stop any other
Pando on the machine first: two on one Docker host step on each other's networks.

```bash
python3 scripts/smoke-image.py docker-compose.yml
```

```bash
make help                    # all targets
make build                   # the pando binary
make console                 # build the console into the embedded assets
make test-integration        # real Postgres and Docker, via testcontainers
make detection-corpus        # detection against ten real repositories (network, slow)
make requirements-coverage   # which requirements have a named acceptance test
make fuzz                    # fuzz the parsers that read untrusted input
```

`make detection-corpus` is not part of `make test-integration` and does not run on a pull request
unless that request touches `internal/detect`. It clones ten real repositories, so it is slow and it
depends on those repositories staying reachable. CI runs it nightly; run it yourself before changing
a detector, because the number it reports — questions per deploy, the R-103 metric — appears nowhere
else.

`make fuzz` runs each target for `FUZZ_TIME` (60s by default) and is not part of `make check`, which
has to stay fast. Their seed corpora do run in `make test`, so a crasher committed to `testdata/fuzz`
fails on every pull request. CI fuzzes for longer on a schedule. If you find a crasher, commit the
input file it writes — that is what turns it into a regression test.

To run a full stack while you work:

```bash
PANDO_RECONCILER_BACKOFF=0s,1s,2s,3s,4s docker compose up -d --build
```

The compressed retry backoff matters: at the shipped defaults a single acceptance test that asserts
the give-up rule takes forty minutes. See [`test/acceptance/README.md`](test/acceptance/README.md).

## How the project is organized

One Go binary contains the HTTP API, the console, the proxy and the CLI. Postgres holds state.

```
cmd/pando/              CLI and server entrypoint. Adapter registration happens here.
internal/
  core/                 Business logic. Adapters may not import any of this except adapter/api.
    authz/              Verb evaluation, both planes.
    audit/              Append-only event log.
    spec/               AppSpec types, validation, classified diffing.
    planner/            spec + policy + adapters -> plan, or a plan-time error.
    reconciler/         The loop and the state machine.
    state/              sqlc-generated queries and repository types.
  adapter/              The seven adapter categories, and their implementations.
  detect/               Build detection: the auction, the detectors, the trial run.
  proxy/                The identity-aware reverse proxy.
  httpapi/              chi handlers. No business logic.
  mcp/                  MCP server, a client of the same service layer as httpapi.
console/                React and TypeScript. Built into internal/console.
migrations/             golang-migrate, embedded in the binary.
test/acceptance/        The four end-to-end sequences.
```

## Documentation

Two sets, and the distinction matters:

| Path | Authority |
|---|---|
| [`docs/requirements.md`](docs/requirements.md) | What Pando is. 210 requirements, IDs `R-###`. Changes slowly. |
| [`docs/design/`](docs/design/) | How it is built. Nine documents, `00`–`08`. |
| [`docs/plan/`](docs/plan/) | Build order by phase, open decisions, risk register. |
| [`docs/traceability/`](docs/traceability/) | Generated index mapping requirements to design, code and tests. |
| [`docs/reference.md`](docs/reference.md) | The external interfaces in one place — API, configuration, what an app receives. |
| [`docs/releasing.md`](docs/releasing.md) | What a version number promises, and how to verify a download's signature. |
| [`SECURITY.md`](SECURITY.md) | The security model, the cryptography in use, and coordinated disclosure. |
| [`CLAUDE.md`](CLAUDE.md) | Conventions, invariants, and the definition of done. |

Requirements are tagged **[D]** decided, **[P]** proposed, **[O]** open. Where the design contradicts
a requirement, the requirement takes precedence — or the requirement is amended in the same change,
never left to diverge silently.

Suggested reading order for a first change: requirements §1–3, then design `01` (the app spec, which
most of the system revolves around), then design `07` (the four end-to-end flows), then the design
document for the area you are working in.

If the documentation does not answer a question your change depends on, add it to
[`docs/plan/open-decisions.md`](docs/plan/open-decisions.md) and raise it in the pull request rather
than deciding it quietly.

## Invariants

§2 of [`CLAUDE.md`](CLAUDE.md) lists the project's invariants and the mechanism that enforces each
one — a database constraint, a lint rule, a trigger, a test. Read it before a change that touches
authorization, the proxy, the audit log or the state schema. Do not weaken one of those mechanisms to
make a test pass.

Two that catch people out:

- **Adapters cannot import `core/authz`, `core/audit`, `core/state` or `core/policy`.** This is a
  depguard rule in `make lint`, not a convention.
- **Builds never get a container runtime socket.** BuildKit runs rootless in its own container, and
  an integration test asserts on that container's actual mount list.

## Static analysis

`make lint` includes `gosec`, and CodeQL runs separately on every push. A `gosec` finding is either
fixed or suppressed with a `//nolint:gosec` comment that names the rule and says why it does not
apply — a bare `//nolint` is indistinguishable from one nobody checked, so it will be asked about in
review. If a finding is real but the fix is a decision rather than a correction, record it in
[`docs/plan/open-decisions.md`](docs/plan/open-decisions.md) and point the suppression at it. O-19
is an example: the comment says the finding is not a false positive.

`govulncheck`, `npm audit` and `gitleaks` run daily as well as on pull requests. A `gitleaks`
allowlist entry goes in `.gitleaks.toml` naming the exact fixture; excluding `*_test.go` wholesale is
the shortcut that stops the check finding the thing it exists to find.

## Writing an adapter

Adapters are compiled into the binary and contributed by pull request; there is no external plugin
system, and none is planned. Seven categories: runtime, routing, builder, secrets, services,
identity, notifications.

Start with [`docs/design/03-adapter-interfaces.md`](docs/design/03-adapter-interfaces.md). Two rules
shape every adapter:

- **The app declares requirements; the adapter translates.** An app never mentions a provider's
  vocabulary, and neither does core. An adapter turns "2 GB, one persistent volume, one exposed HTTP
  port" into whatever its provider wants.
- **Capabilities are reported as data, not discovered by type assertion.** The planner uses them to
  reject an impossible combination before a deploy starts, with an error naming what to change.

## Pull requests

A change is complete when:

1. **It has an acceptance test named for the requirement it satisfies:**
   ```go
   // TestR132_UnfilledRequiredSlotBlocksDeploy asserts R-132.
   func TestR132_UnfilledRequiredSlotBlocksDeploy(t *testing.T) { … }
   ```
   `make requirements-coverage` reports which requirement IDs have one. If a requirement your change
   touches has no test, say so in the pull request and whether that is a gap or deliberate.
2. `make check` passes.
3. Any `[P]` default you overrode is noted in the design document, with the reason — not only in a
   code comment.
4. [`CHANGELOG.md`](CHANGELOG.md) has an entry under **Unreleased** if an operator running an
   installation would notice the change. A refactor does not need one; a new configuration variable,
   a changed default, a fixed bug and anything security-relevant all do. Security entries name the
   advisory or CVE identifier — that is what tells someone whether an upgrade is urgent.

Cite requirement IDs in commit messages and comments where a non-obvious choice traces to one. `R-151`
in a comment explains an absent code path better than three sentences will.

## User-facing text

Error messages and console copy are held to one standard, in the API and the UI alike: say what
happened and what to do, with no apology and no `Error:` prefix. "Which port?" fails it. "This app
appears to be a Node.js service. Pando could not determine which port it serves HTTP on. Valid
answer: a port number such as 3000." passes it.

The console is built from the design system in `.claude/skills/pando-design/`. Colors, type, spacing
and radius come from its tokens; a raw hex value, a raw `px` value, or a font that is not Newsreader,
Public Sans or IBM Plex Mono fails `npm run check`.

## Releasing

What a version number promises to an operator, and how somebody verifies a download, is in
[`docs/releasing.md`](docs/releasing.md). This is the mechanics.

Merging a pull request does not release anything. Cutting a release is one command, run from main:

```bash
gh workflow run release.yml -f version=v0.1.0
```

or **Actions → Release → Run workflow** in the browser. That creates the tag and runs
`.github/workflows/release.yml`, which runs GoReleaser against `.goreleaser.yaml` and publishes:

- tarballs for macOS and Linux, `amd64` and `arm64`, with a `checksums.txt`
- `.deb`, `.rpm` and `.apk` packages
- a Homebrew cask pushed to `trypando/homebrew-tap`, which is what `brew install trypando/tap/pando`
  reads

The version in `pando version` and in the manifest is stamped from the tag, so a build made any other
way reports `dev`.

Release notes come from the `CHANGELOG.md` section for the version being released, and the workflow
refuses to release a version that has no section. That refusal is deliberate: GoReleaser's generated
changelog is one line per commit, which is a version control log rather than something an operator
can read to decide whether to upgrade.

The version number is the one thing not automated, because it is the one part that is a judgment. A
tag with a suffix — `v0.1.0-rc.1` — is published as a prerelease, which `brew upgrade` and the package
managers ignore, so it is the way to exercise the whole pipeline without shipping to anyone.

The workflow also still runs on a `v*` tag pushed by hand, for the case where the tag has to point
somewhere other than the head of main.

If the release fails, the workflow removes the tag it created, so the same version can be tried again
once the cause is fixed — unless the release had already been published, in which case the tag stays
and the fix is to re-run the failed job.

### What the release depends on

- **The tap.** `trypando/homebrew-tap`, public. GoReleaser writes `Casks/pando.rb` into it and
  overwrites it on every release; nothing in that repository is edited by hand.
- **`HOMEBREW_TAP_TOKEN`.** A repository secret here holding a fine-grained token owned by
  `trypando`, scoped to the tap, with `contents: write`. The workflow's own `GITHUB_TOKEN` is scoped
  to this repository and cannot push to another one, so without this the release fails at the cask
  step — after the artifacts have been uploaded. When the token expires that is how it will show up.

### Before you tag

```bash
make release-check
make release-snapshot     # artifacts land in dist/
```

Both need `goreleaser` on your path, and the snapshot needs Node, because the console is embedded in
the binary and is built before the Go build. Nothing is published either way.

The macOS binaries are not signed with a Developer ID, so the cask removes the quarantine attribute
after installing. Signing and notarization would replace that; see the comment in `.goreleaser.yaml`.

## Licensing of contributions

Pando is dual-licensed, so the maintainer has to be able to license every part of it under any terms.
Before your first pull request is merged, you sign the
[Contributor License Agreement](CONTRIBUTOR_AGREEMENT.md). You keep the copyright in your
contribution and grant the maintainer a perpetual, irrevocable license to it under any terms. The
agreement also guarantees your contribution stays available under the license Pando uses on the day
you submit it.

To sign, post this comment on your pull request:

```
I have read the Contributor License Agreement and I agree to it.
```

You do this once. The **Contributor agreement** check records the signature and covers every later
pull request. Anyone named in a `Co-authored-by` line signs once too; AI tools named there are
exempt. Each author's and co-author's email has to be linked to a GitHub account, because that is
how the check matches commits to signatures.

If any part of a pull request is not your own work, such as code copied from another project or work
your employer owns, read section 7 of the agreement before you submit it. See also
[`LICENSING.md`](LICENSING.md).
