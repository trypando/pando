package security_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
)

func finding(s api.Severity, id string) api.Finding {
	return api.Finding{ID: id, Severity: s, Title: id}
}

// TestR311_OnlyADigestNamesAnImageAScanCovers asserts the rule a deploy's
// reuse turns on (R-311, R-312): an image ID or a reference pinned by digest
// names what the image holds, and a tag does not.
func TestR311_OnlyADigestNamesAnImageAScanCovers(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	require.True(t, security.ContentAddressed("sha256:"+digest))
	require.True(t, security.ContentAddressed("registry.internal:5000/pando/apps/app_x@sha256:"+digest))
	require.False(t, security.ContentAddressed("nginx:1.27-alpine"))
	require.False(t, security.ContentAddressed("pando/app_x:dep_01"))
	require.False(t, security.ContentAddressed(""))
}

// TestR313_OneCriticalCostsMoreThanFiftyLows asserts the property the weights
// exist for.
//
// A score that averaged, or that scaled with the size of the app, would let one
// real problem hide behind a long tail of noise — and a threshold set against
// it would mean something different for every app.
func TestR313_OneCriticalCostsMoreThanFiftyLows(t *testing.T) {
	var lows []api.Finding
	for i := 0; i < 50; i++ {
		lows = append(lows, finding(api.SeverityLow, "low"))
	}

	require.Equal(t, 75, security.Score([]api.Finding{finding(api.SeverityCritical, "CVE-1")}))
	require.Equal(t, 50, security.Score(lows))
	require.Less(t,
		security.Score([]api.Finding{finding(api.SeverityCritical, "CVE-1")}),
		security.Score(lows[:20]),
		"one critical is worse than twenty lows")
}

func TestScoreIsFlooredAtZeroAndCleanIsAHundred(t *testing.T) {
	require.Equal(t, 100, security.Score(nil))
	require.Equal(t, 100, security.Score([]api.Finding{}))

	var many []api.Finding
	for i := 0; i < 20; i++ {
		many = append(many, finding(api.SeverityCritical, "CVE"))
	}
	require.Equal(t, 0, security.Score(many), "a score never goes negative")
}

// An unknown severity is not free and is not critical. A finding nobody has
// graded still costs something, and an adapter with a typo must not be able to
// turn a list of critical findings into a clean app.
func TestAnUngradedFindingStillCosts(t *testing.T) {
	require.Equal(t, 99, security.Score([]api.Finding{finding(api.SeverityUnknown, "x")}))
	require.Equal(t, 99, security.Score([]api.Finding{finding(api.Severity("sev:???"), "x")}))
}

func TestWorstIsOrderedAndStable(t *testing.T) {
	findings := []api.Finding{
		finding(api.SeverityLow, "b"),
		finding(api.SeverityCritical, "z"),
		finding(api.SeverityHigh, "a"),
		finding(api.SeverityCritical, "a"),
	}

	worst := security.Worst(findings, 3)
	require.Len(t, worst, 3)
	require.Equal(t, []string{"a", "z", "a"}, []string{worst[0].ID, worst[1].ID, worst[2].ID})
	require.Equal(t, api.SeverityCritical, worst[0].Severity)
	require.Equal(t, api.SeverityHigh, worst[2].Severity)

	require.Equal(t, worst, security.Worst(findings, 3), "the same scan ranks the same way twice")
}

func score(n int) *int { return &n }

// TestR314_ATresholdOnlyBitesWhenThereIsSomethingToEnforceIt covers the four
// answers Evaluate can give, and which of them stop a deploy.
func TestR314_ATresholdOnlyBitesWhenThereIsSomethingToEnforceIt(t *testing.T) {
	off := policy.Document{}
	on := policy.Document{MinSecurityScore: 70}

	// No threshold: nothing to say, whatever the score.
	require.Equal(t, security.VerdictOK, security.Evaluate(off, score(10), time.Now(), true, nil).Verdict)

	// A threshold with no scanner is inert, and a deploy still goes (R-317):
	// blocking every deploy because a component is missing is worse than a rule
	// that is visibly off.
	inert := security.Evaluate(on, nil, time.Time{}, false, nil)
	require.Equal(t, security.VerdictInert, inert.Verdict)
	require.True(t, inert.Deployable())

	// A threshold with a scanner and no scan blocks: "we have never looked" is
	// not a pass (R-318).
	unscanned := security.Evaluate(on, nil, time.Time{}, true, nil)
	require.Equal(t, security.VerdictUnscanned, unscanned.Verdict)
	require.False(t, unscanned.Deployable())

	require.True(t, security.Evaluate(on, score(70), time.Now(), true, nil).Deployable(), "at the threshold passes")
	require.False(t, security.Evaluate(on, score(69), time.Now(), true, nil).Deployable())
}

// TestR316_TheGraceIsMeasuredFromWhenItWasFirstFound asserts that the deadline
// an owner is told about is the deadline that applies.
func TestR316_TheGraceIsMeasuredFromWhenItWasFirstFound(t *testing.T) {
	found := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70, InsecureAction: policy.InsecureStop, InsecureGraceHours: 24}

	standing := security.Evaluate(doc, score(40), found, true, &found)
	require.Equal(t, security.VerdictInsecure, standing.Verdict)
	require.Equal(t, found.Add(24*time.Hour), standing.StopAt)

	require.False(t, security.Due(at(found.Add(23*time.Hour)), doc, &found))
	require.True(t, security.Due(at(found.Add(24*time.Hour)), doc, &found))

	// Warning is the default, and warning never stops anything.
	warns := policy.Document{MinSecurityScore: 70}
	require.False(t, security.Due(at(found.Add(300*time.Hour)), warns, &found))
	require.Zero(t, security.Evaluate(warns, score(40), found, true, &found).StopAt)
}

// A grace of zero with stopping on is the default, not "immediately". An
// accidental zero in a policy document must not empty a host.
func TestR316_AZeroGraceIsTheDefaultNotImmediately(t *testing.T) {
	found := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	doc := policy.Document{MinSecurityScore: 70, InsecureAction: policy.InsecureStop}

	require.Equal(t, policy.DefaultInsecureGraceHours, doc.GraceHours())
	require.False(t, security.Due(at(found), doc, &found))
	require.True(t, security.Due(at(found.Add(time.Duration(policy.DefaultInsecureGraceHours)*time.Hour)), doc, &found))
}

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

func at(t time.Time) fixedClock { return fixedClock(t) }

// TestR313_PolicyMayCountOnlyWhatCanBeFixed asserts the second question a score
// can answer.
//
// "What is wrong with this app" and "what could its owner do about it today" are
// different, and an installation that only acts on the second should not be
// scored on the first. What matters is that one filter drives both the number
// and the list: a score that ignored a finding while the list showed it would
// leave somebody working out why fixing one changed nothing.
func TestR313_PolicyMayCountOnlyWhatCanBeFixed(t *testing.T) {
	findings := []api.Finding{
		{ID: "CVE-1", Severity: api.SeverityCritical, Fix: "1.2.4"},
		{ID: "CVE-2", Severity: api.SeverityHigh},
		{ID: "CVE-3", Severity: api.SeverityHigh, Fix: "2.0.1"},
		{ID: "CVE-4", Severity: api.SeverityMedium},
	}

	require.Equal(t, 100-25-10-10-3, security.Score(findings))

	fixable := security.Fixable(findings)
	require.Len(t, fixable, 2)
	require.Equal(t, []string{"CVE-1", "CVE-3"}, ids(fixable))
	require.Equal(t, 100-25-10, security.Score(fixable))
}

// Findings come back worst first, and the same scan orders the same way twice.
// A list whose order is the scanner's output order changes under the reader for
// no reason.
func TestFindingsAreRankedBySeverity(t *testing.T) {
	findings := []api.Finding{
		{ID: "d", Severity: api.SeverityLow},
		{ID: "b", Severity: api.SeverityCritical},
		{ID: "c", Severity: api.SeverityMedium},
		{ID: "a", Severity: api.SeverityCritical},
		{ID: "e", Severity: api.SeverityHigh},
	}

	ranked := security.Ranked(findings)
	require.Equal(t, []string{"a", "b", "e", "c", "d"}, ids(ranked))
	require.Equal(t, ids(ranked), ids(security.Ranked(findings)))
	require.Len(t, ranked, len(findings), "ranking drops nothing")
}

func ids(findings []api.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.ID)
	}
	return out
}
