// Package detection runs the detection job for an app and records the result.
//
// It is the seam between detection (which knows about repositories and
// detectors) and the rest of Pando (which knows about apps, policy and audit).
// Sequence A's numbered steps 6 through 12 live in internal/detect; the parts
// that need an app row, the source allowlist and a place to write the answer
// live here.
package detection

import (
	"context"
	"path"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/screening"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// SourcePolicy is the source allowlist check (R-092), plus the isolation floors
// a detected spec inherits (R-024, R-114).
type SourcePolicy interface {
	AllowsSource(ctx context.Context, src spec.Source) error
	IsolationFloors(ctx context.Context) (build, runtime spec.IsolationClass, err error)
}

// Installation supplies the answers a repository cannot give about itself.
//
// A repository says it builds from a Dockerfile and listens on 3000. It cannot
// say which runtime this install uses or how apps here are addressed, and R-104
// is explicit that none of that is a question worth asking — it is
// configuration, and configuration gets a default.
type Installation interface {
	// Defaults returns the install's adapters, routing mode and limits. The
	// routing mode comes from the routing adapter's own declared default
	// (R-162): adding an app uses it without asking.
	Defaults(ctx context.Context) spec.Defaults
}

// Runner detects for an app and stores the proposal.
type Runner struct {
	Apps       *state.Apps
	Detections *state.Detections
	Job        *detect.Job

	// Sources fetches the source detection reads, including an uploaded one
	// (R-262).
	Sources source.Sources

	// Policy is checked again here, not only at app creation. The allowlist can
	// change between the two, and a re-detection (R-022) of an app whose source
	// is no longer allowed must not clone it.
	Policy SourcePolicy

	// Install fills in everything the repository cannot answer. Without it a
	// detected spec describes the app and says nothing about where it runs,
	// which is a spec that cannot be planned — the gap that made detection
	// produce proposals nobody could deploy.
	Install Installation

	// Ports assigns a host port in port-mode routing. Only consulted when the
	// routing adapter's default mode is `port`, which the loopback adapter that
	// ships as the laptop default is.
	Ports PortAllocator

	// PortRange bounds that allocation.
	PortRangeStart int
	PortRangeEnd   int

	// Screeners says which AI adapter performs each function, and on which
	// model (R-259). Nil is the ordinary case: an install with no AI adapter
	// configured is not a degraded install, because everything the auction
	// produced is there either way (R-106, R-335).
	Screeners Screeners

	// Screener, when Screeners is nil, performs every function under
	// ScreenerRef. For an embedding with one adapter and nothing to assign.
	Screener     screening.Screener
	ScreenerRef  string
	ScreenPolicy ScreenPolicy

	// Auditor records that a repository's contents left the host (R-337).
	Auditor Auditor

	// Screening budget (R-339). Zero means the package default.
	ScreenMaxFiles int
	ScreenMaxBytes int64
	ScreenTimeout  time.Duration

	// Clock stamps conversation turns. Nil is the system clock.
	Clock clock.Clock

	// Scanner scores the checkout while it is still on disk (R-312).
	//
	// Detection is the first and, for a while, the only moment Pando holds an
	// app's source: an app that has been added and not yet deployed has no
	// image to look at, and waiting for one means the first thing anybody sees
	// about a new app is "not scanned yet". A committed key or a vulnerable
	// lockfile is exactly what somebody wants to know *before* deciding to
	// deploy it.
	//
	// Optional, and never fatal. An installation with no scanner detects
	// exactly as before, and a scanner that fails does not fail a detection —
	// the proposal is the thing being produced here.
	Scanner SourceScanner

	// Builder builds what the app would deploy, so the plan can show the
	// score of the image that will run (R-311). Nil builds nothing: the plan
	// shows the source's findings and waits for the deploy to score the
	// image.
	Builder ImageBuilder

	// NewBuildTag names a detection's build. Nil is a time-based name.
	NewBuildTag func() string

	// Background runs a rebuild after an answer (Rebuild). Nil is a
	// goroutine.
	Background func(func())
}

// SourceScanner scans what detection read: the checkout, and the image the
// proposal runs — a published one, or the one detection built. Narrow, so
// detection cannot reach into the rest of the security service.
type SourceScanner interface {
	// Scans reports whether there is a scanner to scan with.
	Scans() bool

	// ScanSource scans and records in one call.
	ScanSource(ctx context.Context, req api.ScanRequest)

	// ScanPart scans one part, the source or the image, recording nothing;
	// RecordParts records the parts as one scan of both (R-311).
	ScanPart(ctx context.Context, req api.ScanRequest) (api.ScanResult, error)
	RecordParts(ctx context.Context, req api.ScanRequest, parts []api.ScanResult, failed error)
}

// PortAllocator hands out host ports for port-mode routing.
type PortAllocator interface {
	Allocate(ctx context.Context, adapterRef, appID string, from, to int) (int, error)
}

// Detect runs detection for an app and records the result.
//
// The allowlist is checked before anything touches disk, and the assertion in
// Sequence A is exact: a blocked source produces zero disk writes, because
// git clone is never invoked. That is why this check is here and not inside
// source.Fetch — a check that runs after the call has already started is not
// the same promise.
func (r *Runner) Detect(ctx context.Context, appID string) (state.Detection, error) {
	app, err := r.check(ctx, appID)
	if err != nil {
		return state.Detection{}, err
	}

	if err := r.Detections.Start(ctx, appID); err != nil {
		return state.Detection{}, err
	}
	return r.detect(ctx, app)
}

// RunQueued runs a detection the detection queue claimed: Detect without
// marking it running again, which would hand it back to the queue (issue #72,
// O-32). The allowlist is checked again here, since it can change while a
// detection waits (R-092).
func (r *Runner) RunQueued(ctx context.Context, appID string) (state.Detection, error) {
	app, err := r.check(ctx, appID)
	if err != nil {
		return state.Detection{}, err
	}
	return r.detect(ctx, app)
}

func (r *Runner) detect(ctx context.Context, app state.App) (state.Detection, error) {
	appID := app.ID

	// Each stage is recorded as it is reached, with the proposal as far as it
	// has got, so a person watching sees the app take shape rather than
	// waiting on a spinner for the whole of it. Status stays running; a
	// progress write that fails costs a stage of feedback, not the detection.
	ctx, prog := r.withProgress(ctx, appID)

	// Answers kept from an earlier run (Detections.Start keeps them): a build
	// argument somebody gave is built with this time.
	var answers map[string]string
	if d, err := r.Detections.Get(ctx, appID); err == nil {
		answers = d.Answers
	}

	proposal, err := r.run(ctx, prog, app, answers)
	if err != nil {
		// The failure is recorded rather than only returned: detection runs in
		// the background after app creation, and a user who comes back to the
		// console later needs to find out what happened.
		r.fail(ctx, appID, err)
		return state.Detection{}, err
	}

	rec, cancel := recording(ctx)
	defer cancel()
	if err := r.Detections.Save(rec, appID, proposal.Status, proposal, proposal.Commit); err != nil {
		// Recorded as failed rather than left running: a detection whose
		// result could not be stored is finished, and one marked running
		// forever is a spinner nobody can clear.
		_ = r.Detections.FailIfRunning(rec, appID, err)
		return state.Detection{}, err
	}
	return r.Detections.Get(rec, appID)
}

// recordTimeout bounds writing a detection's outcome.
const recordTimeout = 30 * time.Second

// recording is the context a detection's outcome is written under: the run's
// values without its deadline. A detection that reached its result as its time
// ran out used to lose it — the write failed on the expired deadline, and the
// app showed "Could not record the detection result" instead of its plan
// (issue #90).
func recording(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}

// Check reports whether detection may run for an app, without starting it: the
// app exists, has a source, and the source is allowed (R-092).
//
// Separate so a caller that runs detection in the background can refuse at
// once, and write nothing, when it would be refused anyway.
func (r *Runner) Check(ctx context.Context, appID string) error {
	_, err := r.check(ctx, appID)
	return err
}

func (r *Runner) check(ctx context.Context, appID string) (state.App, error) {
	app, found, err := r.Apps.ByID(ctx, appID)
	if err != nil {
		return state.App{}, err
	}
	if !found {
		return state.App{}, errs.New(errs.NotFound, "That app does not exist.")
	}

	if app.Source.Type == "" {
		return state.App{}, errs.New(errs.StateInvalid,
			"This app has no source to detect from.").
			WithRemedy("Create the app with a repository URL, or write a spec for it directly.")
	}

	if r.Policy != nil {
		if err := r.Policy.AllowsSource(ctx, app.Source); err != nil {
			return state.App{}, err
		}
	}
	return app, nil
}

func (r *Runner) run(ctx context.Context, prog *progress, app state.App, answers map[string]string) (detect.Proposal, error) {
	appID, slug, src := app.ID, app.Slug, app.Source
	detect.Report(ctx, detect.StageFetching, nil)
	checkout, err := r.Sources.Fetch(source.ForApp(ctx, appID), src)
	if err != nil {
		return detect.Proposal{}, err
	}
	defer checkout.Close()

	// After the auction: the source scan and the build at once, then the
	// image scan and the trial run at once (build.go).
	scans := r.newScans(appID, checkout.Identity(), prog)
	defer scans.stop()
	proposal, err := r.Job.Run(detect.WithBuild(ctx, r.buildHook(appID, slug, answers, checkout, scans)),
		appID, src, checkout.View(src.Subdir))
	if err != nil {
		return detect.Proposal{}, err
	}

	// A repository whose app is one of the projects inside it: the one a
	// person chose, or the only one it holds. Read again there, so the
	// proposal is that project's — its build, its workloads, its questions —
	// rather than the parent's, which has none (issue #90). Once: the
	// project's own reading does not ask which project it is.
	if dir, why, ok := proposal.Subproject(answers); ok {
		sub := src
		sub.Subdir = path.Join(src.Subdir, dir)
		if info, statErr := checkout.View(sub.Subdir).Stat("."); statErr == nil && info.IsDir {
			src = sub
			proposal, err = r.Job.Run(detect.WithBuild(ctx, r.buildHook(appID, slug, answers, checkout, scans)),
				appID, src, checkout.View(src.Subdir))
			if err != nil {
				return detect.Proposal{}, err
			}
			proposal.Winner.Evidence = append([]string{why}, proposal.Winner.Evidence...)
		}
	}

	// While the checkout exists, and after the proposal is in hand: a scan is
	// worth having and is not worth failing a detection for. Reported as its
	// own stage: a scan can take a while, and the onboarding page shows it as
	// a step of the plan.
	if scans.began() {
		detect.Report(ctx, detect.StageScanning, &proposal)
		scans.record(ctx)
	} else if r.Scanner != nil {
		// An app that runs a published image: the auction did not run, and
		// the image and the checkout are scanned together.
		if req, ok := r.scanRequest(ctx, appID, src, &proposal, checkout); ok {
			detect.Report(ctx, detect.StageScanning, &proposal)
			r.Scanner.ScanSource(ctx, req)
		}
	}

	// Fill in the install's own answers before the proposal is shown, not when
	// it is accepted. The review is where someone sees how their app will run,
	// and a draft that says nothing about routing or limits is not something
	// they can review — they would be approving blanks and finding out at
	// deploy time (R-102: the user sees the reasoning, not a verdict).
	if blocked := r.applyDefaults(ctx, &proposal.DraftSpec, slug); blocked != nil {
		// No port, no app: port-mode routing is how it is reached at all.
		// Carried as the proposal's blocking reason so that the review says
		// every port is taken and what to do about it, and accepting is
		// refused with the same words — rather than the spec being saved
		// portless and the deploy failing on "0 is not a usable port number".
		proposal.Blocked = blocked
	}

	// And to every other reading of the repository, because answering the
	// tie-break adopts one of them. A candidate completed only at accept time
	// is one whose review showed blanks.
	for i := range proposal.RunnersUp {
		if len(proposal.RunnersUp[i].Draft.Workloads) == 0 {
			continue
		}
		candidate := detect.Assemble(appID, src, proposal.RunnersUp[i].Draft)
		candidate.Source.Commit = checkout.Commit
		_ = r.applyDefaults(ctx, &candidate, slug)
		proposal.RunnersUp[i].Spec = &candidate
	}

	// R-120: Ref is what the user asked for, Commit is what runs. Recorded on
	// the proposal so that accepting it pins a revision against a specific
	// commit rather than against a branch that has since moved.
	proposal.Commit = checkout.Commit
	proposal.DraftSpec.Source.Commit = checkout.Commit

	// Step 12a — AI assistance (R-330, R-336, design 10 §4), only when the plan
	// failed or asked something. After the defaults, because an adapter handed
	// a spec with no routing mode and no limits is reviewing blanks for the
	// same reason a person would be. Before the proposal is stored, because
	// what is stored is what gets reviewed.
	//
	// The outcome is recorded whatever it is, including "nothing ran and here
	// is why". Screening never fails a detection (R-335), so there is nothing
	// to check here and that is the point.
	outcome := r.screen(ctx, appID, &proposal, checkout.View(src.Subdir))
	proposal.Screening = &outcome
	if outcome.Changed() {
		// An amendment can answer the last outstanding question, which turns
		// needs_answers into ready — the same recomputation the trial run gets.
		proposal.Status = detect.StatusFor(proposal.Winner, proposal.Questions)

		// And it can change what is built — a build argument it answered,
		// a Dockerfile it pointed at — so the image is built again for the
		// plan as it now stands, and scanned with the source already read.
		if s, ok := r.buildSpec(ctx, appID, slug, &proposal, answers, checkout.Commit); ok &&
			!proposal.Built.Matches(&s, checkout.Identity()) {
			if err := r.buildThenScan(ctx, &s, checkout, scans, &proposal); err != nil {
				return detect.Proposal{}, err
			}
		}
	}
	return proposal, nil
}

// applyDefaults folds the install's configuration into a draft spec.
//
// Isolation floors come from host policy rather than from the defaults struct,
// because policy is a floor and not a preference (R-272): an install that
// requires VM-class isolation must have every new app inherit that, not a
// value someone configured elsewhere.
// A returned error is a reason this reading of the repository cannot be
// accepted as it stands; nil is the ordinary case.
func (r *Runner) applyDefaults(ctx context.Context, s *spec.AppSpec, slug string) *errs.Error {
	if r.Install == nil {
		return nil
	}
	defaults := r.Install.Defaults(ctx)

	if r.Policy != nil {
		if build, runtime, err := r.Policy.IsolationFloors(ctx); err == nil {
			defaults.BuildIsolation = build
			defaults.RuntimeIsolation = runtime
		}
	}

	defaults.Apply(s, slug)

	// A port is the one routing field that cannot be derived from the app's
	// name: two apps called different things still collide if they are both
	// handed 9000. So it is allocated rather than defaulted, and only when the
	// mode that needs it is the one in effect.
	if s.Routing.Mode == spec.RoutingPort && s.Routing.Port == 0 && r.Ports != nil {
		port, err := r.Ports.Allocate(ctx, s.Routing.AdapterRef, s.AppID, r.PortRangeStart, r.PortRangeEnd)
		if err != nil {
			// Reported rather than left at zero. A zero port reached the user
			// as "0 is not a usable port number" at deploy time, which says
			// nothing about the range being full or about deleting an app;
			// Allocate's own error says both.
			if e := errs.As(err); e != nil {
				return e
			}
			return errs.Wrap(errs.Internal, "Could not assign this app a port.", err)
		}
		s.Routing.Port = port
	}
	return nil
}
