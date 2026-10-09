package audit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestR379_EveryActionHasAnOutcome asserts R-379: every catalogued action is
// recorded with the outcome its name says (design 12 §3.1), so a new
// something.denied cannot be recorded as a success.
//
// The expectation is worked out here from the last segment of the name, not by
// calling OutcomeOf, so the test and the function can disagree.
func TestR379_EveryActionHasAnOutcome(t *testing.T) {
	seen := map[Outcome]int{}
	for _, action := range Actions {
		last := action[strings.LastIndex(action, ".")+1:]
		want := OutcomeSuccess
		switch {
		case strings.Contains(last, "denied"), strings.Contains(last, "refused"):
			want = OutcomeDenied
		case strings.Contains(last, "failed"), last == "rolled_back":
			want = OutcomeFailed
		}
		assert.Equal(t, want, OutcomeOf(action), action)
		seen[OutcomeOf(action)]++
	}
	for _, o := range []Outcome{OutcomeSuccess, OutcomeDenied, OutcomeFailed} {
		assert.Positive(t, seen[o], "no catalogued action has outcome %s", o)
	}
}
