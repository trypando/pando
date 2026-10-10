package update_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/update"
)

const noAdvisories = "### Security\n\nNo new advisories.\n\n### Added\n\n- Something (#1).\n"

// A release list as GitHub returns it: newest first, a draft and a release
// candidate among the releases, one release fixing an advisory.
func releaseList() []map[string]any {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return []map[string]any{
		{"tag_name": "v0.5.0-rc.1", "prerelease": true, "published_at": at, "body": noAdvisories, "html_url": "https://github.com/trypando/pando/releases/tag/v0.5.0-rc.1"},
		{"tag_name": "v0.6.0", "draft": true, "published_at": at, "body": noAdvisories},
		{"tag_name": "v0.4.1", "published_at": at, "body": "### Security\n\n- GHSA-xxxx-yyyy-zzzz (high): a forged header reached apps. Upgrade.\n\n### Fixed\n\n- x\n"},
		{"tag_name": "v0.4.0", "published_at": at.Add(-48 * time.Hour), "body": noAdvisories},
		{"tag_name": "v0.3.1", "published_at": at.Add(-96 * time.Hour), "body": noAdvisories},
		{"tag_name": "v0.3.0", "published_at": at.Add(-200 * time.Hour), "body": noAdvisories},
	}
}

func github(t *testing.T, hits *atomic.Int32) *update.GitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		require.Equal(t, "pando/0.3.1", r.Header.Get("User-Agent"))
		require.NoError(t, json.NewEncoder(w).Encode(releaseList()))
	}))
	t.Cleanup(srv.Close)
	g := update.NewGitHub("0.3.1")
	g.URL = srv.URL
	return g
}

func checker(src update.Source, current string, s *update.Settings) *update.Checker {
	return &update.Checker{
		Source:   src,
		Settings: func(context.Context) (update.Settings, error) { return *s, nil },
		Current:  current,
		Install:  update.Install{Kind: update.InstallContainer},
		Clock:    clock.NewFake(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)),
		Logger:   zap.NewNop(),
	}
}

// TestR349_TheUpdateCheckIsOnByDefaultAndOffSendsNothing asserts R-349.
func TestR349_TheUpdateCheckIsOnByDefaultAndOffSendsNothing(t *testing.T) {
	require.True(t, update.SettingsFrom(policy.Default()).Enabled, "on unless policy turns it off")

	var hits atomic.Int32
	s := update.SettingsFrom(policy.Document{DisableUpdateCheck: true})
	c := checker(github(t, &hits), "0.3.1", &s)
	c.Check(context.Background())
	require.Zero(t, hits.Load(), "a disabled check sends no request at all")

	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.False(t, st.Enabled)
	require.False(t, st.Available)
	require.Nil(t, st.CheckedAt)

	s.Enabled = true
	c.Check(context.Background())
	require.EqualValues(t, 1, hits.Load())
}

// TestR350_TheChannelDecidesWhetherReleaseCandidatesAreOffered asserts R-350.
func TestR350_TheChannelDecidesWhetherReleaseCandidatesAreOffered(t *testing.T) {
	var hits atomic.Int32
	s := update.SettingsFrom(policy.Document{})
	c := checker(github(t, &hits), "0.3.1", &s)
	c.Check(context.Background())

	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, policy.UpdateChannelStable, st.Channel)
	require.Equal(t, "0.4.1", st.Latest, "stable skips the release candidate, and drafts are never offered")

	// Changing the channel needs no new request.
	s.Channel = policy.UpdateChannelPrerelease
	st, err = c.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "0.5.0-rc.1", st.Latest)
	require.EqualValues(t, 1, hits.Load())

	require.Error(t, policy.Document{UpdateChannel: "nightly"}.ValidateRules())
}

// TestR351_StatusListsEachVersionBetweenWithSecurityAndBreakingMarked asserts R-351.
func TestR351_StatusListsEachVersionBetweenWithSecurityAndBreakingMarked(t *testing.T) {
	var hits atomic.Int32
	s := update.SettingsFrom(policy.Document{})
	c := checker(github(t, &hits), "v0.3.1", &s)
	c.Check(context.Background())

	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "0.3.1", st.Current)
	require.True(t, st.Available)
	require.NotNil(t, st.CheckedAt)

	var versions []string
	for _, r := range st.Releases {
		versions = append(versions, r.Version)
	}
	require.Equal(t, []string{"0.4.1", "0.4.0"}, versions, "newest first, ending after the running version")

	require.True(t, st.Releases[0].Security, "0.4.1's Security section names an advisory")
	require.False(t, st.Releases[1].Security, `"No new advisories." is not one`)
	require.True(t, st.Security)

	require.False(t, st.Releases[0].Breaking, "0.4.0 to 0.4.1 is a patch")
	require.True(t, st.Releases[1].Breaking, "0.3.1 to 0.4.0 is a MINOR bump before 1.0")
	require.True(t, st.Breaking)
	require.Contains(t, st.Releases[1].Notes, "### Security")

	// Up to date is not available, and lists nothing.
	c.Current = "0.4.1"
	st, err = c.Status(context.Background())
	require.NoError(t, err)
	require.False(t, st.Available)
	require.Empty(t, st.Releases)

	// A development build is told the latest, never that it is behind.
	c.Current = "dev"
	st, err = c.Status(context.Background())
	require.NoError(t, err)
	require.True(t, st.Development)
	require.Equal(t, "0.4.1", st.Latest)
	require.False(t, st.Available)
}

func TestBreaksFollowsTheReleasingRules(t *testing.T) {
	require.True(t, update.Breaks("1.4.2", "2.0.0"))
	require.False(t, update.Breaks("1.4.2", "1.5.0"), "after 1.0 MINOR adds")
	require.True(t, update.Breaks("0.3.1", "0.4.0"), "before 1.0 MINOR breaks")
	require.False(t, update.Breaks("0.3.0", "0.3.1"))
}

// An unreachable GitHub is reported, with what to do, beside what an earlier
// check found.
func TestAFailedCheckSaysWhatToDoAndKeepsWhatItKnew(t *testing.T) {
	var hits atomic.Int32
	g := github(t, &hits)
	s := update.SettingsFrom(policy.Document{})
	c := checker(g, "0.3.1", &s)
	c.Check(context.Background())

	g.URL = "http://127.0.0.1:1/unreachable"
	c.Check(context.Background())
	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Contains(t, st.Error, "disable_update_check")
	require.Equal(t, "0.4.1", st.Latest)
}

// TestR352_AnAvailableUpdateSaysHowToUpgradeThisInstall asserts R-352.
func TestR352_AnAvailableUpdateSaysHowToUpgradeThisInstall(t *testing.T) {
	var hits atomic.Int32
	s := update.SettingsFrom(policy.Document{})
	c := checker(github(t, &hits), "0.3.1", &s)
	c.Check(context.Background())
	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.NotNil(t, st.Upgrade)
	require.Equal(t, "trypando/pando:0.4.1", st.Upgrade.Image)
	require.Contains(t, st.Upgrade.Command, "/releases/download/v0.4.1/docker-compose.yml")
	require.Contains(t, st.Upgrade.Command, "/releases/download/v0.4.1/buildkit-seccomp.json",
		"the compose file names BuildKit's seccomp profile, so an upgrade fetches both")
	require.Contains(t, st.Upgrade.Command, "docker compose up -d")
	require.Contains(t, st.Upgrade.Instructions, "infrastructure-as-code")

	bin := update.Install{Kind: update.InstallBinary}.Upgrade("0.4.1")
	require.Empty(t, bin.Image)
	require.Contains(t, bin.Command, "pando_0.4.1_linux_")
}

func TestSkewIsMajorOrMinor(t *testing.T) {
	require.True(t, update.Skewed("0.3.1", "0.4.0"))
	require.False(t, update.Skewed("0.3.0", "0.3.1"))
	require.False(t, update.Skewed("dev", "0.4.0"), "a development build has nothing to compare")
}
