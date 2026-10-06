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
FROM --platform=$BUILDPLATFORM dhi.io/golang:1.27-alpine3.24-dev@sha256:89778e746908997996f8e1de415d9834154eac97a43298ea54bdb74f78116430 AS console
ENV GOMODCACHE=/cache/go-mod GOCACHE=/cache/go-build npm_config_cache=/cache/npm
WORKDIR /src
RUN apk add --no-cache nodejs npm

# Manifests first, so a change to console source does not re-run npm ci.
COPY console/package.json console/package-lock.json ./console/
RUN --mount=type=cache,target=/cache/npm cd console && npm ci

# The whole tree: the build reads cmd/gen-api-types for the types and
# .claude/skills/pando-design for the design system.
COPY . .
# Vite is configured to write to ../internal/console/dist, which is the path
# go:embed reads.
RUN --mount=type=cache,target=/cache/go-mod --mount=type=cache,target=/cache/go-build \
    cd console && npm run build

FROM --platform=$BUILDPLATFORM dhi.io/golang:1.27-alpine3.24-dev@sha256:89778e746908997996f8e1de415d9834154eac97a43298ea54bdb74f78116430 AS build
ENV GOMODCACHE=/cache/go-mod GOCACHE=/cache/go-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

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

# nixpacks turns a repository with no deployment instructions into a
# Dockerfile, which BuildKit then builds (R-095: wrap an existing
# implementation rather than reimplementing convention-matching).
#
# Fetched here rather than at runtime so an install with no internet still
# builds, and pinned so the same repository produces the same plan a year from
# now. It only ever *generates* — `nixpacks build --out` writes a Dockerfile and
# does not build, so no container runtime socket is involved anywhere (R-112).
FROM --platform=$BUILDPLATFORM dhi.io/alpine-base:3.24-dev@sha256:eb950d88a309a402e2a731ba80bf9067378580becb611aa699ffb87594cd320b AS nixpacks
ARG NIXPACKS_VERSION=1.41.0
ARG TARGETARCH
RUN apk add --no-cache curl tar \
    && case "$TARGETARCH" in \
         arm64) arch=aarch64 ;; \
         amd64) arch=x86_64  ;; \
         *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/nixpacks.tgz \
       "https://github.com/railwayapp/nixpacks/releases/download/v${NIXPACKS_VERSION}/nixpacks-v${NIXPACKS_VERSION}-${arch}-unknown-linux-musl.tar.gz" \
    && mkdir -p /usr/local/bin \
    && tar xzf /tmp/nixpacks.tgz -C /usr/local/bin nixpacks \
    && chmod +x /usr/local/bin/nixpacks

# What the server needs beyond the runtime base, installed where there is a
# package manager and staged for the copy below:
#
# - postgresql17-client for pg_dump and pg_restore (backups, R-210). The client
#   major version has to match the server: pg_dump refuses a server newer than
#   itself, and discovering that during a restore is discovering it at the worst
#   possible moment. Bump this with the postgres service in docker-compose.yml,
#   never separately.
# - su-exec, which the entrypoint drops privileges with.
# - tzdata, which the runtime base does not carry.
#
# apk-tools is removed before staging, so the package database copied below
# lists what the image actually holds: a scanner reads it to know what is
# installed, and an entry for a package manager that is not there is a finding
# about nothing.
FROM dhi.io/alpine-base:3.24-dev@sha256:eb950d88a309a402e2a731ba80bf9067378580becb611aa699ffb87594cd320b AS packages
RUN apk add --no-cache postgresql17-client su-exec tzdata \
    && apk del --no-cache apk-tools \
    && mkdir /staging \
    && tar -cf - /lib /usr/lib /usr/libexec /usr/bin/pg_dump /usr/bin/pg_restore /usr/bin/psql \
         /sbin/su-exec /usr/share/zoneinfo 2>/dev/null \
       | tar -xf - -C /staging

FROM dhi.io/alpine-base:3.24@sha256:a741d4f1ccaf31f9394a36477e50df4620a8030dbff3d9a2ea8e84672b683ada
COPY --from=packages /staging/ /

# The runtime base's own non-root user, nonroot (65532), runs the server. The
# entrypoint needs root only to join the Docker socket's group, so the image
# starts as root and the entrypoint drops to nonroot before Pando starts.
# By number: the hardened base has no root entry in /etc/passwd.
USER 0
RUN mkdir -p /var/lib/pando /etc/traefik/dynamic \
    && chown nonroot:nonroot /var/lib/pando /etc/traefik/dynamic

WORKDIR /var/lib/pando

COPY --from=build /out/pando /usr/local/bin/pando
COPY --from=nixpacks /usr/local/bin/nixpacks /usr/local/bin/nixpacks
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# The entrypoint starts as root only long enough to join the runtime socket's
# group — whose ID differs per host and so cannot be baked in — then drops to
# the unprivileged nonroot user. The server itself never runs as root.
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh", "/usr/local/bin/pando"]
