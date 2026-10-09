package applimit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/trypando/pando/internal/core/applimit"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

func intp(n int) *int { return &n }

var (
	finance     = state.GroupLimit{GroupID: "grp_fin", Name: "Finance", MaxApps: 5}
	development = state.GroupLimit{GroupID: "grp_dev", Name: "Development", MaxApps: 10}
	research    = state.GroupLimit{GroupID: "grp_res", Name: "Research", MaxApps: 0}
)

// TestR244_TheAccountBeatsItsGroups asserts R-244: a value on the account
// applies whatever the groups say, in either direction.
func TestR244_TheAccountBeatsItsGroups(t *testing.T) {
	doc := policy.Document{MaxAppsPerUser: 3}

	l := applimit.Resolve(doc, state.LimitFacts{UserMaxApps: intp(2), Groups: []state.GroupLimit{development}})
	assert.Equal(t, 2, l.Limit)
	assert.Equal(t, applimit.SourceUser, l.Source)

	l = applimit.Resolve(doc, state.LimitFacts{UserMaxApps: intp(0), Groups: []state.GroupLimit{finance}})
	assert.Equal(t, 0, l.Limit, "0 on the account is unlimited")
	assert.Equal(t, applimit.SourceUser, l.Source)
}

// TestR244_TheMostGenerousGroupWins asserts R-244: the highest group value
// applies, and a group's unlimited beats any number.
func TestR244_TheMostGenerousGroupWins(t *testing.T) {
	doc := policy.Document{MaxAppsPerUser: 3}

	l := applimit.Resolve(doc, state.LimitFacts{Groups: []state.GroupLimit{finance, development}})
	assert.Equal(t, 10, l.Limit)
	assert.Equal(t, "grp_dev", l.GroupID)
	assert.Equal(t, "Development", l.GroupName)

	l = applimit.Resolve(doc, state.LimitFacts{Groups: []state.GroupLimit{finance, research, development}})
	assert.Equal(t, 0, l.Limit)
	assert.Equal(t, "grp_res", l.GroupID)
}

// TestR244_HostPolicyIsTheDefaultAndUnlimitedByDefault asserts R-244 and
// R-270: with nothing on the account or its groups, host policy applies, and
// the shipped policy is unlimited.
func TestR244_HostPolicyIsTheDefaultAndUnlimitedByDefault(t *testing.T) {
	l := applimit.Resolve(policy.Document{MaxAppsPerUser: 3}, state.LimitFacts{Owned: 2})
	assert.Equal(t, 3, l.Limit)
	assert.Equal(t, applimit.SourcePolicy, l.Source)
	assert.Equal(t, 2, l.Owned)

	l = applimit.Resolve(policy.Default(), state.LimitFacts{})
	assert.Equal(t, 0, l.Limit)
}

// TestR105_AnAppLimitRefusalSaysWhoSetItAndWhatToDo asserts R-244 and R-105:
// the refusal names the limit, where it comes from and who can raise it.
func TestR105_AnAppLimitRefusalSaysWhoSetItAndWhatToDo(t *testing.T) {
	l := applimit.Resolve(policy.Document{}, state.LimitFacts{Groups: []state.GroupLimit{finance}, Owned: 5})
	err := applimit.Refusal(l, 5)

	var e *errs.Error
	assert.ErrorAs(t, err, &e)
	assert.Equal(t, errs.PolicyAppLimitReached, e.Code)
	assert.Equal(t, "You own 5 apps, and 5 is the limit set for the Finance group, so Pando did not create this one.", e.Message)
	assert.Equal(t, "Ask an administrator to raise the limit, or delete an app you no longer need.", e.Remedy)
	assert.Equal(t, 403, e.Status())

	err = applimit.Refusal(applimit.Resolve(policy.Document{MaxAppsPerUser: 1}, state.LimitFacts{Owned: 1}), 1)
	assert.Contains(t, err.Error(), "You own 1 app, and 1 is the limit this installation sets for each person")
}
