//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

func score(n int) *int { return &n }

// TestR312_AcceptingAProposalDoesNotHideTheScanTakenAtDiscovery asserts the
// fallback that makes "scanned at discovery" mean anything.
//
// Detection scans the checkout before a spec revision exists, so that scan
// belongs to no revision. Accepting the proposal pins one — and a lookup that
// insisted on the pinned revision answered "this app has not been scanned yet"
// a minute after scanning the very source the revision was written from.
func TestR312_AcceptingAProposalDoesNotHideTheScanTakenAtDiscovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	scans := state.NewScans(db)

	app, err := apps.Create(ctx, "crewmate", "crewmate", alice.ID, alice.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/crewmate"})
	require.NoError(t, err)

	// What detection records: a score, and no revision to attach it to.
	atDiscovery, err := scans.Record(ctx, state.Scan{
		AppID: app.ID, ScannerRef: "scn_trivy", Score: score(64),
		Findings: []api.Finding{{ID: "CVE-1", Severity: api.SeverityHigh}},
	})
	require.NoError(t, err)
	require.Empty(t, atDiscovery.SpecID)

	rev, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginDetected, alice.ID)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateProposed, alice.ID))

	found, ok, err := scans.Latest(ctx, app.ID, rev.ID)
	require.NoError(t, err)
	require.True(t, ok, "the scan taken at discovery still describes this app")
	require.Equal(t, atDiscovery.ID, found.ID)
	require.Empty(t, found.SpecID, "and says it belongs to no revision")

	// A scan of the revision itself wins over it, because it describes what is
	// actually deployed.
	ofRevision, err := scans.Record(ctx, state.Scan{
		AppID: app.ID, SpecID: rev.ID, ScannerRef: "scn_trivy", Score: score(80),
	})
	require.NoError(t, err)

	found, ok, err = scans.Latest(ctx, app.ID, rev.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ofRevision.ID, found.ID, "a scan of this revision beats one of none")

	// And another revision's scan is not this revision's, however new it is.
	other, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginManual, alice.ID)
	require.NoError(t, err)
	_, err = scans.Record(ctx, state.Scan{
		AppID: app.ID, SpecID: other.ID, ScannerRef: "scn_trivy", Score: score(10),
	})
	require.NoError(t, err)

	found, ok, err = scans.Latest(ctx, app.ID, rev.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ofRevision.ID, found.ID, "a score for a spec this app is not running is not its score")
}

// TestR312_ADeployOfAScannedCommitUsesThatScan asserts the lookup a deploy
// makes before scanning: a successful scan of the commit it is deploying is
// found, whoever took it; a failed one, another commit's, or one of no known
// commit is not — so the source is scanned once, not once per deploy.
func TestR312_ADeployOfAScannedCommitUsesThatScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	app, err := state.NewApps(db).Create(ctx, "crew", "crew", alice.ID, alice.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/crew"})
	require.NoError(t, err)
	scans := state.NewScans(db)

	_, found, err := scans.ForSource(ctx, app.ID, "abc123", "", "scn")
	require.NoError(t, err)
	require.False(t, found, "never scanned")

	_, err = scans.Record(ctx, state.Scan{AppID: app.ID, Commit: "abc123", ScannerRef: "scn", Error: "trivy is not installed"})
	require.NoError(t, err)
	_, err = scans.Record(ctx, state.Scan{AppID: app.ID, ScannerRef: "scn", Score: score(90)})
	require.NoError(t, err)
	_, found, err = scans.ForSource(ctx, app.ID, "abc123", "", "scn")
	require.NoError(t, err)
	require.False(t, found, "a failed scan, and one of no known commit, cover nothing")

	atDiscovery, err := scans.Record(ctx, state.Scan{AppID: app.ID, Commit: "abc123", ScannerRef: "scn", Score: score(72)})
	require.NoError(t, err)
	got, found, err := scans.ForSource(ctx, app.ID, "abc123", "", "scn")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, atDiscovery.ID, got.ID)
	require.Equal(t, 72, *got.Score)

	_, found, err = scans.ForSource(ctx, app.ID, "def456", "", "scn")
	require.NoError(t, err)
	require.False(t, found, "a new commit is scanned")

	_, found, err = scans.ForSource(ctx, app.ID, "abc123", "", "scn_other")
	require.NoError(t, err)
	require.False(t, found, "another scanner's findings are not this scanner's (issue #84)")

	// R-311: the scan above read the source alone. A deploy that built an
	// image is not covered by it, and a scan of one image does not cover
	// another built from the same commit.
	built := "sha256:" + strings.Repeat("a", 64)
	_, found, err = scans.ForSource(ctx, app.ID, "abc123", built, "scn")
	require.NoError(t, err)
	require.False(t, found, "a scan of the source alone does not cover the image built from it")

	ofImage, err := scans.Record(ctx, state.Scan{AppID: app.ID, Commit: "abc123", Image: built, ScannerRef: "scn", Score: score(40)})
	require.NoError(t, err)
	got, found, err = scans.ForSource(ctx, app.ID, "abc123", built, "scn")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ofImage.ID, got.ID)
	require.Equal(t, built, got.Image)

	_, found, err = scans.ForSource(ctx, app.ID, "abc123", "sha256:"+strings.Repeat("b", 64), "scn")
	require.NoError(t, err)
	require.False(t, found, "another image from the same commit is scanned")

	got, found, err = scans.ForSource(ctx, app.ID, "abc123", "", "scn")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, atDiscovery.ID, got.ID, "a deploy with no image is still covered by the source scan")
}

// TestR312_AReusedScanDescribesTheNewRevision asserts the other half of reuse
// (issue #84): a new revision of an unchanged source is scored by the scan of
// that source, so the threshold and the console see a scanned revision — and
// the app's history still shows one scan, not one per deploy.
func TestR312_AReusedScanDescribesTheNewRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	scans := state.NewScans(db)
	app, err := apps.Create(ctx, "rota", "rota", alice.ID, alice.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/rota"})
	require.NoError(t, err)

	first, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginDetected, alice.ID)
	require.NoError(t, err)
	ran, err := scans.Record(ctx, state.Scan{
		AppID: app.ID, SpecID: first.ID, Commit: "abc123", ScannerRef: "scn", Score: score(64),
		Findings: []api.Finding{{ID: "CVE-1", Severity: api.SeverityHigh}},
	})
	require.NoError(t, err)

	second, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginEdited, alice.ID)
	require.NoError(t, err)
	_, found, err := scans.Latest(ctx, app.ID, second.ID)
	require.NoError(t, err)
	require.False(t, found, "before the reuse, the new revision has no scan")

	from, found, err := scans.ForSource(ctx, app.ID, "abc123", "", "scn")
	require.NoError(t, err)
	require.True(t, found)
	reused, err := scans.Reuse(ctx, from, second.ID)
	require.NoError(t, err)

	got, found, err := scans.Latest(ctx, app.ID, second.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, reused.ID, got.ID)
	require.Equal(t, ran.ID, got.ReusedFrom)
	require.Equal(t, ran.ID, got.Origin())
	require.Equal(t, 64, *got.Score)
	require.Len(t, got.Findings, 1)
	require.WithinDuration(t, ran.RanAt, got.RanAt, time.Millisecond,
		"findings are as old as the scan that found them")

	// A copy of a copy names the scan that ran.
	third, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginEdited, alice.ID)
	require.NoError(t, err)
	again, err := scans.Reuse(ctx, got, third.ID)
	require.NoError(t, err)
	require.Equal(t, ran.ID, again.ReusedFrom)

	history, err := scans.History(ctx, app.ID, 0)
	require.NoError(t, err)
	require.Len(t, history, 1, "one scan ran, however many revisions it describes")
	require.Equal(t, ran.ID, history[0].ID)
}
