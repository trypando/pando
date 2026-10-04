// Package upgrade replaces the running Pando with a newer release (R-355 –
// R-361): checked, backed up, swapped by a helper, and put back on its own
// when the new version does not come up.
package upgrade

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/trypando/pando/internal/core/update"
)

// Repository is the published image, as a deployment names it.
const Repository = update.Image

// MovingTag reports how a deployment's image reference names Pando: the
// moving tag, if it is one, and whether it covers version (R-355). A reason
// is returned when it does not, written for the person who will change it.
//
//	trypando/pando:latest  any release
//	trypando/pando:0.3     0.3's patches
//	trypando/pando:1       1.x
//	trypando/pando:0.3.1   no: an exact version
//	trypando/pando@sha256… no: a digest
func MovingTag(ref, version string) (tag string, reason string) {
	version = strings.TrimPrefix(version, "v")
	target := "trypando/pando:" + version
	repo, tagPart, digest := splitRef(ref)
	switch {
	case repo != Repository:
		return "", fmt.Sprintf("Pando is running from %s, which is not the published image %s, so Pando cannot tell whether a newer release replaces it. To upgrade in place, set the image to %s:latest where Pando is deployed.", ref, Repository, Repository)
	case digest != "":
		return "", fmt.Sprintf("Pando's image is pinned to a digest (%s), and a digest never moves, so the next time Pando's deployment is applied it would put this version back. Set the image to %s, or to a moving tag such as %s:latest, where Pando is deployed.", ref, target, Repository)
	}

	v := "v" + version
	switch {
	case tagPart == "latest":
		if semver.Prerelease(v) != "" {
			return "", fmt.Sprintf("%s is a release candidate, which latest never names. Set the image to %s where Pando is deployed to try it.", version, target)
		}
		return Repository + ":latest", ""
	case isMinorLine(tagPart):
		if semver.MajorMinor(v) == "v"+tagPart && semver.Prerelease(v) == "" {
			return Repository + ":" + tagPart, ""
		}
		return "", fmt.Sprintf("Pando's image is %s, which follows %s's patch releases only, and %s is not one of them. Set the image to %s:%s or %s:latest where Pando is deployed.", ref, tagPart, version, Repository, strings.TrimPrefix(semver.MajorMinor(v), "v"), Repository)
	case isMajor(tagPart):
		if semver.Major(v) == "v"+tagPart && semver.Prerelease(v) == "" {
			return Repository + ":" + tagPart, ""
		}
		return "", fmt.Sprintf("Pando's image is %s, which follows %s.x only, and %s is not one of them. Set the image to %s:latest or the new major version where Pando is deployed.", ref, tagPart, version, Repository)
	}
	return "", fmt.Sprintf("Pando's image is pinned to %s, an exact version, so the next time Pando's deployment is applied it would put this version back. Set the image to %s, or to a moving tag such as %s:latest so Pando can upgrade itself, where Pando is deployed.", ref, target, Repository)
}

// splitRef reads registry/repo:tag@digest, dropping Docker Hub's names for
// itself so docker.io/trypando/pando and trypando/pando compare equal.
func splitRef(ref string) (repo, tag, digest string) {
	if at := strings.Index(ref, "@"); at >= 0 {
		ref, digest = ref[:at], ref[at+1:]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref, tag = ref[:colon], ref[colon+1:]
	}
	for _, p := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		ref = strings.TrimPrefix(ref, p)
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return ref, tag, digest
}

func isMinorLine(tag string) bool {
	return strings.Count(tag, ".") == 1 && semver.IsValid("v"+tag) && semver.Prerelease("v"+tag) == ""
}

func isMajor(tag string) bool {
	return !strings.Contains(tag, ".") && semver.IsValid("v"+tag) && tag != "0"
}
