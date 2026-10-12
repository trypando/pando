package detection

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
)

// scanParts is detection's scan of what an app Pando builds will run, taken in
// two parts so neither waits on what it does not need: the source as soon as
// the auction has read it, beside the build, and the image once it is built,
// beside the trial run (R-311). Recorded as one scan of both, keyed by the
// source and the image's digest, which is the scan the first deploy of that
// image reuses (R-312).
//
// A nil *scanParts scans nothing: no scanner is configured.
type scanParts struct {
	scanner SourceScanner
	appID   string
	source  string
	prog    *progress

	wg sync.WaitGroup

	mu        sync.Mutex
	cancels   []context.CancelFunc
	dir       string
	image     string
	auth      *api.RegistryAuth
	results   map[string]api.ScanResult
	failed    error
	started   bool
	inFlight  int
	sourceRan bool
}

// The parts, in the order a scanner given both reports them.
const (
	partImage  = "image"
	partSource = "source"
)

func (r *Runner) newScans(appID, source string, prog *progress) *scanParts {
	if r.Scanner == nil || !r.Scanner.Scans() {
		return nil
	}
	return &scanParts{
		scanner: r.Scanner, appID: appID, source: source, prog: prog,
		results: map[string]api.ScanResult{},
	}
}

// startSource scans the checkout at dir, once. Nothing for an app with no
// checkout.
func (s *scanParts) startSource(ctx context.Context, dir string) {
	if s == nil || dir == "" {
		return
	}
	s.mu.Lock()
	if s.sourceRan {
		s.mu.Unlock()
		return
	}
	s.sourceRan, s.dir = true, dir
	s.mu.Unlock()
	s.run(ctx, partSource, api.ScanRequest{AppID: s.appID, SourceDir: dir, Commit: s.source})
}

// startImage scans the image detection built, replacing any earlier image's
// part: a build after screening, or after an answer, is a new image.
func (s *scanParts) startImage(ctx context.Context, image string, auth *api.RegistryAuth) {
	if s == nil || image == "" {
		return
	}
	s.mu.Lock()
	s.image, s.auth = image, auth
	delete(s.results, partImage)
	s.mu.Unlock()
	s.run(ctx, partImage, api.ScanRequest{AppID: s.appID, Image: image, PullAuth: auth, Commit: s.source})
}

// run scans one part in the background, until it finishes or stop cancels it.
func (s *scanParts) run(ctx context.Context, part string, req api.ScanRequest) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels = append(s.cancels, cancel)
	s.started = true
	s.inFlight++
	if s.inFlight == 1 {
		s.prog.during(detect.StageScanning, true)
	}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		result, err := s.scanner.ScanPart(ctx, req)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil && s.failed == nil {
			s.failed = err
		} else if err == nil {
			s.results[part] = result
		}
		if s.inFlight--; s.inFlight == 0 {
			s.prog.during(detect.StageScanning, false)
		}
	}()
}

// began reports whether any part was scanned.
func (s *scanParts) began() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// stop abandons the scans: the detection they were for failed.
func (s *scanParts) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// record waits for the parts under way and records them as one scan. False
// when nothing was scanned.
func (s *scanParts) record(ctx context.Context) bool {
	if s == nil {
		return false
	}
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || ctx.Err() != nil {
		return false
	}
	var parts []api.ScanResult
	for _, part := range []string{partImage, partSource} {
		if r, ok := s.results[part]; ok {
			parts = append(parts, r)
		}
	}
	req := api.ScanRequest{AppID: s.appID, Commit: s.source, Image: s.image, SourceDir: s.dir, PullAuth: s.auth}
	s.scanner.RecordParts(ctx, req, parts, s.failed)
	return true
}

// progress is what a running detection stores as it goes: the stage the rest
// waits on, the proposal as far as it has got, and every step under way —
// more than one while a scan runs beside the build or the trial.
type progress struct {
	mu      sync.Mutex
	save    func(body map[string]any)
	stage   string
	partial map[string]any
	side    map[string]bool
}

// withProgress is ctx with detection's stages recorded on appID's detection
// as they are reached.
func (r *Runner) withProgress(ctx context.Context, appID string) (context.Context, *progress) {
	p := &progress{side: map[string]bool{}}
	p.save = func(body map[string]any) {
		// Status stays running; a progress write that fails costs a stage of
		// feedback, not the detection.
		_ = r.Detections.Save(ctx, appID, state.DetectionRunning, body, "")
	}
	return detect.WithProgress(ctx, p.report), p
}

func (p *progress) report(stage string, partial *detect.Proposal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stage = stage
	if partial != nil {
		// Read now, while the caller holds it still: a scan finishing on
		// another goroutine writes progress too.
		p.partial = progressBody(stage, partial)
	}
	p.flush()
}

// during marks a step that runs beside the stage as under way, or finished.
func (p *progress) during(step string, on bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.side[step] = true
	} else {
		delete(p.side, step)
	}
	if p.stage != "" {
		p.flush()
	}
}

func (p *progress) flush() {
	body := map[string]any{}
	for k, v := range p.partial {
		body[k] = v
	}
	body["stage"] = p.stage
	body["running"] = runningSteps(p.stage, p.side)
	p.save(body)
}

// stageOrder is the order detection's steps happen in.
var stageOrder = map[string]int{
	detect.StageFetching: 0, detect.StageDetecting: 1, detect.StageBuilding: 2,
	detect.StageTrying: 3, detect.StageScanning: 4, detect.StageScreening: 5,
}

// runningSteps is every step under way, in the order they happen.
func runningSteps(stage string, side map[string]bool) []string {
	out := []string{stage}
	for step := range side {
		if step != stage {
			out = append(out, step)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return stageOrder[out[i]] < stageOrder[out[j]] })
	return out
}

// progressBody is what a running detection stores: the stage, and the
// proposal's fields as far as it has got. The same shape as a finished
// proposal plus "stage", so one reader serves both.
func progressBody(stage string, partial *detect.Proposal) map[string]any {
	body := map[string]any{}
	if partial != nil {
		if raw, err := json.Marshal(partial); err == nil {
			_ = json.Unmarshal(raw, &body)
		}
	}
	body["stage"] = stage
	return body
}
