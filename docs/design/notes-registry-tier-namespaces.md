# R-094 tier 1: ghcr.io and Docker Hub are not the same evidence

R-094's top tier is:

> **Already-published image** — check the repo's namespace on ghcr.io and
> Docker Hub before building anything.

Implementing it, the two turn out to be different in a way that matters.

## ghcr.io

A ghcr.io namespace belongs to the GitHub account of the same name. Only the
owner of `github.com/acme` can push to `ghcr.io/acme/*`. So finding
`ghcr.io/acme/notes` for a repository at `github.com/acme/notes` really is
evidence that the maintainers of that repository publish that image.

It is not proof the image was built from the commit being deployed — nothing
here verifies provenance — but "the people who own this repository publish this
image" is a true statement, and that is the claim tier 1 rests on.

## Docker Hub

A Docker Hub namespace is an unrelated account on an unrelated service. Anyone
may register the Docker Hub user `acme` whatever happens to be at
`github.com/acme`. A match proves only that two strings are equal.

So for Docker Hub, tier 1 would have Pando propose running a stranger's image
and describe it, in the evidence shown beside it, as *"the image the project's
own maintainers publish"* — a statement that is simply false. The reviewer who
would have to catch that is the person R-005 describes, who may not know what a
container registry is.

The exposure is bounded by review — R-098 means nothing is pinned or deployed
without someone accepting it — but "a human will notice" is not a control when
the interface is actively telling them there is nothing to notice.

## What was built

`registryprobe.Probe.IncludeDockerHub` (`internal/detect/registryprobe`), default **off**. ghcr.io is checked; Docker
Hub is opt-in for an operator who knows their own naming and wants it.

## What needs deciding

R-094 should say which registries qualify and why, rather than naming two as
though they were interchangeable. A useful rule: **a registry qualifies for tier
1 when its namespace is controlled by the same identity that controls the source
repository.** ghcr.io passes for GitHub sources. Docker Hub does not, for any
source. A future GitLab registry check would pass for GitLab sources.

That rule also answers the question for registries nobody has thought about yet,
which a list of two names does not.

## A compose file outranks it (issue #90)

Tier 1 short-circuited the auction, so a repository whose compose file runs its
published image beside a database ran the image alone: Planka, Listmonk,
Linkwarden and a dozen more crashed connecting to a database that was not
there, or on a variable the compose file set. R-094 is amended: a compose file
that imports outranks the published image, and the image outranks everything
else. A compose service that builds the published image (`build: .` beside
`image: ghcr.io/acme/app`) runs the published image instead. A compose file
Pando refuses (R-099) does not outrank it, because the image is an answer and
the refusal is about a file the app does not need.

`detect.Job.Run` asks the probe first, runs the auction, and decides: the image
alone (`checkRegistry`), or the compose candidate with the image as a runner-up
(`withComposeOverImage`). Nothing is asked between them; the ladder settles it.
