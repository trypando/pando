<div align="center">

<img src="docs/assets/pando-mark.png" alt="Pando" width="88">

# Pando

**Deploy and share apps**

[trypando.ai](https://trypando.ai) · [Quickstart](https://trypando.ai/quickstart) · [Docs](https://trypando.ai/docs) · [Sponsor](https://github.com/sponsors/trypando)

[![CI](https://github.com/trypando/pando/actions/workflows/ci.yml/badge.svg)](https://github.com/trypando/pando/actions/workflows/ci.yml)
[![Docker Hub](https://img.shields.io/docker/v/trypando/pando?sort=semver&label=docker%20hub&color=1D63ED)](https://hub.docker.com/r/trypando/pando)
[![codecov](https://codecov.io/gh/trypando/pando/branch/main/graph/badge.svg)](https://codecov.io/gh/trypando/pando)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/trypando/pando/badge)](https://scorecard.dev/viewer/?uri=github.com/trypando/pando)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/14626/badge)](https://www.bestpractices.dev/projects/14626)
[![Quality gate status](https://sonarcloud.io/api/project_badges/measure?project=trypando_pando&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=trypando_pando)

</div>

https://github.com/user-attachments/assets/9c2cc4df-e2c5-4ff7-bc26-a8f08a4b6061

---

## What is Pando?

Pando is an open-source, self-hosted platform for running the apps your team builds, including ones
written with AI. Point it at a git repo or image and it builds the app, scans it for
vulnerabilities, runs it on its own isolated network, and puts it behind your company's SSO, shared
only with the people who need it.

Built for IT and platform teams who want employees to deploy internal tools without those tools
living on laptops or personal cloud accounts. Also works on a home server.

**Status: alpha.** Expect breaking changes; see the [CHANGELOG](CHANGELOG.md) before upgrading.
Needs Docker and Docker Compose. [Quickstart →](https://trypando.ai/quickstart)

## Get started

Install Pando with the [quickstart](https://trypando.ai/quickstart).

## Documentation

On [trypando.ai/docs](https://trypando.ai/docs):

- [API](https://trypando.ai/docs/api), [CLI](https://trypando.ai/docs/cli) and
  [MCP server](https://trypando.ai/docs/mcp)
- [Errors](https://trypando.ai/docs/errors) and [Events](https://trypando.ai/docs/events)
- [Adapters](https://trypando.ai/docs/adapters) and [Configuration](https://trypando.ai/docs/configuration)

In this repository:

- [`docs/identity-providers.md`](docs/identity-providers.md): connecting Okta, Microsoft Entra ID,
  Google Workspace, Keycloak, Authentik or another provider, and SCIM.
- [`docs/rootless.md`](docs/rootless.md): running Pando on rootless Docker or Podman on Linux.
- [`docs/reference.md`](docs/reference.md): running on several hosts or on Kubernetes, host policy at
  startup, and what an app receives from Pando.
- [`CHANGELOG.md`](CHANGELOG.md): what changed in each release, and whether you need to act.

## Reporting a problem

- **A bug**, or something that doesn't work as documented: [open an
  issue](https://github.com/trypando/pando/issues/new/choose).
- **A security vulnerability**: privately, through [the advisory
  form](https://github.com/trypando/pando/security/advisories/new), not as an issue. See
  [`SECURITY.md`](SECURITY.md) for what to include and what response to expect.
- **A question**: [Discussions](https://github.com/trypando/pando/discussions).

## Contributing

Bug reports and pull requests are welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for how the
project is organized, how to build it, and what a change needs before it can be merged.
[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) applies everywhere the project happens.

## License

AGPL-3.0, with a commercial license available for embedding Pando in a proprietary product or
offering it as a hosted service without publishing modifications. See
[`LICENSING.md`](LICENSING.md) for which applies to you, and [`LICENSE`](LICENSE) for the full text.
Contributors sign the [Contributor License Agreement](CONTRIBUTOR_AGREEMENT.md) once, by posting a
comment on their first pull request.
