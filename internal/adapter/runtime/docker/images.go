package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/moby/moby/client"
)

// Pulled images are owned through tags, so that Docker does the counting.
//
// Images Pando built carry a label naming their app and go with it (Destroy).
// An image Pando pulled for a compose `image:` service or a published-image app
// cannot carry one, and stayed on the host forever after the app was deleted
// (issue #55, R-224). It cannot simply be removed either: another app may run
// it, another install on the same Docker host may, and it may have been on the
// host before Pando ever asked for it.
//
// So each image a workload runs gets a tag per app that uses it,
// pando-pulled/<hash of the reference>:<app>, and one more, :pulled, when it
// was Pando that fetched it. Deleting an app removes its tag. When no app's
// tag is left and Pando fetched the image, the image is removed — without
// force, so Docker itself refuses while any container still uses it. An image
// that was already on the host is never removed.
const pulledRepo = "pando-pulled"

// pulledMarker is the tag that says Pando fetched the image.
const pulledMarker = "pulled"

// ownedRepo is the repository an image reference's ownership tags live in.
// Hashed because a reference may hold characters no tag can: a registry host
// and port, a digest.
//
// Of the familiar form (engine.go), because the reference a plan names and the
// one an engine reports must hash alike: Docker says "busybox:1.36", Podman
// "docker.io/library/busybox:1.36", and hashed as written the image Pando
// claimed was never recognized as that image again.
func ownedRepo(ref string) string {
	sum := sha256.Sum256([]byte(familiarRef(ref)))
	return pulledRepo + "/" + hex.EncodeToString(sum[:8])
}

// bundleTag is a bundle ID as a tag. Docker tags are [A-Za-z0-9_.-], up to
// 128 characters, not starting with . or -; Pando's IDs already fit, and one
// that does not gets no ownership tag rather than a mangled one.
func bundleTag(bundleID string) (string, bool) {
	tag := strings.ToLower(bundleID)
	if tag == "" || tag == pulledMarker || len(tag) > 128 || tag[0] == '.' || tag[0] == '-' {
		return "", false
	}
	for _, r := range tag {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.' && r != '-' {
			return "", false
		}
	}
	return tag, true
}

// ownsRef reports whether an image reference is one Pando built, which its
// label accounts for instead.
func builtByPando(ref string) bool {
	return strings.HasPrefix(ref, "pando/")
}

// imageClaim says who an image is being made available for.
type imageClaim struct {
	// Bundle is the app whose workload runs it. Empty for a trial or a
	// helper.
	Bundle string

	// Owned marks an image Pando fetched as Pando's to remove once unclaimed.
	// False for Pando's own helper images, which it keeps.
	Owned bool
}

// forBundle is the claim an app's workload makes.
func forBundle(bundleID string) imageClaim { return imageClaim{Bundle: bundleID, Owned: true} }

// forTrial is the claim a trial makes: no app yet, but an image fetched for
// one is still Pando's.
var forTrial = imageClaim{Owned: true}

// claimImage records that a bundle runs an image, and whether Pando fetched it.
// Best effort: a tag that fails to apply costs disk later, never a deploy now.
func (a *Adapter) claimImage(ctx context.Context, ref string, claim imageClaim, pulled bool) {
	if !claim.Owned || builtByPando(ref) {
		return
	}
	repo := ownedRepo(ref)
	if pulled {
		_, _ = a.cli.ImageTag(ctx, client.ImageTagOptions{Source: ref, Target: repo + ":" + pulledMarker})
	}
	if tag, ok := bundleTag(claim.Bundle); ok {
		_, _ = a.cli.ImageTag(ctx, client.ImageTagOptions{Source: ref, Target: repo + ":" + tag})
	}
}

// releaseImages drops a bundle's claim on every image it ran, and removes the
// ones Pando fetched that no app claims any more.
func (a *Adapter) releaseImages(ctx context.Context, bundleID string) {
	tag, ok := bundleTag(bundleID)
	if !ok {
		return
	}
	claimed, err := a.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", pulledRepo+"/*:"+tag),
	})
	if err != nil {
		return
	}
	for _, img := range claimed.Items {
		for _, rt := range img.RepoTags {
			rt = familiarRef(rt) // Podman reports "localhost/pando-pulled/…"
			repo, t, found := strings.Cut(rt, ":")
			if !found || t != tag || !strings.HasPrefix(repo, pulledRepo+"/") {
				continue
			}
			if _, err := a.cli.ImageRemove(ctx, rt, client.ImageRemoveOptions{}); err != nil {
				continue
			}
			a.removeIfUnclaimed(ctx, img.ID, repo)
		}
	}
}

// removeIfUnclaimed removes an image Pando fetched once no app claims it.
func (a *Adapter) removeIfUnclaimed(ctx context.Context, imageID, repo string) {
	inspect, err := a.cli.ImageInspect(ctx, imageID)
	if err != nil {
		return
	}
	refs, fetched, claimed := unclaimedRefs(inspect.RepoTags, repo)
	if claimed || !fetched {
		return
	}
	// The marker first: if the image turns out to be in use, what is left is an
	// image with no marker, which is never removed — the safe direction.
	if _, err := a.cli.ImageRemove(ctx, repo+":"+pulledMarker, client.ImageRemoveOptions{}); err != nil {
		return
	}
	for _, ref := range refs {
		_, _ = a.cli.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	}
}

// unclaimedRefs reads an image's tags against one ownership repository: the
// references it stands for (those whose hash is the repository's), whether
// Pando fetched it, and whether any app still claims it.
func unclaimedRefs(repoTags []string, repo string) (refs []string, fetched, claimed bool) {
	for _, rt := range repoTags {
		rt = familiarRef(rt)
		r, t, _ := strings.Cut(rt, ":")
		switch {
		case r == repo && t == pulledMarker:
			fetched = true
		case r == repo:
			claimed = true
		case ownedRepo(rt) == repo:
			refs = append(refs, rt)
		}
	}
	return refs, fetched, claimed
}
