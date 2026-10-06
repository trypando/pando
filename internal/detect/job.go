package detect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/screening"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Proposal is what a detection job produces (design 04 §2.2).
//
// A proposal, never a deployment (R-093). It is reviewed, and only then pinned
// as spec revision 1 — accepting is not deploying, which is the assertion
// Sequence A makes twice.
type Proposal struct {
	Status    string       `json:"status"`
	Winner    Candidate    `json:"winning_bid"`
	RunnersUp []Candidate  `json:"runners_up"`
	Questions []Question   `json:"questions"`
	DraftSpec spec.AppSpec `json:"draft_spec"`

	// Blocked is the reason, when the best reading of this repository is one
	// that cannot proceed — a compose file using a construct that cannot cross
	// the boundary (R-099).
	Blocked *errs.Error `json:"blocked,omitempty"`

	// TrialLog is the app's own output from the trial run. On a crash this is
	// the entire answer Pando has, and R-107 says showing it and stopping is
	// the correct outcome rather than a gap to close with inference.
	TrialLog string `json:"trial_log,omitempty"`

	// Commit is what was actually read. R-120: Ref is what the user asked for,
	// Commit is what runs.
	Commit string `json:"commit,omitempty"`

	// Trial is what the trial run observed, minus the log, which TrialLog
	// already carries.
	//
	// Kept on the proposal because the log is not the whole of what the trial
	// established and two things need the rest of it. A screening pass has to
	// know whether the app crashed, because that is what decides whether a
	// dependency it found may be marked required (O-4's fallback, design 10
	// §3.2). And "no trial ran" and "a trial ran and saw nothing" are different
	// facts that an empty log reports identically.
	Trial TrialObservation `json:"trial,omitzero"`

	// Screening is what an AI adapter made of this proposal (R-331, design 10
	// §5): which adapter, which model, what it read, what it changed and why,
	// and what it asked for that Pando refused.
	//
	// On the proposal rather than only in the audit log, because the console
	// renders it as a section of the review. R-334 calls for attribution, and
	// attribution nobody can see is a record rather than attribution.
	//
	// Nil means no screening ran, which is the ordinary case for an install
	// with no AI adapter configured — and not a degraded one (R-335).
	Screening *screening.Outcome `json:"screening,omitempty"`

	// Conversation is what a person reviewing this proposal asked an AI adapter
	// to change, and what it replied and did (R-336's third trigger), oldest
	// first. On the proposal because it is about this reading of this commit:
	// detecting again starts a new one.
	Conversation []Turn `json:"conversation,omitempty"`
}

// Turn is one message about a proposal.
type Turn struct {
	// From is TurnPerson or TurnAI.
	From string    `json:"from"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`

	// Changes and Refused are what an AI turn did to the plan, one line each:
	// the applied amendments' summaries, and what Pando would not do with its
	// reason (R-334).
	Changes []string `json:"changes,omitempty"`
	Refused []string `json:"refused,omitempty"`

	// FilesRead is what an AI turn read (R-337), so the next one can start
	// from it rather than read it all again (api.ScreenRequest.Known).
	FilesRead []string `json:"files_read,omitempty"`
}

// Who a Turn is from.
const (
	TurnPerson = "person"
	TurnAI     = "ai"
)

// TrialObservation is Trial without the log.
type TrialObservation struct {
	Ran            bool     `json:"ran,omitempty"`
	Started        bool     `json:"started,omitempty"`
	Crashed        bool     `json:"crashed,omitempty"`
	ObservedPorts  []int    `json:"observed_ports,omitempty"`
	ObservedWrites []string `json:"observed_writes,omitempty"`
}

// Observation is what survives onto the proposal.
func (t Trial) Observation() TrialObservation {
	return TrialObservation{
		Ran:            t.Ran,
		Started:        t.Started,
		Crashed:        t.Crashed,
		ObservedPorts:  t.ObservedPorts,
		ObservedWrites: t.ObservedWrites,
	}
}

// TrialSummary is the trial as an AI screener sees it (R-330, design 10 §2).
func (p Proposal) TrialSummary() api.TrialSummary {
	return api.TrialSummary{
		Ran:            p.Trial.Ran,
		Crashed:        p.Trial.Crashed,
		ObservedPorts:  p.Trial.ObservedPorts,
		ObservedWrites: p.Trial.ObservedWrites,
		Log:            p.TrialLog,
	}
}

// RegistryProbe and PublishedImage live in the adapter package, because a probe
// is an adapter: it talks to ghcr.io and Docker Hub, and R-027 says nothing
// under internal/adapter may reach into core beyond internal/adapter/api.
type (
	RegistryProbe  = api.RegistryProbe
	PublishedImage = api.PublishedImage
)

// TrialRunner starts a draft in throwaway isolation (R-097).
type TrialRunner interface {
	Capabilities(ctx context.Context) (api.RuntimeCapabilities, error)
	Trial(ctx context.Context, req api.TrialRequest) (api.TrialResult, error)
}

// Builder produces an image from source, so the trial run has something to run.
type Builder interface {
	Build(ctx context.Context, req api.BuildRequest) (api.BuildResult, error)
}

// Job runs detection: Sequence A steps 6 through 12.
//
// Every collaborator is optional, and each one missing degrades rather than
// fails. That is R-106's shape applied to the whole job: with nothing
// configured, each step turns back into a question, not a dead end.
type Job struct {
	Auction  *Auction
	Registry RegistryProbe
	Runtime  TrialRunner
	Builder  Builder

	// Images reads an image app's image from its registry before the trial,
	// with the app's credential: its digest, platforms and configuration.
	Images ImageReader

	// TrialTimeout bounds the observation. Zero uses DefaultTrialTimeout.
	TrialTimeout time.Duration

	// NewTrialID names the throwaway bundle. Injectable so a test can assert
	// what was created and cleaned up.
	NewTrialID func() string
}

// DefaultTrialTimeout is how long an app gets to start and bind.
//
// Generous, because a first run pulls a base image and a JVM or a Rails app can
// take most of a minute to come up — and an app wrongly judged dead costs a
// question the whole trial run exists to avoid.
const DefaultTrialTimeout = 90 * time.Second

// Run detects how to build and run the source at src.
//
// Order matters and follows Sequence A. The registry check comes before the
// auction because a published image makes the auction moot; the trial run comes
// after, because there is nothing to run until a draft exists.
func (j *Job) Run(ctx context.Context, appID string, src spec.Source, view api.SourceView) (Proposal, error) {
	if j.Auction == nil {
		return Proposal{}, errs.New(errs.Internal, "Detection is not configured.")
	}

	// An app created from a published image has no repository to read. It went
	// through the auction anyway, over an empty checkout, and was asked how to
	// build and run something that is already built (issue #55). The image is
	// the whole answer; only its port is unknown, and the trial run watches for
	// that.
	if src.Type == spec.SourceImage {
		return j.runImage(ctx, appID, src), nil
	}

	// Step 7 — R-094 tier 1. An image the maintainer already publishes.
	if p, found := j.checkRegistry(ctx, appID, src); found {
		return p, nil
	}

	// Steps 8 and 9 — the auction.
	Report(ctx, StageDetecting, nil)
	result, err := j.Auction.Run(ctx, view)
	if err != nil {
		return Proposal{}, err
	}

	proposal := Proposal{
		Status:    result.Status,
		Winner:    result.Winner,
		RunnersUp: result.RunnersUp,
		Questions: result.Questions,
	}
	if result.Blocked != nil {
		// Nothing further is useful. There is no point trial-running a compose
		// file that cannot be imported, and the reason is the answer.
		proposal.Blocked = errs.As(result.Blocked)
		proposal.DraftSpec = j.assemble(appID, src, result.Winner.Draft)
		//nolint:nilerr // The blocked reason is the proposal's content, not a
		// failure of the job. Returning it as an error would collapse "Pando
		// ran and found that this cannot be imported, here is which line and
		// why" into "detection failed", losing the only useful thing it knows.
		return proposal, nil
	}

	draft := result.Winner.Draft

	// What the auction found, before the trial run — which can take most of a
	// minute, and is long enough that somebody watching should see the
	// approach, the workloads and the variables rather than a spinner.
	partial := proposal
	partial.DraftSpec = j.assemble(appID, src, draft)
	Report(ctx, StageTrying, &partial)

	// Step 10 — the trial run.
	trial := j.trial(ctx, draft)
	draft, proposal.Questions = ApplyTrial(draft, proposal.Questions, trial)
	proposal.TrialLog = trial.Log
	proposal.Trial = trial.Observation()

	// Step 11 — warnings. Path routing is read from the source rather than the
	// trial, so it is attached here regardless of whether a trial happened.
	//
	// To every candidate, not only the winner. Whether an app writes absolute
	// addresses into its own HTML is a fact about the app, and has nothing to
	// do with how it is built — but the warning used to be attached to the
	// winning draft alone, so answering the build-strategy question with
	// anything else (R-103: the user picks, Pando does not guess) adopted a
	// draft that had never been told. The app then deployed clean, came up
	// blank under its path prefix, and the one thing Pando knew about why was
	// discarded at the moment the user made a choice it was offered.
	pathRouting := PathRoutingWarning(view, draft)
	draft.Warnings = append(draft.Warnings, pathRouting...)
	for i := range proposal.RunnersUp {
		proposal.RunnersUp[i].Draft.Warnings = append(proposal.RunnersUp[i].Draft.Warnings, pathRouting...)
	}

	// Step 12 — the status, recomputed. The trial may have answered the only
	// outstanding question, which turns needs_answers into ready.
	proposal.Status = StatusFor(result.Winner, proposal.Questions)
	proposal.Winner.Draft = draft
	proposal.DraftSpec = j.assemble(appID, src, draft)
	return proposal, nil
}

// StatusFor recomputes detection status after the trial run, and again
// after a screening — an amendment can answer the last outstanding question,
// and a status saying answers are needed while asking for none is one somebody
// has to open the database to understand.
//
// Low confidence on its own no longer says needs_answers. The threshold exists
// so that a shaky read is not presented as settled, and while a modest bid
// always carried a question that was the same thing said twice — but once the
// buildpack detector stopped asking about a port it already had a default for,
// a plain Go module came back as "needs_answers" with an empty question list.
// A status that says answers are needed while asking for none is one somebody
// has to open the database to understand.
//
// Nothing consumed it as a blocker, which is why this is a correction to what
// the API says rather than to what it does: the console branches on running,
// failed and blocked, and both the accept handler and the accept button gate on
// unanswered questions.
func StatusFor(winner Candidate, questions []Question) string {
	switch {
	case winner.Strategy == StrategyUnknown:
		return StatusNeedsAnswers
	case len(Open(questions, nil)) > 0:
		// An AI adapter's suggestion counts as an answer until a person gives
		// one (R-338), so a question it answered does not hold the status.
		return StatusNeedsAnswers
	default:
		return StatusReady
	}
}

// checkRegistry is R-094 tier 1.
//
// A published image short-circuits everything below it, including the trial
// run: there is nothing to detect about how to build something that is already
// built, and spec.BuildPrebuilt is exactly that case.
func (j *Job) checkRegistry(ctx context.Context, appID string, src spec.Source) (Proposal, bool) {
	if j.Registry == nil {
		return Proposal{}, false
	}

	images, err := j.Registry.Published(ctx, src)
	if err != nil || len(images) == 0 {
		// A registry that is unreachable is not a detection failure. Everything
		// below tier 1 still works, and refusing to detect because a network
		// call failed would be the opposite of degrading gracefully.
		return Proposal{}, false
	}

	best := images[0]
	draft := Draft{
		Build: spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{
			Name: "web", Image: best.Ref, Primary: true, Exposed: true,
		}},
	}

	candidate := Candidate{
		Detector:   "registry",
		Strategy:   spec.BuildPrebuilt,
		Confidence: 0.97,
		Evidence: []string{
			fmt.Sprintf("%s is already published at %s", best.Ref, best.Registry),
			"Pando will run this image rather than building the source. Nothing here checks that the " +
				"image was built from the commit you are deploying — only that it is published under " +
				"the same owner and name.",
		},
		Draft: draft,
	}

	// The port is still unknown, and still the trial run's to answer.
	candidate.Questions = []Question{{
		Key:      "primary_port",
		Kind:     api.QuestionPort,
		Deferred: true,
		Prompt: fmt.Sprintf(
			"Pando found a published image for this project (%s) and will run that rather than "+
				"building the source. It could not tell which port the image serves HTTP on. "+
				"Pando will start the image and watch it to work that out, but if that does not "+
				"succeed it needs to be told. Valid answer: a port number, such as 8080.", best.Ref),
		Why: "Pando needs to know where to send traffic once the app is running.",
	}}

	return j.prebuilt(ctx, appID, src, candidate), true
}

// runImage proposes running an app's published image as it is.
func (j *Job) runImage(ctx context.Context, appID string, src spec.Source) Proposal {
	candidate := Candidate{
		Detector:   "image",
		Strategy:   spec.BuildPrebuilt,
		Confidence: 1,
		Evidence:   []string{"this app runs the published image " + src.Image + " as it is"},
		Draft: Draft{
			Build: spec.Build{Strategy: spec.BuildPrebuilt},
			Workloads: []spec.Workload{{
				Name: "web", Image: src.Image, Primary: true, Exposed: true,
			}},
		},
		Questions: []Question{{
			Key:      KeyPrimaryPort,
			Kind:     api.QuestionPort,
			Deferred: true,
			Prompt: fmt.Sprintf(
				"This app runs the published image %s, and Pando could not tell which port it serves "+
					"HTTP on. Pando will start the image and watch it to work that out, but if that does "+
					"not succeed it needs to be told. Valid answer: a port number, such as 8080.", src.Image),
			Why: "Pando needs to know where to send traffic once the app is running.",
		}},
	}

	// The registry before the trial (issue #41): the digest the tag names now
	// is what the revision pins, and the image's own configuration answers
	// what the trial would otherwise have to — or, with no runtime to trial
	// on, what would otherwise be asked.
	src, insp, refused := j.readImage(ctx, appID, src)
	if insp != nil {
		var evidence []string
		candidate.Draft, candidate.Questions, evidence = applyImageConfig(candidate.Draft, candidate.Questions, insp.Config)
		candidate.Evidence = append(candidate.Evidence, evidence...)
	}
	if refused != nil {
		return Proposal{
			Winner:    candidate,
			Status:    StatusBlocked,
			Blocked:   errs.As(refused),
			DraftSpec: j.assemble(appID, src, candidate.Draft),
		}
	}

	var auth *api.RegistryAuth
	if j.Images != nil {
		if a, err := j.Images.Auth(ctx, appID, src.Image); err == nil {
			auth = pullAuth(a)
		}
	}
	return j.prebuiltWith(ctx, appID, src, candidate, auth)
}

// prebuilt finishes a proposal for an image that already exists: the trial run
// answers the port if it can, and the rest is the candidate as given.
func (j *Job) prebuilt(ctx context.Context, appID string, src spec.Source, candidate Candidate) Proposal {
	return j.prebuiltWith(ctx, appID, src, candidate, nil)
}

// prebuiltWith is prebuilt for an image that may be private.
func (j *Job) prebuiltWith(ctx context.Context, appID string, src spec.Source, candidate Candidate, auth *api.RegistryAuth) Proposal {
	draft := candidate.Draft
	proposal := Proposal{Winner: candidate, Questions: candidate.Questions}

	trial := j.trialWith(ctx, draft, auth)
	draft, proposal.Questions = ApplyTrial(draft, proposal.Questions, trial)
	proposal.TrialLog = trial.Log
	proposal.Trial = trial.Observation()
	proposal.Status = StatusFor(candidate, proposal.Questions)
	proposal.Winner.Draft = draft
	proposal.DraftSpec = j.assemble(appID, src, draft)

	// An image that serves only a database's or a mail server's protocol has
	// nothing for Pando's proxy to send a browser to. Redis was deployed, and
	// answered every request with a protocol error (issue #55). Said so, the
	// way a library is.
	if onlyNonHTTP(trial.ObservedPorts) {
		listed := make([]string, 0, len(trial.ObservedPorts))
		for _, p := range trial.ObservedPorts {
			listed = append(listed, strconv.Itoa(p))
		}
		proposal.Status = StatusBlocked
		proposal.Blocked = errs.Newf(errs.PlanCapabilityUnsupported,
			"This image listens only on port %s, which carries a protocol other than HTTP — a "+
				"database's or a mail server's — so there is no web page for Pando to serve.",
			strings.Join(listed, ", ")).
			WithRemedy("To give an app a database, fill one of its slots instead: Pando runs the " +
				"database inside that app, where nothing else can reach it.")
	}
	return proposal
}

// trial runs the draft once in throwaway isolation, if that is possible.
//
// Everything here is a reason it might not be, and none of them is an error.
// A missing runtime, a runtime that cannot do it, an image that has not been
// built: each turns deferred questions back into real ones, which is worse for
// the user and not a failure of detection.
func (j *Job) trial(ctx context.Context, draft Draft) Trial {
	return j.trialWith(ctx, draft, nil)
}

// trialWith is trial for an image pulled with credentials.
func (j *Job) trialWith(ctx context.Context, draft Draft, auth *api.RegistryAuth) Trial {
	if j.Runtime == nil {
		return Trial{}
	}
	caps, err := j.Runtime.Capabilities(ctx)
	if err != nil || !caps.SupportsTrialRun {
		return Trial{}
	}

	primary, ok := primaryWorkload(draft)
	if !ok || primary.Image == "" {
		// Nothing to run. A source build would have to happen first, and
		// building before a proposal is reviewed is work nobody asked for.
		return Trial{}
	}

	timeout := j.TrialTimeout
	if timeout <= 0 {
		timeout = DefaultTrialTimeout
	}

	env := map[string]secret.Value{}
	for _, e := range primary.Env {
		if e.Value != nil {
			env[e.Key] = secret.New(*e.Value)
		}
	}

	result, err := j.Runtime.Trial(ctx, api.TrialRequest{
		TrialID:       j.trialID(),
		Image:         primary.Image,
		Command:       primary.Command,
		Entrypoint:    primary.Entrypoint,
		WorkingDir:    primary.WorkingDir,
		Env:           env,
		PullAuth:      auth,
		DeclaredPaths: declaredPaths(primary),
		Timeout:       timeout,
	})
	if err != nil {
		return Trial{}
	}
	return FromTrialResult(caps, result)
}

func (j *Job) trialID() string {
	if j.NewTrialID != nil {
		return j.NewTrialID()
	}
	return fmt.Sprintf("d%d", time.Now().UnixNano())
}

func primaryWorkload(draft Draft) (spec.Workload, bool) {
	for _, w := range draft.Workloads {
		if w.Primary {
			return w, true
		}
	}
	if len(draft.Workloads) == 1 {
		return draft.Workloads[0], true
	}
	return spec.Workload{}, false
}

// declaredPaths is where the draft says the app keeps data, so the trial run
// can tell a write it expected from one it did not (R-202).
func declaredPaths(w spec.Workload) []string {
	paths := make([]string, 0, len(w.Mounts))
	for _, m := range w.Mounts {
		paths = append(paths, m.Path)
	}
	return paths
}

// assemble turns a draft into a spec that can be shown, diffed and pinned.
//
// Origin is detected, and the revision stays 0: this is a proposal. It becomes
// revision 1 when someone accepts it (Sequence A step 14), and not before.
func (j *Job) assemble(appID string, src spec.Source, draft Draft) spec.AppSpec {
	return Assemble(appID, src, draft)
}

// Assemble turns a draft into a spec that can be shown, diffed and pinned.
//
// A plain function because answering the tie-break needs it too: adopting the
// other candidate's reading means assembling that candidate's draft, and doing
// it a second way is how the two would drift.
func Assemble(appID string, src spec.Source, draft Draft) spec.AppSpec {
	return spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         appID,
		Origin:        spec.OriginDetected,
		Source:        src,
		Build:         draft.Build,
		Workloads:     draft.Workloads,
		Volumes:       draft.Volumes,
		Slots:         draft.Slots,
		Health:        draft.Health,
		Warnings:      draft.Warnings,
	}
}
