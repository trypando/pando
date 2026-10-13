// Package detect works out how to build and run an app from its source.
//
// Two rules shape everything here. R-102: ask, never guess — an uncertain
// detector returns a question, not a plausible default. R-021: fill declared
// slots, never invent topology — if the repo does not say it needs a database,
// Pando does not go hunting through imports to decide that it does.
//
// The auction (R-093) exists so the user can see how the answer was reached.
// Every detector bids, the bids are ranked, and the runners-up are returned
// alongside the winner — a verdict with its reasoning, rather than a verdict.
package detect

import (
	"context"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// Detector examines source and bids on how to build it.
type Detector interface {
	// Name identifies the detector in the auction result.
	Name() string

	// Bid returns this detector's reading of the source. A detector that sees
	// nothing it recognizes returns zero confidence rather than a guess.
	Bid(ctx context.Context, src api.SourceView) (Candidate, error)
}

// Candidate is one detector's proposal.
type Candidate struct {
	Detector string             `json:"detector"`
	Strategy spec.BuildStrategy `json:"strategy"`

	// Confidence is 0..1. It is shown to the user, so it has to mean something
	// consistent across detectors: how sure this detector is that its strategy
	// is the right one, not how much it would like to win.
	Confidence float64 `json:"confidence"`

	// Evidence is human-readable and shown in the review UI, so a person can
	// see why Pando thinks what it thinks (R-102).
	Evidence []string `json:"evidence,omitempty"`

	// Questions are what this detector could not determine. Held to R-105.
	Questions []Question `json:"questions,omitempty"`

	// Draft is what this detector would put in the spec.
	//
	// Serialized, and it has to be. The auction asks which of two close
	// readings is right (see tieBreak), and answering that means adopting the
	// other candidate's draft — impossible if the losing drafts were discarded
	// when the proposal was stored. They were, so the answer could do nothing
	// but stamp the chosen strategy's *name* onto the winner's draft: choosing
	// "compose" produced the Dockerfile detector's spec labeled
	// `strategy: compose`, which no builder implements, and the app was refused
	// at plan time with a message about the builder.
	Draft Draft `json:"draft,omitempty"`

	// Spec is this candidate's draft, assembled and completed with the
	// install's defaults — the thing accepting it would actually pin.
	//
	// Every candidate carries one, not just the winner, because any of them can
	// be adopted by answering the tie-break. Completing only the winner meant an
	// adopted runner-up arrived with no builder, no routing and no port, and
	// the deploy failed on "0 is not a usable port number" with nothing
	// pointing at why.
	Spec *spec.AppSpec `json:"spec,omitempty"`

	// Blocked is set when a detector recognizes the repository and cannot
	// proceed with it — a compose file using a construct that cannot cross the
	// boundary (R-099) is the case this exists for.
	//
	// It is deliberately not an error returned from Bid. The auction drops a
	// detector that errors, because one broken detector must not fail
	// detection; a blocked candidate dropped the same way would leave the user
	// with a buildpack guess and no idea why their compose file was ignored.
	// The reason is the most useful thing Pando has here, so the candidate
	// stays in the auction and carries it.
	Blocked error `json:"-"`
}

// Draft is the part of a spec a detector can fill in.
// Tagged because this crosses the API now. It used to be `json:"-"` on the
// candidate and never left the process, so Go's field names were nobody's
// problem; serializing it without tags put `Build` and `Workloads` into a
// payload where every other object is snake_case, and into the console's
// generated types alongside them.
type Draft struct {
	Workloads []spec.Workload `json:"workloads,omitempty"`
	Volumes   []spec.Volume   `json:"volumes,omitempty"`
	Slots     []spec.Slot     `json:"slots,omitempty"`
	Build     spec.Build      `json:"build"`
	Health    spec.Health     `json:"health,omitempty"`
	Warnings  []spec.Warning  `json:"warnings,omitempty"`
}

// Question is something detection could not work out.
//
// Prompt carries a hard content requirement from R-105: it must be answerable
// by a model that cannot see the repository, because the expected workflow is
// pasting it into the assistant that wrote the app. See Validate.
type Question struct {
	Key     string           `json:"key"`
	Prompt  string           `json:"prompt"`
	Why     string           `json:"why"`
	Kind    api.QuestionKind `json:"kind"`
	Options []string         `json:"options,omitempty"`

	// Deferred marks a question the trial run is expected to answer (R-097).
	//
	// This is the difference between something Pando does not know and something
	// a person has to tell it. A port is the clearest case: R-097 exists so that
	// Pando watches the app bind rather than asking, and R-005 says the person
	// deploying may not know what a port is. Holding such a question against the
	// questions-per-deploy budget would count a cost nobody pays.
	//
	// The question is still carried, because the trial run can fail to observe
	// an answer — at which point it stops being deferred and is put to a person.
	Deferred bool `json:"deferred,omitempty"`

	// Suggested is an AI adapter's answer to this question (R-338).
	//
	// Carried on the question rather than folded into the draft spec, so the
	// question stays on the page and a person can see the answer, check its
	// reason, and change it with the same request that answers any question.
	// It counts as an answer until a person gives one; a person's answer wins.
	Suggested *Suggestion `json:"suggested,omitempty"`
}

// Suggestion is an answer an AI adapter gave, with why and from what.
type Suggestion struct {
	Value    string   `json:"value"`
	Reason   string   `json:"reason,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// Open returns the asked questions nobody has answered: no answer from a
// person in answers, and no suggestion from an AI adapter.
func Open(questions []Question, answers map[string]string) []Question {
	var open []Question
	for _, q := range Asked(questions) {
		if strings.TrimSpace(answers[q.Key]) != "" || q.Suggested != nil {
			continue
		}
		open = append(open, q)
	}
	return open
}

// Asked returns the questions a person has to answer.
//
// Deferred questions are excluded: they are Pando's to resolve by watching the
// app start, not the user's to answer before it does.
func Asked(questions []Question) []Question {
	var asked []Question
	for _, q := range questions {
		if !q.Deferred {
			asked = append(asked, q)
		}
	}
	return asked
}

// Result is the outcome of an auction.
type Result struct {
	Winner    Candidate   `json:"winning_bid"`
	RunnersUp []Candidate `json:"runners_up"`

	// Questions are the winner's, plus anything the auction itself could not
	// settle — such as which of two close bids is right.
	Questions []Question `json:"questions"`

	Status string `json:"status"`

	// Blocked is the winner's reason, when the best reading of this repository
	// is one that cannot proceed. Surfacing it is the whole point: "your
	// compose file sets privileged: true, and here is why that cannot run" is
	// an answer, and silently falling through to a language guess is not.
	Blocked error `json:"-"`
}

// StrategyUnknown is what detection reports when it could not arrive at a build
// strategy. It is not a spec.BuildStrategy anyone can deploy — a spec never
// carries it — which is why it lives here rather than in the spec package.
const StrategyUnknown spec.BuildStrategy = "unknown"

// Statuses a detection can end in.
const (
	StatusReady        = "ready"
	StatusNeedsAnswers = "needs_answers"
	StatusUnknown      = "unknown"

	// StatusBlocked is not "Pando does not know". It is "Pando knows, and the
	// answer is that this cannot run as written" — which is a different thing
	// to show a user, and the reason travels with it.
	StatusBlocked = "blocked"
)

// Auction runs every detector and ranks the results.
type Auction struct {
	detectors []Detector
}

func NewAuction(detectors ...Detector) *Auction {
	return &Auction{detectors: detectors}
}

// Run bids every detector against the source and ranks them.
//
// A close second is not discarded: when two detectors land within
// closeEnough of each other the auction adds a question rather than breaking
// the tie itself. Picking between a compose file and a Dockerfile on a 0.02
// confidence difference is exactly the guess R-102 forbids.
func (a *Auction) Run(ctx context.Context, src api.SourceView) (Result, error) {
	var bids []Candidate
	for _, d := range a.detectors {
		c, err := d.Bid(ctx, src)
		if err != nil {
			// One detector failing must not fail detection: the others may
			// still recognize the repository.
			continue
		}
		c.Detector = d.Name()
		if c.Confidence > 0 {
			bids = append(bids, c)
		}
	}

	if len(bids) == 0 {
		return Result{
			Status: StatusUnknown,
			Winner: Candidate{Strategy: StrategyUnknown, Confidence: 0},
			// The prompt used to invite "how this app is normally started",
			// "npm start" for example, and CheckAnswers refuses every answer
			// when nothing bid: there is no reading for one to choose. So an
			// answer typed as asked was refused with VALID_INVALID (issue #90).
			// It says what Pando looked for and what would let it build the
			// app, and an answer is still refused with the same remedy. What
			// should be asked here instead is O-61.
			Questions: []Question{{
				Key:  "build_method",
				Kind: api.QuestionText,
				Prompt: "Pando could not work out how to build and run this app. It found no Dockerfile, " +
					"no compose file, no Procfile, and no project in a language Pando builds on its own " +
					"(Node.js, Python, Go, Ruby, PHP, Rust, Java, Kotlin, Scala, Clojure, .NET, Elixir, " +
					"Deno, Crystal, Haskell, Perl and R, or a static site with an index.html). A description " +
					"of how the app starts is not something Pando can build from. Valid answer: none, because " +
					"no answer is accepted; add a Dockerfile or a compose file that builds the app, or a Procfile " +
					"naming its start command beside its language's manifest, and run detection again.",
				Why: "Pando needs something in the repository that says how to turn it into something it can run.",
			}},
		}, nil
	}

	sort.SliceStable(bids, func(i, j int) bool { return bids[i].Confidence > bids[j].Confidence })

	winner := bids[0]

	// A compose file Pando refused is not outbid.
	//
	// A compose file is the author's complete statement of what the app is
	// (R-096); a Dockerfile is a statement about one image. A repository with
	// both bids twice, and the Dockerfile bids higher when it declares a port —
	// so a compose file refused for one construct lost the auction quietly, and
	// the app was imported as the Dockerfile alone. Everything the compose file
	// knew went with it: the database beside the app, the variable that reached
	// it, the second service. What came out built and deployed and then logged
	// `DATABASE_URL is required` forever.
	//
	// Refusing to import it is a thing Pando knows about this repository, and
	// R-099 says the reason is shown. Reading it a different way instead is the
	// silent fallback that TestR099_ARejectedComposeFileDoesNotSilentlyBecomeA\
	// BuildpackGuess exists to prevent; it was only ever prevented for the
	// repositories where nothing else outbid it.
	for _, bid := range bids {
		if bid.Blocked != nil && bid.Strategy == spec.BuildCompose {
			winner = bid
			break
		}
	}
	questions := append([]Question(nil), winner.Questions...)

	// A reading that refuses the repository — a library, which has nothing to
	// run (R-021) — is not one of two plausible ways to build it, so it is
	// never offered as the other side of a tie. Offered, a committed static
	// site beside a package.json that publishes files asked "static or
	// buildpack (this looks like a node.js library)" (issue #90).
	if len(bids) > 1 && bids[0].Blocked == nil && bids[1].Blocked == nil &&
		bids[0].Confidence-bids[1].Confidence < closeEnough {
		questions = append(questions, tieBreak(bids[0], bids[1]))
	}

	// Only questions a person must answer make a detection incomplete. One the
	// trial run will resolve leaves detection ready to proceed to it.
	//
	// The same rule the job applies after the trial run, from one function, so
	// the status a caller sees before the trial and the one it sees after
	// cannot disagree about what "ready" means.
	status := StatusFor(winner, questions)

	// A blocked winner overrides everything else. There is no point asking
	// which service is primary in a compose file that cannot be imported.
	if winner.Blocked != nil {
		status = StatusBlocked
		questions = nil
	}

	return Result{
		Winner:    winner,
		RunnersUp: bids[1:],
		Questions: questions,
		Status:    status,
		Blocked:   winner.Blocked,
	}, nil
}

const (
	// closeEnough is the margin within which two bids are treated as a tie the
	// auction must not settle on its own.
	closeEnough = 0.15

	// There was a third rule here: a confidence below 0.5 said needs_answers
	// even with nothing to ask. It was removed with the port question that made
	// it invisible — see statusFor.
)

// tieBreak asks the user to choose between two close readings.
func tieBreak(first, second Candidate) Question {
	return Question{
		Key:     "build_strategy",
		Kind:    api.QuestionChoice,
		Options: []string{string(first.Strategy), string(second.Strategy)},
		Prompt: "Pando found two plausible ways to build this app: " +
			describe(first) + ", and " + describe(second) + ". " +
			"Valid answer: either \"" + string(first.Strategy) + "\" or \"" + string(second.Strategy) + "\".",
		Why: "Pando needs to know which one describes how this app is meant to run.",
	}
}

func describe(c Candidate) string {
	if len(c.Evidence) > 0 {
		return string(c.Strategy) + " (" + strings.ToLower(c.Evidence[0]) + ")"
	}
	return string(c.Strategy)
}
