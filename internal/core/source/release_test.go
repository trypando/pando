package source_test

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
)

func tags(names ...string) []*plumbing.Reference {
	var out []*plumbing.Reference
	for i, n := range names {
		out = append(out, plumbing.NewHashReference(plumbing.NewTagReferenceName(n),
			plumbing.NewHash(string(rune('a'+i))+"000000000000000000000000000000000000000")))
	}
	return out
}

// TestR141_ReleaseTriggerCountsStableSemverTagsByDefault asserts that, with no
// pattern, a release is a stable semantic version, compared as one: v1.10.0 is
// newer than v1.9.0, and a pre-release does not count (O-58).
func TestR141_ReleaseTriggerCountsStableSemverTagsByDefault(t *testing.T) {
	got := source.NewestRelease(tags("v1.9.0", "v1.10.0", "v2.0.0-rc.1", "nightly", "1.2.3"), "")
	require.Equal(t, "refs/tags/v1.10.0", got.Ref)

	require.Empty(t, source.NewestRelease(tags("nightly", "latest"), "").Ref,
		"a repository that does not tag releases has none, rather than its newest tag")
}

// TestR141_ReleaseTriggerFollowsAPattern asserts that a tag pattern decides
// what counts as a release, for the projects that do not use semver (O-58).
func TestR141_ReleaseTriggerFollowsAPattern(t *testing.T) {
	got := source.NewestRelease(tags("release-9", "release-10", "v3.0.0", "release-2"), "release-*")
	require.Equal(t, "refs/tags/release-10", got.Ref)

	got = source.NewestRelease(tags("2026.9.30", "2026.10.1"), "2026.*")
	require.Equal(t, "refs/tags/2026.10.1", got.Ref)

	got = source.NewestRelease(tags("v2.0.0-rc.1", "v1.0.0"), "v*")
	require.Equal(t, "refs/tags/v2.0.0-rc.1", got.Ref, "a pattern can admit pre-releases")
}

// An annotated tag is an object of its own; what deploys is the commit it
// names, which the peeled ref carries.
func TestNewestReleaseUsesThePeeledCommitOfAnAnnotatedTag(t *testing.T) {
	commit := plumbing.NewHash("1111111111111111111111111111111111111111")
	refs := []*plumbing.Reference{
		plumbing.NewHashReference("refs/tags/v1.0.0", plumbing.NewHash("2222222222222222222222222222222222222222")),
		plumbing.NewHashReference("refs/tags/v1.0.0^{}", commit),
	}
	got := source.NewestRelease(refs, "")
	require.Equal(t, "refs/tags/v1.0.0", got.Ref)
	require.Equal(t, commit.String(), got.Commit)
}

// TestR141_BranchTriggerFollowsTheChosenBranch asserts that AutoDeploy.Branch
// is honored, and the ref the app was deployed from is the fallback.
func TestR141_BranchTriggerFollowsTheChosenBranch(t *testing.T) {
	dir, shas := repo(t, map[string]string{"main.go": "package main"})
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := r.Head()
	require.NoError(t, err)
	branch := head.Name().Short()

	// A second branch, one commit ahead.
	wt, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{Branch: "refs/heads/staging", Create: true}))
	staging, err := wt.Commit("staging", &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "Test", Email: "t@example", When: time.Unix(1700000100, 0)},
	})
	require.NoError(t, err)

	src := spec.Source{Type: spec.SourceGit, URL: dir, Ref: branch}

	got, err := source.Sources{}.ResolveTracked(ctx(), src, spec.AutoDeploy{Enabled: true})
	require.NoError(t, err)
	require.Equal(t, shas[0], got.Commit, "no branch named: the one the app was deployed from")

	got, err = source.Sources{}.ResolveTracked(ctx(), src, spec.AutoDeploy{Enabled: true, Branch: "staging"})
	require.NoError(t, err)
	require.Equal(t, staging.String(), got.Commit)
	require.Equal(t, "refs/heads/staging", got.Ref)
}

func TestResolveTrackedFindsAnAnnotatedReleaseTagsCommit(t *testing.T) {
	dir, shas := repo(t, map[string]string{"main.go": "package main"})
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	_, err = r.CreateTag("v1.0.0", plumbing.NewHash(shas[0]), &git.CreateTagOptions{
		Message: "first", Tagger: &object.Signature{Name: "Test", Email: "t@example", When: time.Unix(1700000100, 0)},
	})
	require.NoError(t, err)

	got, err := source.Sources{}.ResolveTracked(ctx(), spec.Source{Type: spec.SourceGit, URL: dir, Ref: "main"},
		spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerReleaseTagged})
	require.NoError(t, err)
	require.Equal(t, "refs/tags/v1.0.0", got.Ref)
	require.Equal(t, shas[0], got.Commit, "the commit, not the tag object")
}
