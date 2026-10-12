package detection

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// Detection builds what the app would deploy (R-311, design 09 §3).
//
// The plan is where somebody decides whether to run an app, and the score is
// part of that decision. The score comes from the image the app runs, so an
// app Pando builds is built here, once its build has everything it needs, the
// way a deploy builds it: through the builder adapter, with the same
// Dockerfile or plan, the same build egress rules (R-118), and no runtime
// socket in the build (R-112). The image is scanned with its source, the
// trial run starts it (R-097), and the first deploy of the same build inputs
// runs it and reuses its scan rather than building again. A build that fails
// fails the detection with its log, which is the answer (R-107).
//
// Nothing waits on what it does not need. The source is scanned while the
// image builds, and the image is scanned while the trial runs; the two scans
// are recorded as one scan of both (security.Union).

// ImageBuilder builds an app's images the way a deploy does. deploy.Runner is
// one.
type ImageBuilder interface {
	BuildImages(ctx context.Context, s *spec.AppSpec, checkout *source.Checkout, sink io.Writer, tag string) (string, map[string]string, error)
	BuiltImageAuth(ctx context.Context, image string) *api.RegistryAuth
}

// maxBuildLog is how much of a failed build's output a detection keeps: the
// end, where the failing step is.
const maxBuildLog = 64 << 10

// Built is what detection built for an app, nil when it built nothing: what a
// deploy of the same build inputs runs instead of building (deploy
// DetectionBuilds).
func (r *Runner) Built(ctx context.Context, appID string) (*detect.Built, error) {
	d, err := r.Detections.Get(ctx, appID)
	if err != nil {
		if errs.CodeOf(err) == errs.NotFound {
			return nil, nil
		}
		return nil, err
	}
	if d.Status == state.DetectionRunning || d.Status == state.DetectionFailed {
		return nil, nil
	}
	var p struct {
		Built *detect.Built `json:"built"`
	}
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return nil, nil //nolint:nilerr // An unreadable proposal has no build to reuse; the deploy builds.
	}
	return p.Built, nil
}

// buildSpec is the spec a proposal would deploy as its answers stand — the
// reading they choose, with the install's defaults — and whether it can be
// built now: something to build, a builder to build it, and no open question
// whose answer would change what is built.
func (r *Runner) buildSpec(ctx context.Context, appID, slug string, p *detect.Proposal, answers map[string]string, commit string) (spec.AppSpec, bool) {
	if r.Builder == nil || p.Blocked != nil || p.Winner.Strategy == detect.StrategyUnknown {
		return spec.AppSpec{}, false
	}
	if len(p.BuildWaitsOn(answers)) > 0 {
		return spec.AppSpec{}, false
	}
	s := p.WithAnswers(answers)
	s.AppID = appID
	if commit != "" {
		s.Source.Commit = commit
	}
	if r.Install != nil {
		d := r.Install.Defaults(ctx)
		if r.Policy != nil {
			if build, runtime, err := r.Policy.IsolationFloors(ctx); err == nil {
				d.BuildIsolation, d.RuntimeIsolation = build, runtime
			}
		}
		d.Apply(&s, slug)
	}
	if !detect.NeedsBuild(&s) || s.Build.AdapterRef == "" {
		return spec.AppSpec{}, false
	}
	return s, true
}

// build builds s from checkout, keeping the end of its log for a failure.
func (r *Runner) build(ctx context.Context, s *spec.AppSpec, checkout *source.Checkout) (*detect.Built, error) {
	sink := &tail{max: maxBuildLog}
	image, images, err := r.Builder.BuildImages(ctx, s, checkout, sink, r.buildTag())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, buildFailure(err, sink.String())
	}
	return &detect.Built{
		Inputs: spec.BuildInputs(s),
		Source: checkout.Identity(),
		Image:  image,
		Images: images,
	}, nil
}

// buildFailure is a build that failed at detection, with its log (R-107):
// the detection fails, and the log says why.
func buildFailure(err error, log string) error {
	reason := "the builder did not say why."
	remedy := "The build log shows the step that failed. Fix it in the repository, then run detection again."
	if e := errs.As(err); e != nil {
		reason = e.Message
		if e.Remedy != "" {
			remedy = e.Remedy
		}
	}
	return errs.Newf(errs.BuildFailed,
		"Pando builds an app while reading it, to score the image it would run, and the build of this app failed: %s",
		reason).
		WithRemedy(remedy).
		WithDetail("build_log", log)
}

func (r *Runner) buildTag() string {
	if r.NewBuildTag != nil {
		return r.NewBuildTag()
	}
	// Named like a deployment's build (R-146), and told apart from one.
	return "detect-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// buildHook is the build detection runs after the auction (detect.WithBuild):
// the source scan starts at once, the build runs beside it, and the image
// scan starts when the image exists, beside the trial run.
func (r *Runner) buildHook(appID, slug string, answers map[string]string, checkout *source.Checkout, scans *scanParts) detect.BuildFunc {
	return func(ctx context.Context, partial *detect.Proposal) (detect.BuildOutcome, error) {
		scans.startSource(checkout.Dir)
		s, ok := r.buildSpec(ctx, appID, slug, partial, answers, checkout.Commit)
		if !ok {
			return detect.BuildOutcome{}, nil
		}
		detect.Report(ctx, detect.StageBuilding, partial)
		built, err := r.build(ctx, &s, checkout)
		if err != nil {
			// The scan beside it is of an app that will not be proposed.
			scans.stop()
			return detect.BuildOutcome{}, err
		}
		image := built.Scanned(&s)
		auth := r.Builder.BuiltImageAuth(ctx, image)
		scans.startImage(image, auth)
		return detect.BuildOutcome{Built: built, PullAuth: auth}, nil
	}
}

// Rebuild builds a finished proposal again when what it would build has
// changed since it was built — an answer to a question the build waited on, a
// build argument, a revised plan — and scans the new image, in the
// background. True when it started; the detection reads as running, at the
// building stage, until it has finished. Accepting waits for it, so the score
// a person accepts is the score of what the first deploy runs.
func (r *Runner) Rebuild(ctx context.Context, appID string) (bool, error) {
	app, err := r.check(ctx, appID)
	if err != nil {
		return false, err
	}
	d, err := r.Detections.Get(ctx, appID)
	if err != nil {
		return false, err
	}
	if d.Status != state.DetectionReady && d.Status != state.DetectionNeedsAnswers {
		return false, nil
	}
	var p detect.Proposal
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return false, errs.Wrap(errs.Internal, "The stored plan could not be read.", err)
	}
	s, ok := r.buildSpec(ctx, appID, app.Slug, &p, d.Answers, p.Commit)
	if !ok || (p.Built != nil && p.Built.Inputs == spec.BuildInputs(&s)) {
		return false, nil
	}
	body := progressBody(detect.StageBuilding, &p)
	body["running"] = []string{detect.StageBuilding}
	started, err := r.Detections.Rebuild(ctx, appID, body)
	if err != nil || !started {
		// Not started: another request got there first, or the detection
		// moved on since it was read.
		return false, err
	}
	// The request that asked returns at once; the build is bounded as a
	// detection is.
	r.background(func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), Timeout)
		defer cancel()
		r.rebuild(bg, app, p)
	})
	return true, nil
}

func (r *Runner) background(fn func()) {
	if r.Background != nil {
		r.Background(fn)
		return
	}
	go fn()
}

// rebuild is Rebuild's work: fetch the reviewed commit, build and scan, and
// store the proposal with its new build. Answers given while it ran are read
// again at the end, and built too.
func (r *Runner) rebuild(ctx context.Context, app state.App, p detect.Proposal) {
	ctx, prog := r.withProgress(ctx, app.ID)
	src := app.Source
	if p.Commit != "" {
		src.Commit = p.Commit
	}
	checkout, err := r.Sources.Fetch(source.ForApp(ctx, app.ID), src)
	if err != nil {
		r.fail(ctx, app.ID, err)
		return
	}
	defer checkout.Close()

	for range 3 {
		d, err := r.Detections.Get(ctx, app.ID)
		if err != nil {
			r.fail(ctx, app.ID, err)
			return
		}
		s, ok := r.buildSpec(ctx, app.ID, app.Slug, &p, d.Answers, checkout.Commit)
		if !ok || p.Built.Matches(&s, checkout.Identity()) {
			break
		}
		scans := r.newScans(ctx, app.ID, checkout.Identity(), prog)
		scans.startSource(checkout.Dir)
		detect.Report(ctx, detect.StageBuilding, &p)
		built, err := r.build(ctx, &s, checkout)
		if err != nil {
			scans.stop()
			r.fail(ctx, app.ID, err)
			return
		}
		p.Built = built
		image := built.Scanned(&s)
		scans.startImage(image, r.Builder.BuiltImageAuth(ctx, image))
		detect.Report(ctx, detect.StageScanning, &p)
		scans.record(ctx)
	}
	if err := r.Detections.Save(ctx, app.ID, p.Status, p, p.Commit); err != nil {
		_ = r.Detections.FailIfRunning(ctx, app.ID, err)
	}
}

// fail records a detection that failed, as detect does.
func (r *Runner) fail(ctx context.Context, appID string, err error) {
	_ = r.Detections.Save(ctx, appID, state.DetectionFailed, failureBody(err), "")
}

// failureBody is what a failed detection stores.
func failureBody(err error) map[string]any {
	// errs.As is nil for an error carrying no envelope, and storing that would
	// leave the user looking at "error: null" where the reason should be.
	recorded := any(map[string]any{"message": err.Error()})
	if e := errs.As(err); e != nil {
		recorded = e
	}
	return map[string]any{"error": recorded}
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "")
}
