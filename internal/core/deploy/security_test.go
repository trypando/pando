package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// fakeSecurity answers a deploy's questions about scans as a test needs.
type fakeSecurity struct {
	scannedCommit string
	scannedImage  string
	scannedAt     time.Time
	findings      []api.Finding
	standing      security.Standing
	scans         []api.ScanRequest
	reusedFor     []string
}

func (f *fakeSecurity) Scan(_ context.Context, req api.ScanRequest, _ audit.Event) (state.Scan, error) {
	f.scans = append(f.scans, req)
	score := 90
	return state.Scan{Score: &score}, nil
}

func (f *fakeSecurity) Reuse(_ context.Context, _, specID, source, image string) (state.Scan, bool, error) {
	if source != "" && source == f.scannedCommit && image == f.scannedImage {
		f.reusedFor = append(f.reusedFor, specID)
		return state.Scan{SpecID: specID, Commit: source, Image: image, RanAt: f.scannedAt, Findings: f.findings}, true, nil
	}
	return state.Scan{}, false, nil
}

func (f *fakeSecurity) Allows(context.Context, string, string) (security.Standing, error) {
	return f.standing, nil
}

func (f *fakeSecurity) Configured() (string, bool) { return "scn_test", true }

// TestR312_ADeployOfAScannedCommitDoesNotScanItAgain asserts design 09 §4.1:
// the source and image are scanned once, not once per deploy. A deploy of a
// commit and image with a scan uses it and says so; a commit nobody scanned is
// scanned, with its commit and image recorded so the next deploy of them does
// not.
func TestR312_ADeployOfAScannedCommitDoesNotScanItAgain(t *testing.T) {
	built := "sha256:" + strings.Repeat("ab", 32)
	sec := &fakeSecurity{
		scannedCommit: "3f9a2c1d0000",
		scannedImage:  built,
		scannedAt:     time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		standing:      security.Standing{Verdict: security.VerdictOK},
	}
	r := &Runner{security: sec}
	dep := state.Deployment{AppID: "app_x", SpecID: "spec_1"}

	var log strings.Builder
	require.NoError(t, r.scan(context.Background(), dep, nil, built, "/src", "3f9a2c1d0000", &log))
	require.Empty(t, sec.scans, "the scan of that commit and image is reused")
	require.Equal(t, []string{"spec_1"}, sec.reusedFor, "and attached to the revision being deployed")
	require.Contains(t, log.String(), "Using the security scan of 3f9a2c1")
	require.Contains(t, log.String(), "(source and image unchanged)")

	log.Reset()
	require.NoError(t, r.scan(context.Background(), dep, nil, built, "/src", "def4560000", &log))
	require.Len(t, sec.scans, 1, "a commit nobody scanned is scanned")
	require.Equal(t, "def4560000", sec.scans[0].Commit)
	require.Equal(t, built, sec.scans[0].Image, "with the image the deploy built")
	require.Equal(t, "/src", sec.scans[0].SourceDir, "and the source it was built from")
	require.Contains(t, log.String(), "Scanning for known vulnerabilities")
}

// TestR311_ASourceScanDoesNotCoverTheBuiltImage asserts R-311 in the deploy
// path: a scan of the commit that read no image — detection's — is not reused
// by a deploy that built one. The deploy scans the image and the source.
func TestR311_ASourceScanDoesNotCoverTheBuiltImage(t *testing.T) {
	sec := &fakeSecurity{
		scannedCommit: "3f9a2c1d0000",
		standing:      security.Standing{Verdict: security.VerdictOK},
	}
	r := &Runner{security: sec}
	built := "sha256:" + strings.Repeat("ab", 32)

	var log strings.Builder
	dep := state.Deployment{AppID: "app_x", SpecID: "spec_1"}
	require.NoError(t, r.scan(context.Background(), dep, nil, built, "/src", "3f9a2c1d0000", &log))
	require.Empty(t, sec.reusedFor, "the source-only scan is not reused")
	require.Len(t, sec.scans, 1)
	require.Equal(t, built, sec.scans[0].Image)
	require.Equal(t, "/src", sec.scans[0].SourceDir)
	require.Contains(t, log.String(), "Scanning for known vulnerabilities")
}

// TestR314_AReusedScanStillFacesTheThreshold asserts R-314 holds when the scan
// is reused: skipping the scan never skips the decision, and the refusal names
// the findings that cost the most, as it does after a scan that ran (R-105).
func TestR314_AReusedScanStillFacesTheThreshold(t *testing.T) {
	score := 20
	built := "sha256:" + strings.Repeat("ab", 32)
	sec := &fakeSecurity{
		scannedCommit: "3f9a2c1d0000",
		scannedImage:  built,
		findings:      []api.Finding{{ID: "CVE-2026-0001", Severity: api.SeverityCritical, Title: "Remote code execution"}},
		standing:      security.Standing{Verdict: security.VerdictInsecure, Score: &score, Threshold: 60},
	}
	r := &Runner{security: sec}

	var log strings.Builder
	err := r.scan(context.Background(), state.Deployment{AppID: "app_x"}, nil, built, "/src", "3f9a2c1d0000", &log)
	require.Equal(t, errs.PlanSecurityBelowThreshold, errs.CodeOf(err))
	require.Empty(t, sec.scans)
	require.Contains(t, errs.As(err).Details, "findings", "the reused scan's findings travel with the refusal")
}

// TestR312_UnchangedImageIsNotRescanned asserts R-312 as amended by issue #84:
// a scan runs when what the app runs changes, not on every deploy. Redeploying
// an upload whose archive is unchanged — as a new revision, which every
// `pando deploy .` is — uses the scan of that archive and says so in words the
// person who uploaded it recognizes.
func TestR312_UnchangedImageIsNotRescanned(t *testing.T) {
	archive := "sha256:" + strings.Repeat("ab", 32)
	built := "sha256:" + strings.Repeat("ef", 32)
	sec := &fakeSecurity{
		scannedCommit: archive,
		scannedImage:  built,
		scannedAt:     time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC),
		standing:      security.Standing{Verdict: security.VerdictOK},
	}
	r := &Runner{security: sec}
	upload := &spec.AppSpec{Source: spec.Source{Type: spec.SourceUpload, UploadID: "app_x"}}

	for _, specID := range []string{"spec_2", "spec_3"} {
		var log strings.Builder
		dep := state.Deployment{AppID: "app_x", SpecID: specID}
		require.NoError(t, r.scan(context.Background(), dep, upload, built, "/src", archive, &log))
		require.Contains(t, log.String(),
			"=> Using the security scan of the uploaded source from 2026-09-29T08:00:00Z (source and image unchanged)")
		require.NotContains(t, log.String(), "Scanning for known vulnerabilities")
	}
	require.Empty(t, sec.scans, "neither deploy ran the scanner")
	require.Equal(t, []string{"spec_2", "spec_3"}, sec.reusedFor, "each revision gets the scan that describes it")

	var log strings.Builder
	dep := state.Deployment{AppID: "app_x", SpecID: "spec_4"}
	require.NoError(t, r.scan(context.Background(), dep, upload, built, "/src", "sha256:"+strings.Repeat("cd", 32), &log))
	require.Len(t, sec.scans, 1, "a new archive is scanned")
	require.Contains(t, log.String(), "Scanning for known vulnerabilities")
}
