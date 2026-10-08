package detection

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/screening"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// Revising a plan because a person asked (R-336's third trigger, design 10
// §4.3).
//
// Somebody reviewing a proposal knows things the repository does not say out
// loud — "it serves on 8080", "you missed the Redis" — and has had to carry
// each one to a settings screen after accepting. Here they say it, and the AI
// adapter reads the repository again to check it before anything changes:
// what it proposes goes through the same closed set and the same refusals as
// every other amendment (R-332 – R-334), so a person asking is not a way
// around them. A claim the repository does not support is answered, not
// applied.

// MaxInstruction is the longest message a person may send, in characters.
const MaxInstruction = 2_000

// ActionRevise is the audit event a revision writes (R-337): repository
// contents went to the provider because a person asked.
const ActionRevise = "detection.revise"

// Revise asks the AI adapter to change an app's proposal as a person
// described, records the exchange on the proposal, and stores it.
func (r *Runner) Revise(ctx context.Context, appID, message string) (state.Detection, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return state.Detection{}, errs.New(errs.ValidInvalid, "Say what should change about the plan.").
			WithRemedy(`For example: "The app serves on port 8080" or "It also needs Redis".`)
	}
	if utf8.RuneCountInString(message) > MaxInstruction {
		return state.Detection{}, errs.Newf(errs.ValidInvalid,
			"That message is longer than %d characters. Say what should change in a few sentences.", MaxInstruction)
	}
	if _, _, _, ok := r.screenerFor(api.AIFunctionRevisePlan); !ok {
		return state.Detection{}, errs.New(errs.AdapterUnavailable,
			"Plan revision is not assigned to an AI adapter on this installation, so there is nothing to ask.").
			WithRemedy("Change the plan by answering its questions and setting its variables, or assign plan revision " +
				"to an AI adapter under Adapters in the console, or with PUT /api/v1/ai/functions/revise_plan.")
	}
	if r.ScreenPolicy != nil {
		if reason := r.ScreenPolicy.AllowsScreening(ctx); reason != "" {
			return state.Detection{}, errs.New(errs.PermDenied, reason)
		}
	}

	app, err := r.check(ctx, appID)
	if err != nil {
		return state.Detection{}, err
	}
	d, err := r.Detections.Get(ctx, appID)
	if err != nil {
		return state.Detection{}, err
	}
	if d.Status != state.DetectionReady && d.Status != state.DetectionNeedsAnswers {
		return state.Detection{}, errs.New(errs.StateInvalid,
			"There is no finished plan to change: detection is still running, or it did not produce one.").
			WithRemedy("Wait for detection to finish, or run it again.")
	}
	var proposal detect.Proposal
	if err := json.Unmarshal(d.Body, &proposal); err != nil {
		return state.Detection{}, errs.Wrap(errs.Internal, "The stored plan could not be read.", err)
	}

	// The commit that was reviewed, not wherever the branch has moved since:
	// the AI checks what the person is looking at (R-120).
	src := app.Source
	if proposal.Commit != "" {
		src.Commit = proposal.Commit
	}
	checkout, err := r.Sources.Fetch(source.ForApp(ctx, appID), src)
	if err != nil {
		return state.Detection{}, err
	}
	defer checkout.Close()

	if err := r.revise(ctx, appID, &proposal, d.Answers, checkout.View(src.Subdir), message); err != nil {
		return state.Detection{}, err
	}

	if err := r.Detections.Save(ctx, appID, proposal.Status, proposal, proposal.Commit); err != nil {
		return state.Detection{}, err
	}
	return r.Detections.Get(ctx, appID)
}

// revise does the asking and the applying, against a proposal in memory.
//
// It fails only when the adapter could not answer at all; everything the
// adapter proposed that Pando would not do is recorded on the turn instead.
func (r *Runner) revise(
	ctx context.Context,
	appID string,
	proposal *detect.Proposal,
	answers map[string]string,
	view api.SourceView,
	message string,
) error {
	now := r.clock().Now()

	// The reading accepting would pin: the one the build-method answer picks,
	// else the winner's (detect.Proposal.chosen). Amending the winner's draft
	// while the person reviews another reading would change nothing they see.
	target, evidence := revisionTarget(proposal, proposal.Answers(answers))

	history := make([]api.Turn, 0, len(proposal.Conversation))
	for _, t := range proposal.Conversation {
		history = append(history, api.Turn{From: t.From, Text: t.Text})
	}

	trial := proposal.TrialSummary()
	screener, ref, model, _ := r.screenerFor(api.AIFunctionRevisePlan)
	result, outcome := screening.Run(ctx, screener, ref, api.AIFunctionRevisePlan, api.ScreenRequest{
		Model:        model,
		Source:       view,
		Spec:         *target,
		Evidence:     evidence,
		Questions:    questionsFor(proposal.Questions),
		Values:       emptyValues(*proposal),
		Trial:        trial,
		Instruction:  message,
		Conversation: history,
		Known:        knownFiles(proposal),
		Budget: api.ScreenBudget{
			MaxFiles: r.ScreenMaxFiles,
			MaxBytes: r.ScreenMaxBytes,
			Timeout:  r.ScreenTimeout,
		},
	})
	outcome.Why = "A person reviewing the plan asked for a change."
	r.audit(ctx, ActionRevise, appID, outcome)

	if !outcome.Ran {
		code := errs.AdapterFailed
		if outcome.SkipCode == screening.SkipUnsupported {
			code = errs.AdapterUnavailable
		}
		return errs.New(code, outcome.Skipped).
			WithRemedy("Try again in a moment, or change the plan by answering its questions and setting its variables.")
	}

	env := screening.Env{Source: view, Trial: trial}
	turn := detect.Turn{From: detect.TurnAI, Text: strings.TrimSpace(result.Reply), At: now, FilesRead: outcome.FilesRead}

	// Answers land as suggestions on their questions, as screening's do, so
	// the person can still see and change them (design 10 §4.2).
	given, rest, refused := screening.Split(env, result.Amendments, outstanding(proposal.Questions))
	for _, no := range refused {
		turn.Refused = append(turn.Refused, fmt.Sprintf("%s: %s", describe(no.Amendment), no.Reason))
	}
	for key, value := range given {
		if key == detect.KeyBuildStrategy || key == detect.KeyBuildMethod {
			if err := proposal.CheckAnswers(map[string]string{key: value}); err != nil {
				turn.Refused = append(turn.Refused, fmt.Sprintf("answer %s with %s: %s", key, value, errs.As(err).Message))
				continue
			}
		}
		for _, a := range result.Amendments {
			if a.Kind == api.AmendAnswerQuestion && strings.TrimSpace(a.Key) == key {
				suggest(proposal.Questions, key, detect.Suggestion{Value: value, Reason: a.Reason, Evidence: nonEmpty(a.Evidence)})
				turn.Changes = append(turn.Changes, fmt.Sprintf("answered %s: %s", key, value))
				break
			}
		}
	}

	applied, refusedRest := screening.Apply(target, env, rest)
	for _, a := range applied {
		turn.Changes = append(turn.Changes, a.Summary)
	}
	for _, no := range refusedRest {
		turn.Refused = append(turn.Refused, fmt.Sprintf("%s: %s", describe(no.Amendment), no.Reason))
	}

	// The winner's draft is kept in step with its spec, as screening does,
	// because it is what the review renders beside the evidence.
	if target == &proposal.DraftSpec {
		proposal.Winner.Draft.Build = proposal.DraftSpec.Build
		proposal.Winner.Draft.Workloads = proposal.DraftSpec.Workloads
		proposal.Winner.Draft.Volumes = proposal.DraftSpec.Volumes
		proposal.Winner.Draft.Slots = proposal.DraftSpec.Slots
		proposal.Winner.Draft.Health = proposal.DraftSpec.Health
		proposal.Winner.Draft.Warnings = proposal.DraftSpec.Warnings
	}

	if turn.Text == "" {
		turn.Text = defaultReply(len(turn.Changes))
	}
	proposal.Conversation = append(proposal.Conversation,
		detect.Turn{From: detect.TurnPerson, Text: message, At: now}, turn)
	proposal.Status = detect.StatusFor(proposal.Winner, proposal.Questions)
	return nil
}

// knownFiles is every file an AI adapter has already read about this
// proposal — when detection called it, and on each earlier turn — once each,
// in the order first read. The same commit, so the same contents.
func knownFiles(p *detect.Proposal) []string {
	var out []string
	seen := map[string]bool{}
	add := func(files []string) {
		for _, f := range files {
			if f != "" && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	if p.Screening != nil && p.Screening.Ran {
		add(p.Screening.FilesRead)
	}
	for _, t := range p.Conversation {
		add(t.FilesRead)
	}
	return out
}

// revisionTarget is the spec accepting would pin, and the evidence for that
// reading: a runner-up's when the build-method answer adopts it.
func revisionTarget(p *detect.Proposal, answers map[string]string) (*spec.AppSpec, []string) {
	want := strings.TrimSpace(answers[detect.KeyBuildStrategy])
	if want == "" {
		want = strings.TrimSpace(answers[detect.KeyBuildMethod])
	}
	if want != "" && spec.BuildStrategy(want) != p.Winner.Strategy {
		for i := range p.RunnersUp {
			c := &p.RunnersUp[i]
			if string(c.Strategy) == want && c.Spec != nil && len(c.Draft.Workloads) > 0 {
				return c.Spec, c.Evidence
			}
		}
	}
	return &p.DraftSpec, p.Winner.Evidence
}

func describe(a api.Amendment) string {
	switch a.Kind {
	case api.AmendAnswerQuestion:
		return fmt.Sprintf("answer %s with %s", a.Key, a.Value)
	case api.AmendSetEnv:
		return fmt.Sprintf("set %s", a.Key)
	case api.AmendAddSlot:
		return fmt.Sprintf("add %s", a.Key)
	default:
		return strings.ReplaceAll(string(a.Kind), "_", " ")
	}
}

func defaultReply(changes int) string {
	switch changes {
	case 0:
		return "The repository doesn’t support a change here, so the plan is as it was."
	case 1:
		return "Changed one thing in the plan."
	default:
		return fmt.Sprintf("Changed %d things in the plan.", changes)
	}
}

func (r *Runner) clock() clock.Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return clock.System{}
}
