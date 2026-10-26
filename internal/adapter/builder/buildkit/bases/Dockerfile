# The base images Pando supplies to the Dockerfiles it writes.
#
# Never built. The builder reads each stage's image from here (bases.go), and
# Dependabot reads this file to propose a newer tag, which it cannot do for a
# Go constant. Before this file, nginx 1.27 on Alpine 3.21 stayed pinned here
# with two critical and forty-one high findings that every static site
# inherited.
#
# Each is pinned to a line and a digest. The tag says which line; the digest
# is what a build pulls, so a tag repushed upstream, by a rebuild or by anyone
# who can push to it, cannot change what every app is built on. A digest nobody
# updates is a known vulnerability that stays, so Dependabot moves the digest
# when the registry rebuilds the line, and the tag when there is a next line.
# TestBaseImagesStayOnMaintainedLines refuses a base without a digest.
#
# The builder reads only the FROM lines. Each stage ends with USER nobody so
# that this file, which no one builds, does not describe an image that runs as
# root. The user an app's image runs as is set by the Dockerfile the builder
# writes, not here.

# static-server serves a built static site. [P] nginx because it is the
# smallest well-understood thing that serves a directory over HTTP, alpine for
# the same reason the provisioned services use alpine, and a minor version so a
# rebuild a year from now produces the same server.
#
# A stable line: an even minor. An odd minor is nginx's mainline, which is
# replaced every few weeks and is no longer rebuilt once it is.
# TestBaseImagesStayOnMaintainedLines refuses an odd minor, so a
# Dependabot pull request proposing one cannot merge.
FROM nginx:1.30-alpine@sha256:47928bf67eb62a1863cb08d9ecb52d3e704304ba614eaee17884204677702951 AS static-server
USER 65534

# program-runtime runs a Go program Pando compiled, or a program committed in
# the repository. [P] Debian slim, because a binary built for Linux most often
# links against glibc. Debian's current stable release, because the golang
# images build on it, and a binary linked against a newer glibc than the one it
# runs on does not start.
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS program-runtime
USER 65534
