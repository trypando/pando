<div align="center">

<img src="docs/assets/pando-mark.png" alt="Pando" width="88">

# Pando

**Deploy and share apps**

[trypando.ai](https://trypando.ai) · [Quickstart](https://trypando.ai/quickstart)

[![CI](https://github.com/trypando/pando/actions/workflows/ci.yml/badge.svg)](https://github.com/trypando/pando/actions/workflows/ci.yml)
[![Docker Hub](https://img.shields.io/docker/v/trypando/pando?sort=semver&label=docker%20hub&color=1D63ED)](https://hub.docker.com/r/trypando/pando)
[![codecov](https://codecov.io/gh/trypando/pando/branch/main/graph/badge.svg)](https://codecov.io/gh/trypando/pando)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)
[![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/trypando/pando/badge)](https://scorecard.dev/viewer/?uri=github.com/trypando/pando)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/14626/badge)](https://www.bestpractices.dev/projects/14626)
[![Quality gate status](https://sonarcloud.io/api/project_badges/measure?project=trypando_pando&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=trypando_pando)
[![AI Code Assurance](https://sonarcloud.io/api/project_badges/ai_code_assurance?project=trypando_pando)](https://sonarcloud.io/summary/new_code?id=trypando_pando)

</div>

<img src="docs/assets/hero.png" alt="Deploy and share any app. Pando runs on your own server and hosts the apps your team builds, including the ones written with AI. Beside the text, a topographic contour map with a red summit marker." width="100%">

---

## What is Pando?

Pando is a self-hosted deployment platform. It runs on a machine you control — a laptop, a home
server, a VPS, a server inside a company network — and hosts web applications on it: building them
from a git repository, running them in containers, giving each an address, and deciding who can
reach them.

It is used two ways, and does the same job in both. On your own machine it takes the place of a
hosting provider for side projects and tools you would rather not pay to host. Inside a team or a
company it is one place to run internal applications, with accounts, groups, roles, host policy and
an audit log — and with every app reached through one Pando hostname rather than a public hostname,
certificate and firewall rule per application.

What it does:

- **Deploys from a repository URL.** Pando clones it, works out how the app should be built and run,
  and shows you what it worked out before building anything.
- **Builds five ways**, picked automatically: a Dockerfile, a compose file, an image you already
  publish, a static site, or a buildpack for apps with none of those. Compose files are imported as
  written, including multiple services, healthchecks, volumes and startup order.
- **Puts sign-in in front of every app.** Apps are private by default. Share one with specific people
  or groups, or make it public. Your app receives a signed token describing who is visiting, so it
  does not need its own login.
- **Creates databases on request.** Postgres, MySQL and Redis can be provisioned per app, with
  credentials generated, injected as environment variables, and kept stable across redeploys.
- **Manages secrets, environment variables, volumes and resource limits** per app.
- **Gives you logs, a terminal into a running container, and one-click rollback** to any previous
  configuration.
- **Backs up app storage**, including automatically before an app is deleted.

### For teams and organizations

The same installation, configured rather than upgraded. There is no separate edition and no feature
gating.

- **Accounts, groups and roles.** Permissions are individual verbs in two scopes — what someone can
  do to one app, and what they can do to the installation. The four built-in roles cannot be edited,
  and custom roles are composed from the verb list. Access can be granted to a group as easily as to
  a person, and group membership is read at the moment of the decision rather than copied into
  grants.
- **Single sign-on and provisioning.** People sign in with a local password or through your identity
  provider over OpenID Connect or SAML 2.0 — Okta, Microsoft Entra ID, Google Workspace, Keycloak,
  Authentik or any other. A provider's groups arrive in Pando, where you decide what they can do, and
  SCIM 2.0 creates, suspends and regroups people the moment the provider says so. Each provider is
  tested with a real sign-in before anyone else sees it, and password sign-in can be turned off once
  it is working. See [`docs/identity-providers.md`](docs/identity-providers.md).
- **Two separate planes.** Being able to *use* an app and being able to *administer* it are separate
  grants, so somebody who runs an application need not be able to open it, and somebody who uses it
  daily need not be able to change it.
- **Host policy**, set once and enforced everywhere: which sources apps may be deployed from, whether
  apps may be made public, minimum isolation for builds and for runtime, where apps may connect out
  to (an allowlist or a denylist, which apps may narrow and loosen only as policy allows), deploys
  that wait for somebody's approval, whether a backup is required before anything is destroyed, a
  maximum lifetime for API tokens, and verbs
  disabled installation-wide — `app.exec` most often, and separately for automated tokens.
- **An audit log that cannot be rewritten.** Every action is recorded with who did it, and the
  database role Pando runs as holds no `UPDATE` or `DELETE` on that table, so neither Pando nor an
  adapter can alter it after the fact.
- **Events, sent where you will see them.** Subscribe to deploys, failures, sign-ins, grants, backups
  and the rest — on one app, or the whole installation — and Pando posts each to a webhook, signed so
  the receiver can check it, or to Slack, Microsoft Teams, Discord, email or ntfy. Failed deliveries are
  retried and recorded, and can be sent again. The catalog is [`docs/events.md`](docs/events.md).
- **Isolation between apps.** Each app runs on its own private network and publishes nothing to the
  host. Builds run in a rootless builder with no access to a container runtime socket.
- **One way in.** All traffic reaches applications through Pando's proxy, which authenticates the
  caller and makes the authorization decision. There is no bypass for public apps or for websockets.

## Console

`http://localhost:8080`. Two views, depending on your permissions: a page of app tiles for people who
only need to open apps, and an admin interface for people who deploy and configure them.

The admin interface covers apps and their configuration, sharing and access, environment variables
and secrets, host policy, user accounts, groups and roles, backups, the audit log, event
subscriptions, and a terminal into any running container.

## CLI

[Installed separately](https://trypando.ai/quickstart#cli), or used from inside the container with
`docker compose exec pando pando …`.

```bash
pando login https://pando.example.com    # stores an API token for this machine

pando app add https://github.com/you/notes
pando app list
pando app show notes
pando deploy notes                       # or: pando deploy ./local-directory
pando logs notes --follow
pando exec notes -- sh

pando slot set notes database --provision   # let Pando create the database
pando secret set notes STRIPE_KEY
pando grant add notes --user usr_01HQ8…     # give someone access
pando rollback notes                        # to the previous configuration
pando export notes                          # the app's full spec, as JSON
```

Also `pando backup`, `pando policy`, `pando token` and `pando subscriptions`. Run `pando <command> --help` for details, and
`--server` to talk to an installation other than the one you logged into. Every command, with its
flags: [`docs/cli.md`](docs/cli.md).

For CI, a container or anything else with no home directory to store a login in, set a token in the
environment instead:

```bash
export PANDO_SERVER=https://pando.example.com
export PANDO_TOKEN=tok_…      # from the console, under API and tools
```

## MCP server

Pando exposes its API to coding agents over MCP, so an agent can deploy and inspect apps directly.
It runs on your machine over stdio and uses the token from `pando login`.

```bash
pando login https://pando.example.com
```

**Claude Code:**

```bash
claude mcp add pando -- pando mcp
```

**Any other MCP client**, in its config file:

```json
{
  "mcpServers": {
    "pando": {
      "command": "pando",
      "args": ["mcp"]
    }
  }
}
```

The tools, with their arguments: [`docs/mcp.md`](docs/mcp.md). An MCP client that runs `pando mcp`
without a login of its own takes `PANDO_SERVER` and `PANDO_TOKEN` from its `env` block.

An agent's token carries the same permissions you do and no more, and everything it does appears in
the audit log under your name. Running commands inside apps, reading secret values and changing who
has access are not available as MCP tools, and are denied to tokens by host policy by default.

## HTTP API

Everything above is a client of `/api/v1`, which you can use directly with a session cookie or a
bearer token from `pando token`. Errors return a machine-readable code, a description, and a
suggested fix:

```json
{
  "code": "PLAN_SLOT_UNFILLED",
  "message": "This app needs a PostgreSQL database, and one hasn't been chosen yet.",
  "remedy": "Choose how to fill the database slot: provision one inside this app, connect to an existing one, or paste a connection string.",
  "details": { "slots": [{ "key": "database", "type": "postgres" }] },
  "request_id": "req_01HQ8…"
}
```

Every endpoint, the verb it asks for and every error code: [`docs/api.md`](docs/api.md) — generated
from the running code, so it cannot describe a version of Pando that no longer exists. The same
document is in the console under **API and tools**, which is also where you mint a token, and at
`GET /api/v1/reference` for anything that would rather read it as JSON.

## Configuration

Set on the `pando` service in `docker-compose.yml`, or in the environment.

| Variable | Default | Purpose |
|---|---|---|
| `PANDO_PORT` | `8080` | Port the console and API are served on. |
| `PANDO_ADMIN_PASSWORD` | generated | Initial admin password. Read only on first run. |
| `PANDO_APP_PORT_START` / `_END` | `9000` / `9019` | Range of host ports apps are given. Sets both what Compose publishes and what Pando allocates. |
| `PANDO_BASE_DOMAIN` | `localtest.me` | Domain per-app subdomains are taken from, when using hostname routing. |
| `PANDO_DATABASE_URL` | the bundled Postgres | Point Pando at an existing database instead. |
| `PANDO_DOCKER_SOCKET` | `/var/run/docker.sock` | The Docker socket Pando's container mounts. On Linux, set it to rootless Docker's, usually `/run/user/1000/docker.sock`; see [`docs/rootless.md`](docs/rootless.md). |
| `PANDO_SERVER_EXTERNAL_URL` | — | The address browsers reach Pando on, such as `https://pando.example.com`. Set this whenever something else terminates TLS — it is what marks the session cookie `Secure`. |
| `PANDO_SERVER_TRUSTED_PROXIES` | — | The proxies in front of Pando, such as `10.0.0.5`, separated by commas. Set it so the audit log records each client's own address rather than the proxy's. |
| `PANDO_POLICY_<SETTING>` | — | Fixes a host policy setting, such as `PANDO_POLICY_MIN_SECURITY_SCORE=70`. It cannot then be changed in the console. Compose passes only the variables listed on the `pando` service, so add it there. See [the reference](docs/reference.md#host-policy-at-startup). |

## What Pando does not do

It does not schedule across machines: one installation runs apps on its host, and multi-machine
support would come from a runtime adapter that spans machines. It does not run your tests. It is not
an app marketplace, not a disaster-recovery product with RPO/RTO guarantees, not multi-region, and
not multi-tenant — one installation serves one organization.

It reports problems rather than working around them. If an app loads its assets from the domain root
and will break under a path prefix, Pando says so; it does not rewrite the app's pages.

## Documentation

- [`docs/reference.md`](docs/reference.md) — the external interfaces in one place: the HTTP API, what
  an app receives, every configuration variable, and the guarantees worth relying on.
- [`docs/design/04-api.md`](docs/design/04-api.md) — the full API reference.
- [`docs/identity-providers.md`](docs/identity-providers.md) — connecting Okta, Entra ID, Google
  Workspace, Keycloak, Authentik or another provider, and SCIM.
- [`docs/rootless.md`](docs/rootless.md) — running Pando on rootless Docker on a Linux server, so a
  compromise of Pando is not root on the host.
- [`CHANGELOG.md`](CHANGELOG.md) — what changed in each release, and whether you need to act.

## Reporting a problem

- **A bug**, or something that does not work as documented: [open an
  issue](https://github.com/trypando/pando/issues/new/choose).
- **A security vulnerability**: privately, through [the advisory
  form](https://github.com/trypando/pando/security/advisories/new) — not as an issue. See
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
