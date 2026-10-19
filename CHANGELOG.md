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

### Security

No new advisories.

### Added

- **Pando creates the GitHub App itself** (R-091). A GitHub source connection signs in with a GitHub
  App by default, marked recommended. Saving one opens GitHub with the App filled in — read access
  to repository contents and nothing more — under your account or the connection's organization;
  choose **Create GitHub App**, pick the repositories, choose **Install**, and the connection is
  ready. No App ID, private key or installation ID is copied by hand. Each installation of Pando
  gets its own App. `pando source authorize` and the MCP tool hand you a console link to finish it
  in the browser, where GitHub requires it.
- The App Pando creates carries a **push webhook** when Pando's address is public (R-142), so a push
  starts an automatic deploy at once rather than at the next poll; **Deploy on push** turns it off.
  On a private address the App has no webhook, which GitHub would refuse, and auto-deploy polls.

### Changed

- The GitHub App fields (App ID, installation ID, private key) are no longer in the form. A
  connection set up with them before keeps working.

## [0.10.0] - 2026-10-19

### Security

No new advisories.

### Added

- **Guided setup for adapters and sign-in providers** (#268, R-261). Adding one now walks through
  it a step at a time: choose the kind, then the provider when there are several (an audit sink's
  SIEM, an identity provider such as Okta or Microsoft Entra ID), then that provider's own steps,
  each saying what to do in its console with a link to the page, then a name and a review. A new
  identity provider's redirect URI, ACS URL and entity ID are shown on the step that registers
  them, before it is saved. **Show all settings** keeps the full form. `pando adapter add` walks
  the same steps at a terminal for whatever `--set` leaves out, and takes `--preset`.
- Saving an adapter can return **warnings** about something that saved but may not work as wanted;
  the console shows them and `pando adapter add` prints them. Cloudflare Tunnel warns of a console
  hostname more than one level below its zone, which its included certificate does not cover.
- The Anthropic adapter takes an optional workspace ID (#275).
- The HTTPS audit sink can send Datadog's log format, and caps each batch by size (#275).
- Trivy takes mirrors for its vulnerability database, Java database and checks bundle (#275).
- Image registry health notes, such as a missing delete permission, show on the adapters screen and in
  plans (#275). GHCR images are deleted through GitHub's Packages API.

### Changed

- Cloudflare Tunnel's settings are a **primary zone** and **secondary zones**, typed into one field,
  and a new adapter's console hostname starts at `pando.<primary zone>`.
- Saving an adapter now refuses settings that could never start, with a message saying what to change
  (#275). SMTP refuses a username without a password, a username with no encryption to a remote host,
  and a port that doesn't match the security setting; Teams refuses a retired Office 365 webhook.
- The bundled Traefik is v3.7.15, and the Route 53 DNS challenge requires `AWS_REGION` (#275).
- BuildKit is no longer offered under **Add adapter**: Pando sets it up itself (#275).
- The Docker services adapter takes one image setting per service type (#275). An existing `images`
  map is carried over by migration 000076.
- Okta SAML sign-in identifies people by `okta_user_id` and no longer trusts the email Okta sends
  (#275).

### Fixed

- Browser sign-in to a Gitea source asked for too little scope to list repositories; it now asks
  for `read:user` as well (#268).
- Several adapters' setup help, stated defaults and error remedies sent people to pages or values
  that had changed, among them Cloudflare's account ID, the GitHub App private key, the Incus server
  certificate and BuildKit's default address (#268).
- An Authentik SAML provider read the person's full name as their username (#268).
- A registry cleanup that fails no longer blocks app teardown (#275). Once 20 were stuck, every later
  teardown waited behind them.
- Entra sign-in syncs the groups of a person in more than 200 of them instead of none (#275).
- An Authentik provider with no signing key is refused with a remedy instead of failing every sign-in
  (#275).
- ECR dual-stack registry addresses are accepted (#275).
- The built-in routing adapter no longer shows a base URL setting that did nothing (#275).
- Local backups warn at startup when the backup directory isn't on a mounted volume, so backups
  don't vanish with the container (#275).

### Upgrade notes

- A Traefik adapter using Amazon Route 53 for DNS challenges needs `AWS_REGION` beside its access
  key. Without it, certificate requests were already failing, and the adapter can no longer be saved.
  Add `AWS_REGION=us-east-1` for a standard AWS account.
- Migration 000076 moves the Docker services adapter's `images` map to one setting per service type.
  It runs at startup and needs nothing from you.

## [0.9.0] - 2026-10-18

### Security

No new advisories.

- The console's test dependency `undici` is at 8.12.2 (#260), clearing eight CVEs reported against
  8.11.2 from dependency scans: CVE-2026-87018, CVE-2026-87029, CVE-2026-109383, CVE-2026-109829,
  CVE-2026-109841, CVE-2026-109869, CVE-2026-109924 and CVE-2026-109932. It reaches the console
  only through its test DOM, so nothing that ships changed.

### Added

- **Two-factor authentication for local accounts** (#264, R-405). A local account can add an
  authenticator app, set up with a QR code, and sign-in then asks for its 6-digit code after the
  password. Setting it up gives ten single-use recovery codes. Pando offers it once at a person's
  first sign-in, and it is set up, replaced or turned off from **Your settings** or with
  `pando two-factor`. Host policy `require_local_two_factor` requires it on every local account. An
  administrator resets someone's from their account page or with `pando user reset-two-factor`, and
  `pando admin reset-two-factor` does it from the host. Accounts from an identity provider are
  unchanged. Suggested by [@realPhrojoe](https://github.com/realPhrojoe) in
  [discussion #263](https://github.com/trypando/pando/discussions/263).
- **Who can open each app, and who owns it, on the apps list** (#265). The admin console's apps
  list has an **Access** column — Private, Private – shared, Public, or Public with passcode — with
  a filter, so the apps open to everyone are one click away; hovering a label says what it means. It
  also has an **Owner** column, and each app's address under its name, opening the app in a new
  tab. `GET /apps` returns `access` and `owner_name` for each app, and `pando app list` and the
  `pando_list_apps` MCP tool show them. Suggested by [@realPhrojoe](https://github.com/realPhrojoe)
  in [discussion #262](https://github.com/trypando/pando/discussions/262).

## [0.8.0] - 2026-10-16

This release fixes five vulnerabilities found in a security assessment of Pando. All affect 0.7.0
and earlier. Upgrade now; see **Upgrade notes** for what changes on the first start.

### Security

- **[GHSA-w85m-2rf9-m3vj](https://github.com/trypando/pando/security/advisories/GHSA-w85m-2rf9-m3vj)**
  (critical, CVSS 9.0; 0.7.0 and earlier). Under subdomain routing, an app's own hostname answered
  Pando's `/api/v1`, so an app's script could mint an API token carrying the permissions of whoever
  was signed in to it, an administrator included. Every path on an app's hostname except `/.pando`
  now goes to the app (#253). After upgrading, revoke any API token you did not create.
- **[GHSA-w854-ppwh-rhh5](https://github.com/trypando/pando/security/advisories/GHSA-w854-ppwh-rhh5)**
  (high, CVSS 8.7; 0.7.0 and earlier). An app could be given the console's hostname and receive its
  traffic. Pando's own hostnames are now refused as app addresses (#255, R-165). After upgrading,
  check that no app's address is the console's.
- **[GHSA-2w23-jjrw-wjhx](https://github.com/trypando/pando/security/advisories/GHSA-2w23-jjrw-wjhx)**
  (high, CVSS 7.7; 0.7.0 and earlier). A repository address could be a `file://` URL or local path,
  or reach the cloud metadata service, Pando's own port or its database. Only `https`, `http` and
  `ssh` are accepted now, and every connection a fetch makes is checked where it lands, redirects and
  SSH included. Repositories on private networks are still allowed (#255, R-092).
- **[GHSA-2v7f-8w8v-24ww](https://github.com/trypando/pando/security/advisories/GHSA-2v7f-8w8v-24ww)**
  (high, CVSS 7.2; 0.7.0 and earlier). A custom role holding one installation permission, such as
  `install.users.manage`, `install.adapters.manage` or `install.backup.manage`, could become
  Administrator. Granting or changing a role, changing another account, configuring an identity
  provider and restoring the installation now need every installation permission involved (#257,
  R-080a). Installations that grant only the built-in Administrator role were not affected.
- **[GHSA-f425-x6jh-6cfm](https://github.com/trypando/pando/security/advisories/GHSA-f425-x6jh-6cfm)**
  (medium, CVSS 4.6; 0.7.0 and earlier). Another site could sign a visitor in to Pando as an account
  it controlled, and an app could plant or set Pando's session cookie. On HTTPS the session cookie is
  now `__Host-pando_session`; sign-in refuses cross-site form posts; Pando's pages send a Content
  Security Policy, `X-Frame-Options: DENY` and, on HTTPS, HSTS; an email address an account's holder
  set for themselves is never linked to a provider; and wrong app passcodes are also limited per app
  (#256).
- App containers are held to a process limit (`pids_limit`, 1024 by default), run with
  `no-new-privileges`, and keep only Podman's default capabilities, without `NET_RAW`, `MKNOD` and
  `AUDIT_WRITE`. Where the storage driver can size a container's own layer, it is capped at the app's
  disk limit (#258).

### Added

- **Change an app's address from the CLI and MCP** (#251, R-261). `pando app address <app>` shows
  the address and, with `--adapter`, `--mode`, `--hostname`, `--domain` or `--path`, changes it.
  MCP gains `pando_get_address` and `pando_set_address`.
- **The Cloudflare adapter serves several zones** (#251). `other_zones` lists zones beside `zone`,
  and an address can take a `domain` to be named in any of them.

### Changed

- **An app on its own hostname gets every path** (#253), including `/api/v1`, `/healthz` and
  `/.well-known`, which Pando used to answer there.
- **A repository address may not carry a credential** (#255, R-190). Use a source connection.
- **An account that holds an installation role is not linked to a provider by email** (#257,
  R-045). The refusal names the provider ID an administrator needs to link it explicitly.
- **Roles show a count of their permissions** (#252). Pressing it lists each one with what it
  allows.

### Upgrade notes

- **On HTTPS installs everyone signs in again once.** The session cookie is renamed to
  `__Host-pando_session`, and the old one is no longer read.
- **Every app restarts once.** Each app's containers are recreated on the first reconcile to take
  the process limit and reduced capabilities.
- **An app whose repository address is a local path, a `file://` or `git://` URL, or carries a
  credential stops fetching.** Change its source to an `https` or `ssh` address, with a source
  connection for a private repository.
- People who hold some installation permissions but not all can no longer change roles, accounts or
  providers beyond what they hold. Give Administrator to anyone who needs to.

## [0.7.0] - 2026-10-15

### Security

No new advisories.

- `github.com/docker/distribution` is excluded from the module graph (#244), clearing
  CVE-2026-85747 (GHSA-685w-q87j-wqw3) from dependency scans. Pando's own binary never compiled
  it: it was required only by a development tool's dependencies.

### Added

- **Report a bug or suggest an idea from the console** (#249). A bug button beside the
  notifications inbox, in the launcher and the admin console, opens GitHub's bug form with the
  running version filled in, or starts a discussion in the Ideas category. Both open in a new tab
  under your own GitHub account; Pando sends nothing itself.
- **A failed app's banner links to its logs** (#247). "Pando has stopped trying to start this app"
  now carries an **Open logs** button, for anyone who holds `app.logs.read`.

### Fixed

- **Hourly backups failed for every app with a database volume** (#243, R-212). The backup helper
  could not read a directory owned by another user with mode 0700, which is what Postgres requires
  for its data directory, so the backup failed with "the helper exited with status 1". It can now
  read such volumes, still mounted read-only, and a restore puts back the owners and modes the
  backup recorded, so a restored database starts. A failed helper's error now quotes its last line
  of output.
- **Read notifications stayed in the inbox** (#245, R-377). The inbox now opens on **Unread**, and a
  notification leaves it once opened or after **Mark all read**; read ones are under **All**.
  Opening a notification that links into the console no longer comes back unread.
- The three-dot menu on a launcher tile ran off the left edge of a narrow window and cut off its
  items. It now stays inside the window (#246).

## [0.6.1] - 2026-10-14

### Security

No new advisories.

### Changed

- Console copy is shorter throughout (#240). Section descriptions keep one sentence, column
  headers use one term each (**Source**, **Scope**, **Destination**, **Type**, **Members**), and
  settings read as noun phrases, such as **Allow CPU oversubscription** and **Stop idle apps after
  (days)**. Requirement IDs no longer appear in the Policy screen's text.

### Fixed

- **The notifications inbox opened behind the app tiles** (#239, R-377). It now opens above
  everything on the page, from the launcher and from the admin console.
- The Adapters screen's button read "Add a image_registry adapter"; it now reads "Add an image
  registry adapter" (#240).
- **An in-place upgrade blocked the next one** (#241, R-355). The upgrade recreated Pando's container from
  the image digest it had verified, and the next upgrade read that digest as the deployment's own pin
  and refused: "Pando's image is pinned to a digest". The recreated container now keeps the image
  reference the deployment named, and the next upgrade reads that.

### Upgrade notes

- If Pando was upgraded in place before this release, its container is still pinned to a digest, and
  the version running it cannot upgrade itself. Recreate the container once from your deployment
  (with Compose, `docker compose up -d --force-recreate pando`), then upgrade from the console.

## [0.6.0] - 2026-10-14

### Security

No new advisories.

- `npm ci` runs with `--ignore-scripts` in the image build and CI, so a compromised console
  dependency cannot run code at install time (#235).
- The Kubernetes manifests' `pando-edge-traefik` Role names the ten `traefik.io` kinds Traefik
  watches instead of `"*"` (#235).

### Added

- **Check for updates now.** Pando checks for a newer release at startup and every six hours; the
  console's Updates screen now has a **Check for updates** button beside when it last checked, and
  `pando updates --check`, MCP `pando_check_updates` and `POST /api/v1/updates/check` do the same.
  A second check within a minute is answered from the first.
- **Accounts show installation roles held through groups** (#230, R-078). An account that held a
  role only through a group, such as one synced from Okta, showed **None**. The account list and
  page now show every role, with the group it comes through. `GET /users`, `GET /users/{id}` and
  `GET /groups/{id}/members` carry `install_roles`, each with `via` naming the account itself or the
  group. `pando user list`, `pando user get`, MCP `pando_list_users` and `pando_get_user` are new.

### Changed

- **Making an adapter the default hands the default over** (#231). Saving an adapter as the default
  when its category already has one used to fail with a 500; it now replaces the old default in one
  step. The console asks first, naming both adapters. `POST /adapters` returns `replaced_default`
  and `handover`, and the audit event records `replaced_default`.
- **`pando adapter add` no longer makes the adapter the default unless asked** (#231). `--default`
  used to default to `true`. Without it, an existing adapter keeps its default status, and a new one
  becomes the default only when its category has none.
- **The plan lists the services Pando creates under Services** (#228, R-131), ahead of Variables,
  with the variables the app reads each one from and the choice between Pando creating it and using
  your own. A variable a created service fills is no longer marked **Required**.
- Comboboxes announce the active option to screen readers, and tooltips open on keyboard focus
  (#236).

### Fixed

- **A deploy that became healthy late stayed "Deployed, not healthy"** (#234, R-221). The app was
  marked running, but its deploy kept `degraded`. The current deploy is now promoted to `running`
  when the app comes up.

### Upgrade notes

- Scripts that run `pando adapter add` and relied on it making the adapter the default must now pass
  `--default`.

## [0.5.0] - 2026-10-13

Detection now builds the app and scores the image that will run, and the categories of deploy
failure the deploy benchmark (#90) found are fixed. Apps can run in their own Incus virtual machine.
Provisioned Postgres now serves TLS, and its containers are recreated once when you upgrade; see
**Upgrade notes**.

### Security

No new advisories.

- **Static sites and compiled programs are built on maintained base images** (#199, #219). Every
  static site was served from `nginx:1.27-alpine`, a line nginx no longer rebuilds, on Alpine 3.21.
  It is now `nginx:1.30-alpine` on Alpine 3.24, and Go programs and committed binaries run on
  `debian:trixie-slim` rather than `debian:bookworm-slim`, which is past regular security support.
  A site built from gabrielecirulli/2048 went from 2 critical and 41 high findings to none critical
  and 3 high. Both bases are pinned by digest, so base image fixes reach apps through a Pando release.

### Added

- **The `incus` runtime runs each app in its own Incus virtual machine** (#201, R-115), so no two
  apps share a kernel. Every workload of the app and every service provisioned for it runs inside that
  VM; a network ACL admits only Pando's addresses, on the forwarding agent's port. Volumes are Incus
  custom volumes and outlive the VM (R-204). Built images reach the VMs through the install's image
  registry. Setup is in [`docs/reference.md`](docs/reference.md#running-apps-on-incus).
- **The CLI can finish detection** (#218, R-261). `pando app detection answer`, `revise`, `diff`,
  `rerun` and `accept` do what the console and MCP do, and each open question is printed with the
  `answer` command for it. MCP gains `pando_revise_detection` and `pando_get_detection_diff`.
- **MongoDB and Memcached can be provisioned** (#224), as Postgres and Redis already could, and a
  variable can take one part of a provisioned connection string, such as its host or password.
- **Required variables reach the plan** (#222, #224, R-132). Detection finds them from framework
  conventions (`SECRET_KEY_BASE`, Laravel's `APP_KEY`, Symfony's `APP_SECRET`, Strapi's keys),
  environment schemas (AdonisJS, envalid, t3-env and zod), compose `${VAR}` with no default, Python's
  module-level `os.environ["X"]`, and what the trial's log says is missing. Each blocks the deploy
  until it is filled. A secret slot has a format — `random`, `random_long`, `hex_64`, `django_key` or
  `laravel_key` — that the console's **Generate** produces, and the API refuses a value that does not
  fit it.
- **Build plans for more languages and frameworks** (#222, #224): R (plumber and Shiny), Clojure
  (lein and `deps.edn`), sbt and Play, Crystal, Haskell Stack, Perl, Meteor, Hugo, Zola and MkDocs,
  `setup.py`-only Python and PHP without Composer. WAR files deploy on Tomcat 11, or Tomcat 9 for
  `javax`. Quarkus, Helidon, Ktor, Gradle `application` builds, SvelteKit on adapter-node and Nuxt SSR
  start as servers. A start command is read from `railway.json`, `railway.toml`, `render.yaml` and
  `deno.json` tasks (R-094).

### Changed

- **A repository's compose file outranks its published image** (#222, R-094). A compose service
  whose `image:` names the project's published image runs that image instead of building it. Compose
  files are interpolated in full (`${VAR:-default}`, `${VAR:?}`, the committed `.env` and
  `env_file`), a build several services share runs once, services at `deploy.replicas: 0` are left
  out, host data directories become managed volumes, and a provisioned service answers to the compose
  service name it replaced.
- **The web port is chosen by what answers HTTP** (#222, R-097). The trial probes every port the app
  opens, then falls back to `EXPOSE` and then to the log. A tie is asked as `primary_port` (R-102).
  An app that answers no HTTP request on any port is refused, as are images that expose only database
  or broker ports, programs that exit without listening, libraries, Electron apps and phone-only Expo
  apps. An app that listens only on 127.0.0.1 is retried with `HOST=0.0.0.0`, then asked how to start.
- **When Railpack fails, Pando changes the plan and builds again** (#222, #224), up to three times,
  for causes it can name: a Rust or Python version, a missing PHP extension, a C extension, a
  Bundler binstub. What it changed is recorded in the spec (R-020), so the deploy builds the same plan.
- **Provisioned Postgres serves TLS** (#224), with a certificate derived from the service's password,
  so it is stable across reconciles. The URL carries no `sslmode`, and its scheme is `postgresql://`,
  or `postgres://` when the app's own configuration spells it that way.
- **Git submodules are not fetched** (#224). A build that reads an empty submodule fails, naming it.
- **Detection may take 40 minutes** (#216), up from 10: the time it needs to read the repository plus
  the 30 minutes a deploy's build gets, now that detection builds the app.
- **A deploy waits 3 minutes for an app to start** (#222), up from 2. With no health check, a deploy
  is done once the primary port accepts a connection (R-221).
- Node builds use the `-slim` images rather than Alpine, and pnpm is pinned from `packageManager` or
  the lockfile (#222). Dockerfiles Pando writes no longer read the repository's `.dockerignore`.
- Docker Hub images are pulled through the Docker daemon's registry mirrors when it has any (#224).
- The **Add app** dialog puts the source first and says less (#203).

- **Detection builds the app, so the plan shows its real security score** (R-311). For an app Pando
  builds, detection now builds the image the app would deploy, the way a deploy builds it, and scans
  that image with the source; the plan's score covers both, a finding both report counted once
  (gabrielecirulli/2048: 61 / 100, where the plan used to say "Scored once the image is built"). The
  first deploy runs that image and keeps that score rather than building again, unless something it
  is built from changed after detection. The build runs beside the source scan, and the image scan
  beside the trial run, which now starts the built image and watches it for its port (R-097). A
  build that fails stops detection and shows the build log (R-107). A Dockerfile that needs a build
  argument nobody has given waits for the answer, then builds; accepting waits for that. While
  running, `GET /apps/{id}/detection` reports a `building` stage and `running`, every step under way.
  A compose app's services build at once, at most `max_concurrent_deploys` at a time.

### Fixed

- **The first deploy of an app never scanned the image it built** (R-311). Detection scans the
  source before anything is built, and the first deploy reused that scan, so an app's score left
  out its image's operating system packages: a static site served from `nginx:1.27-alpine` showed
  100 / 100 while its image had 2 critical and 41 high findings. A deploy now reuses a scan only when
  that scan read the same source and the same image, named by its digest; otherwise it scans both.
  The deploy log says "(source and image unchanged)" when it reuses one, and the app's security
  section says what a scan read: the image it runs and its source, the image only, or the source only.
- **A repository's published image was never scanned** (R-311). When Pando ran a project's own
  published image instead of building it, only the repository's files were scanned:
  `bastienwirtz/homer` scored 80 while `ghcr.io/bastienwirtz/homer:latest` has 6 high findings. The
  deploy now pins the tag to the digest it names, scans that image with the repository, and runs that
  digest, so the image scanned is the image that runs. An image app's scan now pulls with the app's
  registry credential.
- **Choosing a username another account has returned a 500** (#217). It now answers 409
  `STATE_USERNAME_TAKEN`, naming the username, including one held by a deleted account, which cannot
  be reused.
- **A detection that finished at its deadline lost its result** (#216) with "Could not record the
  detection result." The result is now recorded after the deadline.
- **Accepting a failed detection pinned an empty spec** (#222). It now returns the build's error.
- An image detection built and that has since been removed is built again rather than failing the
  deploy (#222).
- **The plan's Accept buttons sometimes never appeared** (#202) when the security scan arrived while
  the console showed "Plan ready". A reload was the only way out.
- The **In use** panel reads usage again when a deploy finishes (#203), and the onboarding page's
  terrain keeps its shape when detection finishes (#208).
- On rootless Docker, a BuildKit that restarts in a loop because its user has too few subordinate
  IDs is now named as the cause, with the `usermod --add-subuids` fix (#216).

### Upgrade notes

- **Provisioned Postgres containers are recreated once** when the new version reconciles them, to
  serve TLS. Data is on the service's volume and is kept; the app restarts with its new URL.
- **Rootless Docker installs:** if BuildKit restarts in a loop logging `newuidmap: write to uid_map
  failed`, give Pando's user more subordinate IDs as [`docs/rootless.md`](docs/rootless.md) now says.
- An app built on the old static or program base image keeps it until its next deploy.
- `RuntimeAdapter` gained `HasImage` (#222). A runtime adapter built outside this repository must
  implement it.
- An app whose current score came from a scan of its source alone keeps that score, now labeled
  "of the source only, not the image it runs", until its next deploy scans the image as well.

## [0.4.0] - 2026-10-11

### Security

- **An open websocket outlived a revoked session or a suspended account** (#72). A long-lived
  connection through the proxy is re-authorized every two minutes, but the check reused who the
  person was when the connection opened, so signing them out everywhere or suspending them (R-048,
  R-049) left it open. It now authenticates the connection's session or token again each time, and
  closes it with a policy-violation close frame when that no longer works.

- **BuildKit no longer runs with seccomp unconfined** (#130). Pando starts BuildKit itself, as
  `pando-buildkit` on the Docker it runs apps on (R-111), under a seccomp profile compiled into Pando:
  Docker's default plus only the calls rootless BuildKit needs, creating namespaces, mounting inside
  them, and each build step's keyring and hostname. Builds can no longer load BPF programs, read
  performance counters, load kernel modules, set the clock or reboot. AppArmor stays unconfined.
  BuildKit is v0.33.1 (from v0.17.2), pinned by digest, and Postgres is pinned by digest too, which
  Dependabot now keeps current. The compose file no longer has a `buildkit` service;
  `PANDO_BUILDKIT_ADDRESS` still points Pando at a BuildKit you run instead.

- **Setting up a new installation needs the setup token from Pando's log** (#130). Before, whoever
  reached the console first became the administrator. Pando now prints a one-time `setup_token` on
  the startup warning that the installation is not set up yet, and the setup form asks for it.
  `pando admin setup-token`, run where Pando runs, makes a new one if the log line is gone.
  `PANDO_ADMIN_PASSWORD` installs are unaffected.

- **The bundled Postgres no longer defaults to the password `pando`** (#130, R-401). A one-shot
  `secrets` service generates one on the first start and keeps it in the new `pando-secrets` volume;
  Postgres and Pando read it from there as a file, through `POSTGRES_PASSWORD_FILE` and the new
  `PANDO_DATABASE_PASSWORD_FILE`. `POSTGRES_PASSWORD` set before the first start is used instead.

- **On Linux, run Pando on rootless Docker** (#130, R-402). Pando holds the Docker socket, so on
  rootful Docker a compromise of Pando is root on the host. [`docs/rootless.md`](docs/rootless.md)
  sets up rootless Docker for it, and the compose file mounts whichever socket
  `PANDO_DOCKER_SOCKET` names. Installation, Capacity shows whether the runtime is rootless.
- **Pando refuses to deploy where Docker would drop an app's CPU or memory limits** (#130, R-240).
  Docker accepts limits its host cannot apply and starts the container without them, which rootless
  Docker does unless the cpu cgroup controller is delegated to its user. The refusal says which
  limit and how to fix it. Before, the app ran with the whole host.

- **An app is held to its disk limit** (#130, R-403). Every app has one (10 GiB unless set), and
  until now it was only counted against the host's disk. Pando now measures each running app's
  containers and volumes every 10 minutes, tells its owner at 90% and when it goes over, and stops
  it if the next reading is still over. Its data is kept; starting it clears the stop. The console
  shows "Stopped over disk limit", and a visitor is told why.

### Added

- The audit log can be streamed to a SIEM and exported (#129, design 12). `GET /audit/stream`,
  `pando audit tail --follow` and the `pando_read_audit_stream` MCP tool read every event in commit
  order from a cursor, and resume from it after a restart, an upgrade or with several replicas without
  missing one. **Audit sinks** are a new adapter category, `audit_sink`: `syslog` (RFC 5424 over TLS)
  and `https`, with presets for Splunk HEC, Datadog, Elastic, Sumo Logic and Azure Monitor (Microsoft
  Sentinel), sending Pando's native JSON or OCSF 1.3.0 (`docs/audit-formats.md`). Each sink's
  backlog, last error and any gap are on the Audit log screen, `GET /audit/sinks` and
  `pando audit sinks`; one that keeps failing for a day is turned off and its administrators are told.
  A month is not archived while an enabled sink has not been sent it. `GET /audit/export` and
  `pando audit export` download any range of the live log. Sending the log off the installation needs
  the new `install.audit.export` permission, which the Administrator role holds.
- Every audit event now records its outcome (success, denied or failed), the client's address and
  user agent, and the acting person's name and email. Behind a proxy, list the proxy in
  `PANDO_SERVER_TRUSTED_PROXIES` for the client's own address to be recorded rather than the proxy's.
- More is audited: the end of a terminal session (`app.exec.end`), every deploy's outcome
  (`deploy.finish`), what a rollback rolled back from, which fields a policy or spec change touched,
  opening an app's logs (`app.logs.read`), and reading the audit log itself (`audit.read`,
  `audit.export`, `audit.archive.download`).
- An install image registry (#72). `PANDO_REGISTRY_URL` and its credential name a registry that
  builds are pushed to, by digest, for a runtime that pulls rather than imports; runtimes and the
  scanner pull with the registry's credential, and a deleted app's images are deleted from it.
  `docker-compose.registry.yml` adds a registry to the Compose topology. A single-host install needs
  none and keeps importing builds directly, unless `PANDO_REGISTRY_ALWAYS` is set. See
  `docs/reference.md` for the settings. The registry is an adapter category of its own
  (`image_registry`, kinds `oci` and `ecr`, #153), configured like any adapter from the console, the
  API (`POST /adapters`), `pando adapter add` and MCP, and used from the moment it is saved; the
  password is stored encrypted and never shown. `PANDO_REGISTRY_*` declares one, read-only while it is
  set.
- A DR bundle now includes uploaded source (#72). The registry's images are not in it, so an uploaded
  app's archive is what it is rebuilt from after a restore.
- `disable_anonymous_denial_audit` host policy setting (#72), also settable as
  `PANDO_POLICY_DISABLE_ANONYMOUS_DENIAL_AUDIT`. Off by default: a visitor who is not signed in being
  refused a private app is still written to the audit log as `authz.denied`. Turned on, those rows
  stop; refusals of signed-in people and tokens are always recorded.
- Event subscriptions (#50). Subscribe to what happens in Pando — deploys started, succeeded and failed,
  app state and health, security scores, shares and grants, backups, sign-ins, policy, adapter health,
  upgrades — on one app or the whole installation, from the console's new **Events** screen,
  `pando subscriptions`, `/api/v1/subscriptions` or MCP. Each is sent to a webhook, as JSON signed with a
  per-subscription key (`Pando-Signature`), or through a notification adapter. Deliveries are retried for
  about a day, every attempt is recorded and can be sent again, and an endpoint that fails everything for
  a day is turned off and its owner told. The catalog is [`docs/events.md`](docs/events.md). New
  permission `install.events.manage`, held by Administrator, for install-wide subscriptions; anyone who
  can see an app can subscribe to it. Webhooks cannot reach private, loopback or link-local addresses
  unless host policy's new `allow_private_webhooks` is on.
- Notification adapters for email (SMTP, which covers SendGrid, Mailgun and Amazon SES), Slack,
  Microsoft Teams, Discord and ntfy. Email reaches people at the address on their account; the others
  post to a channel and are sent to only by subscriptions.
- Notification preferences, under **Settings**: which of Pando's own notifications reach you, on the
  console and by email. "An app was shared with you" is off by default; the launcher tile is still the
  notification.
- A notifications inbox: a bell beside Settings with the number unread, `pando notifications list` and
  `GET /api/v1/me/notifications`. Console notifications were recorded before and shown nowhere.
- Each app has an **Events** tab: its recent events, and its subscriptions. Also `pando events --app` and
  `GET /api/v1/apps/{id}/events`.
- A webhook can send the request its receiver expects: method, content type, headers of its own (stored
  encrypted, for an API key the receiver needs) and a body template. Deliveries, notifications and the
  inbox link to the app when `external_url` is set.
- A failed deploy tells the app's owner and whoever started it; a failed backup tells the owner and
  whoever manages backups. An account token can own a subscription, within its own grants.

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
- **An OpenAPI 3.1 document for the API**, `docs/openapi.json` (R-261). Every endpoint with its
  parameters, request body, responses and the error envelope with every error code, and which
  credential each takes. It is generated from the code that serves the API by `make reference`, and
  the build fails when it is out of date.

### Changed

- **Apps with no deployment instructions are planned by Railpack instead of nixpacks** (#172).
  nixpacks is in maintenance mode, and Railpack is its maintainers' replacement. The plan Pando
  shows and stores for such an app is now Railpack's JSON build plan (each step's commands, the
  images it runs on, the start command) rather than a Dockerfile, and it is still editable before
  it runs. It is built through Railpack's frontend image inside Pando's own rootless BuildKit, with
  no privileged container and no runtime socket. Railpack's images are pinned by digest. Railpack
  defaults Node to the current LTS and Python to 3.13, reads `.node-version`, `.ruby-version` and the
  Gemfile's `ruby` line itself, starts a Mix project from its release, and builds C sources with
  the compiler in its builder image. Laravel, Jekyll, static sites, Node servers, Go, .NET and JVM
  apps are still planned by Pando's own Dockerfiles, as before. Planning now looks language versions
  up from the package indexes, so an installation that cannot reach the internet cannot plan these
  apps; building already needed it. The image is about 110 MB larger, for the mise binary Railpack
  resolves versions with, and now carries `git` and `bash`, which mise needs to list Python
  releases. nixpacks is no longer in the image.

- API tokens are stored as HMAC-SHA-256 under a key rather than argon2id (#93). A token is 256 random
  bits Pando made, so the slow hash cost 64 MiB and tens of milliseconds on every request a token made
  for no benefit. The key is a new file, `/var/lib/pando/token.key` (`PANDO_SERVER_TOKEN_KEY_PATH`),
  created on first start and kept out of the database so that a database dump alone cannot be used to
  check a guessed token. Every replica must read the same file, and a DR bundle includes it. Existing
  tokens keep working and are rewritten on their next use; passwords are still argon2id. A token's
  last-used time is now recorded to the minute rather than written on every request.
- The proxy reuses its connections to an app instead of opening one per request, finds the app a
  request is for with one indexed lookup instead of reading every app's spec, and the app screen's
  permission check reads grants and policy once instead of once per permission (#72).
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

- **A build refused by the security scan or the port check could still run** (#72, R-146). Builds were
  loaded as `pando/<app>:latest` and deployments recorded that tag, so a refused build moved it, and a
  container the reconciler recreated afterwards ran the refused image. Each build is now tagged with
  its deployment and recorded by its image ID. Older tags of an app's builds are removed past the
  newest eleven.
- The identity assertion and `X-Pando-Email` header for somebody signed in with a session carried no
  email or name (R-054). They now do.
- **An app that failed did not tell its owner** (#50). The reconciler wrote the notification and had no
  notifier wired to send it. It now goes to the owner on the console, and by email where that is set up.
- **A security-score warning reached only the console** (#50). It now reaches the owner on every channel
  that reaches people, as their notification preferences allow.
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

- **Existing apps keep the nixpacks plan they were deployed with** (#172). A plan is part of the
  spec (R-020), so rebuilding an app replays its stored nixpacks Dockerfile unchanged, and it still
  builds. To move an app to Railpack, re-run its detection (R-022) and accept the new proposal.

- **On Ubuntu 23.10 and later, builds need the host to allow unprivileged user namespaces.** Rootless
  BuildKit cannot start otherwise, and this was as true of the compose file's `buildkit` service as
  it is of Pando's own. As root: `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`, and the
  same line in a file under `/etc/sysctl.d/`. The build service's health check now says so.

- **The compose file's `buildkit` service is gone; Pando starts `pando-buildkit` instead.** After
  `docker compose up -d` with the new file, remove the old one with
  `docker compose up -d --remove-orphans`. The old `buildkit-cache` volume is no longer used and can
  be removed with `docker volume rm` once the new build cache has filled. With
  `docker-compose.registry.yml`, take the new copy of it too: it gives Pando the registry's CA for its
  BuildKit, rather than configuring a `buildkit` service.

- An installation upgraded before anyone set it up prints its setup token on the first start of the
  new version. Scripts that call `POST /api/v1/setup` must now send `setup_token`; take it from
  `pando admin setup-token`, which prints the token alone on stdout.

- **An installation made with the old compose file keeps its database password only if you say what
  it is.** Its Postgres was created with `pando`, or with your `POSTGRES_PASSWORD`, and the new
  compose file would otherwise generate a different one that the database does not accept. Before
  the first `docker compose up -d` with the new file, put that password in a `.env` file beside it
  (`POSTGRES_PASSWORD=pando` if you never set one); the `secrets` service writes it to the
  password file, and later starts read it from there. Back up the `pando-secrets` volume with
  `postgres-data` from then on.

- If Docker on your host cannot apply CPU or memory limits, deploys are now refused until it can;
  the refusal says what to change. To check before upgrading, run `docker info` and look for
  warnings about CPU quotas, memory limits or cgroups. After upgrading, Installation, Capacity shows
  `cpu_limits` and `memory_limits` under Show details.

- Migration 000067 adds columns to `audit_events` without rewriting the rows already there; events
  written before it have no outcome, source or actor name, and the stream reads them first, in the
  order they were written. The audit log grows faster than before, because reading it, opening an
  app's logs and the end of each terminal session are now recorded.

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

[Unreleased]: https://github.com/trypando/pando/compare/v0.10.0...main
[0.10.0]: https://github.com/trypando/pando/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/trypando/pando/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/trypando/pando/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/trypando/pando/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/trypando/pando/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/trypando/pando/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/trypando/pando/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/trypando/pando/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/trypando/pando/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/trypando/pando/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/trypando/pando/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/trypando/pando/releases/tag/v0.1.0
