package detection

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/secret"
)

// fakeImages answers registry questions for one digest, or refuses them all.
type fakeImages struct {
	digest string
	down   bool
}

func (f fakeImages) Inspect(context.Context, string, string, oci.Platform) (oci.Inspection, error) {
	if f.down {
		return oci.Inspection{}, errors.New("registry unreachable")
	}
	return oci.Inspection{Digest: f.digest}, nil
}

func (f fakeImages) Auth(context.Context, string, string) (*oci.Auth, error) {
	if f.down {
		return nil, nil
	}
	return &oci.Auth{Registry: "ghcr.io", Username: "bot", Password: secret.New("pw")}, nil
}

func prebuiltProposal(image string) *detect.Proposal {
	return &detect.Proposal{DraftSpec: spec.AppSpec{
		Build:     spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{Name: "web", Primary: true, Image: image}},
	}}
}

// TestR311_DetectionScansWhatTheAppWillRun asserts what detection hands the
// scanner (R-311): a published image by digest with its checkout, an image
// app's pinned digest, and the source alone for an app Pando builds, whose
// image does not exist yet.
func TestR311_DetectionScansWhatTheAppWillRun(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("c", 64)
	git := spec.Source{Type: spec.SourceGit, URL: "https://github.com/bastienwirtz/homer"}
	checkout := &source.Checkout{Dir: "/tmp/checkout", Commit: "3f9a2c1"}

	r := &Runner{Job: &detect.Job{Images: fakeImages{digest: digest}}}
	req, ok := r.scanRequest(ctx, "app_1", git, prebuiltProposal("ghcr.io/bastienwirtz/homer:latest"), checkout)
	require.True(t, ok)
	require.Equal(t, "ghcr.io/bastienwirtz/homer@"+digest, req.Image, "pinned the way a deploy pins it")
	require.Equal(t, "3f9a2c1", req.Commit)
	require.Equal(t, "/tmp/checkout", req.SourceDir)
	require.NotNil(t, req.PullAuth, "with the app's registry credential")

	pinned := "ghcr.io/bastienwirtz/homer@" + digest
	req, ok = r.scanRequest(ctx, "app_1", git, prebuiltProposal(pinned), checkout)
	require.True(t, ok)
	require.Equal(t, pinned, req.Image, "a reference already pinned is used as it is")

	down := &Runner{Job: &detect.Job{Images: fakeImages{down: true}}}
	req, ok = down.scanRequest(ctx, "app_1", git, prebuiltProposal("ghcr.io/bastienwirtz/homer:latest"), checkout)
	require.True(t, ok, "a registry that cannot be read leaves the source to scan")
	require.Empty(t, req.Image, "never the tag in place of a digest")
	require.Nil(t, req.PullAuth)

	noRegistry := &Runner{}
	req, ok = noRegistry.scanRequest(ctx, "app_1", git, prebuiltProposal("ghcr.io/bastienwirtz/homer:latest"), checkout)
	require.True(t, ok, "with no way to read a registry, the source is still scanned")
	require.Empty(t, req.Image)
	req, _ = noRegistry.scanRequest(ctx, "app_1", git, prebuiltProposal(pinned), checkout)
	require.Equal(t, pinned, req.Image, "a pinned reference needs no registry to name it")
	require.Nil(t, req.PullAuth, "and has no credential to send")

	noPrimary := &detect.Proposal{DraftSpec: spec.AppSpec{Build: spec.Build{Strategy: spec.BuildPrebuilt}}}
	req, ok = r.scanRequest(ctx, "app_1", git, noPrimary, checkout)
	require.True(t, ok)
	require.Empty(t, req.Image, "a prebuilt plan with no workload names no image")

	built := &detect.Proposal{DraftSpec: spec.AppSpec{Build: spec.Build{Strategy: spec.BuildDockerfile}}}
	req, ok = (&Runner{}).scanRequest(ctx, "app_1", git, built, checkout)
	require.True(t, ok)
	require.Empty(t, req.Image, "an image Pando builds is not built to be scanned")

	image := spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}
	app := &detect.Proposal{DraftSpec: spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1", Digest: digest}}}
	req, ok = r.scanRequest(ctx, "app_2", image, app, &source.Checkout{})
	require.True(t, ok)
	require.Equal(t, "ghcr.io/acme/web@"+digest, req.Image)
	require.Equal(t, digest, req.Commit, "an image app's source is its digest")
	require.Empty(t, req.SourceDir)

	unpinned := &detect.Proposal{DraftSpec: spec.AppSpec{Source: image}}
	_, ok = r.scanRequest(ctx, "app_2", image, unpinned, &source.Checkout{})
	require.False(t, ok, "an image app with no digest and no checkout has nothing to scan")
}
