package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/logstream"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Small helpers so handlers name adapter types in one place.
func apiBundleRef(appID string) api.BundleRef { return api.BundleRef{BundleID: appID} }
func apiWorkloadRef(appID, workload string) api.WorkloadRef {
	return api.WorkloadRef{BundleID: appID, Workload: workload}
}
func apiLogOptions(follow bool, tail int) api.LogOptions {
	return api.LogOptions{Follow: follow, Tail: tail}
}

type deployRequest struct {
	SpecRevision int    `json:"spec_revision"`
	Trigger      string `json:"trigger"`
}

// handleDeploy starts a deployment.
//
// Returns 202 and runs the pipeline in the background: a build can take minutes,
// and holding the request open for it would make every client's timeout a
// deployment timeout. The deployment ID comes back immediately and its logs
// stream from their own endpoint.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppDeploy)
	if !ok {
		return
	}

	var req deployRequest
	idempotencyKey, err := decodeWithKey(r, &req)
	if err != nil {
		Error(w, r, err)
		return
	}

	// A retry of a deploy that already happened replays the first answer rather
	// than deploying again. This is what makes the endpoint safe to give an
	// agent (R-262): an agent retries on a timeout, and a deploy resolves a ref
	// — which means cloning — so it regularly outlasts a client's patience.
	if s.replayed(w, r, idempotencyKey, "POST /apps/{id}/deployments") {
		return
	}

	// Two deploys racing on one bundle is how an app ends up in a state neither
	// intended. Refused rather than queued — a queue on a fast-moving branch
	// produces a backlog nobody wants (R-141's reasoning applies here too).
	inFlight, err := s.Deployments.InFlight(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if inFlight {
		Error(w, r, errs.New(errs.StateInvalid, "This app is already deploying.").
			WithRemedy("Wait for the current deploy to finish, then try again."))
		return
	}

	rev, err := s.revisionToDeploy(r, app, req.SpecRevision)
	if err != nil {
		Error(w, r, err)
		return
	}

	p := PrincipalFrom(r.Context())

	// Resolving a ref to a commit is an explicit act that produces a revision,
	// never something the deploy does on its own (design 01 §2.1).
	prepared, err := s.Deployer.PrepareRevision(r.Context(), rev, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if prepared.ID != rev.ID {
		s.auditApp(r, app.ID, "spec.create")
	}

	trigger := state.TriggerManual
	if req.Trigger == state.TriggerRollback {
		trigger = state.TriggerRollback
	}

	// Planned first, so a deploy that cannot succeed is refused before
	// anything is created; then started, or — when the deploy needs
	// somebody's approval (R-154) — recorded as a request waiting for it,
	// with the app left as it is. Either way the answer is 202 and the
	// deployment.
	dep, err := s.Approvals.Deploy(r.Context(), p, app, prepared, trigger)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.remember(r, idempotencyKey, "POST /apps/{id}/deployments", http.StatusAccepted, dep)
	deps := []state.Deployment{dep}
	if err := s.Approvals.Describe(r.Context(), p, deps); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusAccepted, deps[0])
}

func (s *Server) revisionToDeploy(r *http.Request, app state.App, requested int) (state.Revision, error) {
	if requested > 0 {
		rev, found, err := s.Apps.RevisionByNumber(r.Context(), app.ID, requested)
		if err != nil {
			return state.Revision{}, err
		}
		if !found {
			return state.Revision{}, errs.Newf(errs.NotFound, "This app has no revision %d.", requested)
		}
		return rev, nil
	}

	if app.PinnedSpecID == "" {
		return state.Revision{}, errs.New(errs.StateInvalid, "This app has no spec to deploy yet.").
			WithRemedy("Write a spec for the app and pin it first.")
	}
	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil {
		return state.Revision{}, err
	}
	if !found {
		return state.Revision{}, errs.New(errs.NotFound, "This app's pinned spec is missing.")
	}
	return rev, nil
}

// handleRollback deploys a previous revision (R-152).
//
// Rollback is repointing at a revision that provably existed — the same
// machinery as any other deploy, with a different trigger recorded.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppDeploy)
	if !ok {
		return
	}

	var req struct {
		To int `json:"to"`

		// What `pando rollback --to` sends. Read as well as "to", which it
		// used to be ignored in favor of: the CLI's --to rolled back to the
		// previous revision whatever it named.
		SpecRevision int `json:"spec_revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.To <= 0 {
		req.To = req.SpecRevision
	}

	if req.To <= 0 {
		revs, err := s.Apps.ListRevisions(r.Context(), app.ID)
		if err != nil {
			Error(w, r, err)
			return
		}
		// The previous revision that was actually live. A revision that was
		// never deployed is not something to roll back to.
		for _, rev := range revs {
			if rev.ID != app.PinnedSpecID && rev.EverPinned {
				req.To = rev.Revision
				break
			}
		}
		if req.To == 0 {
			Error(w, r, errs.New(errs.StateInvalid, "There is no earlier version of this app to go back to."))
			return
		}
	}

	body, _ := json.Marshal(deployRequest{SpecRevision: req.To, Trigger: state.TriggerRollback})
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.handleDeploy(w, r)
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	deps, err := s.Deployments.ListForApp(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if err := s.Approvals.Describe(r.Context(), PrincipalFrom(r.Context()), deps); err != nil {
		Error(w, r, err)
		return
	}
	if err := s.Deployments.QueuePositions(r.Context(), deps); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"deployments": deps})
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	dep, found, err := s.Deployments.ByID(r.Context(), chi.URLParam(r, "depID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found || dep.AppID != app.ID {
		Error(w, r, errs.New(errs.NotFound, "There is no such deploy for this app."))
		return
	}
	deps := []state.Deployment{dep}
	if err := s.Deployments.QueuePositions(r.Context(), deps); err != nil {
		Error(w, r, err)
		return
	}
	if err := s.Approvals.Describe(r.Context(), PrincipalFrom(r.Context()), deps); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, deps[0])
}

// --- deploy approval (R-154 – R-159) ----------------------------------------

type decisionRequest struct {
	Comment string `json:"comment"`
}

// handleApproveDeploy approves a deploy waiting for approval, and starts it
// when that is the last approval it needs. Who may is the approval service's
// question (R-155): app.view gets the caller this far, so that somebody who
// cannot see the app is told it does not exist rather than that they may not
// approve it.
func (s *Server) handleApproveDeploy(w http.ResponseWriter, r *http.Request) {
	s.decideDeploy(w, r, s.Approvals.Approve)
}

// handleRejectDeploy rejects a deploy waiting for approval, which ends the
// request (R-156).
func (s *Server) handleRejectDeploy(w http.ResponseWriter, r *http.Request) {
	s.decideDeploy(w, r, s.Approvals.Reject)
}

func (s *Server) decideDeploy(w http.ResponseWriter, r *http.Request,
	decide func(ctx context.Context, p authz.Principal, appID, depID, comment string) (state.Deployment, error)) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	var req decisionRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read. Send {\"comment\": \"...\"}, or nothing."))
			return
		}
	}
	dep, err := decide(r.Context(), PrincipalFrom(r.Context()), app.ID, chi.URLParam(r, "depID"), req.Comment)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, dep)
}

// handleListApprovals lists the deploys waiting for approval on every app the
// caller may view, each saying whether the caller may decide it. Signed in and
// nothing more: what comes back is already limited to apps the caller sees.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}
	page, err := pageFrom(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	waiting, next, err := s.Approvals.Awaiting(r.Context(), p, page)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"approvals": waiting, "next_cursor": next})
}

// handleDeploymentLogs streams build output as server-sent events.
//
// Unbuffered and flushed per line (R-170): a build log that arrives in one lump
// at the end is not a live log, and the whole point is watching a build happen.
func (s *Server) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppLogsRead)
	if !ok {
		return
	}

	depID := chi.URLParam(r, "depID")
	dep, found, err := s.Deployments.ByID(r.Context(), depID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found || dep.AppID != app.ID {
		Error(w, r, errs.New(errs.NotFound, "There is no such deploy for this app."))
		return
	}

	rc := http.NewResponseController(w)

	// Queued: no replica holds its log yet, so this one says where it is in
	// the queue until one takes it (issue #93), then carries on below with
	// the stream already open.
	open := false
	if dep.Status == state.DeployPending && r.Header.Get(relayHeader) == "" {
		var done bool
		open, done = s.waitInQueue(w, r, rc, app.ID, depID)
		if done {
			return
		}
	}

	// A deploy's live log is in the memory of the replica running it (issue
	// #72). Asked of another replica, the request goes there: the load
	// balancer chose this replica, not the deploy.
	if open {
		if s.relayDeployLogBody(w, r, rc, depID) {
			return
		}
	} else if s.relayDeployLog(w, r, depID) {
		return
	}

	if !open {
		startEventStream(w)
	}
	if dep.FinishedAt != nil && !s.Logs.Has(depID) {
		// Run by a replica that has since stopped, by this one before a
		// restart, or long enough ago that the log was dropped from memory
		// (deploy.LogRetention). Following would wait for lines that cannot
		// come.
		fmt.Fprint(w, "data: The live log of this deploy is no longer held. Pando keeps it in the memory of the Pando process that ran the deploy, and only while the deploy runs and for a few minutes after it ends. The deploy's outcome and error are on the deploy itself.\n\n")
		fmt.Fprint(w, "event: end\ndata: \n\n")
		_ = rc.Flush()
		return
	}
	backlog, updates, cancel := s.Logs.Follow(depID)
	defer cancel()

	for _, line := range backlog {
		// G705: not an HTML context. The response is text/event-stream, set
		// above before anything is written, and a browser never parses an SSE
		// frame as markup. The console renders these lines as text.
		fmt.Fprintf(w, "data: %s\n\n", line) //nolint:gosec
	}
	_ = rc.Flush()

	// A deploy can run for many minutes, and a stream checked once at the
	// start would keep showing its log to someone whose access was revoked
	// meanwhile (R-048). Checked again as the app log stream is (O-51).
	every := s.DeployLogReauthEvery
	if every <= 0 {
		every = logstream.DefaultReauthEvery
	}
	reauth := time.NewTicker(every)
	defer reauth.Stop()
	allowed := s.stillAllowed(r, app.ID, authz.AppLogsRead)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-reauth.C:
			if err := allowed(r.Context()); err != nil {
				// A line and then the stream's ordinary end, which every
				// client of this endpoint already stops on: the deploy log
				// is Pando's own output, so a line from Pando belongs in it.
				fmt.Fprint(w, "data: Your access to this app's logs has ended, so Pando stopped showing this deploy's log. "+
					"Someone with permission to manage the app's access can grant app.logs.read again.\n\n")
				fmt.Fprint(w, "event: end\ndata: \n\n")
				_ = rc.Flush()
				return
			}
		case line, open := <-updates:
			if !open {
				fmt.Fprint(w, "event: end\ndata: \n\n")
				_ = rc.Flush()
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			_ = rc.Flush()
		}
	}
}

func startEventStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// queueLine is what a queued deploy's log says while it waits (issue #93).
func queueLine(ahead int) string {
	switch ahead {
	case 0:
		return "=> Waiting for a build slot: this deploy is next."
	case 1:
		return "=> Waiting for a build slot: 1 deploy ahead of this one."
	default:
		return fmt.Sprintf("=> Waiting for a build slot: %d deploys ahead of this one.", ahead)
	}
}

// waitInQueue streams a queued deploy's place in the queue, a line each time
// it changes, until a replica takes the deploy. open reports whether the
// event stream was started; done, that the response is finished — the client
// left, access ended, or the queue could not be read.
//
// Here rather than in the log itself because a queued deploy has no log yet:
// the replica that takes it starts one, and until then no replica could write
// a line every reader would see.
func (s *Server) waitInQueue(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, appID, depID string) (open, done bool) {
	every := s.QueuePollEvery
	if every <= 0 {
		every = time.Second
	}
	reauthEvery := s.DeployLogReauthEvery
	if reauthEvery <= 0 {
		reauthEvery = logstream.DefaultReauthEvery
	}
	allowed := s.stillAllowed(r, appID, authz.AppLogsRead)
	lastCheck := time.Now()
	last := -1

	for {
		ahead, waiting, err := s.Deployments.QueuePosition(r.Context(), depID)
		if err != nil {
			if open {
				fmt.Fprint(w, "event: end\ndata: \n\n")
				_ = rc.Flush()
				return true, true
			}
			Error(w, r, err)
			return false, true
		}
		if !waiting {
			return open, false
		}
		if !open {
			startEventStream(w)
			open = true
		}
		if ahead != last {
			fmt.Fprintf(w, "data: %s\n\n", queueLine(ahead))
			_ = rc.Flush()
			last = ahead
		}
		if time.Since(lastCheck) >= reauthEvery {
			lastCheck = time.Now()
			if err := allowed(r.Context()); err != nil {
				fmt.Fprint(w, "data: Your access to this app's logs has ended, so Pando stopped showing this deploy's log. "+
					"Someone with permission to manage the app's access can grant app.logs.read again.\n\n")
				fmt.Fprint(w, "event: end\ndata: \n\n")
				_ = rc.Flush()
				return true, true
			}
		}
		select {
		case <-r.Context().Done():
			return open, true
		case <-time.After(every):
		}
	}
}

// --- secrets ---------------------------------------------------------------

// handleListSecrets returns keys and metadata only, never values.
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	keys, err := s.Secrets.Keys(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"secrets": keys})
}

// handlePutSecret stores or rotates a value. Requires app.secrets.write, which
// is deliberately separable from app.secrets.read (R-083).
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSecretsWrite)
	if !ok {
		return
	}

	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	key := chi.URLParam(r, "key")
	if err := s.Secrets.Put(r.Context(), app.ID, key, secret.New(req.Value)); err != nil {
		Error(w, r, err)
		return
	}

	// The key is audited; the value is not, and cannot be — it is a
	// secret.Value everywhere it travels.
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(PrincipalFrom(r.Context()).Kind),
		PrincipalID:   PrincipalFrom(r.Context()).ID,
		OnBehalfOf:    PrincipalFrom(r.Context()).UserID,
		Action:        "secret.write",
		AppID:         app.ID,
		TargetKind:    "secret",
		TargetID:      key,
	})
	JSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSecretsWrite)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	if err := s.Secrets.Delete(r.Context(), app.ID, key); err != nil {
		Error(w, r, err)
		return
	}
	s.auditApp(r, app.ID, "secret.delete")
	JSON(w, http.StatusNoContent, nil)
}

// --- app logs and status ---------------------------------------------------

func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}

	body := map[string]any{
		"app_id":        app.ID,
		"state":         app.State,
		"desired_state": app.DesiredState,

		// R-158: the pinned spec asks for auto-deploy, and approval now
		// stops it. The console says so on the app.
		"auto_deploy_paused": false,
	}

	if app.PinnedSpecID != "" {
		rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
		if err == nil && found {
			if runtime, ok := s.Registry.Runtime(rev.Body.Runtime.AdapterRef); ok {
				observed, err := s.Observations.Observe(r.Context(), rev.Body.Runtime.AdapterRef, runtime, app.ID)
				if err != nil {
					// An adapter being unreachable is a platform problem, not
					// app failure (design 05 §2). Reported as such rather than
					// presented as the app being broken.
					body["observability"] = "unreachable"
				} else {
					body["workloads"] = workloadStatuses(rev.Body, observed)
				}
			}
			body["revision"] = rev.Revision
			if s.Approvals != nil {
				paused, err := s.Approvals.AutoDeployPaused(r.Context(), app.ID, rev.Body)
				if err != nil {
					Error(w, r, err)
					return
				}
				body["auto_deploy_paused"] = paused
			}
		}
	}
	JSON(w, http.StatusOK, body)
}

// WorkloadStatus is one part of an app, as it is right now.
//
// The adapter's own struct used to be serialized straight out, which put Go
// field names on the wire — `Running`, `RestartCount` — in an API that is
// snake_case everywhere else, and left the console reading a shape nothing
// documented. It is the product (R-261), so it gets a type.
type WorkloadStatus struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary"`

	Present bool `json:"present"`
	Running bool `json:"running"`

	// Restarting is the crash loop, which is the thing somebody looking at a
	// degraded app most needs to see. A container that exits and is restarted
	// by the runtime is "running" at almost every instant Pando looks at it.
	Restarting   bool `json:"restarting"`
	RestartCount int  `json:"restart_count"`

	// Healthy is null when the workload declares no health check: no signal is
	// not the same as unhealthy (R-221).
	Healthy  *bool  `json:"healthy"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Image    string `json:"image,omitempty"`

	StartedAt *time.Time `json:"started_at,omitempty"`
}

// workloadStatuses pairs what the runtime sees with what the spec declares.
func workloadStatuses(s *spec.AppSpec, observed api.ObservedBundle) []WorkloadStatus {
	primary, _ := s.PrimaryWorkload()

	found := make(map[string]api.ObservedWorkload, len(observed.Workloads))
	for _, w := range observed.Workloads {
		found[w.Name] = w
	}

	out := make([]WorkloadStatus, 0, len(observed.Workloads))
	add := func(name string, w api.ObservedWorkload, declared bool) {
		status := WorkloadStatus{
			Name:         name,
			Primary:      declared && name == primary.Name,
			Present:      w.Present,
			Running:      w.Running,
			Restarting:   w.Restarting,
			RestartCount: w.RestartCount,
			Healthy:      w.Healthy,
			ExitCode:     w.ExitCode,
			Image:        w.ImageDigest,
		}
		if !w.StartedAt.IsZero() {
			at := w.StartedAt
			status.StartedAt = &at
		}
		out = append(out, status)
	}

	// The spec's order first, so the app's own parts read in the order its
	// author wrote them, then anything else the runtime is running for this
	// app — a provisioned database, which is part of what is running and would
	// otherwise be invisible.
	for _, w := range s.Workloads {
		add(w.Name, found[w.Name], true)
		delete(found, w.Name)
	}
	for _, w := range observed.Workloads {
		if _, still := found[w.Name]; still {
			add(w.Name, w, false)
			delete(found, w.Name)
		}
	}
	return out
}

// handleAppLogs is the app's own output as plain text.
//
// One-shot by default: the last `tail` lines, capped at logstream.MaxTail so
// one request cannot ask the runtime for an app's whole history. With
// follow=true it stays open on the replica's shared stream for the part
// (O-51), which is what `pando logs --follow` reads; the console reads the
// same stream as server-sent events from /logs/stream.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	src, ok := s.appLogSource(w, r)
	if !ok {
		return
	}
	if follow, _ := strconv.ParseBool(r.URL.Query().Get("follow")); follow {
		s.followAppLogs(w, r, src, plainViewer{w: w, rc: http.NewResponseController(w)})
		return
	}

	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	rc, err := src.runtime.Logs(r.Context(), apiWorkloadRef(src.key.AppID, src.key.Workload),
		apiLogOptions(false, logstream.ClampTail(tail)))
	if err != nil {
		Error(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
