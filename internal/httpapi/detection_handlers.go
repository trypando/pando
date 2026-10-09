package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/detection"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// handleGetDetection returns the current auction result (design 04 §2.2).
//
// runners_up is part of the response on purpose: R-102 is "ask, never guess",
// and showing what else bid is how a user sees the auction rather than being
// handed a verdict.
//
// `wait` makes it a long poll (issue #80): while detection is running, the
// answer is held until it reaches a new stage or finishes, or until the wait
// runs out. Every client waits this one way — the console, `pando app
// detection --wait` and pando_get_detection's wait_seconds (R-261).
func (s *Server) handleGetDetection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	wait, err := waitParam(r.URL.Query().Get("wait"))
	if err != nil {
		Error(w, r, err)
		return
	}

	d, err := detection.Await(r.Context(), s.Detections, s.clock(), app.ID, wait)
	if err != nil {
		Error(w, r, err)
		return
	}

	JSON(w, http.StatusOK, withPlannedAddress(r, app, s.detectionResponse(d), d))
}

// waitParam reads `wait`: a number of seconds, or a duration such as 30s.
// Empty is no wait. More than detection.MaxWait is held to it rather than
// refused, so a client asking for "as long as you will" gets exactly that.
func waitParam(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	wait, err := time.ParseDuration(raw)
	if err != nil {
		seconds, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return 0, errs.Newf(errs.ValidInvalid,
				"The wait parameter %q is not a length of time Pando can read.", raw).
				WithRemedy(fmt.Sprintf("Give a number of seconds up to %d, such as wait=30, or a duration such as wait=30s.",
					int(detection.MaxWait.Seconds())))
		}
		wait = time.Duration(seconds) * time.Second
	}
	if wait < 0 {
		return 0, errs.New(errs.ValidInvalid, "The wait parameter cannot be negative.").
			WithRemedy("Give a number of seconds, such as wait=30, or leave it out to answer at once.")
	}
	return min(wait, detection.MaxWait), nil
}

// withPlannedAddress adds where the app will be reachable once deployed, from
// the proposal's routing — which detection has already filled, port and all
// (Runner.applyDefaults). The same spec.Address the app's own page uses once
// it is configured, so the review and the running app name one address, and
// no client re-derives it (R-261).
func withPlannedAddress(r *http.Request, app state.App, out map[string]any, d state.Detection) map[string]any {
	p, err := decodeProposal(d)
	if err != nil {
		return out
	}
	if address := spec.Address(r.Host, app.Slug, p.DraftSpec.Routing); address != "" {
		out["address"] = address
	}
	return out
}

// handleReviseDetection asks the AI adapter to change the plan as a person
// described (R-336's third trigger, design 10 §4.3).
//
// Answered in the request rather than in the background, unlike a re-run: it is
// one bounded call to the adapter (R-339), and the person is waiting on the
// reply. The rules live in core/detection.Runner.Revise; this reads the body
// and hands it over.
func (s *Server) handleReviseDetection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	if s.Detector == nil {
		Error(w, r, errs.New(errs.AdapterUnavailable, "Detection is not configured on this install."))
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send what should change, for example {"message": "The app serves on port 8080."}.`))
		return
	}

	updated, err := s.Detector.Revise(r.Context(), app.ID, req.Message)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, withPlannedAddress(r, app, s.detectionResponse(updated), updated))
}

// handleRerunDetection re-detects, explicitly (R-022).
//
// Explicitly is the whole point. Detection never re-runs on its own: a spec
// that changed under someone because a file moved in their repository is a spec
// they did not write, and R-098 pins the reviewed proposal precisely so that
// cannot happen.
func (s *Server) handleRerunDetection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	if s.Detector == nil {
		Error(w, r, errs.New(errs.AdapterUnavailable, "Detection is not configured on this install."))
		return
	}

	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "detection.rerun",
		AppID:         app.ID,
		TargetKind:    "app",
		TargetID:      app.ID,
	})

	// Queued, the way detection on create is. A clone, a build plan and a
	// trial run held this request open for minutes (issue #55), and every
	// client already polls GET /detection for the outcome. Marked running
	// first, so the first poll cannot read the previous outcome as this one's;
	// the detection queue runs it where there is room (issue #72, O-32).
	//
	// What would refuse it is checked here, before anything is written: a
	// source the allowlist does not permit is refused with nothing recorded
	// and nothing cloned (R-092).
	if err := s.Detector.Check(r.Context(), app.ID); err != nil {
		Error(w, r, err)
		return
	}
	if s.DetectionQueue == nil {
		Error(w, r, errs.New(errs.AdapterUnavailable, "Detection is not configured on this install."))
		return
	}
	// The detection as queued: this response describes the request, not
	// the work, which may already have finished on another replica.
	d, err := s.DetectionQueue.Enqueue(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusAccepted, s.detectionResponse(d))
}

// handleDetectionDiff compares the proposal against the pinned spec (R-022).
//
// The same machinery as every other diff (design 01 §4), so a re-detection that
// would remove a volume is shown as destructive here exactly as it would be
// anywhere else.
func (s *Server) handleDetectionDiff(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}

	d, err := s.Detections.Get(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}

	proposal, err := decodeProposal(d)
	if err != nil {
		Error(w, r, err)
		return
	}

	if app.PinnedSpecID == "" {
		// Nothing pinned yet, so every part of the proposal is new. Saying so
		// is more useful than an empty diff that reads as "no changes".
		JSON(w, http.StatusOK, map[string]any{
			"app_id":     app.ID,
			"pinned":     nil,
			"changes":    nil,
			"is_first":   true,
			"draft_spec": proposal.DraftSpec,
		})
		return
	}

	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return
	}

	changes := spec.Compare(rev.Body, &proposal.DraftSpec)
	JSON(w, http.StatusOK, map[string]any{
		"app_id":   app.ID,
		"pinned":   rev.Revision,
		"changes":  changes,
		"is_first": false,
	})
}

type answersRequest struct {
	Answers map[string]string `json:"answers"`
}

// handleDetectionAnswers records answers to outstanding questions.
//
// R-105's workflow is a person pasting a question into the assistant that wrote
// their app and pasting the answer back, so answers arrive one or a few at a
// time and are merged rather than replacing what is there.
func (s *Server) handleDetectionAnswers(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}

	var req answersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	if len(req.Answers) == 0 {
		Error(w, r, errs.New(errs.ValidInvalid, "No answers were supplied.").
			WithRemedy(`Send answers keyed by question, for example {"answers": {"primary_port": "8080"}}.`))
		return
	}

	d, err := s.Detections.Get(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	proposal, err := decodeProposal(d)
	if err != nil {
		Error(w, r, err)
		return
	}

	// An answer to a question nobody asked is a mistake worth reporting rather
	// than storing: it usually means a typo in the key, and silently accepting
	// it would leave the real question unanswered and the deploy blocked with
	// no explanation.
	known := map[string]bool{}
	for _, q := range proposal.Questions {
		known[q.Key] = true
	}
	var unknown []string
	for key := range req.Answers {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		Error(w, r, errs.Newf(errs.ValidInvalid,
			"There are no outstanding questions with these keys: %v.", unknown).
			WithDetail("outstanding", keysOf(known)).
			WithRemedy("Use the `key` from one of the questions in GET /detection."))
		return
	}

	// And an answer that cannot become a spec is refused now, rather than
	// recorded and refused at accept as "This app has no workloads".
	if err := proposal.CheckAnswers(req.Answers); err != nil {
		Error(w, r, err)
		return
	}

	if err := s.Detections.SaveAnswers(r.Context(), app.ID, req.Answers); err != nil {
		Error(w, r, err)
		return
	}
	// The answers become the spec when the proposal is accepted, so who gave
	// them is recorded (R-391). Which questions, not the answers: an answer
	// can be a value somebody typed.
	answered := make([]string, 0, len(req.Answers))
	for k := range req.Answers {
		answered = append(answered, k)
	}
	sort.Strings(answered)
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "detection.answer", AppID: app.ID, TargetKind: "app", TargetID: app.ID,
		Detail: map[string]any{"questions": answered},
	})

	updated, err := s.Detections.Get(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, withPlannedAddress(r, app, s.detectionResponse(updated), updated))
}

// handleAcceptDetection pins the proposal as spec revision 1 (Sequence A 13–17).
//
// Accepting does not deploy. It pins a revision and writes the grants, and the
// app moves to `proposed` — deploying is a separate, deliberate act, which is
// the assertion Sequence A makes explicitly.
func (s *Server) handleAcceptDetection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}

	// Accepting onto an app that is already configured replaces that
	// configuration, and nothing warns about it afterwards: the new spec comes
	// from detection, so every environment variable, slot resolution and volume
	// added since is simply absent from it. The old revision survives —
	// spec_revisions is append-only (R-152) — but the app is pinned to one that
	// does not have them, and the next deploy ships that.
	//
	// Confirmed rather than refused, because re-accepting is a real thing to
	// want: the repository changed and detection should win. And confirmed on
	// the server rather than in the console, because the console is one client
	// of three (R-261) — a guard only the browser enforces is not a guard.
	var req struct {
		Confirm bool `json:"confirm"`

		// Values are variables the person set while reviewing — the ones
		// detection found and could not know the value of, and any they
		// added. Written into the accepted spec, so accepting and saving them
		// is one step rather than a configuration followed by an edit.
		Values []struct {
			Workload string `json:"workload"`
			Key      string `json:"key"`
			Value    string `json:"value"`
			// Secret stores the value through the secrets adapter and puts a
			// reference in the spec, as the Environment tab does.
			Secret bool `json:"secret"`
		} `json:"values"`

		// Optional names slots the person judged the app runs without, so an
		// empty one leaves its variable unset instead of refusing the deploy
		// (spec.MarkSlotOptional).
		Optional []string `json:"optional"`

		// Hostname is the address the person chose, where the app's routing
		// serves it at a hostname of its own (spec.SetHostname). Empty keeps
		// the one detection assigned.
		Hostname string `json:"hostname"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	for _, v := range req.Values {
		if v.Key == "" {
			Error(w, r, errs.New(errs.ValidInvalid, "A variable needs a name."))
			return
		}
		if v.Secret {
			// Writing a secret is its own verb (R-083), accepted or not.
			if err := s.Authz.CheckControl(r.Context(), PrincipalFrom(r.Context()), app.ID, authz.AppSecretsWrite); err != nil {
				Error(w, r, err)
				return
			}
		}
	}
	if app.PinnedSpecID != "" && !req.Confirm {
		Error(w, r, errs.New(errs.ValidInvalid,
			"This app is already configured, and accepting again replaces how it is built and what it runs with what detection found.").
			WithDetail("pinned_spec_id", app.PinnedSpecID).
			WithRemedy("Environment variables you set, how each dependency is filled, and storage you added are carried over. The build and the workloads come from the repository. Send confirm: true to go ahead."))
		return
	}

	d, err := s.Detections.Get(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	proposal, err := decodeProposal(d)
	if err != nil {
		Error(w, r, err)
		return
	}

	if proposal.Blocked != nil {
		Error(w, r, proposal.Blocked)
		return
	}

	// Every question a person still has to answer is a blocker. R-104: Pando
	// asks only when it genuinely cannot proceed, so an outstanding question is
	// by definition something without which the app cannot run.
	if outstanding := unanswered(proposal, d.Answers); len(outstanding) > 0 {
		Error(w, r, errs.Newf(errs.StateInvalid,
			"This proposal still has %d unanswered question(s).", len(outstanding)).
			WithDetail("questions", outstanding).
			WithRemedy("Answer them with POST /detection/answers, then accept again."))
		return
	}

	draft := proposal.WithAnswers(d.Answers)
	draft.AppID = app.ID

	// The install's defaults, applied to whatever draft this turned out to be.
	//
	// Detection applies them to the winning draft only. Answering the tie-break
	// adopts a *different* candidate's draft, which never went through that —
	// so accepting one produced a spec with no builder, no isolation floor and
	// no timeout, and the next deploy failed on a builder named "". Found by
	// deploying it, not by a test.
	if s.Defaults != nil {
		s.Defaults.Defaults(r.Context()).Apply(&draft, app.Slug)
	}

	// What a person decided outlives a re-detection (R-022).
	//
	// A proposal describes the repository, not the app: every slot arrives
	// unfilled, and it carries none of the environment somebody typed. Pinning
	// it as-is meant that re-detecting after adding a Dockerfile — the ordinary
	// thing to do — unfilled the database slot and dropped the variables, while
	// the app kept running on the old spec and said nothing. The next deploy
	// was the first sign.
	var pinned *spec.AppSpec
	if app.PinnedSpecID != "" {
		if rev, found, revErr := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID); revErr == nil && found {
			pinned = rev.Body
			draft = *spec.Carry(rev.Body, &draft)
		}
	}

	// The values set during review. A secret goes to the secrets adapter first
	// and the spec gets only its name, so the pinned spec is safe to export.
	for _, v := range req.Values {
		// A variable filled from a slot gets its value on the slot, as a
		// secret, so the slot is filled and the deploy is not refused for it
		// (R-132, spec.FillSlotLiteral).
		if slot, ok := spec.EnvSlot(&draft, v.Workload, v.Key); ok {
			if !v.Secret {
				spec.FillSlotValue(&draft, slot, v.Value)
				continue
			}
			ref := spec.SlotSecretKey(slot)
			if err := s.Secrets.Put(r.Context(), app.ID, ref, secret.New(v.Value)); err != nil {
				Error(w, r, err)
				return
			}
			spec.FillSlotLiteral(&draft, slot, ref)
			continue
		}

		entry := spec.EnvEntry{}
		if v.Secret {
			if err := s.Secrets.Put(r.Context(), app.ID, v.Key, secret.New(v.Value)); err != nil {
				Error(w, r, err)
				return
			}
			ref := v.Key
			entry.SecretRef = &ref
		} else {
			value := v.Value
			entry.Value = &value
		}
		spec.SetEnv(&draft, v.Workload, v.Key, entry)
	}
	for _, key := range req.Optional {
		spec.MarkSlotOptional(&draft, key)
	}
	if req.Hostname != "" {
		if err := spec.SetHostname(&draft, req.Hostname); err != nil {
			Error(w, r, err)
			return
		}
	}

	// Refused here rather than pinned and found at deploy. A spec that cannot
	// deploy was being stored and reported much later, in the deploy's words:
	// an app with port-mode routing and no port arrived as "0 is not a usable
	// port number" with nothing saying which step went wrong.
	if err := spec.Validate(&draft); err != nil {
		Error(w, r, err)
		return
	}

	// Carry keeps the pinned spec's egress, so this asks nothing of an
	// ordinary re-detection. It is here for a proposal that brings egress of
	// its own — the same rules as a spec written by hand (R-184, R-158).
	if _, err := s.gateSpec(r, app, pinned, &draft); err != nil {
		Error(w, r, err)
		return
	}

	p := PrincipalFrom(r.Context())
	rev, err := s.Apps.CreateRevision(r.Context(), app.ID, &draft, spec.OriginDetected, p.UserID)
	if err != nil {
		Error(w, r, err)
		return
	}

	// Pinning moves the app to `proposed`, not to anything running. Accepting a
	// proposal is not deploying it — that is a separate, deliberate act, and
	// Sequence A asserts it.
	if err := s.Apps.Pin(r.Context(), app.ID, rev.ID, state.StateProposed, p.UserID); err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "spec.pin",
		AppID:         app.ID,
		TargetKind:    "spec_revision",
		TargetID:      rev.ID,
		Detail:        map[string]any{"revision": rev.Revision, "origin": string(spec.OriginDetected)},
	})

	JSON(w, http.StatusOK, map[string]any{
		"app_id":   app.ID,
		"spec_id":  rev.ID,
		"revision": rev.Revision,
		"state":    state.StateProposed,
		"deployed": false,
	})
}

// --- helpers ---------------------------------------------------------------

func (s *Server) detectionResponse(d state.Detection) map[string]any {
	out := map[string]any{
		"status":     d.Status,
		"detection":  json.RawMessage(d.Body),
		"answers":    d.Answers,
		"commit":     d.Commit,
		"started_at": d.StartedAt,
		"updated_at": d.UpdatedAt,
	}
	// While running: the stage, beside the status rather than only inside the
	// partial proposal, and how long it has been going by the server's clock,
	// not the caller's (issue #80). Elapsed time rather than an estimate: the
	// trial run (R-097) dominates and varies too widely for one to be honest.
	if d.Status == state.DetectionRunning {
		var body struct {
			Stage string `json:"stage"`
		}
		_ = json.Unmarshal(d.Body, &body)
		if body.Stage != "" {
			out["stage"] = body.Stage
		}
		out["elapsed_seconds"] = max(0, int(s.clock().Since(d.StartedAt).Seconds()))
	}
	// The keys of the questions still waiting for an answer. Status stays
	// needs_answers until accept, because answers are applied then, so status
	// alone could not tell "waiting for answers" from "answered, ready to
	// accept" (issue #55). An empty list is the second.
	if d.Status == state.DetectionNeedsAnswers {
		keys := []string{}
		if p, err := decodeProposal(d); err == nil {
			if open := unanswered(p, d.Answers); open != nil {
				keys = open
			}
		}
		out["unanswered"] = keys
	}
	return out
}

func decodeProposal(d state.Detection) (detect.Proposal, error) {
	var p detect.Proposal
	if len(d.Body) == 0 {
		return p, errs.New(errs.StateInvalid, "Detection has not finished for this app yet.").
			WithRemedy("Wait for detection to finish, then try again.")
	}
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return p, errs.Wrap(errs.Internal, "The stored detection result could not be read.", err)
	}
	return p, nil
}

// unanswered returns the questions a person still has to answer.
//
// Deferred questions are excluded: those belong to the trial run (R-097), and
// one still marked deferred by the time a proposal is accepted was resolved by
// observation rather than left hanging.
func unanswered(p detect.Proposal, answers map[string]string) []string {
	// An AI adapter's suggestion counts as an answer (R-338).
	var out []string
	for _, q := range detect.Open(p.Questions, answers) {
		out = append(out, q.Key)
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// DetectionQueue queues a detection to run on whichever replica has room
// (detection.Queue). Enqueue returns the detection as queued, read before any
// replica can claim it: a detection that fails at once would otherwise finish
// between queueing it and reading it back, and a 202 would report the outcome
// of work it says has only begun.
type DetectionQueue interface {
	Enqueue(ctx context.Context, appID string) (state.Detection, error)
}

// Detector runs detection for an app.
//
// An interface rather than the concrete job, so the HTTP layer does not have to
// know how source is fetched or which adapters are involved — and so a test can
// assert what the endpoints do without a network, a builder or a daemon.
type Detector interface {
	Detect(ctx context.Context, appID string) (state.Detection, error)

	// Check refuses what Detect would refuse before it starts, without
	// writing anything.
	Check(ctx context.Context, appID string) error

	// Revise changes a finished proposal as a person asked, through the AI
	// adapter, and records the exchange on it (R-336).
	Revise(ctx context.Context, appID, message string) (state.Detection, error)
}

// clock is the server's time source.
func (s *Server) clock() clock.Clock {
	if s.Clock == nil {
		return clock.System{}
	}
	return s.Clock
}
