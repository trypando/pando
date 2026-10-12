//go:build integration

package deploy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/detection"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
)

// publishedProbe answers detection's tier-1 question (R-094) with one image.
type publishedProbe struct{ ref string }

func (p publishedProbe) Published(context.Context, spec.Source) ([]detect.PublishedImage, error) {
	return []detect.PublishedImage{{Ref: p.ref, Registry: "test"}}, nil
}

// runDetection runs real detection for the harness's app: the checkout, the
// auction or the registry probe, and the scan it takes before anything is
// built.
func (h *scanHarness) runDetection(probe detect.RegistryProbe, images *oci.Images) {
	h.t.Helper()
	job := &detect.Job{Auction: detect.NewAuction(detect.StaticDetector{}, detect.DockerfileDetector{})}
	if probe != nil {
		job.Registry = probe
	}
	if images != nil {
		job.Images = images
	}
	runner := &detection.Runner{
		Apps:       h.apps,
		Detections: state.NewDetections(h.db),
		Sources:    source.Sources{WorkDir: h.t.TempDir()},
		Job:        job,
		Scanner:    security.DetectionScanner{Service: h.service},
	}
	_, err := runner.Detect(context.Background(), h.app.ID)
	require.NoError(h.t, err)
}

// TestR311_APlanDoesNotScoreAnImageNotYetBuilt asserts R-311 for the plan of an
// app Pando builds from source on an install where detection builds nothing
// (no builder to build with; TestR311_APlanScoresTheImageDetectionBuilt is the
// ordinary case). Detection scans the repository alone, and that number is the
// source's: the 2048 plan showed 100 / 100 and the deploy, scanning the image
// it built, 61. The plan, the app's row and the app's report during the first
// deploy carry the source's findings and no score for the app, saying it waits
// for the image; the scan's own number is still there, labeled as the
// source's. The deploy's scan of the image is the app's score.
func TestR311_APlanDoesNotScoreAnImageNotYetBuilt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	h.runDetection(nil, nil)
	scans := h.scanner.requests()
	require.Len(t, scans, 1, "detection scanned the source")
	require.Empty(t, scans[0].Image, "and built nothing to scan")

	plan, err := h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.NotNil(t, plan.Scan)
	require.True(t, plan.AwaitingImage, "the plan says the score waits for the image")
	require.Nil(t, plan.Standing.Score, "and does not present the source's number as the app's")
	require.Equal(t, security.ScopeSource, plan.Scope)
	require.Equal(t, 100, *plan.Scan.Score, "the source scan's own number is kept, as the source's")

	row, found, err := h.apps.ByID(ctx, h.app.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Nil(t, row.SecurityScore, "the app's row has no score yet")
	require.True(t, row.SecurityAwaitingImage)

	all, err := h.apps.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Nil(t, all[0].SecurityScore, "nor does the apps list")
	require.True(t, all[0].SecurityAwaitingImage)

	// Accepted, and the first deploy under way: the revision is pinned and
	// the image not scanned yet. The overview reads the same.
	accepted, err := h.apps.CreateRevision(ctx, h.app.ID, h.spec, spec.OriginDetected, h.owner)
	require.NoError(t, err)
	require.NoError(t, h.apps.Pin(ctx, h.app.ID, accepted.ID, state.StateProposed, h.owner))
	deploying, err := h.service.Report(ctx, h.app.ID, accepted.ID)
	require.NoError(t, err)
	require.True(t, deploying.AwaitingImage)
	require.Nil(t, deploying.Standing.Score)

	rev, err := h.deploy()
	require.NoError(t, err)
	deployed, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.False(t, deployed.AwaitingImage)
	require.Equal(t, security.ScopeImageAndSource, deployed.Scope)
	require.NotNil(t, deployed.Standing.Score, "the image's scan is the app's score")
	require.Less(t, *deployed.Standing.Score, 100)

	row, _, err = h.apps.ByID(ctx, h.app.ID)
	require.NoError(t, err)
	require.False(t, row.SecurityAwaitingImage)
	require.Equal(t, *deployed.Standing.Score, *row.SecurityScore)
}

// TestR311_APlanScoresThePublishedImageItWillRun asserts R-311 for the plan of
// a repository whose maintainer publishes its image (R-094 tier 1), as
// bastienwirtz/homer does with ghcr.io/bastienwirtz/homer. The image exists
// before anything is deployed, so detection scans it by the digest its tag
// names now, with the repository, and the plan shows the score of what will
// run. The deploy of that same digest reuses the scan rather than taking it
// again (R-312).
func TestR311_APlanScoresThePublishedImageItWillRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	tag := strings.TrimPrefix(srv.URL, "http://") + "/bastienwirtz/homer:latest"
	digest := publish(t, tag)
	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	h.runner.WithImages(images)

	h.runDetection(publishedProbe{ref: tag}, images)
	scans := h.scanner.requests()
	require.Len(t, scans, 1)
	pinned := oci.Pin(tag, digest)
	require.Equal(t, pinned, scans[0].Image, "the published image, by digest")
	require.NotEmpty(t, scans[0].SourceDir, "with the repository")
	require.Equal(t, h.commit, scans[0].Commit)

	plan, err := h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.False(t, plan.AwaitingImage)
	require.Equal(t, security.ScopeImageAndSource, plan.Scope)
	require.NotNil(t, plan.Standing.Score, "the plan shows the score of the image that will run")
	require.Equal(t, 2, plan.Counts.Critical)

	row, _, err := h.apps.ByID(ctx, h.app.ID)
	require.NoError(t, err)
	require.Equal(t, *plan.Standing.Score, *row.SecurityScore)

	h.spec.Build = spec.Build{Strategy: spec.BuildPrebuilt}
	h.spec.Workloads[0].Image = tag
	rev, err := h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 1, "the deploy of the same digest reuses the plan's scan")

	deployed, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.Equal(t, plan.Scan.ID, deployed.Scan.ReusedFrom)
	require.Equal(t, *plan.Standing.Score, *deployed.Standing.Score, "the plan's number is the app's")
}

// TestR311_AnImageAppsPlanIsScoredByItsImage asserts R-311 for the plan of an
// app added as an image: detection pins the digest, scans it, and the deploy
// of that digest reuses the scan.
func TestR311_AnImageAppsPlanIsScoredByItsImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	tag := strings.TrimPrefix(srv.URL, "http://") + "/acme/web:1"
	digest := publish(t, tag)
	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	h.runner.WithImages(images)

	app, err := h.apps.Create(ctx, "web", "web", h.owner, h.owner, spec.Source{Type: spec.SourceImage, Image: tag})
	require.NoError(t, err)
	h.app = app
	h.runDetection(nil, images)

	scans := h.scanner.requests()
	require.Len(t, scans, 1, "an image app's plan is scanned, though it has no checkout")
	require.Equal(t, oci.Pin(tag, digest), scans[0].Image)
	require.Equal(t, digest, scans[0].Commit)
	require.Empty(t, scans[0].SourceDir)

	plan, err := h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.Equal(t, security.ScopeImage, plan.Scope)
	require.NotNil(t, plan.Standing.Score)

	h.spec.AppID = app.ID
	h.spec.Source = spec.Source{Type: spec.SourceImage, Image: tag, Digest: digest}
	h.spec.Build = spec.Build{Strategy: spec.BuildPrebuilt}
	h.spec.Workloads[0].Image = tag
	_, err = h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 1, "the deploy of the same digest reuses the plan's scan")
}
