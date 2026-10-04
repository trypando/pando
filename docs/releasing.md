# Versions and verifying a download

What a Pando version number promises, and how to check that the file you downloaded is the one the
project published.

**Cutting a release** is not here — it is one command, and it is documented with the rest of the
contributor workflow in [`CONTRIBUTING.md`](../CONTRIBUTING.md#releasing), next to the tap and token
it depends on.

## Version numbers

Pando follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html). A version is
`MAJOR.MINOR.PATCH`, and the tag is that string with a `v` prefix: `v0.3.1`.

What each part promises, for an operator running an installation:

| Part | Changes when |
|---|---|
| **MAJOR** | Something breaks on upgrade: an HTTP API endpoint or response shape is removed or changed incompatibly, an app spec field is removed, a configuration variable is removed, a migration cannot be applied to the previous version's database, or an adapter interface changes. |
| **MINOR** | Something is added: a new endpoint, spec field, configuration variable, adapter, console capability or CLI command. Existing behavior is unchanged. |
| **PATCH** | A bug or a vulnerability is fixed, with no interface change. |

Before `1.0.0`, MINOR carries the breaking changes — `0.4.0` may break what `0.3.0` did. Patch
releases within a `0.x` line never break.

A version with a suffix — `v1.0.0-rc.1` — is published as a prerelease, which `brew upgrade` and the
package managers skip.

The API is versioned separately and independently, at `/api/v1`. A Pando MAJOR bump does not imply an
API version bump, and the API version changes only when `/api/v1` itself stops being supported.

## What identifies a release

- **The tag** — `v0.3.1`, on the commit it was built from.
- **`pando version`** — prints the version, the commit and the build date, stamped by the release
  build. A binary built any other way prints `pando (development build)`, and that is the only thing
  it can honestly say.
- **The GitHub release** — carries the tarballs, the `.deb`, `.rpm` and `.apk` packages,
  `checksums.txt`, the signature over it, and the `docker-compose.yml` that runs the image below.
- **The server image** — [`trypando/pando`](https://hub.docker.com/r/trypando/pando) on Docker Hub,
  for `linux/amd64` and `linux/arm64`, with the version, commit and build date in `pando version`
  and in its OCI labels. Tagged with the exact version (`0.3.1`) and the minor line (`0.3`); from
  1.0 on also the major (`1`); and `latest` for a stable release. A prerelease is tagged with its
  own version only, so nobody following `latest` or a minor line is moved onto it. The release's
  `docker-compose.yml` pins the exact version. Published by `.github/workflows/image.yml` after the
  release, in an order that keeps a failing image away from anyone following a moving tag:
  1. Scan the image with Trivy, and smoke test it: start it from a compose file and deploy an app.
     A fixable critical vulnerability or a failed test stops it here, before anything is pushed.
  2. Push and sign it under its exact version only.
  3. Pull that back from Docker Hub and smoke test it again, from the compose file the release ships.
  4. Only then attach that compose file to the release, and point the minor line, the major and
     `latest` at the same digest.

  A failure at any step leaves the moving tags where they were.

## Before tagging

1. `make check` and `make test-integration` are green on the commit being released.
2. `CHANGELOG.md` has a section for the version, dated, with every user-visible change in it. The
   **Security** subsection names every publicly known vulnerability fixed in this release that
   already had a CVE or GHSA identifier when the release was cut — that is a requirement, not a
   courtesy, because it is the only thing telling an operator whether the upgrade is urgent.
3. Any open advisory that this release fixes is ready to publish at the same moment. The fix and the
   disclosure go out together; a fix pushed ahead of its advisory tells an attacker about a bug
   before it tells an operator.
4. Migrations apply cleanly from the previous release's schema.

## Verifying a download

`checksums.txt` is served from the same place as the artifacts, so on its own it proves only that the
release page agrees with itself: anyone who could replace a tarball could replace the checksum beside
it. The signature is what makes the check mean something.

Signing is keyless — there is no Pando signing key to distribute, and none to leak. cosign holds a
short-lived certificate bound to the release workflow's identity, so what you verify is *"this was
produced by Pando's release workflow"*.

Download the tarball, `checksums.txt` and `checksums.txt.sigstore.json` from the release, then:

```bash
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/(bemeek-io|trypando)/pando/\.github/workflows/release\.yml@refs/(heads/main|tags/v)' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'

sha256sum --check --ignore-missing checksums.txt
```

The identity matches two refs because there are two ways to release. The normal one dispatches the
workflow from `main` and lets it create the tag, so the run — and therefore the certificate — belongs
to `refs/heads/main`; the escape hatch pushes a `v*` tag by hand and the run belongs to that tag. A
regex naming only `refs/tags/v` rejects every release cut the normal way, which is worse than no
instruction: it tells the careful reader that a good release is forged.

Both halves of the identity still matter. The certificate has to name *this* repository's
`release.yml`, issued by GitHub's OIDC provider — a signature from any other workflow, in any other
repository, fails.

The first command establishes that `checksums.txt` came from Pando's release workflow. The second
establishes that the file you have is the one it describes. Running the second without the first is
the case this section exists to warn about.

Homebrew checks the cask's own SHA-256 on install, so `brew install trypando/tap/pando` covers the
second step but not the first — the cask is written by the same release that wrote the artifact.

## Verifying the image

The image is signed the same way, keyless, by digest, so the signature covers every tag that points
at it. The certificate names `image.yml` rather than `release.yml`: the release calls it as a
separate workflow, and a keyless certificate names the workflow that ran.

```bash
cosign verify trypando/pando:0.3.1 \
  --certificate-identity-regexp '^https://github\.com/(bemeek-io|trypando)/pando/\.github/workflows/image\.yml@refs/(heads/main|tags/v)' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

Each image also carries an SBOM and SLSA provenance, attached by the build as attestations:

```bash
docker buildx imagetools inspect trypando/pando:0.3.1 --format '{{ json .SBOM }}'
docker buildx imagetools inspect trypando/pando:0.3.1 --format '{{ json .Provenance }}'
```

To run exactly what you verified, pin the digest `cosign verify` printed in place of the tag in
`docker-compose.yml`: `image: trypando/pando@sha256:…`.

## What running installations read from a release

Every installation with the update check on reads this repository's GitHub release list
(R-349 – R-351, design 00 §1.4). It depends on three things a release already does, so changing any
of them changes what installations show:

- **The release body is the version's `CHANGELOG.md` section.** It is shown as the changelog on the
  Updates screen and in `pando updates`.
- **The Security subsection says exactly `No new advisories.` when there are none.** Anything else
  there marks the release as a security fix on every installation behind it.
- **A tag with a suffix is a prerelease.** Only installations on the `prerelease` channel are offered
  one.

## Upgrading and going back

Upgrading is moving to a newer image or binary; migrations run at startup. Migrations run forward only.
An older Pando refuses to start against a database a newer one migrated (R-354), so going back to an
older version means restoring the backup taken before the upgrade. Take one first:
`pando backup create`, or **Backups** in the console.

## Security releases

A release that fixes a vulnerability follows the normal process, plus:

- The GitHub Security Advisory is published in the same window as the release.
- The `CHANGELOG.md` **Security** subsection names the identifier, the severity, the affected version
  range and what an operator should do beyond upgrading, if anything.
- A critical or high severity fix is released on its own rather than waiting for unrelated work, so
  the upgrade carries the smallest possible change.

See [`SECURITY.md`](../SECURITY.md) for how a vulnerability reaches this process in the first place.
