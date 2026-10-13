package detect

import (
	"context"
	"fmt"
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

	// Built is the image detection built for this proposal, when it built one
	// (R-311): what the plan's score was read from, and what a deploy of the
	// same build inputs runs instead of building again. Nil for an app that
	// runs a published image, and for one whose build waits on an answer.
	Built *Built `json:"built,omitempty"`

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

// RegistryProbe looks for an image the maintainer already publishes.
//
// R-094 tier 1, the top of the confidence ladder, and it sits there because a
// published image is the maintainer's own answer to "how is this built" —
// already built, already shipped by whoever maintains it. Nothing Pando infers
// about the source beats it.
//
// It is part of detection, not an adapter (issue #153): the planner asks it
// nothing, and the OCI distribution API it speaks is no provider's vocabulary
// (design 03 §8.1). The registry Pando pushes built images to is the adapter.
type RegistryProbe interface {
	// Published reports images published for a source repository, best first.
	// No result is the normal case and not an error.
	Published(ctx context.Context, src spec.Source) ([]PublishedImage, error)
}

// PublishedImage is an image that already exists.
type PublishedImage struct {
	Ref      string `json:"ref"`
	Digest   string `json:"digest,omitempty"`
	Registry string `json:"registry"`
}

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
	published := j.published(ctx, src)

	// Steps 8 and 9 — the auction.
	Report(ctx, StageDetecting, nil)
	result, err := j.Auction.Run(ctx, view)
	if err != nil {
		return Proposal{}, err
	}

	// A compose file outranks the published image (R-094, amended by issue
	// #90); the image outranks everything else the auction found.
	if len(published) > 0 {
		compose, ok := composeAnswer(result)
		if !ok {
			return j.checkRegistry(ctx, appID, src, published, view), nil
		}
		result = withComposeOverImage(result, compose, registryCandidate(published, view))
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

	draft, trialDraft, trialAuth, err := j.buildForTrial(ctx, appID, src, draft, &proposal, &partial)
	if err != nil {
		return Proposal{}, err
	}
	Report(ctx, StageTrying, &partial)

	// Step 10 — the trial run.
	trial := j.trialWith(ctx, trialDraft, trialAuth)
	draft, trial, movedHost := j.listenEverywhere(ctx, draft, trialDraft, trialAuth, trial)
	if movedHost {
		proposal.Winner.Evidence = append(append([]string(nil), proposal.Winner.Evidence...), listenedEverywhere)
	}
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

	// A program that ran, finished and never listened is not a web app, and
	// the trial is the evidence (R-107).
	if refused := exitedRefusal(trial); refused != nil {
		proposal.Status = StatusBlocked
		proposal.Blocked = refused
	}
	return proposal, nil
}

// buildForTrial is step 9a — the build (R-311). What the app would deploy,
// built the way a deploy builds it, so the plan can show the score of the image
// that will run and the trial run has something to start. A build that fails
// stops here with its log, the way a trial run that crashes does (R-107).
//
// It returns the draft with the build's plan applied, and the draft and pull
// credentials the trial run should start; proposal and partial record what
// was built.
func (j *Job) buildForTrial(ctx context.Context, appID string, src spec.Source, draft Draft, proposal, partial *Proposal) (Draft, Draft, *api.RegistryAuth, error) {
	trialDraft, trialAuth := draft, (*api.RegistryAuth)(nil)
	build := buildFrom(ctx)
	if build == nil {
		return draft, trialDraft, trialAuth, nil
	}
	outcome, err := build(ctx, partial)
	if err != nil {
		return draft, trialDraft, trialAuth, err
	}
	if len(outcome.Plan) > 0 {
		draft = withPlan(draft, outcome.Plan)
		partial.Winner.Draft = draft
		partial.DraftSpec = j.assemble(appID, src, draft)
	}
	if outcome.Built != nil {
		proposal.Built, partial.Built = outcome.Built, outcome.Built
		if image := outcome.Built.Scanned(&partial.DraftSpec); image != "" {
			trialDraft, trialAuth = withPrimaryImage(draft, image), outcome.PullAuth
		}
	}
	return draft, trialDraft, trialAuth, nil
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

// published asks the registry probe for images the maintainer publishes.
func (j *Job) published(ctx context.Context, src spec.Source) []PublishedImage {
	if j.Registry == nil {
		return nil
	}
	images, err := j.Registry.Published(ctx, src)
	if err != nil {
		// A registry that is unreachable is not a detection failure. Everything
		// below tier 1 still works, and refusing to detect because a network
		// call failed would be the opposite of degrading gracefully.
		return nil
	}
	return images
}

// composeAnswer returns the compose candidate that outranks a published image:
// one that imported, and describes the app rather than only what it needs.
//
// A compose file Pando refused does not. Its refusal is shown when nothing
// else answers (see Auction.Run), but a published image is an answer — the
// maintainer's own — and refusing an app that has one for a construct in a
// file it does not need would be a regression nobody asked for.
func composeAnswer(result Result) (Candidate, bool) {
	for _, c := range append([]Candidate{result.Winner}, result.RunnersUp...) {
		if c.Strategy == spec.BuildCompose && c.Blocked == nil && !onlyBackingServices(c.Draft) &&
			len(c.Draft.Workloads) > 0 {
			return c, true
		}
	}
	return Candidate{}, false
}

// withComposeOverImage makes the compose candidate the winner over a published
// image, and the image a runner-up.
//
// R-094 put the published image above everything, and a repository's compose
// file is where its maintainer says how that image runs: beside which
// database, with which variables, keeping which volumes. Planka, Listmonk and
// a dozen more ran their image alone and crashed on `ECONNREFUSED
// 127.0.0.1:5432` (issue #90). The compose file is the answer now, and a
// service in it that builds the image the maintainer publishes runs the
// published image instead — the same image, already built.
//
// The ladder settles it, so there is no question between them: compose over
// the image, and the image over every other reading the auction found.
func withComposeOverImage(result Result, compose, image Candidate) Result {
	compose.Draft = runPublished(compose.Draft, image)
	compose.Evidence = append(compose.Evidence,
		"the compose file says how the published image "+image.Draft.Workloads[0].Image+" runs, so it is used rather than the image alone")

	runnersUp := []Candidate{image}
	for _, c := range append([]Candidate{result.Winner}, result.RunnersUp...) {
		if c.Detector != compose.Detector {
			runnersUp = append(runnersUp, c)
		}
	}
	questions := append([]Question(nil), compose.Questions...)
	return Result{
		Winner:    compose,
		RunnersUp: runnersUp,
		Questions: questions,
		Status:    StatusFor(compose, questions),
	}
}

// runPublished has each compose service that builds the published image run
// it instead: `build: .` beside `image: ghcr.io/acme/app` is the maintainer's
// build of the image they publish, and that image already exists.
func runPublished(d Draft, image Candidate) Draft {
	published := imageRepository(image.Draft.Workloads[0].Image)
	out := d
	out.Workloads = append([]spec.Workload(nil), d.Workloads...)
	building := false
	for i, w := range out.Workloads {
		if w.Build != nil && w.Image != "" && imageRepository(w.Image) == published {
			out.Workloads[i].Build = nil
		}
		if out.Workloads[i].Build != nil {
			building = true
		}
	}
	if !building && out.Build.Strategy == spec.BuildCompose {
		out.Build.Strategy = spec.BuildPrebuilt
	}
	return out
}

// imageRepository is an image reference without its tag or digest, with
// Docker Hub's implied parts spelled out, so `planka` and
// `docker.io/library/planka:latest` compare equal.
func imageRepository(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref = ref[:colon]
	}
	first, rest, nested := strings.Cut(ref, "/")
	switch {
	case !nested:
		return "docker.io/library/" + ref
	case (first == "docker.io" || first == "index.docker.io") && !strings.Contains(rest, "/"):
		return "docker.io/library/" + rest
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		return "docker.io/" + first + "/" + rest
	case first == "index.docker.io":
		return "docker.io/" + rest
	}
	return ref
}

// registryCandidate is R-094 tier 1's reading: run the image as published.
func registryCandidate(images []PublishedImage, view api.SourceView) Candidate {
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
	}

	// The image is the repository built, so what the repository says the app
	// needs, the image needs: a Rails app published by its maintainer still
	// signs with SECRET_KEY_BASE, and nobody had been asked for it (R-132).
	if view != nil {
		var evidence []string
		draft, evidence = withSourceRequirements(view, draft, ".", "")
		candidate.Evidence = append(candidate.Evidence, evidence...)
	}
	candidate.Draft = draft

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
	return candidate
}

// checkRegistry proposes the published image, R-094 tier 1, when no compose
// file outranks it.
//
// It short-circuits everything the auction found below it, and the build: there
// is nothing to build of something that is already built, and
// spec.BuildPrebuilt is exactly that case. The trial run still answers the port.
func (j *Job) checkRegistry(ctx context.Context, appID string, src spec.Source, images []PublishedImage, view api.SourceView) Proposal {
	return j.prebuilt(ctx, appID, src, registryCandidate(images, view))
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
	draft, trial, movedHost := j.listenEverywhere(ctx, draft, draft, auth, trial)
	if movedHost {
		proposal.Winner.Evidence = append(append([]string(nil), proposal.Winner.Evidence...), listenedEverywhere)
	}
	draft, proposal.Questions = ApplyTrial(draft, proposal.Questions, trial)
	proposal.TrialLog = trial.Log
	proposal.Trial = trial.Observation()
	proposal.Status = StatusFor(candidate, proposal.Questions)
	proposal.Winner.Draft = draft
	proposal.DraftSpec = j.assemble(appID, src, draft)

	// An image that serves only a database's or a mail server's protocol has
	// nothing for Pando's proxy to send a browser to, and one that runs and
	// exits has nothing at all. Said so, the way a library is.
	refused := nonHTTPRefusal(trial, draft)
	if refused == nil {
		refused = exitedRefusal(trial)
	}
	if refused != nil {
		proposal.Status = StatusBlocked
		proposal.Blocked = refused
	}
	return proposal
}

// trialWith runs the draft once in throwaway isolation, if that is possible,
// pulling its image with auth when that is set.
//
// Everything here is a reason it might not be, and none of them is an error.
// A missing runtime, a runtime that cannot do it, an image that has not been
// built: each turns deferred questions back into real ones, which is worse for
// the user and not a failure of detection.
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
		// Nothing to run: an app Pando builds whose build has not happened,
		// because it waits on an answer or because no builder is configured.
		// Detection builds the image when it can (WithBuild, R-311), and the
		// trial runs that.
		return Trial{}
	}

	timeout := j.TrialTimeout
	if timeout <= 0 {
		timeout = DefaultTrialTimeout
	}

	env := map[string]secret.Value{}
	for _, e := range primary.Env {
		// A name read out of .env.example and left empty is unset, as it is
		// at deploy (deploy.resolveEnv). Set empty, it overrode the image's
		// own ENV and the app's defaults: Strapi read HOST="" and PORT="" and
		// stopped on "http://:NaN/admin", AdonisJS reported NODE_ENV missing
		// though its Dockerfile sets it — and the trial's log, which is what
		// finds what an app is missing, described a failure no deploy has.
		if e.Value == nil || (*e.Value == "" && (e.Source == spec.EnvFromDetection || e.Source == "")) {
			continue
		}
		env[e.Key] = secret.New(*e.Value)
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
//
// A build that reaches above the app's directory moves the directory up to
// where it reaches (liftBuildContexts): what the build reads is decided here,
// once, for whichever reading is accepted.
func Assemble(appID string, src spec.Source, draft Draft) spec.AppSpec {
	src, draft = liftBuildContexts(src, draft)
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
