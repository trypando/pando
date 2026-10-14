# Pando ships as one binary (R-253). This image is how it reaches a host —
# the artifact is unchanged, the container is just the delivery.
#
# Every stage is a Docker Hardened Image (dhi.io). The published image runs on
# the runtime variant of alpine-base: busybox, musl, CA certificates and a
# non-root user, with no package manager. What Pando adds to it is installed in
# the -dev variant and copied across, and the toolchains never leave the build
# stages. Pulling from dhi.io needs a Docker account: `docker login dhi.io`.
#
# Base images are pinned by digest, with the tag kept as the readable half. A
# tag moves — `alpine-base:3.24` is a different filesystem this month than last
# — so a build is only reproducible against a digest. Dependabot updates these,
# which is what keeps the pin from meaning "old".
#
# The console is embedded in the binary rather than served beside it (R-253),
# so it has to exist before the Go build, not after it. Building it here rather
# than relying on whatever the host happens to have in internal/console/dist:
# that directory holds only a README in a fresh clone, so an image built without
# this stage starts, serves the API, and 404s the UI.
#
# Go as well as Node, because `npm run build` regenerates the API types from the
# Go types first (R-261: the API is the product, so the console's view of it is
# generated rather than written). A Node-only stage cannot run it, and splitting
# the steps here would put a second definition of how the console builds next to
# the one in package.json.
#
# Neither toolchain reaches the final image.
#
# Both build stages run on the builder's own platform and cross-compile: the
# console is JavaScript and the binary is built with cgo off, so neither needs
# to run on the platform it is for. The release builds amd64 and arm64 in one
# go (issue #52), and running npm and the Go toolchain under emulation for the
# other one took the better part of an hour.
#
# Go's module and build caches, and npm's, are cache mounts at fixed paths:
# they persist in the builder between builds without entering any layer, so a
# source change recompiles what changed rather than every dependency. CI keeps
# the Go build cache between runs too (the image job in ci.yml). The paths are
# set rather than left to each image's defaults because CI names them.
#
# One Go base for the console and server stages, so the image is pinned in one
# place.
FROM --platform=$BUILDPLATFORM dhi.io/golang:1.27-alpine3.24-dev@sha256:9a9160e3c89a37572e35271c65a2b38a2d1671c8ab0d6abee19ce3d6cecb9be9 AS go
# GOTOOLCHAIN=auto because go.mod requires Go 1.27.2, the security release for
# GO-2026-6599 – GO-2026-6617, and the hardened image is still 1.27.1. The go
# command fetches 1.27.2 from the module proxy, checked against the checksum
# database like any module, into the module cache. Remove it, and bump the
# digest, once dhi.io/golang:1.27 ships 1.27.2.
ENV GOMODCACHE=/cache/go-mod GOCACHE=/cache/go-build GOTOOLCHAIN=auto
WORKDIR /src

FROM go AS console
ENV npm_config_cache=/cache/npm
RUN apk add --no-cache nodejs npm

# Manifests first, so a change to console source does not re-run npm ci.
COPY console/package.json console/package-lock.json ./console/
RUN --mount=type=cache,target=/cache/npm cd console && npm ci --ignore-scripts

# The whole tree: the build reads cmd/gen-api-types for the types and
# .claude/skills/pando-design for the design system.
COPY . .
# Vite is configured to write to ../internal/console/dist, which is the path
# go:embed reads.
RUN --mount=type=cache,target=/cache/go-mod --mount=type=cache,target=/cache/go-build \
    cd console && npm run build

FROM go AS build
ARG TARGETOS
ARG TARGETARCH

# Dependencies first, so a source change does not re-download the module cache.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/cache/go-mod go mod download

COPY . .
COPY --from=console /src/internal/console/dist/ ./internal/console/dist/

# go:embed is satisfied by the directory's README alone, so a console that
# failed to arrive would produce a binary that builds, starts, and has no UI.
# Fail here instead, where the cause is still visible.
RUN test -f internal/console/dist/index.html \
    || { echo "the console did not reach the build stage" >&2; exit 1; }

# Stamped only when the release workflow says what it is building
# (.github/workflows/image.yml), with the same three values GoReleaser stamps
# into the released CLI. A local build passes none and `pando version` says
# "development build", which is accurate: it is not a release and must not
# claim to be one.
ARG VERSION=""
ARG COMMIT=""
ARG BUILD_DATE=""
RUN --mount=type=cache,target=/cache/go-mod --mount=type=cache,target=/cache/go-build \
    stamp=""; \
    if [ -n "$VERSION" ]; then \
      stamp="-X main.buildVersion=${VERSION} -X main.buildCommit=${COMMIT} -X main.buildDate=${BUILD_DATE}"; \
    fi; \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
      go build -trimpath -ldflags="-s -w ${stamp}" -o /out/pando ./cmd/pando

# What the server needs beyond the runtime base, installed where there is a
# package manager and staged for the copy below:
#
# - postgresql17-client for pg_dump, pg_restore and psql (backups and restore,
#   R-210, R-212; restore pipes pg_restore into psql). The client
#   major version has to match the server: pg_dump refuses a server newer than
#   itself, and discovering that during a restore is discovering it at the worst
#   possible moment. Bump this with the postgres service in docker-compose.yml,
#   never separately.
# - su-exec, which the entrypoint drops privileges with.
# - tzdata, which the runtime base does not carry.
# - git and bash, for Railpack's planner. mise lists the Python releases it can
#   install with pyenv's python-build, a bash script it fetches with git, and
#   Railpack runs mise with every config file switched off, so there is no
#   setting that avoids it. Without them every Python app failed to plan with
#   "env: can't execute 'bash'" (issue #172). Nothing else in Pando runs either.
# - Railpack, which turns a repository with no deployment instructions into a
#   build plan that Pando's BuildKit builds through Railpack's frontend image
#   (R-095: wrap an existing implementation rather than reimplementing
#   convention-matching). Fetched here rather than at runtime, pinned and
#   checked against the release's checksums, so the same repository produces
#   the same plan a year from now. It only ever *plans* — `railpack prepare`
#   writes a JSON file and builds nothing, so no container runtime socket is
#   involved anywhere (R-112). mise comes with it, at the release this Railpack
#   bundles: Railpack resolves language versions by running mise, and
#   downloads it into /tmp/railpack/mise on first use if it is not already
#   there. Putting it there now means planning downloads no program; version
#   lookups still reach the package indexes. Bump RAILPACK_VERSION,
#   MISE_VERSION, their checksums and the image digests in
#   internal/adapter/builder/buildkit/railpack.go together. curl fetches them
#   and is removed again.
#
# apk-tools is removed before staging, so the package database copied below
# lists what the image actually holds: a scanner reads it to know what is
# installed, and an entry for a package manager that is not there is a finding
# about nothing.
FROM dhi.io/alpine-base:3.24-dev@sha256:08d37dc414e07a688b0c3b9405e0b33688507e41a74bc24118a7f0817b772e27 AS packages
ARG RAILPACK_VERSION=0.40.0
ARG MISE_VERSION=2026.9.12
ARG TARGETARCH
RUN apk add --no-cache bash curl git postgresql17-client su-exec tzdata \
    && case "$TARGETARCH" in \
         arm64) rp=arm64;   rpsum=86c5a3db888c710b2014262de55fc87a56c85c4e96ba7cd10894e5df210a212b; \
                mise=arm64; misesum=22e7ecd3c6c84ef36e4d0c78c4eef49a79e3fb83aa88e091e2993fd9c9a3f8a1 ;; \
         amd64) rp=x86_64;  rpsum=52e5558f830b7349386e9448e04351d0834df041d4c31c20b7ec4d539f0d66d7; \
                mise=x64;   misesum=1b2051d8d3efab4d9c85ceb59849a3eb052db24ad406a3901fd6b45374c56ebe ;; \
         *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL --proto =https --tlsv1.2 -o /tmp/railpack.tgz \
       "https://github.com/railwayapp/railpack/releases/download/v${RAILPACK_VERSION}/railpack-v${RAILPACK_VERSION}-${rp}-unknown-linux-musl.tar.gz" \
    && echo "${rpsum}  /tmp/railpack.tgz" | sha256sum -c - \
    && curl -fsSL --proto =https --tlsv1.2 -o /tmp/mise.tgz \
       "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${mise}-musl.tar.gz" \
    && echo "${misesum}  /tmp/mise.tgz" | sha256sum -c - \
    && mkdir -p /usr/local/bin /tmp/railpack/mise \
    && tar xzf /tmp/railpack.tgz -C /usr/local/bin railpack \
    && tar xzf /tmp/mise.tgz -C /tmp mise/bin/mise \
    && mv /tmp/mise/bin/mise "/tmp/railpack/mise/mise-${MISE_VERSION}" \
    && chmod +x /usr/local/bin/railpack "/tmp/railpack/mise/mise-${MISE_VERSION}" \
    && rm -rf /tmp/railpack.tgz /tmp/mise.tgz /tmp/mise \
    && apk del --no-cache curl \
    && apk del --no-cache apk-tools \
    && mkdir /staging \
    && tar -cf - /lib /usr/lib /usr/libexec /usr/bin/pg_dump /usr/bin/pg_restore /usr/bin/psql \
         /sbin/su-exec /usr/share/zoneinfo /usr/bin/git /bin/bash /usr/share/git-core \
         /usr/local/bin/railpack 2>/dev/null \
       | tar -xf - -C /staging

FROM dhi.io/alpine-base:3.24@sha256:a741d4f1ccaf31f9394a36477e50df4620a8030dbff3d9a2ea8e84672b683ada
COPY --from=packages /staging/ /
# Railpack's mise directory, writable by the server, which takes locks there.
# A copy of its own rather than part of the staged tree: staging it under /tmp
# replaced /tmp's own mode with the staging directory's 755, and the server
# could no longer make a temporary directory for any build (issue #172).
COPY --from=packages --chown=nonroot:nonroot /tmp/railpack /tmp/railpack

# The runtime base's own non-root user, nonroot (65532), runs the server. The
# entrypoint needs root only to join the Docker socket's group, so the image
# starts as root and the entrypoint drops to nonroot before Pando starts.
# By number: the hardened base has no root entry in /etc/passwd.
USER 0
RUN mkdir -p /var/lib/pando /etc/traefik/dynamic \
    && chown nonroot:nonroot /var/lib/pando /etc/traefik/dynamic

WORKDIR /var/lib/pando

COPY --from=build /out/pando /usr/local/bin/pando
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# The entrypoint starts as root only long enough to join the runtime socket's
# group — whose ID differs per host and so cannot be baked in — then drops to
# the unprivileged nonroot user. The server itself never runs as root.
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh", "/usr/local/bin/pando"]
