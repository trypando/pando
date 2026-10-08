package source

import (
	"context"
	"path"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Tracked is what an auto-deploy trigger currently points at: the ref it
// found and the commit under it. Both empty when there is nothing to deploy.
type Tracked struct {
	// Ref is fully qualified: refs/heads/main or refs/tags/v1.2.0. It goes in
	// the revision's Source.Ref, so the revision says what it was cut from.
	Ref    string
	Commit string
}

// ResolveTracked answers "what would auto-deploy deploy now" for an app's
// source and its auto-deploy settings (R-141), without cloning.
//
// The branch trigger follows AutoDeploy.Branch, or the ref the app was
// deployed from when that is empty. The release trigger follows the newest tag
// that counts as a release (NewestRelease).
//
// With the credential the app's source connection gives, as every other read
// of the repository is (issue #127), so a private repository auto-deploys.
func (s Sources) ResolveTracked(ctx context.Context, src spec.Source, ad spec.AutoDeploy) (Tracked, error) {
	if src.Type != spec.SourceGit || src.URL == "" {
		return Tracked{}, nil
	}

	acc, err := s.access(ctx, src, PurposeCheck)
	if err != nil {
		return Tracked{}, err
	}
	refs, err := listRefs(ctx, acc)
	if err != nil {
		if denied := accessError(err, src.URL, acc.connection); denied != nil {
			return Tracked{}, denied
		}
		return Tracked{}, errs.Wrap(errs.ValidInvalid,
			"Pando could not reach this app's source to check for new commits.", err).
			WithDetail("url", src.URL)
	}

	if ad.Trigger == spec.TriggerReleaseTagged {
		return NewestRelease(refs, ad.TagPattern), nil
	}

	branch := ad.Branch
	if branch == "" {
		branch = src.Ref
	}
	if branch == "" {
		return Tracked{}, nil
	}
	wanted := referenceFor(branch)
	for _, ref := range refs {
		if ref.Name() == wanted {
			return Tracked{Ref: wanted.String(), Commit: ref.Hash().String()}, nil
		}
	}
	return Tracked{}, nil
}

// stableSemver is a release when no pattern is given: v1.2.3 or 1.2.3, and
// nothing after the patch number. A pre-release (v2.0.0-rc.1) is not one: an
// app that deploys every release candidate has asked for that with a pattern.
var stableSemver = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// NewestRelease picks the newest release tag from a remote's refs.
//
// With no pattern, a release is a stable semantic version tag. With one, it is
// any tag whose name matches the pattern as a glob (path.Match: `release-*`,
// `v*`), because plenty of projects tag releases without following semver
// (issue #40).
//
// Newest is by version order, not by date: a remote's ref list carries no
// dates, and reading them would mean cloning. Names are compared a run of
// digits at a time as numbers, so release-10 is newer than release-9 and
// 2026.10.1 is newer than 2026.9.30.
func NewestRelease(refs []*plumbing.Reference, pattern string) Tracked {
	var best string
	commits := map[string]string{}
	for _, ref := range refs {
		name := ref.Name()
		if !name.IsTag() {
			continue
		}
		short := name.Short()
		peeled := strings.HasSuffix(short, "^{}")
		short = strings.TrimSuffix(short, "^{}")
		if !isRelease(short, pattern) {
			continue
		}
		// The peeled entry, when there is one, is the commit an annotated
		// tag names, and wins over the tag object's own hash.
		if _, seen := commits[short]; !seen || peeled {
			commits[short] = ref.Hash().String()
		}
		if best == "" || versionLess(best, short) {
			best = short
		}
	}
	if best == "" {
		return Tracked{}
	}
	return Tracked{Ref: plumbing.NewTagReferenceName(best).String(), Commit: commits[best]}
}

func isRelease(tag, pattern string) bool {
	if pattern == "" {
		return stableSemver.MatchString(tag)
	}
	ok, err := path.Match(pattern, tag)
	return err == nil && ok
}

// versionLess orders tag names the way people number releases: runs of
// digits compare as numbers, everything else as text.
func versionLess(a, b string) bool {
	for a != "" && b != "" {
		ca, ra := chunk(a)
		cb, rb := chunk(b)
		if ca != cb {
			da, db := isDigits(ca), isDigits(cb)
			switch {
			case da && db:
				na, nb := strings.TrimLeft(ca, "0"), strings.TrimLeft(cb, "0")
				if len(na) != len(nb) {
					return len(na) < len(nb)
				}
				if na != nb {
					return na < nb
				}
			default:
				return ca < cb
			}
		}
		a, b = ra, rb
	}
	return len(a) < len(b)
}

// chunk splits off the leading run of digits, or of non-digits.
func chunk(s string) (string, string) {
	digits := s[0] >= '0' && s[0] <= '9'
	i := 1
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') == digits {
		i++
	}
	return s[:i], s[i:]
}

func isDigits(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }
