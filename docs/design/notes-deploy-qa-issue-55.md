# Deploy QA, issue #55: what changed and why

The first deploy QA run (`test/deploy-qa`, `main` @ 53d5939) deployed 169 apps
and 63 passed without the AI adapter. Issue #55 assigned each failure one cause.
This note records the decisions made while fixing them, including every `[P]`
default that was set or overridden, so that the reasons are in the design docs
and not only in code comments.

## Builds

**Static sites.** The generated Dockerfile wrote nginx's config with Go's `%q`
inside `printf '%s'`. The shell expanded `$uri` to nothing and printf wrote the
`\n` escapes literally, so nginx refused the config and every static site exited
on start. The config is now written with a single-quoted printf format
(`printfFormat` in `internal/adapter/builder/buildkit/synthesize.go`), and a test
runs the generated line through a real shell.

**Answered start commands reach the build.** `api.BuildRequest.StartCommand`
carries the primary workload's command to a builder that plans the build itself.
The buildkit builder passes it to nixpacks as `--start-cmd`. Before this, the
answer stopped at the workload and nixpacks failed with "No start command could
be found".

**Node version `[P]`.** nixpacks 1.41 defaults to Node 18, which is end-of-life
and which current Vite, Next.js, Angular and NestJS refuse. Pando passes
`NIXPACKS_NODE_VERSION=22` only when the repository names no version: nixpacks
lets that variable override `engines.node` and `.nvmrc`, so passing it
unconditionally would override the author. A `.node-version` file, which nixpacks
does not read, is passed through.

**Sites that build to static files `[P]`.** Astro, Vite, Angular, Create React
App and Gatsby build a directory of files. Through nixpacks they failed three
ways: "No start command could be found" for a site with nothing to start; Vite 8
builds that could not find Rolldown's native binding under nixpacks' Node (the
lockfile lists it; npm under nixpacks' Node did not install it); and Angular,
whose CLI refuses the Node 22.11 that nixpacks pins. Such a site is now planned
by the builder itself, before nixpacks is asked (`staticsite.go`): build with the
official `node:<major>-alpine` image, then serve the output with the nginx image
and config a committed static site gets. It applies when `package.json` has a
build script and the start script is absent, a framework development server
(`vite`, `astro dev`, `gatsby develop`, `ng serve`), or a static file server
over a directory (`sirv public`, `serve`, `http-server`). The output directory
is each framework's default, Angular's is read from `angular.json`, and a static
server's is the directory it names. The Node major comes from `.nvmrc`,
`.node-version` or a single-major `engines.node`, else 22. The plan is written
into `.nixpacks/Dockerfile`, so it is stored, reviewed and replayed like any
generated plan (R-020).

**JVM projects `[P]`.** nixpacks 1.41 refuses Gradle 9, builds Maven projects
on a JDK of its choosing, and has no plan for Java sources with no build tool.
The builder now plans these itself (`jvm.go`): a Gradle project with a wrapper
builds `bootJar` (else `build`) with its wrapper on `eclipse-temurin:<jdk>-jdk`;
a Maven project runs `package` with `mvnw` or `maven:3-eclipse-temurin-<jdk>`;
top-level Java sources in the default package are compiled with `javac` and the
class with `main` is run. The JDK is read from the Gradle toolchain or
`sourceCompatibility`, or Maven's `java.version` or `release`, rounded up to a
published Temurin release; the default is 21. The jar runs on the matching JRE.

**Node servers `[P]`.** nixpacks' Nix package set has Node 22.11, which cannot
`require()` an ES module (current uuid and magic-string are ES modules, so
Express and NestJS apps exited with `ERR_REQUIRE_ESM`), and has no Node 24:
asking for it failed every build with "undefined variable 'nodejs_24'". A Node
app with a way to start — an answered command, a `start` script, `main`, or a
`server.js`/`index.js`/`app.js` — is now planned by the builder on the official
`node:<major>-slim` image (`node.go`): install as the lockfile says, run `build`
if there is one, then start. Bun projects and workspace roots are left to
nixpacks, as is a Node app with nothing to start (which is asked). nixpacks'
own Node default stays 22, and only reaches a Node client inside another
language's repository.

**Other start commands.** A Python module that creates a FastAPI or Flask app
and does not run it is started under uvicorn or gunicorn, when the app depends
on one (nixpacks ran `python main.py`, which exited 0). A Mix project without
Phoenix starts with `mix run --no-halt` (nixpacks ran `mix phx.server`). A Rack
app that is not Rails starts with `rackup -o 0.0.0.0` (rackup binds
127.0.0.1). A Procfile that runs a program committed in the repository gets a
Debian slim image to run it in.

**Go programs `[P]`.** A module with one `main` package (at the root or the
only one under `cmd/`) and no declared build is built on `golang:<version>`
from its `go` directive, with cgo on, and run on Debian slim beside the
repository's files. nixpacks' Go could not download a toolchain newer than its
own ("toolchain not available") and built without cgo, which a SQLite driver
needs. A repository with a Makefile, workflow or embedded client keeps the
declared-build path. A Procfile does not: it says how to start the program, not
how to build it, and Heroku's Go sample (whose Procfile names the binary its
buildpack makes) had stayed on nixpacks and its toolchain failure. The plan runs
the binary it compiled.

**Rails runs in production.** A Rails app (`config/application.rb` and
`bin/rails`) gets `RAILS_ENV` and `RACK_ENV` set to `production` and a required
`SECRET_KEY_BASE` slot. Rails' generated Puma config binds loopback outside
production, so the app was unreachable; forcing a bind address instead would
override the app's own configuration. The key is asked for rather than
generated, because it is a secret the person deploying owns.

**Heroku-only commands and Django static files.** A Procfile running
`heroku-php-apache2` or `heroku-php-nginx` gets nixpacks' own PHP server, with
the document root the command names. A Django project whose settings name a
`STATIC_ROOT` runs `collectstatic` as its build step.

**Every plan Pando writes starts through a shell.** Its `ENTRYPOINT` is
`["/bin/sh", "-c"]` and its `CMD` is the command line, the same shape as a
nixpacks image (whose entrypoint is `bash -l -c`). An answered start command for
a buildpack app is stored as that one command line; it had been stored as
`sh -c <line>`, which under nixpacks' entrypoint ran a bare `sh` that read an
empty stdin and exited 0.

**Build arguments of a plan made at build time.** When no plan is stored, the
builder plans at build time and now reads that plan's arguments from the
`build.sh` it just wrote. It had read them only from a stored plan, so a Poetry
build ran `pip install poetry==` with the version empty.

**Build cache cap `[P]`.** The BuildKit cache was never pruned; the QA run grew
Docker's disk by about 75 GB. After each build the builder prunes the build
service's cache to `cache_max_bytes` (default 10 GiB), in the background.

**Images are labeled with their app.** The builder sets the image label
`io.pando.built-for` (`api.ImageLabelBundle`) to the app ID. The Docker runtime's
`Destroy` removes every image carrying it, without force, including the untagged
images earlier builds left behind. It is a separate key from `io.pando.bundle`
because containers inherit image labels, and a trial container started from a
built image must not appear to be one of the app's workloads.

## Ports

**`PORT` is set on the primary workload `[P]`.** It is the port Pando routes to,
and it is not set when the spec sets `PORT` itself. Most frameworks written for
a platform honor it, and gunicorn binds `0.0.0.0:$PORT` when it is set. A
Procfile with `--bind 0.0.0.0:$PORT` previously started with an empty port.

**A guessed port is checked against the built image (R-097).** Detection cannot
run a trial for a source build, because there is no image yet, so a buildpack
app's port is the language's usual one (`PortFramework`). After the build, the
deploy starts the image once with `PORT` set (`internal/core/deploy/observe.go`).
If the app listens on the assumed port nothing changes. If it listens elsewhere,
a new spec revision records the observed port (origin `detected`) and the deploy
uses it. If it listens only on 127.0.0.1, the deploy stops before the running
version is touched, with `BUILD_LISTENS_ON_LOOPBACK` and the fix in the remedy.
A plan that declares `EXPOSE` gives its port as `PortExpose` and is not checked.

**Compose web ports.** The proxy routes to a workload's first port. The compose
importer now marks well-known non-HTTP ports (SSH, database, mail and similar)
as `tcp` and orders them after the others, so Gitea is routed to 3000 and not 22.

## Compose

**Only a service reached by URL becomes a provisioned slot (R-096).** The
importer had replaced every recognized backing service (`postgres`, `mysql`,
`redis`) with a slot resolved `provisioned`. That works when the app reaches the
service through a connection URL, because the variable holding it is rewired to
the provisioned instance. It fails when the app reaches the service by
hostname: `REDIS_HOST: redis` was given the whole DSN (Python reported "label
too long"), a host written into the app's code found no such host, and a
provisioned Redis required a password the app did not send. The rule is now:
a backing service becomes a slot only when some workload's variable holds a URL
in that service's own scheme that points at it. Otherwise the service is
imported as the compose file wrote it, which is what R-096 requires. A JDBC URL
does not count, because the provisioned URL cannot stand in for it.

**A compose file of only databases does not outbid the app.** When every
service is a recognized backing service, the compose detector bids 0.2 and says
why. Spring Petclinic's compose file runs a MySQL and a Postgres for
development; taken as the app, it deployed two databases and nothing to open.

**Blank names in an `env_file` template are required values (R-132).** A name
in the template for a compose `env_file` that the template gives no value
becomes a required slot of unknown type, filled by pasting a value (R-131), and
the service's variable refers to it. Compose will not start without the
env_file, so these are required by the author's own file; left as empty
variables, the app crash-looped on "Missing required environment variables". A
name with a sample value stays an empty variable, as before, and a bare
`.env.example` with no `env_file` pointing at it is read as before
(`readEnvExample`).

**Compose secrets are placed.** A service's file-backed `secrets:` are carried
in the spec and placed at `/run/secrets/<name>` (or the target named), as
compose does. They were dropped, and a Postgres given `POSTGRES_PASSWORD_FILE`
never initialized.

**Commands keep their quoting.** `command` and `entrypoint` strings are split
the way a shell would (`github.com/google/shlex`), as compose does. A
`CMD-SHELL` health check, or a string one, runs under `sh -c`; it had been passed
as a single argv element and never ran. `$$` is compose's escape for `$` and is
unescaped in commands, entrypoints and health checks; passed on doubled, the
shell read it as its own process ID.

**A service's build target and arguments are used.** `build.target` and
`build.args` were read and dropped, so a service meant to build its
`development` stage was built as its last stage: a backend whose command runs
`nodemon` found no nodemon, and a frontend meant to run a development server
ran the production nginx stage instead. `spec.WorkloadBuild` now carries `Args`,
`api.BuildRequest` carries `Target`, and the buildkit builder passes it to the
Dockerfile frontend. An argument listed without a value, which compose takes
from the shell running it, is left out.

**A mounted repository directory keeps its files (R-020, R-096).** A relative
bind mount of a directory became an empty Pando-managed volume whatever it held,
so the voting app's `./healthchecks:/healthchecks` had no scripts, its database
never became healthy, and everything waiting on it waited. A directory the
repository has files in is now read as configuration or source, not data (a
data directory is created by compose, or holds only a `.gitkeep`). When it holds
at most 16 text files `[P]` within the per-file limit, they are carried in the
spec like a single mounted file, and a file starting with `#!` is placed
executable. This applies inside the service's build context too: a development
stage often copies nothing and relies on the mount for its source (the voting
app's `vote` builds `target: dev` and exited with "can't open file app.py"),
and where the image does have the files the copy is of the same commit. A
directory too big to carry is dropped when it is inside the build context,
leaving the image's copy, and is a volume otherwise.

**An anonymous volume belongs to its service (R-200).** Compose makes an
anonymous volume per container. Pando named it by its path alone, so a frontend
and a backend that each keep `/usr/src/app/node_modules` in one shared a
volume, and the frontend ran with the backend's packages ("react-scripts: not
found"). Its name now starts with the service's.

**A service behind a profile is not imported.** `docker compose up` leaves out
a service with `profiles:`, and so does Pando now, with a warning naming it.
The voting app's `seed` service, opt-in by its author, ran on every deploy and
exited 50. When every service has a profile, all are imported.

## Detection

**The planner can recognize what the manifest list does not.** The buildpack
detector read eight manifest files. When none matches, it now asks the planner,
and bids when the plan says how the app starts. This covers a bare `main.py`, a
Deno module, `mix.exs`, a `.csproj`, a Gradle build and a plain `index.php`,
which each got the generic "could not work out how to build" question.

**Dockerfile layouts.** `Containerfile` is read like `Dockerfile`. A repository
whose only deployable Dockerfile is in a subdirectory uses it without asking,
built from the repository root. With several, the question remains, and the
draft now carries a primary workload and a port question, so an answer produces
something to run; it had failed at accept with "This app has no workloads".

**A site in `docs/`** is served from there, as GitHub Pages does.

**Required build arguments** are asked for, as `build_arg.<NAME>`, when the
Dockerfile declares the `ARG` with no default and refuses to build without it
(`${NAME:?…}` or a `-z` test). Other `ARG`s without defaults are left alone
(R-104). The answer becomes a build argument.

**An answer applies only to the reading that asked it.** When the tie-break
adopts a runner-up, only answers to that candidate's own questions are applied
to it. A port given for the Dockerfile reading of `ac-fastapi` was stamped on the
adopted compose app, and traffic went to 80 while the compose file had the app on
8000.

**An answer that cannot become a spec is refused when given.** A
`build_method` or `build_strategy` answer must name a reading some detector made
that has something to run (`Proposal.CheckAnswers`). The answers endpoint
refuses anything else at once, saying which readings exist, or that none does
and what to add; screening records such an answer from the AI adapter as
refused rather than applying it. They had been stored and then refused at
accept as "This app has no workloads".

**Published-image apps are proposed as the image.** An app created from an
image skips the auction and is proposed as a prebuilt workload running that
image; the trial run observes its port. It had gone through repository
detection over an empty checkout and been asked how to build it. An app with no
checkout now also gets an empty source view: the view had been rooted at `""`,
which resolved names against the server's own filesystem root, so a detector,
the source scanner or a screener sending files to a provider read the server's
files.

**What an image's trial shows is used (R-097, R-200).** Observed ports are
ordered web first (Gitea's image listens on SSH and HTTP, Mailpit's on SMTP and
HTTP). An image whose only observed ports are a database's or a mail server's
is blocked with a reason rather than deployed. Paths the image declares with
`VOLUME` get a volume (`Declared: "image"`) mounted on the primary workload;
Vaultwarden refuses to start without one. NUL bytes are dropped from a
detection before it is saved: the proposal is jsonb, which refuses `\u0000`, and
Grafana's detection (a NUL in its trial log) could not be saved, nor could any
repository with one in text detection quotes, such as a Dockerfile's `CMD` in
the evidence; a detection whose result cannot be saved is now recorded failed
rather than left running.

**Libraries are refused (R-021).** A Go module with Go files and no
`package main` outside the directories the go tool ignores (`testdata`, and
names starting with `_` or `.`), and a Python package under `src/` with no entry
point at the root, are reported as `blocked` with an explanation.

## Runtime and platform

**App networks come from Pando's own range `[P]`.** Docker's default pool holds
about thirty networks and Pando takes one per app. A deleted app's network is
removed only at the next restart, because Pando is attached to it and detaching
the running container was measured to drop its published ports on Docker
Desktop (see notes-detected-specs-were-not-deployable.md). After about 25
deletions every deploy failed. The Docker runtime now gives each app network a
/26 from `network_pool` (default `10.213.0.0/16`, 1,024 networks), skipping any
block an existing network overlaps, and falls back to Docker's pool when the
range is full or set to `off`. The disconnect was re-tested on Docker Desktop
28.4 and did not drop the published port, but it was not reproduced under the
original conditions, so the restriction stays.

**Startup reclaim spares stopped apps.** A stopped container is not a network
endpoint, so a stopped app's network looked empty and was removed. A network is
now reclaimed only when no container of its bundle exists in any state.

**Two installs on one Docker host leave each other alone.** Startup passes the
runtime a predicate, `owns`, backed by `Apps.Known` (any app this install ever
created, deleted ones included). Reclaim and rejoin skip a network whose bundle
the install does not own, and any network with no bundle label. Each install
had rejoined the other's apps and removed the other's empty networks.

**Re-detection runs in the background.** `POST /detection/rerun` checks what
would refuse it (the app, its source, the allowlist, R-092) in the request,
marks the detection running, and returns 202; the clone, auction and trial run
happen after. Every client already polled `GET /detection`. It had held the
request open for the whole detection, over 180 s under load.

**`GET /detection` says which questions are open.** While the status is
`needs_answers`, the response carries `unanswered`, the keys still without an
answer; empty means ready to accept (design 04 §2.2).

**Deleting an app tears it down immediately.** The delete handler wakes the
garbage collector's teardown pass through `GC.TeardownNow`; it had waited for
the hourly pass.

**Work interrupted by a restart is recorded.** At startup, before the loops that
start new work, detections still `running` and deploys still `pending`,
`building` or `applying` are marked failed with a message saying Pando
restarted. They had stayed in progress forever, and an app with a deploy in
flight refuses the next one.

**A dependency with a health check is waited for (R-096).** The Docker runtime
started dependencies first and never waited for them, so a backend started
while its database was initializing and crashed; compose's `condition:
service_healthy` was read and discarded, and the health checks on provisioned
databases guarded nothing. A workload now starts only once each dependency that
has a health check reports healthy, for up to two minutes each.

**A deploy waits for the app to stay up, and restarts what stops.** A deploy
counts the app as running only once every part has stayed up for ten seconds;
an app that exited a second after starting had been reported deployed on the
first look. While it waits (two minutes; three since issue #90, see design 05 §4), a part that stops is started again
every five seconds by re-applying the same plan — a compose backend that
cannot yet reach its database, or a proxy whose upstream is that backend, is
how compose apps start, and under `docker compose up` their restart policy
covers it. If the primary workload is still stopped when the wait ends, the
deploy fails with `STATE_APP_EXITED` and the app's last 30 lines of output in
the deploy log. An app that is running but not yet healthy is still reported
`degraded`, as before.

**A deleted app leaves nothing on disk but what it was asked to keep
(R-224).** The QA harness removed whatever a delete left before checking for
residue, so every case reported clean while Pando left, per app: its build
cache, its uploaded source, every image pulled for it, and an anonymous volume
for every container of an image that declares `VOLUME`. A forced delete of an
ordinary Python app left about 364 MB. Now:

- The builder has a `Forget` call (`api.BuilderAdapter`), and the GC's teardown
  has the builder that built the app forget its cache, and removes its stored
  upload (`source.DiscardUpload`). Both run for an app never deployed too,
  since an upload is stored first. A restore goes to an app created again, with
  an upload of its own (R-206), so nothing needs the old one.
- After each build the builder prunes the cache blobs its new index no longer
  reaches. BuildKit's local cache export adds each build's layers and never
  removes the last build's, so a cache grew for the life of the app. Anything a
  manifest or cache config mentions is kept, and an unreadable index prunes
  nothing.
- Containers are removed with their anonymous volumes. The next container never
  reattached one, so it held nothing reachable; named volumes are untouched
  (R-204).
- Pulled images are owned through tags `[P]`: each app that runs an image tags
  it `pando-pulled/<hash of the reference>:<app>`, and an image Pando had to
  fetch is also tagged `:pulled`. Destroy removes the app's tag, and when no
  app's tag is left on an image Pando fetched, removes the image without force,
  so Docker refuses while any container still uses it. An image that was on the
  host before Pando asked for it is never removed, and one another install on
  the same host runs carries that install's tag. Pando's own helper images are
  kept. Tags rather than a record in Pando's state because the runtime adapter
  may not touch the state store (R-027), and because Docker then does the
  counting across apps and installs.
- A delete that settled the app's storage takes the volumes (R-204). R-204
  has a delete keep a final backup or discard, and one is refused until it
  says which. Both answers removed the volume rows and left the volumes on disk,
  unreachable — a forced delete said "discard" and discarded nothing. The
  decision is now recorded on the app (`apps.discard_storage`, migration 31)
  and the GC's teardown destroys the volumes with the bundle: at once for
  `force=true`, and after the backup for `backup=true`, which completes before
  the delete returns. Only that recorded decision sets `KeepVolumes` false.
- The QA harness records what Pando's own delete left before it sweeps
  (`pando_left`), logs it as `PANDO LEFT`, and the summary counts it.

**Filling a slot keeps the slots filled before it.** `PUT /apps/{id}/slots/{key}`
wrote its new revision from the pinned spec, so the second slot filled after an
accept replaced the first. It now builds on the newest revision.

**The reconciler does not bring back a deleted app.** A correction pass that
read an app before it was deleted went on to apply it, re-creating a volume the
teardown had just removed. The reconciler now checks that the app still exists
immediately before applying (`Apps.Live`).

**Other fixes.** A failed image pull reports the registry's reason (the daemon
reports it inside a 200 response, and the stream was discarded). Uploaded
sources are removed when their checkout is closed. Git fetches over HTTP use a
client with HTTP/2 health-check pings, a response-header timeout and a short
idle-connection lifetime, aimed at the fetch that hung until the detection
deadline after a period of use; a clone whose connection fails that way is
tried up to three times, and one that fails because of the repository is not.

**App resource defaults `[P]` (R-240).** `apps.cpu_millis`, `apps.memory_bytes`
and `apps.disk_bytes` in the install configuration (`PANDO_APPS_*`) set what
every new app is given. The shipped values are unchanged: one core, 512 MiB and
10 GiB. They had been constants.

## Not addressed here

- Required environment variables the app refuses to start without, when nothing
  in the repository declares them (`ctr-dockerfile-env-required`). The deploy
  now fails with the app's own output instead of reporting success.
- A CLI tool is not refused at detection; its deploy fails because the process
  exits (`edge-cli-tool`).
- Pre-delete backups have no retention for deleted apps. They are kept until
  discarded (R-204), which R-224's disk bound does not yet account for.
- The audit log has no retention (issue #60).
- An image a detection trial pulled for an app that is deleted before its first
  deploy stays: no app ever claimed it, so no teardown releases it.
