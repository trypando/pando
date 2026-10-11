# The base images Pando supplies to the Dockerfiles it writes.
#
# Never built. The builder reads each stage's image from here (bases.go), and
# Dependabot reads this file to propose a newer tag, which it cannot do for a
# Go constant. Before this file, nginx 1.27 on Alpine 3.21 stayed pinned here
# with two critical and forty-one high findings that every static site
# inherited.
#
# Each is pinned to a line, not a digest. The registry rebuilds a line's tag
# when its OS packages are patched, and a build resolves the tag when it runs,
# so a site built by an older Pando still gets those fixes while the line is
# maintained. Moving to the next line is the pull request Dependabot opens.

# static-server serves a built static site. [P] nginx because it is the
# smallest well-understood thing that serves a directory over HTTP, alpine for
# the same reason the provisioned services use alpine, and a minor version so a
# rebuild a year from now produces the same server.
#
# A stable line: an even minor. An odd minor is nginx's mainline, which is
# replaced every few weeks and is no longer rebuilt once it is.
# TestBaseImagesStayOnMaintainedLines refuses an odd minor, so a
# Dependabot pull request proposing one cannot merge.
FROM nginx:1.30-alpine AS static-server

# program-runtime runs a Go program Pando compiled, or a program committed in
# the repository. [P] Debian slim, because a binary built for Linux most often
# links against glibc. Debian's current stable release, because the golang
# images build on it, and a binary linked against a newer glibc than the one it
# runs on does not start.
FROM debian:trixie-slim AS program-runtime
