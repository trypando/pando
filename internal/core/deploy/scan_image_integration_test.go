//go:build integration

package deploy

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// imageScanner finds nothing in a source tree and what an outdated base image
// carries in an image — the shape of the 2048 reproduction: a static site whose
// HTML is clean, served by an nginx image with OpenSSL findings.
type imageScanner struct {
	mu    sync.Mutex
	scans []api.ScanRequest
}

func (s *imageScanner) Kind() string                                     { return "image-scanner" }
func (s *imageScanner) Category() api.Category                           { return api.CategoryScanner }
func (s *imageScanner) Configure(context.Context, json.RawMessage) error { return nil }
func (s *imageScanner) HealthCheck(context.Context) error                { return nil }
func (s *imageScanner) ScannerCapabilities() api.ScannerCapabilities {
	return api.ScannerCapabilities{ScansImages: true, ScansSource: true}
}

func (s *imageScanner) Scan(_ context.Context, req api.ScanRequest) (api.ScanResult, error) {
	s.mu.Lock()
	s.scans = append(s.scans, req)
	s.mu.Unlock()
	if req.Image == "" {
		return api.ScanResult{Scanner: "test"}, nil
	}
	return api.ScanResult{Scanner: "test", Findings: []api.Finding{
		{ID: "CVE-2025-9230", Severity: api.SeverityCritical, Title: "libssl3: out-of-bounds read", Target: "alpine 3.21.3"},
		{ID: "CVE-2025-9231", Severity: api.SeverityCritical, Title: "libcrypto3: timing side channel", Target: "alpine 3.21.3"},
		{ID: "CVE-2025-9232", Severity: api.SeverityHigh, Title: "libssl3: out-of-bounds read", Target: "alpine 3.21.3"},
	}}, nil
}

func (s *imageScanner) requests() []api.ScanRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.ScanRequest(nil), s.scans...)
}

// sameImageDocker loads every build as the image ID it is told to: a rebuild
// of an unchanged commit that produced the same image, or one that did not.
type sameImageDocker struct {
	singleHostDocker
	id string
}

func (d *sameImageDocker) ImportImage(_ context.Context, r io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, r)
	return d.id, nil
}

type fixedPolicy struct{ doc policy.Document }

func (p *fixedPolicy) Document(context.Context) (policy.Document, error) { return p.doc, nil }

type scanHarness struct {
	t       *testing.T
	db      *state.DB
	apps    *state.Apps
	deploys *state.Deployments
	app     state.App
	owner   string
	spec    *spec.AppSpec
	commit  string
	runner  *Runner
	docker  *sameImageDocker
	scanner *imageScanner
	service *security.Service
	policy  *fixedPolicy
}

func newScanHarness(t *testing.T) *scanHarness {
	t.Helper()
	ctx := context.Background()
	repo, commit := gitRepo(t)

	db, _ := statetest.Connect(t)
	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID
	apps := state.NewApps(db)
	deploys := state.NewDeployments(db)
	src := spec.Source{Type: spec.SourceGit, URL: repo, Ref: "master", Commit: commit}
	app, err := apps.Create(ctx, "game", "game-2048", owner, owner, src)
	require.NoError(t, err)

	docker := &sameImageDocker{
		singleHostDocker: singleHostDocker{tags: map[string]string{}},
		id:               "sha256:" + strings.Repeat("a", 64),
	}
	scanner := &imageScanner{}
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("rt", docker))
	require.NoError(t, reg.Register("bld", &taggingBuilder{}))
	require.NoError(t, reg.Register("rte", routeAnything{}))
	require.NoError(t, reg.Register("scn", scanner))

	pol := &fixedPolicy{}
	svc := &security.Service{Scans: state.NewScans(db), Deployments: deploys, Registry: reg, Policy: pol}
	r := NewRunner(reg, nil, apps, deploys, noSecrets{}, nil, NewLogStore(), nil, "http://pando:8080").
		WithSources(source.Sources{WorkDir: t.TempDir()}).
		WithSecurity(svc)

	return &scanHarness{
		t: t, db: db, apps: apps, deploys: deploys, app: app, owner: owner, commit: commit,
		runner: r, docker: docker, scanner: scanner, service: svc, policy: pol,
		spec: &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
			Build:     spec.Build{Strategy: spec.BuildDockerfile, AdapterRef: "bld"},
			Runtime:   spec.RuntimeRef{AdapterRef: "rt"},
			Routing:   spec.Routing{AdapterRef: "rte", Mode: spec.RoutingPort, Port: 8080},
			Workloads: []spec.Workload{{Name: "web", Primary: true, Exposed: true}},
		},
	}
}

// detect is what detection does: a scan of the checkout, before anything is
// built and before there is a revision.
func (h *scanHarness) detect() {
	h.t.Helper()
	_, err := h.service.Scan(context.Background(), api.ScanRequest{
		AppID: h.app.ID, SourceDir: h.t.TempDir(), Commit: h.commit,
	}, audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "detection"})
	require.NoError(h.t, err)
}

func (h *scanHarness) deploy() (state.Revision, error) {
	h.t.Helper()
	ctx := context.Background()
	rev, err := h.apps.CreateRevision(ctx, h.app.ID, h.spec, spec.OriginManual, h.owner)
	require.NoError(h.t, err)
	dep, err := h.deploys.Create(ctx, h.app.ID, rev.ID, state.TriggerManual, h.owner)
	require.NoError(h.t, err)
	return rev, h.runner.Run(ctx, dep, rev)
}

// TestR311_ADeployScansTheImageItBuilt asserts R-311: the score comes from the
// image that was built as well as the source. Detection's scan of the source
// alone, taken before there was an image, does not stand in for the image the
// first deploy builds — reusing it showed an nginx-based static site with two
// critical OpenSSL findings as 100 / 100. The deploy scans the built image and
// its source together, the revision's score is that scan's, and the threshold
// (R-314) is placed against the image's findings.
func TestR311_ADeployScansTheImageItBuilt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	h.detect()
	atDetection, found, err := h.service.Scans.Latest(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 100, *atDetection.Score, "the source alone is clean")
	require.Empty(t, atDetection.Image, "and the scan says it read no image")

	rev, err := h.deploy()
	require.NoError(t, err)

	scans := h.scanner.requests()
	require.Len(t, scans, 2, "the deploy ran the scanner, though its commit had a scan")
	require.Equal(t, h.docker.id, scans[1].Image, "on the image it built")
	require.NotEmpty(t, scans[1].SourceDir, "and the source it was built from")
	require.Equal(t, h.commit, scans[1].Commit)

	report, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.NotNil(t, report.Scan)
	require.Equal(t, rev.ID, report.Scan.SpecID)
	require.Equal(t, h.docker.id, report.Scan.Image, "the revision's scan names the image it read")
	require.Equal(t, h.commit, report.Scan.Commit)
	require.Empty(t, report.Scan.ReusedFrom, "it ran; it is not detection's scan reused")
	require.Equal(t, 2, report.Counts.Critical, "the image's findings reach the score")
	require.Equal(t, 1, report.Counts.High)
	require.Less(t, *report.Scan.Score, 100)

	// With a threshold the image does not meet, the next deploy is refused on
	// the image's findings, not passed on the source's.
	h.policy.doc.MinSecurityScore = 90
	_, err = h.deploy()
	require.Equal(t, errs.PlanSecurityBelowThreshold, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Details, "findings")
}

// TestR312_AnUnchangedImageAndSourceReuseTheirScan asserts R-312 under R-311's
// rule: a scan is reused by a deploy of the same source and the same image, by
// digest, and by nothing less. A redeploy whose build produced the same image
// uses the scan that read it; a rebuild that produced a different image from
// the same commit is scanned.
func TestR312_AnUnchangedImageAndSourceReuseTheirScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	_, err := h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 1, "the first deploy scans")

	again, err := h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 1, "the same source and image are not scanned again")

	reused, found, err := h.service.Scans.Latest(ctx, h.app.ID, again.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, again.ID, reused.SpecID, "the scan describes the revision deployed")
	require.NotEmpty(t, reused.ReusedFrom)
	require.Equal(t, h.docker.id, reused.Image)

	h.docker.id = "sha256:" + strings.Repeat("b", 64)
	rebuilt, err := h.deploy()
	require.NoError(t, err)
	scans := h.scanner.requests()
	require.Len(t, scans, 2, "a different image from the same commit is scanned")
	require.Equal(t, h.docker.id, scans[1].Image)

	got, found, err := h.service.Scans.Latest(ctx, h.app.ID, rebuilt.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, got.ReusedFrom)
	require.Equal(t, h.docker.id, got.Image)
}

// publish pushes a new image to reference in an in-memory registry and
// returns its digest.
func publish(t *testing.T, reference string) string {
	t.Helper()
	img, err := random.Image(64, 1)
	require.NoError(t, err)
	ref, err := name.ParseReference(reference, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))
	digest, err := img.Digest()
	require.NoError(t, err)
	return digest.String()
}

// TestR311_ADeployScansThePublishedImageItRuns asserts R-311 for a repository
// whose own published image Pando runs instead of building it (R-094 tier 1),
// as it does for bastienwirtz/homer: the score is the image's as well as the
// repository's. Before, only the repository's files were scanned, and an image
// with 6 high findings scored 80. The deploy pins the tag to the digest it
// names now, scans that digest with the checkout, and runs that digest — so
// what was scanned is what runs. A redeploy of the same digest reuses the
// scan; a tag that moved to a new image is scanned again.
func TestR311_ADeployScansThePublishedImageItRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	tag := strings.TrimPrefix(srv.URL, "http://") + "/bastienwirtz/homer:latest"
	first := publish(t, tag)
	h.runner.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})

	h.spec.Build = spec.Build{Strategy: spec.BuildPrebuilt}
	h.spec.Workloads[0].Image = tag

	h.detect()
	rev, err := h.deploy()
	require.NoError(t, err)

	scans := h.scanner.requests()
	require.Len(t, scans, 2, "the deploy scanned, though detection had scanned the repository")
	pinned := oci.Pin(tag, first)
	require.Equal(t, pinned, scans[1].Image, "the published image, by the digest its tag named")
	require.NotEmpty(t, scans[1].SourceDir, "with the repository")
	require.Equal(t, h.commit, scans[1].Commit)
	require.Equal(t, []string{pinned}, h.docker.applied, "and the digest scanned is the digest that runs")

	report, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.Equal(t, pinned, report.Scan.Image)
	require.Equal(t, 2, report.Counts.Critical, "the image's findings reach the score")

	_, err = h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 2, "an unchanged image and repository are not scanned again")

	second := publish(t, tag)
	_, err = h.deploy()
	require.NoError(t, err)
	scans = h.scanner.requests()
	require.Len(t, scans, 3, "a tag that now names another image is scanned")
	require.Equal(t, oci.Pin(tag, second), scans[2].Image)
}

// TestR311_AnImageAppIsScoredByItsImage asserts R-311 for an app added as an
// image that is already built: there is no repository, and the scan reads the
// pinned digest the app runs.
func TestR311_AnImageAppIsScoredByItsImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	tag := strings.TrimPrefix(srv.URL, "http://") + "/acme/web:1"
	digest := publish(t, tag)
	h.runner.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})

	h.spec.Source = spec.Source{Type: spec.SourceImage, Image: tag, Digest: digest}
	h.spec.Build = spec.Build{Strategy: spec.BuildPrebuilt}
	h.spec.Workloads[0].Image = tag

	rev, err := h.deploy()
	require.NoError(t, err)

	scans := h.scanner.requests()
	require.Len(t, scans, 1)
	require.Equal(t, oci.Pin(tag, digest), scans[0].Image, "the pinned digest, never the tag")
	require.Empty(t, scans[0].SourceDir, "there is no repository to read")

	report, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.Equal(t, oci.Pin(tag, digest), report.Scan.Image)
	require.Equal(t, digest, report.Scan.Commit, "the source an image app names is its digest")
	require.Equal(t, 2, report.Counts.Critical)

	_, err = h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 1, "the same digest is not scanned again")
}
