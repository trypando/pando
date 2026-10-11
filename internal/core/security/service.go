package security

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Service runs scans and answers "where does this app stand".
//
// The adapter produces findings, this produces the score, records it, and tells
// the audit log. Nothing here knows what a CVE is and nothing in the adapter
// knows what a threshold is (R-251).
type Service struct {
	Scans       *state.Scans
	Deployments *state.Deployments
	Registry    *api.Registry
	Policy      PolicyLoader
	Auditor     Auditor
	Logger      *zap.Logger

	// running is every scan in progress, by app. In memory rather than in
	// app_scans: that table is append-only and holds what a scan found, and a
	// scan that has not finished has found nothing (R-319). A scan also dies
	// with the process that runs it, so a row saying "running" would outlive
	// the scan it described after a restart, and a map cannot.
	mu      sync.Mutex
	running map[string]*inProgress
}

// inProgress is one app's running scans: when the first began, and how many.
// More than one is possible — a deploy's scan and a person's "Scan now" — and
// the app is scanning until the last of them ends.
type inProgress struct {
	since time.Time
	count int
}

// PolicyLoader is host policy, narrowed to the one call this needs.
type PolicyLoader interface {
	Document(ctx context.Context) (policy.Document, error)
}

// Auditor records what happened (R-319).
type Auditor interface {
	Write(ctx context.Context, e audit.Event) error
}

// Report is an app's security standing, with the scan behind it.
type Report struct {
	Standing Standing      `json:"standing"`
	Scan     *state.Scan   `json:"scan,omitempty"`
	Counts   Counts        `json:"counts"`
	Worst    []api.Finding `json:"worst,omitempty"`

	// Scanner is what would run, or what did. Empty when the installation has
	// none configured, which is what makes a threshold inert (R-317).
	Scanner string `json:"scanner,omitempty"`

	// ScanningSince is when a scan of this app that has not finished began,
	// and absent when none is running. Whoever started it: a deploy, detection,
	// or somebody asking — so every client can show that a scan is under way
	// rather than only the one that asked for it (R-261).
	ScanningSince *time.Time `json:"scanning_since,omitempty"`

	// IgnoringUnfixable says the score and the list above leave out findings
	// with no fix, because host policy says so (R-313). Said rather than
	// implied: a list somebody cannot explain the length of is a list they
	// stop trusting.
	IgnoringUnfixable bool `json:"ignoring_unfixable"`
}

// Configured reports whether this installation can scan at all.
func (s *Service) Configured() (string, bool) {
	if s == nil || s.Registry == nil {
		return "", false
	}
	_, ref, ok := s.Registry.DefaultScanner()
	return ref, ok
}

// Scan runs a scan and records it, whatever the outcome.
//
// A failure is recorded as a scan with no score rather than swallowed: a
// scanner that cannot run is a thing somebody has to fix, and silence would
// leave an unscannable app looking clean (R-318).
func (s *Service) Scan(ctx context.Context, req api.ScanRequest, principal audit.Event) (state.Scan, error) {
	scanner, ref, ok := s.Registry.DefaultScanner()
	if !ok {
		return state.Scan{}, errs.New(errs.AdapterUnavailable,
			"This installation has no security scanner configured, so there is nothing to scan with.").
			WithRemedy("Configure a scanner adapter, or ask whoever administers this installation to.")
	}

	done := s.begin(req.AppID)
	defer done()

	result, err := scanner.Scan(ctx, req)
	if err != nil {
		recorded, recordErr := s.Scans.Record(ctx, state.Scan{
			AppID: req.AppID, SpecID: req.SpecID, Commit: req.Commit, Image: req.Image, ScannerRef: ref,
			Error: messageOf(err), RanAt: time.Now().UTC(),
		})
		if recordErr != nil {
			return state.Scan{}, recordErr
		}
		s.audit(ctx, principal, "app.scan.failed", req.AppID, map[string]any{
			"scanner": ref, "reason": messageOf(err),
		})
		return recorded, err
	}

	// Both numbers, at the moment the findings are in hand. Which one an
	// installation means is policy's, and policy changes without rescanning —
	// so deriving the second later would mean either a rescan or re-deriving it
	// on every read of every row in a list.
	findings := Ranked(result.Findings)
	score := Score(findings)
	fixable := Score(Fixable(findings))

	recorded, err := s.Scans.Record(ctx, state.Scan{
		AppID:        req.AppID,
		SpecID:       req.SpecID,
		Commit:       req.Commit,
		Image:        req.Image,
		ScannerRef:   ref,
		Scanner:      result.Scanner,
		Score:        &score,
		ScoreFixable: &fixable,
		Findings:     findings,
		RanAt:        result.Ran,
	})
	if err != nil {
		return state.Scan{}, err
	}

	counts := Count(result.Findings)
	s.audit(ctx, principal, "app.scan", req.AppID, map[string]any{
		"scanner":  ref,
		"score":    score,
		"critical": counts.Critical,
		"high":     counts.High,
		"medium":   counts.Medium,
		"low":      counts.Low + counts.Unknown,
		"spec_id":  req.SpecID,
	})
	return recorded, nil
}

// begin marks a scan of appID as running, and returns the call that marks it
// finished.
func (s *Service) begin(appID string) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]*inProgress{}
	}
	p, ok := s.running[appID]
	if !ok {
		p = &inProgress{since: time.Now().UTC()}
		s.running[appID] = p
	}
	p.count++

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if p.count--; p.count == 0 {
				delete(s.running, appID)
			}
		})
	}
}

// Scanning reports when a scan of appID that is still running began, and
// whether one is.
func (s *Service) Scanning(appID string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.running[appID]
	if !ok {
		return time.Time{}, false
	}
	return p.since, true
}

// Reuse finds a successful scan of this app's source and image by the scanner
// that would run now, and makes it the scan of specID: a deploy of that source
// and image uses it rather than scanning the same thing again (R-312, design 09
// §4.1).
//
// Source is what names the source exactly — a commit, an image digest, an
// upload's archive digest — and image is what the deploy built or pulled, by
// digest, or empty when it has none. The scan reused has to have read both
// (R-311): detection's scan of the source alone does not stand in for the image
// a deploy built from it, and neither does a scan of another build of the same
// commit. An image named by anything but its digest is never reused, since the
// name says nothing about what is in it. A scan by another scanner does not
// count either: its findings are not the ones this installation's scanner would
// report. Found is false when there is nothing to reuse, and the caller scans.
//
// A scan already attached to specID is returned as it is. One attached to
// another revision, or to none, is recorded again against specID, so the
// threshold check and every screen that asks for the revision's score find it.
func (s *Service) Reuse(ctx context.Context, appID, specID, source, image string) (state.Scan, bool, error) {
	if image != "" && !ContentAddressed(image) {
		return state.Scan{}, false, nil
	}
	ref, configured := s.Configured()
	if !configured {
		return state.Scan{}, false, nil
	}
	found, ok, err := s.Scans.ForSource(ctx, appID, source, image, ref)
	if err != nil || !ok {
		return state.Scan{}, false, err
	}
	if specID == "" {
		return found, true, nil
	}

	current, has, err := s.Scans.Latest(ctx, appID, specID)
	if err != nil {
		return state.Scan{}, false, err
	}
	if has && current.SpecID == specID && current.Origin() == found.Origin() {
		return current, true, nil
	}
	reused, err := s.Scans.Reuse(ctx, found, specID)
	if err != nil {
		return state.Scan{}, false, err
	}
	return reused, true, nil
}

// ContentAddressed reports whether an image reference names its content: an
// image ID (sha256:…) or a reference pinned by digest (repo@sha256:…). Only
// such a name says two images are the same image.
func ContentAddressed(image string) bool {
	return strings.HasPrefix(image, "sha256:") || strings.Contains(image, "@sha256:")
}

// Report is where an app stands right now.
//
// The scan of the revision it is running, not the newest scan of the app: an
// app whose newest scan is of a revision it is not running has not been scanned
// in the sense that matters.
func (s *Service) Report(ctx context.Context, appID, specID string) (Report, error) {
	doc, err := s.Policy.Document(ctx)
	if err != nil {
		return Report{}, err
	}

	ref, configured := s.Configured()

	scan, found, err := s.Scans.Latest(ctx, appID, specID)
	if err != nil {
		return Report{}, err
	}

	security, _, err := s.Scans.SecurityStateFor(ctx, appID)
	if err != nil {
		return Report{}, err
	}

	report := Report{Scanner: ref}
	if since, running := s.Scanning(appID); running {
		report.ScanningSince = &since
	}
	if found {
		// What policy counts is what the reader sees. An installation that
		// ignores findings with no fix gets a score and a list that agree —
		// showing findings that did not count would leave somebody trying to
		// work out why fixing one changed nothing.
		applied := effective(doc, scan)
		report.Scan = &applied
		report.Counts = Count(applied.Findings)
		report.Worst = Worst(applied.Findings, 5)
		report.IgnoringUnfixable = doc.IgnoreUnfixableFindings
		report.Standing = Evaluate(doc, applied.Score, applied.RanAt, configured, security.InsecureSince)
		return report, nil
	}

	report.Standing = Evaluate(doc, nil, time.Time{}, configured, security.InsecureSince)
	return report, nil
}

// Place puts a set of already-fetched scores against policy, in one pass.
//
// For a list: the alternative is a policy load and a scanner lookup per row,
// which is the shape that turns a page of twenty apps into forty queries. The
// scores come from the caller because the list query already fetched them.
func (s *Service) Place(ctx context.Context, scores map[string]Scores) (map[string]Placed, error) {
	doc, err := s.Policy.Document(ctx)
	if err != nil {
		return nil, err
	}
	_, configured := s.Configured()

	out := make(map[string]Placed, len(scores))
	for appID, pair := range scores {
		score := pair.All
		if doc.IgnoreUnfixableFindings && pair.Fixable != nil {
			score = pair.Fixable
		}
		_, scanning := s.Scanning(appID)
		out[appID] = Placed{
			Score:    score,
			Verdict:  Evaluate(doc, score, time.Time{}, configured, nil).Verdict,
			Scanning: scanning,
		}
	}
	return out, nil
}

// Scores is one app's two numbers, as the list query returns them.
type Scores struct {
	All     *int
	Fixable *int
}

// Placed is the number this installation means, and what it means.
type Placed struct {
	Score   *int
	Verdict Verdict

	// Scanning says a scan of the app is running now, so a list can show it
	// rather than "Not scanned" until the scan ends.
	Scanning bool
}

// Allows reports whether this revision may be deployed (R-314).
//
// Called from the deploy path with the score of the image that was just built,
// and from the planner with whatever is already known about the revision.
func (s *Service) Allows(ctx context.Context, appID, specID string) (Standing, error) {
	doc, err := s.Policy.Document(ctx)
	if err != nil {
		return Standing{}, err
	}
	_, configured := s.Configured()

	scan, found, err := s.Scans.Latest(ctx, appID, specID)
	if err != nil {
		return Standing{}, err
	}
	if !found {
		return Evaluate(doc, nil, time.Time{}, configured, nil), nil
	}
	applied := effective(doc, scan)
	return Evaluate(doc, applied.Score, applied.RanAt, configured, nil), nil
}

// effective is the scan as this installation's policy counts it.
//
// A scan carries both numbers and every finding; policy decides which number is
// the score and which findings are shown. The pair moves together on purpose.
func effective(doc policy.Document, scan state.Scan) state.Scan {
	// Ranked on the way out as well as on the way in: a scan recorded before
	// findings were ordered is still read worst-first (R-313b), and ordering a
	// list that is already ordered costs nothing.
	scan.Findings = Ranked(scan.Findings)

	if !doc.IgnoreUnfixableFindings {
		return scan
	}

	scan.Findings = Fixable(scan.Findings)
	if scan.ScoreFixable != nil {
		scan.Score = scan.ScoreFixable
		return scan
	}

	// A scan written before the second number existed. Deriving it from the
	// findings is exact — they are all here — and costs one pass.
	derived := Score(scan.Findings)
	scan.Score = &derived
	return scan
}

// Refusal is the plan-time error for an app that may not be deployed.
//
// Written here rather than at the call site so the message is the same wherever
// the refusal comes from, and so the three findings that cost the most travel
// with it — R-105: an error somebody can act on, or paste into the assistant
// that wrote the app.
func Refusal(standing Standing, worst []api.Finding) error {
	if standing.Verdict == VerdictUnscanned {
		return errs.New(errs.PlanSecurityBelowThreshold,
			"This installation requires a security score and this app has never been scanned.").
			WithRemedy("Scan it from the app's settings, or ask whoever administers this installation about the requirement.").
			WithDetail("threshold", standing.Threshold)
	}

	score := 0
	if standing.Score != nil {
		score = *standing.Score
	}
	err := errs.Newf(errs.PlanSecurityBelowThreshold,
		"This app scores %d and this installation requires at least %d.", score, standing.Threshold).
		WithRemedy("Fix the findings below and deploy again, or ask whoever administers this installation to change the requirement.").
		WithDetail("score", score).
		WithDetail("threshold", standing.Threshold)

	if len(worst) > 0 {
		summary := make([]map[string]string, 0, len(worst))
		for _, f := range worst {
			summary = append(summary, map[string]string{
				"id": f.ID, "severity": string(f.Severity), "title": f.Title, "target": f.Target, "fix": f.Fix,
			})
		}
		err = err.WithDetail("findings", summary)
	}
	return err
}

func (s *Service) audit(ctx context.Context, base audit.Event, action, appID string, detail map[string]any) {
	if s.Auditor == nil {
		return
	}
	event := base
	event.Action = action
	event.AppID = appID
	event.TargetKind = "app"
	event.TargetID = appID
	event.Detail = detail

	if err := s.Auditor.Write(ctx, event); err != nil && s.Logger != nil {
		s.Logger.Warn("could not record a security event", zap.Error(err), zap.String("action", action))
	}
}

func messageOf(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
