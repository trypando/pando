//go:build integration

package deploy

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/detection"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// countingBuilder builds as the harness's builder: it counts its builds,
// keeps what each was asked, writes a line of log, and can fail the way a
// build fails, or wait at a barrier.
type countingBuilder struct {
	api.BuilderAdapter

	mu       sync.Mutex
	requests []api.BuildRequest

	// fail makes every build fail, with this line in its log.
	fail string

	// started, when set, is closed as the first build starts; release, when
	// set, is waited on before the build finishes.
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *countingBuilder) Category() api.Category { return api.CategoryBuilder }
func (b *countingBuilder) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return api.BuilderCapabilities{Strategies: []api.BuildStrategy{spec.BuildDockerfile, spec.BuildStatic}}, nil
}

func (b *countingBuilder) Build(ctx context.Context, req api.BuildRequest) (api.BuildResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()
	if b.started != nil {
		b.once.Do(func() { close(b.started) })
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return api.BuildResult{}, ctx.Err()
		case <-time.After(20 * time.Second):
			return api.BuildResult{}, errs.New(errs.BuildFailed, "The barrier was never released.")
		}
	}
	_, _ = io.WriteString(req.LogSink, "#1 [1/2] FROM docker.io/library/nginx:alpine\n")
	if b.fail != "" {
		_, _ = io.WriteString(req.LogSink, b.fail+"\n")
		return api.BuildResult{}, errs.New(errs.BuildFailed, "The build failed at RUN npm ci.")
	}
	_, _ = io.WriteString(req.ImageSink, "image-bytes")
	return api.BuildResult{ImageRef: "pando/app:" + req.Tag}, nil
}

func (b *countingBuilder) builds() []api.BuildRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]api.BuildRequest(nil), b.requests...)
}

// fixedInstall is an install's defaults: this harness's adapters.
type fixedInstall struct{}

func (fixedInstall) Defaults(context.Context) spec.Defaults {
	d := spec.StandardDefaults()
	d.RuntimeAdapter, d.BuilderAdapter, d.RoutingAdapter = "rt", "bld", "rte"
	return d
}

// repoWith is a repository holding files, in one commit, and its SHA.
func repoWith(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)
	for name, body := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		_, err = wt.Add(name)
		require.NoError(t, err)
	}
	sha, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@example", When: time.Unix(1700000000, 0)},
	})
	require.NoError(t, err)
	return dir, sha.String()
}

// game2048 is the shape of gabrielecirulli/2048: a static site, built into
// an image that serves it.
var game2048 = map[string]string{
	"index.html":                "<html><head><script src=\"js/application.js\"></script></head><body>2048</body></html>",
	"js/application.js":         "window.requestAnimationFrame(function () { new GameManager(4) })",
	"style/main.css":            "body { margin: 0 }",
	"meta/apple-touch-icon.png": "png",
}

// buildArgRepo builds from a Dockerfile that stops without a build argument.
var buildArgRepo = map[string]string{
	"Dockerfile": "FROM nginx:alpine\nARG TOKEN\nRUN : \"${TOKEN:?TOKEN is required}\"\n" +
		"COPY index.html /usr/share/nginx/html/\nEXPOSE 80\n",
	"index.html": "<h1>notes</h1>",
}

// detector is real detection over the harness, building through its deploy
// runner as main wires it, with a rebuild run at once rather than behind.
func (h *scanHarness) detector() *detection.Runner {
	d := &detection.Runner{
		Apps:       h.apps,
		Detections: state.NewDetections(h.db),
		Sources:    source.Sources{WorkDir: h.t.TempDir()},
		Install:    fixedInstall{},
		Job:        &detect.Job{Auction: detect.NewAuction(detect.StaticDetector{}, detect.DockerfileDetector{})},
		Scanner:    detectionScans{service: h.service},
		Builder:    h.runner,
		Background: func(fn func()) { fn() },
	}
	h.runner.WithDetectionBuilds(d)
	return d
}

// accept is what accepting the proposal pins: its draft with the answers
// given, as the accept handler applies them.
func (h *scanHarness) accept(d *detection.Runner) {
	h.t.Helper()
	det, err := d.Detections.Get(context.Background(), h.app.ID)
	require.NoError(h.t, err)
	require.NotEqual(h.t, state.DetectionRunning, det.Status, "accepting waits for detection")
	var p detect.Proposal
	require.NoError(h.t, json.Unmarshal(det.Body, &p))
	require.Empty(h.t, p.BuildWaitsOn(det.Answers))
	accepted := p.WithAnswers(det.Answers)
	accepted.AppID = h.app.ID
	fixedInstall{}.Defaults(context.Background()).Apply(&accepted, h.app.Slug)
	h.spec = &accepted
}

// sourceFindings is what a 2048-shaped repository's own files hold: a key
// committed in a script, and the base image's OpenSSL finding again, which
// the image reports too and which costs what it costs once.
var sourceFindings = []api.Finding{
	{ID: "aws-access-key-id", Severity: api.SeverityCritical, Title: "AWS access key", Target: "js/application.js"},
	{ID: "CVE-2025-9232", Severity: api.SeverityHigh, Title: "libssl3: out-of-bounds read", Target: "alpine 3.21.3"},
}

// TestR311_APlanScoresTheImageDetectionBuilt asserts R-311 for the plan of an
// app Pando builds, as it does for gabrielecirulli/2048. Detection builds the
// image the app would deploy and scans it with its source, so the plan shows
// a real score — the union of what the image and the source hold, a finding
// both report counted once — and not "scored once the image is built". The
// deploy of that plan reuses the image and its scan, and its score is the
// plan's.
func TestR311_APlanScoresTheImageDetectionBuilt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, commit := repoWith(t, game2048)
	h := newScanHarnessAt(t, repo, commit)
	h.scanner.source = sourceFindings
	d := h.detector()

	_, err := d.Detect(ctx, h.app.ID)
	require.NoError(t, err)

	require.Len(t, h.builder.builds(), 1, "detection built the image")
	scans := h.scanner.requests()
	require.Len(t, scans, 2, "the source and the image, scanned as two parts")
	var sawSource, sawImage bool
	for _, req := range scans {
		sawSource = sawSource || (req.SourceDir != "" && req.Image == "")
		sawImage = sawImage || (req.Image == h.docker.id && req.SourceDir == "")
	}
	require.True(t, sawSource && sawImage)

	plan, err := h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.False(t, plan.AwaitingImage, "the plan does not wait for an image: it has one")
	require.Equal(t, security.ScopeImageAndSource, plan.Scope, "one scan of both")
	require.NotNil(t, plan.Standing.Score)
	require.Equal(t, h.docker.id, plan.Scan.Image)
	require.Equal(t, commit, plan.Scan.Commit)
	require.Equal(t, 3, plan.Counts.Critical, "the image's two and the source's key")
	require.Equal(t, 1, plan.Counts.High, "the finding both report, once")
	require.Equal(t, 100-3*25-10, *plan.Standing.Score)

	h.accept(d)
	rev, err := h.deploy()
	require.NoError(t, err)
	require.Len(t, h.scanner.requests(), 2, "the deploy reused the plan's scan")

	deployed, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.Equal(t, plan.Scan.ID, deployed.Scan.ReusedFrom)
	require.Equal(t, *plan.Standing.Score, *deployed.Standing.Score, "the plan's score is the deploy's")
	require.Equal(t, plan.Counts, deployed.Counts)

	// The same scan as one scan of both would have read, which is what a
	// deploy that scans takes: a rescan of the deployed image agrees.
	direct, err := h.service.Scan(ctx, api.ScanRequest{AppID: h.app.ID, SpecID: rev.ID, Commit: commit,
		Image: h.docker.id, SourceDir: t.TempDir()}, audit.Event{PrincipalKind: audit.KindUser, PrincipalID: h.owner})
	require.NoError(t, err)
	require.Equal(t, *plan.Standing.Score, *direct.Score)
}

// TestR311_TheFirstDeployRunsTheImageDetectionBuilt asserts that an app is
// built once (R-311, design 09 §4.1): across detection and the first deploy
// the builder runs exactly once, and the runtime runs the image detection
// built — the one the plan's score describes.
func TestR311_TheFirstDeployRunsTheImageDetectionBuilt(t *testing.T) {
	t.Parallel()
	repo, commit := repoWith(t, game2048)
	h := newScanHarnessAt(t, repo, commit)
	d := h.detector()

	_, err := d.Detect(context.Background(), h.app.ID)
	require.NoError(t, err)
	built, err := d.Built(context.Background(), h.app.ID)
	require.NoError(t, err)
	require.NotNil(t, built)

	h.accept(d)
	_, err = h.deploy()
	require.NoError(t, err)

	require.Len(t, h.builder.builds(), 1, "built once, at detection, and not again by the deploy")
	require.Equal(t, []string{built.Image}, h.docker.applied, "the deploy ran the image detection built")
	require.True(t, strings.HasPrefix(h.builder.builds()[0].Tag, "detect-"), "named as detection's build")
}

// TestR311_ABuildInputChangedAfterDetectionBuildsAgain asserts the other half:
// a spec that changed what is built after detection — here a build argument
// added to the accepted spec — is built by the deploy, once, and scanned, so
// nothing runs that was not scanned.
func TestR311_ABuildInputChangedAfterDetectionBuildsAgain(t *testing.T) {
	t.Parallel()
	repo, commit := repoWith(t, game2048)
	h := newScanHarnessAt(t, repo, commit)
	d := h.detector()

	_, err := d.Detect(context.Background(), h.app.ID)
	require.NoError(t, err)
	h.accept(d)
	h.spec.Build.Args = append(h.spec.Build.Args, spec.KV{Key: "SITE_TITLE", Value: "2048"})

	h.docker.id = "sha256:" + strings.Repeat("e", 64)
	_, err = h.deploy()
	require.NoError(t, err)

	builds := h.builder.builds()
	require.Len(t, builds, 2, "the deploy built, since what it builds changed")
	require.Equal(t, "2048", builds[1].Args["SITE_TITLE"])
	scans := h.scanner.requests()
	require.Equal(t, h.docker.id, scans[len(scans)-1].Image, "and scanned what it built")
	require.Equal(t, []string{h.docker.id}, h.docker.applied)
}

// TestR311_AnUnansweredBuildArgDefersTheScore asserts R-311 for a build that
// cannot run yet: a Dockerfile that stops without a build argument nobody has
// given. Detection asks for it and builds nothing; the plan shows the source's
// findings and no score, waiting for the image. Answering it builds and scans
// the image, and the plan then has the score the first deploy keeps.
func TestR311_AnUnansweredBuildArgDefersTheScore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, commit := repoWith(t, buildArgRepo)
	h := newScanHarnessAt(t, repo, commit)
	d := h.detector()

	det, err := d.Detect(ctx, h.app.ID)
	require.NoError(t, err)
	var p detect.Proposal
	require.NoError(t, json.Unmarshal(det.Body, &p))
	require.Equal(t, []string{detect.BuildArgKeyPrefix + "TOKEN"}, p.BuildWaitsOn(nil), "the question is asked")
	require.Equal(t, state.DetectionNeedsAnswers, det.Status, "and accepting is refused until it is answered")
	require.Nil(t, p.Built)
	require.Empty(t, h.builder.builds(), "nothing is built without it")

	plan, err := h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.True(t, plan.AwaitingImage, "the score waits for the image")
	require.Nil(t, plan.Standing.Score)

	require.NoError(t, d.Detections.SaveAnswers(ctx, h.app.ID, map[string]string{"build_arg.TOKEN": "s3cret-free"}))
	started, err := d.Rebuild(ctx, h.app.ID)
	require.NoError(t, err)
	require.True(t, started, "the answer builds the image")

	builds := h.builder.builds()
	require.Len(t, builds, 1)
	require.Equal(t, "s3cret-free", builds[0].Args["TOKEN"], "with the answer")

	plan, err = h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.False(t, plan.AwaitingImage)
	require.NotNil(t, plan.Standing.Score, "the plan now has the score")
	require.Equal(t, h.docker.id, plan.Scan.Image)

	again, err := d.Rebuild(ctx, h.app.ID)
	require.NoError(t, err)
	require.False(t, again, "an unchanged build is not built again")

	h.accept(d)
	rev, err := h.deploy()
	require.NoError(t, err)
	require.Len(t, h.builder.builds(), 1, "the deploy runs what the answer built")
	deployed, err := h.service.Report(ctx, h.app.ID, rev.ID)
	require.NoError(t, err)
	require.Equal(t, *plan.Standing.Score, *deployed.Standing.Score)
}

// TestR107_ABuildThatFailsAtDetectionShowsItsLog asserts R-107's pattern for
// a build: detection that cannot build the app stops, says so in a sentence
// that stands alone (R-105), and shows the build's own log, which is the
// answer. Nothing is proposed and nothing is scored.
func TestR107_ABuildThatFailsAtDetectionShowsItsLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, commit := repoWith(t, game2048)
	h := newScanHarnessAt(t, repo, commit)
	h.builder.fail = `ERROR: failed to solve: process "/bin/sh -c npm ci" did not complete successfully: exit code: 1`
	d := h.detector()

	_, err := d.Detect(ctx, h.app.ID)
	require.Error(t, err)
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))

	det, err := d.Detections.Get(ctx, h.app.ID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionFailed, det.Status)
	var body struct {
		Error errs.Error `json:"error"`
	}
	require.NoError(t, json.Unmarshal(det.Body, &body))
	require.Equal(t, errs.BuildFailed, body.Error.Code)
	require.Contains(t, body.Error.Message, "The build failed at RUN npm ci.", "the builder's reason, in the sentence")
	require.NotEmpty(t, body.Error.Remedy)
	log, _ := body.Error.Details["build_log"].(string)
	require.Contains(t, log, "did not complete successfully: exit code: 1", "with the build's own log")

	_, found, err := h.service.Scans.Latest(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.False(t, found, "nothing proposed, nothing scored")
}

// TestR311_TheSourceScanRunsWhileTheImageBuilds asserts that detection does
// not make the build wait on the source scan or the scan on the build: the
// builder holds until the source scan has started, and the scanner holds the
// source scan until the build has started. Run one after the other, neither
// would ever start.
func TestR311_TheSourceScanRunsWhileTheImageBuilds(t *testing.T) {
	t.Parallel()
	repo, commit := repoWith(t, game2048)
	h := newScanHarnessAt(t, repo, commit)
	buildStarted, sourceStarted := make(chan struct{}), make(chan struct{})
	h.builder.started, h.builder.release = buildStarted, sourceStarted
	var once sync.Once
	h.scanner.onScan = func(req api.ScanRequest) error {
		if req.SourceDir == "" {
			return nil
		}
		once.Do(func() { close(sourceStarted) })
		select {
		case <-buildStarted:
			return nil
		case <-time.After(20 * time.Second):
			return errs.New(errs.AdapterFailed, "The build never started while the source was scanned.")
		}
	}
	d := h.detector()

	_, err := d.Detect(context.Background(), h.app.ID)
	require.NoError(t, err)
	plan, err := h.service.Report(context.Background(), h.app.ID, "")
	require.NoError(t, err)
	require.Empty(t, plan.Scan.Error, "both ran, each while the other was under way")
	require.Equal(t, security.ScopeImageAndSource, plan.Scope)
}
