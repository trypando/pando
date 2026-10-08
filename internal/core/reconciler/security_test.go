package reconciler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/state"
)

// The security pass, without a database, a scanner or a clock that moves.
//
// What is being tested is the sequence — mark, warn, wait, stop, recover — and
// each step of it is a decision about somebody's running service.

type fakeScores struct {
	report security.Report
	ref    string
	calls  int
}

func (f *fakeScores) Configured() (string, bool) { return f.ref, f.ref != "" }

// Place answers with the report's standing for every app, and counts calls.
func (f *fakeScores) Place(_ context.Context, scores map[string]security.Scores) (map[string]security.Placed, error) {
	f.calls++
	out := make(map[string]security.Placed, len(scores))
	for appID := range scores {
		out[appID] = security.Placed{Score: f.report.Standing.Score, Verdict: f.report.Standing.Verdict}
	}
	return out, nil
}

type fakeState struct {
	apps    []state.SecurityState
	marked  map[string]time.Time
	cleared map[string]bool
	stopped map[string]bool

	// threshold is what the pass last asked for apps against.
	threshold state.SecurityThreshold
}

func newFakeState(apps ...state.SecurityState) *fakeState {
	return &fakeState{
		apps:    apps,
		marked:  map[string]time.Time{},
		cleared: map[string]bool{},
		stopped: map[string]bool{},
	}
}

func (f *fakeState) LiveSecurityState(_ context.Context, t state.SecurityThreshold) ([]state.SecurityState, error) {
	f.threshold = t
	return f.apps, nil
}
func (f *fakeState) MarkInsecure(_ context.Context, appID string, at time.Time) error {
	if _, already := f.marked[appID]; !already {
		f.marked[appID] = at
	}
	return nil
}
func (f *fakeState) ClearInsecure(_ context.Context, appID string) error {
	f.cleared[appID] = true
	return nil
}
func (f *fakeState) SetStoppedForSecurity(_ context.Context, appID string, stopped bool) error {
	f.stopped[appID] = stopped
	return nil
}

type fakeDesired struct{ desired map[string]string }

func (f *fakeDesired) SetDesiredState(_ context.Context, appID, desired string) error {
	f.desired[appID] = desired
	return nil
}

type fakePolicy struct{ doc policy.Document }

func (f fakePolicy) Document(context.Context) (policy.Document, error) { return f.doc, nil }

type fakeNotifier struct{ sent []string }

func (f *fakeNotifier) Notify(_ context.Context, _, _, subject, body string) {
	f.sent = append(f.sent, subject+" — "+body)
}

func score(n int) *int { return &n }

func gc(t *testing.T, doc policy.Document, report security.Report, now time.Time, apps ...state.SecurityState) (*GC, *fakeState, *fakeDesired, *fakeNotifier) {
	t.Helper()
	st := newFakeState(apps...)
	desired := &fakeDesired{desired: map[string]string{}}
	notifier := &fakeNotifier{}
	return &GC{
		Logger:        zap.NewNop(),
		Clock:         clock.NewFake(now),
		Security:      &fakeScores{report: report, ref: "scn_trivy"},
		SecurityState: st,
		PolicyStore:   fakePolicy{doc: doc},
		Desired:       desired,
		Notifier:      notifier,
	}, st, desired, notifier
}

// TestR315_AnAppThatFallsBelowIsWarnedAndNotStopped asserts the promise that
// makes this feature liveable: a policy change or a newly published CVE does
// not take somebody's working service away in the same minute.
func TestR315_AnAppThatFallsBelowIsWarnedAndNotStopped(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70, InsecureAction: policy.InsecureStop, InsecureGraceHours: 24}
	report := security.Report{Standing: security.Standing{Verdict: security.VerdictInsecure, Score: score(40), Threshold: 70}}

	g, st, desired, notifier := gc(t, doc, report, now, state.SecurityState{
		AppID: "app_1", Name: "notes", OwnerUserID: "usr_1",
		State: state.StateRunning, DesiredState: state.StateRunning, PinnedSpecID: "spec_1",
	})

	g.enforceSecurity(context.Background())

	require.Equal(t, now, st.marked["app_1"], "the clock starts now")
	require.Empty(t, desired.desired, "nothing is stopped in the same pass that found it")
	require.Len(t, notifier.sent, 1, "the owner is told at the moment the clock starts")
	require.Contains(t, notifier.sent[0], "24 hours", "the deadline is in the message")
	require.Contains(t, notifier.sent[0], "70")
}

// TestR316_StoppingWaitsForTheGraceAndThenStops asserts both halves of the
// grace period, at the hour either side of it.
func TestR316_StoppingWaitsForTheGraceAndThenStops(t *testing.T) {
	found := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70, InsecureAction: policy.InsecureStop, InsecureGraceHours: 24}
	report := security.Report{Standing: security.Standing{Verdict: security.VerdictInsecure, Score: score(40), Threshold: 70}}

	app := state.SecurityState{
		AppID: "app_1", Name: "notes", OwnerUserID: "usr_1",
		State: state.StateRunning, DesiredState: state.StateRunning, PinnedSpecID: "spec_1",
		InsecureSince: &found,
	}

	before, _, desiredBefore, _ := gc(t, doc, report, found.Add(23*time.Hour), app)
	before.enforceSecurity(context.Background())
	require.Empty(t, desiredBefore.desired, "an hour of grace left is grace")

	after, st, desiredAfter, _ := gc(t, doc, report, found.Add(24*time.Hour), app)
	after.enforceSecurity(context.Background())
	require.Equal(t, state.StateStopped, desiredAfter.desired["app_1"],
		"stopping is a desired state, never a delete and never `failed`")
	require.True(t, st.stopped["app_1"], "who stopped it is recorded, because it decides who may start it")
}

// Warning is the default, and a warning never stops anything however long it
// has been true.
func TestR315_WarnOnlyNeverStops(t *testing.T) {
	found := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70}
	report := security.Report{Standing: security.Standing{Verdict: security.VerdictInsecure, Score: score(10), Threshold: 70}}

	g, _, desired, _ := gc(t, doc, report, found.Add(300*time.Hour), state.SecurityState{
		AppID: "app_1", Name: "notes", State: state.StateRunning, DesiredState: state.StateRunning,
		PinnedSpecID: "spec_1", InsecureSince: &found,
	})
	g.enforceSecurity(context.Background())
	require.Empty(t, desired.desired)
}

// TestR316_AnAppPandoStoppedStartsAgainAndOneItsOwnerStoppedDoesNot asserts the
// distinction `stopped_for_security` exists for.
func TestR316_AnAppPandoStoppedStartsAgainAndOneItsOwnerStoppedDoesNot(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	found := now.Add(-48 * time.Hour)
	doc := policy.Document{MinSecurityScore: 70, InsecureAction: policy.InsecureStop}
	recovered := security.Report{Standing: security.Standing{Verdict: security.VerdictOK, Score: score(90), Threshold: 70}}

	pando, st, desired, _ := gc(t, doc, recovered, now, state.SecurityState{
		AppID: "app_1", Name: "notes", State: state.StateStopped, DesiredState: state.StateStopped,
		PinnedSpecID: "spec_1", InsecureSince: &found, StoppedForSecurity: true,
	})
	pando.enforceSecurity(context.Background())
	require.Equal(t, state.StateRunning, desired.desired["app_1"], "Pando starts what Pando stopped")
	require.True(t, st.cleared["app_1"])
	require.False(t, st.stopped["app_1"])

	owner, _, ownerDesired, _ := gc(t, doc, recovered, now, state.SecurityState{
		AppID: "app_2", Name: "quiet", State: state.StateStopped, DesiredState: state.StateStopped,
		PinnedSpecID: "spec_2", InsecureSince: &found, StoppedForSecurity: false,
	})
	owner.enforceSecurity(context.Background())
	require.Empty(t, ownerDesired.desired, "an app its owner stopped stays stopped")
}

// A threshold with no scanner behind it enforces nothing (R-317).
func TestR317_WithNoScannerThePassDoesNothing(t *testing.T) {
	found := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70, InsecureAction: policy.InsecureStop}

	st := newFakeState(state.SecurityState{
		AppID: "app_1", State: state.StateRunning, DesiredState: state.StateRunning,
		PinnedSpecID: "spec_1", InsecureSince: &found,
	})
	desired := &fakeDesired{desired: map[string]string{}}

	g := &GC{
		Logger:        zap.NewNop(),
		Clock:         clock.NewFake(found.Add(300 * time.Hour)),
		Security:      &fakeScores{ref: ""},
		SecurityState: st,
		PolicyStore:   fakePolicy{doc: doc},
		Desired:       desired,
	}
	g.enforceSecurity(context.Background())
	require.Empty(t, desired.desired)
	require.Empty(t, st.stopped)
}

// TestR315_TheSecurityPassPlacesEveryAppInOneCall asserts the batched path
// (issue #72): however many apps there are, the pass places them against
// policy once, rather than reading a report — a policy load and two queries —
// per app.
func TestR315_TheSecurityPassPlacesEveryAppInOneCall(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70, IgnoreUnfixableFindings: true}
	report := security.Report{Standing: security.Standing{Verdict: security.VerdictInsecure, Score: score(40), Threshold: 70}}

	var apps []state.SecurityState
	for i := 0; i < 50; i++ {
		apps = append(apps, state.SecurityState{
			AppID: fmt.Sprintf("app_%02d", i), Name: "notes",
			State: state.StateRunning, DesiredState: state.StateRunning, PinnedSpecID: "spec_1",
		})
	}
	g, st, _, _ := gc(t, doc, report, now, apps...)
	g.enforceSecurity(context.Background())

	require.Equal(t, 1, g.Security.(*fakeScores).calls)
	require.Len(t, st.marked, 50, "and every app was placed")
	// The store is asked only for apps the pass could act on, under the
	// threshold policy sets (issue #72).
	require.Equal(t, state.SecurityThreshold{MinScore: 70, IgnoreUnfixable: true}, st.threshold)
}
