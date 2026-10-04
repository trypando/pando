# Changelog

Notable changes to Pando, written for the person deciding whether to upgrade and what will change
when they do. This file is not a git log; a change that nobody operating an installation would notice
does not belong here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Pando follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). What a version number promises is in
[`docs/releasing.md`](docs/releasing.md); how a release is cut is in
[`CONTRIBUTING.md`](CONTRIBUTING.md#releasing).

Every release section names, in this order: **Security** (including every publicly known vulnerability
fixed in it, with its CVE or GHSA identifier), **Added**, **Changed**, **Deprecated**, **Removed**,
**Fixed**, and **Upgrade notes** for anything requiring an operator action.

<!-- Rename this heading to the version and date when the next release is cut — the release
workflow reads the section matching the tag and refuses to release without one — and open a fresh
Unreleased above it. -->

## [Unreleased]

### Added

- In-place upgrades (#53). Pando can upgrade itself from the **Updates** screen, `POST /upgrade` or
  `pando upgrade`: it verifies the new image's signature against Pando's release workflow, takes a full
  backup (or records that you skipped it), and starts a helper that replaces Pando's container, waits
  for the new version to be ready, and puts the previous version and its database back if it is not.
  Off until the deployment turns it on: set the image to a moving tag such as `trypando/pando:latest`
  and `PANDO_POLICY_UPGRADE_IN_PLACE: "true"`, or use **Let Pando upgrade itself** on the Policy screen.
  New permission `install.upgrade`, held by Administrator and denied to agent tokens by default. Host
  policy gains `upgrade_in_place`, `auto_upgrade_patches` and `maintenance_window` for automatic patch
  upgrades. `pando upgrade last` and `GET /upgrade/last` say how the last one went.
- `pando self-update` replaces a CLI installed from a release archive or with `go install`, after
  checking the release's signature. Holders of `install.upgrade` are notified once of each new release.

- Update checks (#53). Pando asks GitHub at startup and every six hours whether a newer release is
  out, sending only its version. The new **Updates** screen, `GET /updates`, `pando updates` and MCP's
  `pando_get_updates` show the version running, the latest release, and the changelog of every version
  in between, with security fixes and breaking changes marked, then the command that upgrades this
  installation. Host policy gains `disable_update_check`, which stops the check and every request it
  sends, and `update_channel` (`stable` or `prerelease`), on the Policy screen under **Updates**.
  The CLI warns when it and the server differ in major or minor version, and names the command that
  upgrades it the way it was installed. Upgrading from inside Pando is not part of this release.
- Pando refuses to start against a database a newer version of Pando migrated, and says to run that
  version or restore the backup taken before the upgrade. It used to fail with "Database migration
  failed." and restart in a loop, leaving every app unreachable.

- Audit log retention (#60). The live audit log keeps **three months** by default; an older month is
  archived as gzipped JSON lines with a manifest (row count, first and last event, time range, SHA-256),
  read back and checked, and only then removed — and the removal is itself an audit event. Archives are
  kept under `/var/lib/pando/audit-archives` (`server.audit_archive_dir`) or exported to a backup
  destination. Host policy gains `audit_retention_months` (at least 3), `audit_archive` (`keep`,
  `export` or `off`) and `audit_archive_destination`, on the Policy screen under **Audit log**.
  Archived months are listed and downloaded from the **Audit log** screen, `GET /audit/archives`,
  `pando audit archives` and `pando audit archives download`, and listed by MCP's
  `pando_list_audit_archives`. The account Pando serves traffic as still cannot change or remove any
  audit event: a month is removed by a separate database role, through a function that refuses one
  under three months old or without a verified archive.

- Single sign-on and provisioning (#51). Connect an identity provider over **OpenID Connect** or
  **SAML 2.0** on the new **Sign-in** screen, with presets for Okta, Microsoft Entra ID, Google
  Workspace, Keycloak and Authentik; test it with a real sign-in that shows every claim the provider
  sent and what Pando would do with it; then turn it on. Providers are added and changed without a
  restart. **SCIM 2.0** per provider creates, updates and suspends accounts and sets group membership
  at once; deprovisioning suspends and never deletes. A provider's groups become synced groups that
  you give roles and app access, or link to an existing Pando group. Accounts are made at a first
  sign-in only when a provider is set to (off by default), and an administrator links an identity to
  an existing account — email linking is opt-in and only on a verified email. Host policy gains
  **Turn off password sign-in** and **Don't create accounts at first sign-in**. The CLI has
  `pando identity-provider` (`pando idp`); setup per provider is in `docs/identity-providers.md`.

- Waiting for detection (#80). `GET /apps/{id}/detection?wait=30` answers as soon as detection
  reaches a new stage or finishes, and while it runs says which stage it is on and for how long.
  `pando app detection <app> --wait` prints each stage and exits 0 when the app is ready to accept,
  2 when there are questions to answer and 1 when detection failed or was blocked; `pando app add
  --wait` does the same. MCP's `pando_get_detection` takes `wait_seconds`. Apps in `GET /apps`,
  `GET /apps/{id}` and `pando app list` say where their detection is, so a draft says why.
- Who used an app is in the audit log: `app.use`, once per visit (a browser session, or a token's use
  within twelve hours), with the first page visited. Visitors who are not signed in are recorded too,
  with the address Pando saw; host policy's **Don't record visits from people who aren't signed in**
  (`disable_anonymous_use_audit`) turns that off. Audit search can now answer "who accessed this app".
- AI functions beyond detection (#74): drafting roles and groups, drafting host policy, searching the
  audit log with a question, and answering "How can I…" from the API, CLI and MCP reference. Each
  proposes and a person applies. Each AI function is assigned to one AI adapter, optionally on a model
  of its own, chosen when editing the adapter.
- OpenAI and local-model AI adapters, beside Anthropic. The local adapter talks to any server that
  speaks the OpenAI API, such as Ollama, LM Studio, llama.cpp's server or vLLM, and sends nothing to a
  provider.
- Adapters, and the AI functions each handles, can be declared in the config file's `adapters:`
  section, with credentials named by environment variable or file. Declared ones are read-only in the
  console, API, CLI and MCP.

- A security scan now shows while it runs: in the app's Security section and in the app list's
  Security column, wherever the scan was started (a deploy, detection, the CLI, MCP or **Scan
  now**). The API reports it as `scanning_since` on `GET /apps/{id}/security` and
  `security_scanning` on each app in `GET /apps`.

- **Egress rules** (#79). Host policy sets where apps may connect out to: anywhere, anywhere except
  a denylist, or only an allowlist, plus a separate switch that blocks private addresses (the local
  network, loopback, the cloud metadata address). An app can add or remove entries, or keep a list of
  its own that narrows the installation's. Changes that loosen the installation's rules are allowed by
  permission, only after a deploy approval, or never, as policy says. The plan and the app's
  **Settings** tab show the merged rules and where each came from. On Docker, a restricted app runs
  on a network with no route out and reaches the internet only through a per-app egress gateway, as
  HTTP or HTTPS via `HTTP_PROXY`/`HTTPS_PROXY`; an app with no restriction runs exactly as before.
- **Deploy approval** (#39). Policy can require approval for every app's deploys or for chosen apps,
  and an app can require it of itself. A deploy then waits until enough people holding
  `install.deploys.approve` (administrators) or `app.deploy.approve` (no built-in role) approve it,
  with a configurable count and expiry (default one approval, seven days). Rolling back to a revision
  that ran needs no approval; auto-deploy is refused while approval is required. The console has an
  **Approvals** screen; the API, `pando approvals` and three MCP tools do the same. Agent tokens
  cannot approve by default.
- **Permissions on every app, one at a time** (#81). Each app permission has an installation-wide
  counterpart that grants it on every app, including apps added later: `install.apps.deploy`,
  `install.apps.logs.read`, `install.apps.secrets.read` and so on. A group gets "read every app's logs"
  once instead of a grant on each app. Three new built-in installation roles: **App viewer** (see every
  app and its logs), **App manager** (Owner on every app, without approving deploys) and **Auditor**
  (every app's view and logs, and the audit log). A change allowed through one of these, rather than a
  grant on the app, is written to the audit log as `authz.install_wide`, naming the grant.

### Changed

- `install.apps.manage` is gone; the **App manager** role holds its thirteen verbs. `install.apps.view`
  now means seeing every app only, and every role that held it also holds `install.apps.logs.read`, so
  what it allowed is unchanged.

- An install has at most one AI adapter per provider, and an AI adapter has no default: each AI
  function is off until assigned. The existing AI adapter keeps plan repair, answering detection's
  questions and plan revision on upgrade; the new functions start off.
- Pando sets a `pando_visit_<app>` cookie on responses from apps, to recognize a visit. Like every
  cookie named `pando_…`, it never reaches the app.
- Pando moved to the `trypando` GitHub organization. The repository is
  `github.com/trypando/pando`, the Go module path is `github.com/trypando/pando`, and the Homebrew tap
  is `trypando/tap/pando`. Old `github.com/bemeek-io/...` URLs redirect.

- `app.egress.override` is now `app.egress.loosen`, and a new `app.egress.tighten` (Owner and
  Operator) changes an app's egress within the installation's rules. An app's own allowlist now
  narrows the installation's instead of replacing it.

### Fixed

- **Redeploying an unchanged source ran the security scan again** (#84). A deploy of a commit,
  image or uploaded archive that already has a successful scan by the configured scanner uses that
  scan and says so in the deploy log ("Using the security scan of … (source unchanged)"). That
  includes a new revision of the same source, which used to be refused as never scanned where a
  minimum score was set, and every `pando deploy .` of an unchanged directory. A scan that failed,
  or one by a different scanner, is not reused. "Scan now" in app settings always scans.
- **A Laravel app failed to build.** Laravel 13 needs PHP 8.3, the build installed an older PHP, and
  the build log said only `syntax error, unexpected token "{"` (#58). A Laravel app is now built on
  the newest PHP its `composer.json` allows, with the extensions it requires, its Vite assets built,
  and served by Apache from `public/`; it runs its migrations before it starts. When a dependency
  needs a PHP version or extension the build does not have, the build fails saying which. Deploying
  one now asks for `APP_KEY`, which the app cannot serve a page without; `php artisan key:generate
  --show` prints one. Detect a Laravel app again to pick this up.
- **`pando deploy <dir>` printed blank lines where detection's questions belonged.** It read a field
  the server never sends. The questions are now printed exactly as Pando wrote them.
- **A static site's pages redirected to the wrong port.** Opening a folder without its trailing slash
  (`/solutions`) on an app reached at an address with a port, such as `localhost:9001`, sent the
  browser to the same path on port 80, where nothing answers (#67). The redirect now keeps the
  address the browser used. Redeploy a static site to pick this up.
- **A Jekyll site was served as its source.** A GitHub Pages site deployed and looked healthy, but its
  Markdown pages showed as raw Markdown or failed to load (#67). Pando now builds a Jekyll site with
  Jekyll before serving it. A GitHub Pages site (no Gemfile, or one naming `github-pages`) is built
  the way GitHub builds it, with the current `github-pages` gem; any other Jekyll site with its own
  Gemfile, without its development and test gems. Either is served at the root of the app's address
  whatever `baseurl` the site sets for GitHub. Detect the app again to get the new build plan.
- **Daily app backups could silently not happen** (#87). Two apps that each kept a volume with the
  same name (`data` is common) shared one record of it: the second app was never backed up, the
  first app's backups copied the second app's data, and the second app lost the protection against
  recreating a lost volume empty. Each app now has its own record, and Pando repairs existing records
  from what the runtime reports for each running app within a minute of starting. An app whose configuration was saved without
  a backup count is now backed up with the default of 7 rather than skipped. A whole-installation
  backup names each app's volumes separately; older backups still restore.
- Every daily backup attempt is recorded, and the last one — taken, skipped or failed, with why and
  what to do — is shown on the app's overview and on **Backups**. The server log says "took a rolling
  backup" only when one was taken.

### Upgrade notes

- **Audit events older than three months leave the live log** at the first daily pass after
  upgrading, archived under `/var/lib/pando/audit-archives` first. To keep everything in the live
  log, start the upgraded server with `PANDO_POLICY_AUDIT_ARCHIVE=off` (or `audit_archive: off` in
  host policy) and nothing is archived or removed. Migration 000041 partitions `audit_events` by
  month by copying it, which takes a while on a large log, and adds a second restricted database
  role, `pando_audit_archiver`; an external database must let Pando's account create it, as it
  already creates `pando_app`.

- Migration 000040 replaces `install.apps.manage` with its thirteen `install.apps.*` verbs in every role
  that held it, custom roles included, adds `install.apps.logs.read` wherever `install.apps.view` was,
  and adds the App viewer, App manager and Auditor roles. A custom role already named one of those is
  renamed with " (custom)". Scripts that create a role with `install.apps.manage` need the new verbs,
  or a grant of `role_app_manager`.

- Migration 000039 renames `app.egress.override` to `app.egress.loosen` in every role that held it,
  custom roles included, and in host policy's disabled-verb lists. Scripts that name the old verb need
  the new one.
- **An install that set `egress_allowlist` is now held to it.** That setting was never enforced
  before; after upgrading, apps reach only what it lists. Clear it, or set `egress_mode`, before
  upgrading if that is not what you want. Rules take effect at each app's next deploy.
- Restricted egress on Docker needs the gateway image: Pando uses its own container's image, or the
  runtime adapter's `egress_gateway_image`. Pando run directly on a host has neither, so a deploy with
  a restriction is refused with a message saying why.

- Migration 000037 adds the external identity tables and the `effective_group_members` view, which
  authorization now reads for group membership. No existing behavior changes until a provider is
  added. To use identity providers behind a proxy, set `PANDO_SERVER_EXTERNAL_URL`: the addresses you
  register with a provider are built from it.
- `pando admin enable-password-sign-in` is new: the way back in from the host if password sign-in is
  off and no provider works.

- An install with more than one AI adapter of the same provider must remove all but one before
  upgrading; the migration stops with a message naming the provider otherwise.
- `go install github.com/bemeek-io/pando/cmd/pando@latest` keeps installing the last version
  released before the move. Use `go install github.com/trypando/pando/cmd/pando@latest`.
- A Homebrew install from the old tap: `brew untap bemeek-io/tap && brew install trypando/tap/pando`.
- `cosign` verification in `docs/releasing.md` accepts both the old and the new signing identity, so
  releases from before the move still verify.

## [0.3.0] - 2026-09-24

Pando is now installed from a prebuilt, signed image rather than built on the host. An existing
installation, whether it moves to the image or keeps building from a clone, needs its data volume's
owner changed once, because the server now runs as a different user; a clone's build also needs a
Docker account now. Both are in the upgrade notes.

### Security

No new advisories. The server image is new in this release: it is built on Docker Hardened Images,
runs the server as a non-root user, and is scanned with Trivy before it is published.

### Added

- **A prebuilt server image, `trypando/pando` on Docker Hub** (#52). Installing no longer means
  cloning the repository and building on the host: download the release's `docker-compose.yml` and
  run `docker compose up -d`. Built on Docker Hardened Images, with no package manager in the image
  and the server running as the base's non-root user; for `linux/amd64` and `linux/arm64`; signed keyless with cosign,
  with an SBOM and SLSA provenance attached, and scanned with Trivy before it is pushed. Tagged with
  the exact version, the minor line, and `latest` for a stable release. Each release runs the
  published image from its own compose file and deploys an app on it before it counts as done.
  Verifying the image is in [`docs/releasing.md`](docs/releasing.md#verifying-the-image).
- The release carries a `docker-compose.yml` that pins the image to that version.

### Upgrade notes

- An installation started from a clone of the repository can move to the published image: download
  the release's `docker-compose.yml` into the same directory, so the compose project and its named
  volumes stay the same, and run `docker compose up -d`. Keep the `POSTGRES_PASSWORD` it was started
  with. The server now runs as UID 65532 rather than 10001, so an existing `pando-data` volume needs
  its owner changed once: `docker compose run --rm --user 0 --entrypoint chown pando -R 65532:65532 /var/lib/pando`.
- Building the image from source needs `docker login dhi.io` with a Docker account first, because its
  base images are Docker Hardened Images.

## [0.2.0] - 2026-09-23

Detection stops asking for things the repository already told it, reads the build instructions an app
carries rather than inferring them, and plans more kinds of app itself. This release also adds a
security score for every app, optional AI screening of detection proposals, account, group and sharing
management in the console, and a launcher each person can arrange. Several changes alter what Pando
does with apps you already run — a forced delete now destroys the app's volumes, every deploy is
scanned, and administrators can manage every app — so read the upgrade notes before upgrading.

### Security

No new advisories. The six open against `github.com/docker/docker` are unchanged and remain accepted
with their reasoning in [`.github/govulncheck-allowlist.txt`](.github/govulncheck-allowlist.txt):
all six are Moby **daemon** vulnerabilities, and `go list -deps` on the runtime adapter resolves to
`api/…`, `client` and `pkg/stdcopy` with no `daemon/…` or `plugin/…` package in the binary. The
module has no fixed release and will not get one.

- An open redirect in the console's sign-in page. `returnTo` accepted a `next` parameter after
  checking it began with one `/` and not two — but `/\evil.example` passes that and the URL parser
  still reads it as `//evil.example`, because where an authority may begin a backslash and a slash
  mean the same thing. Following a crafted link, signing in on the real hostname with a real
  password, and landing on somebody else's site was a working attack. `next` is now resolved against
  the current document and accepted only when the origins match, which agrees with what the browser
  will do by construction and turns away `javascript:` and `data:` in the same breath.
- **An app created from a published image read the Pando server's own files as its source.** Such
  an app has no checkout, and its source view was rooted at an empty path, which resolved against the
  filesystem root of the Pando process. Detectors read from there, so what they found could be quoted
  in the app's proposal. The app now has an empty source, and the source scan and AI screening skip
  it. Present in 0.1.x.
- **Deleting an app left its data on disk.** R-204 has a delete either keep a final backup or discard
  the app's storage, and both answers removed Pando's record of the volumes while leaving the volumes
  themselves in place, where nothing could reach or reclaim them. The app's uploaded source was also
  kept. A delete now destroys the volumes once the backup, if asked for, has been taken, and removes
  the upload (R-204, R-224). Volumes left by earlier deletes are not removed by the upgrade; see the
  upgrade notes.
- **A custom role could carry a built-in role's name.** Role names were unique case-sensitively, and
  the built-ins are stored lowercase and shown capitalized, so a custom role called "Administrator"
  appeared in every role picker beside the real one, looking identical. Names are now unique ignoring
  case and surrounding spaces, built-ins included (R-082); migration 000028 renames existing clashes.

### Added

- **Pando builds an app the way its repository says to.** Where a repository states its build — a
  `.github/workflows` build job, a `Makefile`, `Taskfile.yml` or `justfile` target, a `Procfile`'s
  web process — the plan runs those commands instead of inferring them from the language. R-094's
  confidence ladder always ranked "the maintainer's own build commands" above convention-matching;
  this implements it.
- **A client that builds into a directory the binary embeds is built first.** Where a bundler config
  names an output directory and a `//go:embed` directive names the same one, the ordering is stated
  by the repository rather than guessed, and the client build runs ahead of the binary's. Read from
  Vite, Astro, Vue CLI, Angular, Next.js (static exports), webpack and SvelteKit, or from an
  `--outDir`-style flag in the build script.
- **Pando plans more builds itself, on official images** (R-095). Static sites (Astro, Vite, Angular,
  Create React App, Gatsby) are built with `node` and served with nginx; Node servers, Go programs
  with one `main` package, Gradle and Maven projects, plain Java sources and .NET projects each get a
  plan on their language's official image, with the version read from the repository where it is
  stated. Other languages stay on nixpacks. Detection also reads a `Containerfile`, a Dockerfile in a
  subdirectory and a site in `docs/`; asks for a Dockerfile `ARG` the build refuses to run without;
  runs Rails in production with a required `SECRET_KEY_BASE`; and refuses a library with an
  explanation rather than deploying it (R-021). All [P] defaults are recorded in
  [`docs/design/notes-deploy-qa-issue-55.md`](docs/design/notes-deploy-qa-issue-55.md).
- **An app created from a published image is proposed as that image**, rather than sent through
  repository detection over an empty checkout. The trial run finds its port, paths the image declares
  with `VOLUME` get a volume, and an image that serves only a database or mail protocol is refused with
  the reason (R-097, R-200).
- **A security score for every app** (R-310 – R-320). A number from 0 to 100 from scanning the image
  an app deploys and the source it was built from, weighted by severity (25 per critical, 10 high,
  3 medium, 1 low), shown on the app's overview and in the apps list with the findings behind it,
  worst first. Scanning is a new adapter category; the Trivy adapter runs in a container pinned by
  digest, with no container runtime socket. A new app is scanned when it is detected, every deploy is
  scanned, and **Scan now** works on an app never built. Host policy can refuse deploys below a
  minimum score (`min_security_score`, `PLAN_SECURITY_BELOW_THRESHOLD`), and for a running app that
  falls below it, notify the owner and optionally stop it after a grace period (`insecure_action`,
  `insecure_grace_hours`) — never delete it. `ignore_unfixable_findings` scores only what can be fixed.
  Nothing is enforced until an administrator sets a threshold.
- **AI screening of detection proposals** (R-106, R-330 – R-339). AI is a new adapter category; the
  first adapter uses Anthropic's API (default model `claude-opus-5-5`). A screener reads the
  repository and the proposal and returns amendments from a closed set — no policy, isolation,
  routing, resources, egress, grants or secrets — and Pando refuses any without a reason or evidence
  in the repository, any that overwrite what a person set, and a port the trial run observed. Any
  failure leaves the proposal as detection made it. The review marks each change "Suggested by AI",
  lists what was refused, and each run is audited as `detection.screen` with the files read. No AI
  adapter is configured by default; host policy's `disable_ai_screening` forbids it. Configured, it
  sends the repository files it reads to the provider.
- **Adapter credentials are stored encrypted** (R-190). `POST /adapters` takes a write-only
  `credentials` object, sealed by the installation's secrets adapter into its own table; the database
  refuses a `credentials` key in an adapter's plain configuration, and `GET /adapters` names the
  credentials set, never their values.
- **Adapters can be added and changed from the console and the CLI**, not only the API
  (`GET /adapters/kinds`, `pando adapter list | kinds | add`, credentials prompted rather than typed).
  **Pando can restart itself** to load them: `POST /api/v1/restart`, `pando restart` and a button on
  the Adapters screen finish the requests in flight and re-execute the binary in the same process.
  Behind `install.adapters.manage` and audited as `install.restart`. `GET /adapters` says which
  adapters are waiting for a restart.
- **Stop, start and restart an app** from the console, the CLI (`pando app stop | start | restart`)
  and MCP. The API had these endpoints and no client called them, so the only way to take an app down
  was to delete it. A stopped app stays stopped across a restart of Pando.
- **Each part of an app has its own status, logs and resource use.** `GET /apps/{id}/status` lists
  every workload with whether it is running, restarting and how often, its health and exit code;
  `pando app status` prints it. Logs take a workload (`pando logs --workload`), and the console's new
  Logs tab holds the app's output and every deploy's build log. `GET /apps/{id}/usage`,
  `pando app usage` and an **In use** section show each part's CPU, memory and disk against its
  limits, and each volume's size — a reading, not a history (R-245).
- **Accounts, groups and roles are managed in the console.** Each account has a page with its
  details, groups, role, apps and audit history; an administrator can edit an account, reset its
  password, and add it to or remove it from groups. Groups hold an installation role and app roles
  that every member holds while in the group (R-078). Custom groups and roles can be deleted. A
  generated password (`POST /passwords/generate`) is offered wherever an administrator sets one, and
  by default must be changed at the next sign-in. CLI: `pando user create | update | reset-password |
  apps`, `pando group list | add-member | remove-member | role | apps`, `pando grant role | remove`.
- **A built-in Creator role** (R-081): one verb, `app.create`. A creator manages the apps they made,
  as their owner, and nothing else.
- **Administrators manage every app.** Two installation verbs, `install.apps.view` and
  `install.apps.manage`, cover every app, and the Administrator role holds both (R-080, R-081).
  Managing an app does not grant using it (R-087). `GET /apps/{id}` returns the caller's verbs, and the
  console hides controls the caller cannot use rather than letting them fail.
- **Sharing picks people and groups, and can make an app public behind a passcode** (R-075a). The
  passcode is stored as an argon2id digest; a visitor's unlock lasts a day, rides in a `pando_` cookie
  that never reaches the app (R-173), and ends when the passcode changes or the app is made private.
  Host policy's `public_sharing` (`allowed`, `passcode_only` or `none`) decides what is allowed
  (R-076). CLI: `pando grant add --group | --anyone [--passcode]`, `pando grant passcode`.
- **Host policy can be fixed at startup** (R-271): a `policy:` section in the config file or
  `PANDO_POLICY_<FIELD>`. A fixed field cannot be changed through the API or the console, which say
  where it is set; an unknown field or a mistyped value stops startup. `GET /config`, `pando config`
  and MCP's `pando_get_config` report every non-secret setting and where it came from. The Policy
  screen now reaches every policy field, including isolation floors, the egress allowlist, token
  lifetime, agent-disabled verbs and the log disk budget.
- **Resource defaults for new apps are configurable** (R-240): `PANDO_APPS_CPU_MILLIS`,
  `PANDO_APPS_MEMORY_BYTES` and `PANDO_APPS_DISK_BYTES`. Unset keeps one core, 512 MiB and 10 GiB.
- **Tokens in the console, and service tokens** (R-058, R-060). Anyone signed in can mint, list and
  revoke their own tokens; `POST /tokens/service` mints a service token for automation. The CLI reads
  `PANDO_SERVER` and `PANDO_TOKEN`.
- **A reference generated from the code.** [`docs/api.md`](docs/api.md), [`docs/cli.md`](docs/cli.md)
  and [`docs/mcp.md`](docs/mcp.md) are built from the router, the CLI and the MCP tool list, and the
  console's **API and tools** screen renders the same document from `GET /api/v1/reference`. The CLI
  page says how to install the CLI.
- **A launcher each person can arrange.** App images on square tiles (R-340; PNG, JPEG, WebP or GIF
  up to 256 KiB, with SVG converted to PNG in the browser), a generated terrain picture for apps with
  none, favorites (R-341) and named sections (R-342), with drag and drop and search. Each is per
  account and grants nothing. CLI and MCP have the same (`pando app icon | favorite | unfavorite`,
  `pando section …`).
- **Adding an app opens an onboarding page** that fills in as detection runs. Variables can be given
  values before accepting, as can `pando deploy --env KEY=VALUE` and MCP's `pando_accept_proposal`.
- **A configuration file an app cannot start without travels in the spec** (R-099a). A single file a
  compose service mounts from the repository — a `Caddyfile`, for example — is read at detection,
  stored in the spec (text, up to 64 KB) and placed in the container before it starts; a mounted
  directory of up to 16 text files is carried the same way. Editing the file in the repository changes
  nothing until the app is detected again (R-020).
- **The console:** first-run setup, dark mode, a settings page with sign out, a phone layout, search
  and column filters on every list, and audit log filters by time range, target, kind of actor and
  "involving" an account, kept in the address. `pando audit` takes the same filters and `--before` for
  paging. An app can be deleted from the console, with R-204's question about its storage.
- `WARN_NO_PERSISTENT_VOLUME` now appears for apps built from source. It previously required a trial
  run, which does not happen before an image exists — so an app that kept data on disk and declared
  no volume got no warning at all.

### Changed

- **Release signatures are a single Sigstore bundle,** `checksums.txt.sigstore.json`, instead of
  `checksums.txt.sig` and `checksums.txt.pem`. Verify with `cosign verify-blob checksums.txt --bundle
  checksums.txt.sigstore.json …`; [`docs/releasing.md`](docs/releasing.md#verifying-a-download) has the
  full command. cosign 3 writes this format by default and refused to sign without a bundle path,
  which is what failed the first 0.2.0 release attempt.
- **Package files are named like the archives:** `pando_<version>_linux_<arch>.deb`, `.rpm` and
  `.apk`, rather than each packager's own convention.
- **A new installation is set up in the console** (R-046). Without `PANDO_ADMIN_PASSWORD`, Pando
  creates no account and prints no password; the first person to reach the sign-in page chooses the
  administrator's username and password, and Pando logs a warning at every start until someone has.
  Set it up before exposing Pando to anyone else, or supply `PANDO_ADMIN_PASSWORD` as before.
- **Pando restarts a stopped workload, not Docker.** Containers are created with no restart policy;
  one that exits is started by the reconciler's next pass, so R-149's backoff and R-150's give-up rule
  actually apply. Before, Docker restarted a crashing container every two seconds underneath Pando's
  "stopped trying" message. Giving up now stops the app, keeping its containers, storage and address,
  and a person can stop or start a failed app (R-151).
- **A deploy waits for the app to stay up.** Every part must stay up for ten seconds; a part that
  stops within the two-minute wait is started again, and if the primary workload is still stopped at
  the end the deploy fails with `STATE_APP_EXITED` and the app's last 30 lines of output. A workload
  now waits for dependencies with a health check to report healthy. The deploy history shows
  "Deployed, not healthy" for a deploy whose app never became healthy.
- **`PORT` is set on an app's primary workload** to the port Pando routes to, unless the spec sets it.
  A port detection guessed for a source build is checked against the built image and corrected if the
  app listens elsewhere; an app that listens only on 127.0.0.1 is refused with
  `BUILD_LISTENS_ON_LOOPBACK` before the running version is touched (R-097).
- **Compose files import more faithfully** (R-096). A backing service the app reaches by connection
  URL becomes a Pando-provisioned slot with that variable rewired to it; one reached by hostname is
  imported as the file wrote it. A compose file of only databases no longer outbids the app. A
  refused compose file is shown with its reasons instead of silently losing to the repository's
  Dockerfile. Services behind a `profiles:` entry are left out, as `docker compose up` leaves them.
- **Only variables that name a connection become dependencies** (R-130). A variable from
  `.env.example` is a dependency when its name says connection (`DATABASE_URL`, `REDIS_URI`) or its
  sample value carries a known URL scheme; everything else is a variable with no value, listed for a
  person to fill in. A variable Pando has no value for is not set in the container at all, rather than
  set to an empty string.
- **Service tokens need their own verb,** `install.tokens.manage`, rather than `install.users.manage`
  (R-060, R-080). Revoking somebody else's personal token still needs `install.users.manage`.
- **Deleting an app takes effect at once.** Teardown runs immediately rather than at the next hourly
  pass, the app's name can be used again, and its port returns to the range (R-204).
- **App networks come from Pando's own address range,** a /26 each from `10.213.0.0/16` (the Docker
  runtime's `network_pool`, `off` for Docker's pool). Docker's default pool holds about thirty
  networks, and deploys failed after about 25 deletions.
- **The build cache is capped** at 10 GiB after each build (the BuildKit builder's `cache_max_bytes`).
  It had never been pruned.
- **API:** `GET /apps/{id}/status` returns a snake_case list of workloads, where it returned the
  adapter's struct with Go field names. `POST /apps/{id}/detection/rerun` returns 202 and detects in
  the background; clients already poll `GET /detection`, which now lists the questions still
  `unanswered`. `GET /roles` takes `scope` (`install`, the default, `app` or `all`).
- **Adapter interface:** `BuildPlanner.Plan` returns an `*api.PlanDeclaration` alongside the
  generated files, naming what in the repository dictated the plan; nil means convention-matching
  chose it. Builders gain `Forget`, and runtimes gain `Usage` and the `ReportsUsage` and
  `SupportsCarriedFiles` capabilities. Only affects out-of-tree adapters, of which there are none;
  everything ships compiled in (R-253).
- Detection reports `ready` rather than `needs_answers` when it has nothing to ask. A bid below the
  confidence threshold used to force `needs_answers` on its own, which became visible — and wrong —
  once the questions below stopped being asked.
- The console builds with TypeScript 6.0.3, and its `tsconfig.json` no longer sets `baseUrl`, which
  TypeScript 6 rejects and 7 removes.

### Deprecated

- Host policy's `allow_anonymous_grants` is superseded by `public_sharing`. It is still read —
  `false` means `none` — and `public_sharing` wins when both are set.

### Fixed

- **Deploying a repository with no Dockerfile asked two questions it could answer itself.** A plain
  Go module was asked for a start command that the generated build plan already contained, and for a
  port that the language's framework default already supplied. Both are gone; the port rides in the
  proposal as an editable default, marked as the guess it is.
- **Rolling backups failed for the whole installation** whenever any app had no storage. That app's
  spec stored `"volumes": null`, the sweep's query raised an error on it, and one failing query is
  the whole sweep (R-210).
- **A compose app's application container was replaced with a copy of its proxy.** A deployment
  recorded one image for the whole app, and the reconciler restored every workload from it and
  compared every workload against one digest, so fifteen seconds after a good deploy the app was gone.
  Each part's image and digest are now recorded and checked separately.
- **Builds that failed on Pando's own plan.** The static-site nginx config was written with shell
  escaping that broke it, so every static site exited on start; a generated plan lost
  `.nixpacks/assets`; its build arguments were never passed, so a single-page app served its source
  `index.html`; an answered start command never reached nixpacks; a mirrored workflow's `npm ci`
  failed on nixpacks' cache mount; and adding `nodejs` to a plan broke nixpacks' npm overlay. The
  console's plan editor now opens every file of the plan, not only the Dockerfile.
- **Compose details that were read and dropped:** shell quoting and `$$` in commands, `CMD-SHELL`
  health checks, `${VAR:-default}` in environment values, `env_file:`, file-backed `secrets:`,
  `build.target` and `build.args`, and anonymous volumes, which two services now no longer share.
  The web port is routed rather than the first one listed.
- **An app accepted without a primary workload could not be deployed.** Accept did not validate the
  spec. It does now, and detection picks the workload that serves HTTP and that nothing depends on,
  saying which it picked.
- **The health probe needed `wget`.** An image with only `curl` never came ready. The probe now uses
  curl, wget, a TCP connection or bash's `/dev/tcp`, whichever the image has (R-221).
- **App logs began every line with control bytes,** Docker's stream framing (R-071).
- **Deleted apps leaked disk** (R-224): their build cache, every image pulled for them, and an
  anonymous volume for each container of an image that declares `VOLUME`. Pulled images are removed
  when the last app using one is deleted, unless the image was on the host before Pando pulled it.
  After each build the cache drops what the new build no longer uses.
- **Only one group made in Pando could exist.** The second was refused as a duplicate of the first
  whatever its name (R-078; migration 000026).
- **Deleting a custom role that anyone held failed** with an internal error. Its grants are now
  removed with it, and deleting a group or role, or removing a member, is refused when it would leave
  nobody able to manage accounts (R-088).
- **Work interrupted by a restart stayed in progress forever,** and an app with a deploy in flight
  refuses the next one. Such detections and deploys are marked failed at startup.
- **Two Pando installations on one Docker host interfered:** each rejoined the other's app networks
  and removed the other's empty ones. Startup also removed a stopped app's network.
- Filling a second slot after accepting a proposal undid the first. The reconciler could re-apply
  an app deleted while it was working on it. A git fetch could hang until the detection deadline; it
  now fails fast and retries. A failed image pull now reports the registry's reason.
- A deploy's apply and routing steps reported the adapter's headline only; they now print the cause
  and the remedy, as the build step does (R-105).
- `/index.html` requested by name was served with no `Cache-Control`, so a browser could keep an old
  console after an upgrade.
- A variable named after a target — `build := ./out` — was read as declaring that target, so the plan
  ran `make build` against something that did not exist.
- Audit log paging stopped on any `limit` above the cap. The API applied the default page size but
  not the ceiling, compared a capped page of 500 against the requested 1000, decided the page was not
  full, and returned no cursor.
- The `cosign verify-blob` command in [`docs/releasing.md`](docs/releasing.md#verifying-a-download)
  rejected every release cut the normal way. It required a certificate identity ending
  `@refs/tags/v`, but a release dispatched from `main` lets the workflow create the tag, so the run —
  and the certificate — belongs to `refs/heads/main`. Anyone following the instructions on v0.1.1
  would have concluded a good release was forged. The documented regex now matches both paths.

### Upgrade notes

Migrations 000015 to 000031 run on first start. Most add tables and columns for the features above;
these change behavior or need a decision:

- **A delete now destroys the app's volumes** (000031). `force=true` discards them at once, and
  `backup=true` takes the backup and then discards them, as R-204 says; before, both left the volumes
  on disk. Volumes of apps deleted before the upgrade are still there and are not removed. Each
  carries the label `io.pando.bundle=<app ID>`; `docker volume ls --filter label=io.pando.bundle`
  lists them. Remove the ones you no longer need by hand.
- **Administrators can manage every app** (000027). Everyone holding the Administrator role gains
  `install.apps.view` and `install.apps.manage`: they see every app in the list and can deploy,
  configure and delete any of them, though not use one without a grant. Review who holds the role.
- **Service tokens moved to `install.tokens.manage`** (000030). The Administrator role has it. A
  custom role that held `install.users.manage` for the sake of service tokens needs the new verb
  added.
- **Role names that clash ignoring case are renamed** (000028). A custom role named like a built-in
  or an older custom role gets " (custom)" appended, or its ID if that is taken too. Update anything
  that refers to such a role by name.
- **Adapter credentials** (000021) are sealed with the key the local secrets adapter already uses for
  app secrets, `/var/lib/pando/secrets.key` unless configured otherwise. Nothing new to configure;
  that key now protects adapter credentials as well, so keep it backed up.
- **Every deploy is now scanned.** The Trivy scanner is added to existing installations as well as new
  ones. The first scan pulls its pinned image and downloads a vulnerability database of about 50 MB
  into the `pando-trivy-cache` volume. The threshold starts at 0, so nothing is refused or stopped
  until an administrator sets one.
- **New app networks use `10.213.0.0/16`.** If that range is in use on your network, set the Docker
  runtime's `network_pool` to another range, or to `off` for Docker's own pool. Existing networks are
  unchanged.
- **Existing app containers keep Docker's `unless-stopped` restart policy** until they are next
  recreated, for example by a deploy. New containers have none.
- **Multi-service apps deployed before this release** carry no per-part image record (000019). The
  reconciler reports a missing part of such an app rather than recreating it from the wrong image.
  Deploy each one again. An app accepted without a primary workload is fixed by accepting its
  configuration again.
- **The shipped `docker-compose.yml` passes named variables only.** To set `PANDO_APPS_*` or
  `PANDO_POLICY_*`, take the new file or add them to the `pando` service's `environment`.
- **Download verification** uses `checksums.txt.sigstore.json`; the `.sig` and `.pem` files are no
  longer published. Package file names change as listed above.
- First-run setup changes only affect new installations. An existing installation keeps its accounts.

Detection does not re-run on its own (R-022), so an app pinned before this release keeps the spec it
was pinned with. To pick up the new reading for an existing app, re-run detection from its page and
review the proposal as usual.

## [0.1.1] - 2026-09-14

### Security

- Base images, GitHub Actions and the two scanners CI installs are pinned by digest or exact version
  rather than by a mutable tag.
- `containerd/v2` to 2.3.5 (GHSA-7jxh-36q5-gcqv) and `moby/go-archive` to 0.3.0 (GO-2026-6253, a
  crafted tar writing outside the extraction directory).
- The console's `vite` to 8.3.0, with `@vitejs/plugin-react` 6.1.1 alongside it, clearing six
  high-severity dev-server advisories.
- A malformed stored credential hash no longer crashes the sign-in path or verifies against an
  arbitrary password. `argon2.IDKey` panics rather than returning an error on a zero time cost or
  zero parallelism, and an empty key field compared equal to an empty candidate, so a corrupted or
  hand-edited row could take the process down or accept anything. Both are now rejected during
  decoding. Found by fuzzing; covered by `TestR042_AMalformedStoredHashDeniesRatherThanPanics`.
- Session cookies are marked `Secure` behind a TLS-terminating reverse proxy. Pando sees plain HTTP
  in that topology, so it could not tell an encrypted browser connection from an unencrypted one and
  sent the cookie without the attribute; one plaintext request to the hostname put a live session on
  the wire. Set `PANDO_SERVER_EXTERNAL_URL` to the address browsers use. **Upgrade note:** an
  installation behind a proxy should set it — unset keeps the previous behavior, which is correct
  only when Pando serves TLS itself or runs on localhost. (O-19)
- The API server sets `ReadHeaderTimeout` and `IdleTimeout`. Without them a client dribbling header
  bytes held a connection open indefinitely. The proxy's per-app listeners already did this; the API
  server was the one that did not. Found by `gosec`.

### Added

- Security policy, coordinated disclosure process and documented security model
  ([`SECURITY.md`](SECURITY.md)).
- Checksums signed with cosign on every release, and the verification procedure that goes with them
  ([`docs/releasing.md`](docs/releasing.md#verifying-a-download)).
- CodeQL, `gosec`, `govulncheck`, `gitleaks` and OpenSSF Scorecard in continuous integration.
- Fuzz targets over the parsers that see untrusted input, run in continuous integration.
- Issue and pull request templates, a code of conduct, Dependabot, and a reference index of the
  external interfaces ([`docs/reference.md`](docs/reference.md)).

### Fixed

- The Docker image ships with the console in it. `docker compose up -d` built an image whose binary
  had no UI embedded, so it served the API and returned 404 for every console route.


## [0.1.0] - 2026-09-14

The first release. Its notes were generated from the commit log, which is what this file now exists
to replace; see the release page for the artifact list.

[Unreleased]: https://github.com/trypando/pando/compare/v0.2.0...main
[0.2.0]: https://github.com/trypando/pando/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/trypando/pando/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/trypando/pando/releases/tag/v0.1.0
