package detect_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

const pinned = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

type fakeImages struct {
	insp oci.Inspection
	err  error
	auth *oci.Auth

	asked oci.Platform
}

func (f *fakeImages) Inspect(_ context.Context, _, _ string, want oci.Platform) (oci.Inspection, error) {
	f.asked = want
	return f.insp, f.err
}

func (f *fakeImages) Auth(context.Context, string, string) (*oci.Auth, error) { return f.auth, nil }

// noTrialRuntime reports a platform and cannot trial, so what the proposal
// says comes from the image alone.
type noTrialRuntime struct{ platform string }

func (r noTrialRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{Platform: r.platform}, nil
}

func (noTrialRuntime) Trial(context.Context, api.TrialRequest) (api.TrialResult, error) {
	return api.TrialResult{}, nil
}

func amd64Image(cfg *oci.Config) oci.Inspection {
	return oci.Inspection{
		Reference: "ghcr.io/acme/web:1",
		Digest:    pinned,
		Platforms: []oci.Platform{{OS: "linux", Architecture: "amd64"}},
		Config:    cfg,
	}
}

// TestR120_AnImageAppPinsTheDigestItsTagNamesNow asserts R-120's rule for
// images: the revision records the digest the tag resolved to, so a tag pushed
// again later does not change what runs without a new revision (issue #41).
func TestR120_AnImageAppPinsTheDigestItsTagNamesNow(t *testing.T) {
	images := &fakeImages{insp: amd64Image(&oci.Config{})}
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: noTrialRuntime{"linux/amd64"}, Images: images}

	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}, nil)
	require.NoError(t, err)
	require.Equal(t, pinned, p.DraftSpec.Source.Digest)
	require.Equal(t, "ghcr.io/acme/web:1", p.DraftSpec.Source.Image, "what was asked for is kept beside what runs")
	require.Equal(t, "amd64", images.asked.Architecture, "the configuration read is the host's build's")
}

// TestR101_AnImagesOwnDeclarationsFillTheSpec asserts that an image app,
// which has no repository to detect from, gets its port, storage and health
// check from what its image declares (issue #41), and is not asked for a port
// the image already names.
func TestR101_AnImagesOwnDeclarationsFillTheSpec(t *testing.T) {
	images := &fakeImages{insp: amd64Image(&oci.Config{
		ExposedPorts: []int{22, 3000},
		Volumes:      []string{"/data"},
		Healthcheck:  &oci.Health{Command: []string{"/bin/sh", "-c", "wget -qO- localhost:3000"}, Interval: 30 * time.Second, Retries: 3},
	})}
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: noTrialRuntime{"linux/amd64"}, Images: images}

	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}, nil)
	require.NoError(t, err)

	web := p.DraftSpec.Workloads[0]
	require.Equal(t, 3000, web.Ports[0].Number, "the web port first, not SSH")
	require.Equal(t, spec.PortExpose, web.Ports[0].Source, "declared, not watched — the review says which")
	require.NotNil(t, web.Health)
	require.Equal(t, 30, web.Health.IntervalSeconds)
	for _, q := range p.Questions {
		require.NotEqual(t, detect.KeyPrimaryPort, q.Key, "the image named its port")
	}
	require.Contains(t, web.Mounts, spec.Mount{VolumeID: p.DraftSpec.Volumes[0].ID, Path: "/data"})
	require.Equal(t, spec.VolumeFromImage, p.DraftSpec.Volumes[0].Declared)
}

// TestR203_AnImagesVolumeIsKeptEvenWithNoTrialRun asserts R-203 for images:
// the path an image declares with VOLUME is recorded as storage from the
// registry alone, so it does not sit in the container's writable layer until
// the second deploy discards it.
func TestR203_AnImagesVolumeIsKeptEvenWithNoTrialRun(t *testing.T) {
	images := &fakeImages{insp: amd64Image(&oci.Config{Volumes: []string{"/var/lib/postgresql/data"}})}
	job := &detect.Job{Auction: detect.NewAuction(), Images: images}

	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/db:1"}, nil)
	require.NoError(t, err)
	require.Len(t, p.DraftSpec.Volumes, 1)
	for _, w := range p.DraftSpec.Warnings {
		require.NotEqual(t, spec.WarnNoPersistentVolume, w.Code, "the storage is declared, so there is nothing to warn about")
	}
}

// An image with no build for the host is refused at detection, with the
// platforms named, rather than pulled and left to exit on start.
func TestAnImageWithNoBuildForTheHostIsBlocked(t *testing.T) {
	images := &fakeImages{insp: amd64Image(&oci.Config{})}
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: noTrialRuntime{"linux/arm64"}, Images: images}

	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}, nil)
	require.NoError(t, err)
	require.Equal(t, detect.StatusBlocked, p.Status)
	require.Equal(t, errs.PlanImagePlatformUnsupported, p.Blocked.Code)
	require.Contains(t, p.Blocked.Message, "linux/amd64")
	require.Contains(t, p.Blocked.Message, "linux/arm64")
}

// A registry that refuses — no such image, or credentials needed — blocks
// with its reason, which says what to do.
func TestARegistryRefusalBlocksWithItsReason(t *testing.T) {
	images := &fakeImages{err: errs.New(errs.ValidInvalid, "The registry ghcr.io would not let Pando read the image without signing in.")}
	job := &detect.Job{Auction: detect.NewAuction(), Images: images}

	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/private:1"}, nil)
	require.NoError(t, err)
	require.Equal(t, detect.StatusBlocked, p.Status)
	require.Contains(t, p.Blocked.Message, "without signing in")
}

// A registry that does not answer is not a refusal: the trial pulls the image
// anyway, and the proposal goes on without the registry's answers.
func TestAnUnreachableRegistryDoesNotBlock(t *testing.T) {
	images := &fakeImages{err: errs.New(errs.AdapterFailed, "Pando could not read the image.")}
	job := &detect.Job{Auction: detect.NewAuction(), Images: images}

	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}, nil)
	require.NoError(t, err)
	require.NotEqual(t, detect.StatusBlocked, p.Status)
	require.Empty(t, p.DraftSpec.Source.Digest)
}

// The trial pulls a private image with the app's credential.
func TestTheTrialPullsWithTheAppsCredential(t *testing.T) {
	rt := &recordingTrial{}
	images := &fakeImages{
		insp: amd64Image(&oci.Config{}),
		auth: &oci.Auth{Registry: "ghcr.io", Username: "ben", Password: secret.New("tok")},
	}
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: rt, Images: images}

	_, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}, nil)
	require.NoError(t, err)
	require.NotNil(t, rt.req.PullAuth)
	require.Equal(t, "ben", rt.req.PullAuth.Username)
	require.Equal(t, "tok", rt.req.PullAuth.Password.Reveal())
}

type recordingTrial struct{ req api.TrialRequest }

func (*recordingTrial) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsTrialRun: true, Platform: "linux/amd64"}, nil
}

func (r *recordingTrial) Trial(_ context.Context, req api.TrialRequest) (api.TrialResult, error) {
	r.req = req
	return api.TrialResult{Started: true, ObservedPorts: []int{8080}}, nil
}
