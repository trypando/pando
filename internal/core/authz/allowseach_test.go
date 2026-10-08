package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// batching is a counting store that also reads several apps' control grants
// at once (authz.BatchStore), and counts that too.
type batching struct {
	*counting
	batches int
	fail    bool
}

func (b *batching) ControlGrantsForApps(ctx context.Context, appIDs []string, p authz.Principal) (map[string][]authz.Grant, error) {
	b.batches++
	if b.fail {
		return nil, errs.New(errs.Internal, "Could not read permissions for these apps.")
	}
	out := map[string][]authz.Grant{}
	for _, appID := range appIDs {
		grants, err := b.store.ControlGrantsFor(ctx, appID, p)
		if err != nil {
			return nil, err
		}
		out[appID] = grants
	}
	return out, nil
}

// TestR081_AllowsEachAsksOnceForEveryAppAndAnswersAsAllowsDoes asserts that
// AllowsEach — what a list of apps asks to show what the caller may do on
// each — gives, app by app and verb by verb, exactly Allows's answer, while
// reading the principal, the policy, the install grants and every app's
// control grants once for the whole list rather than once per app (issue #72).
func TestR081_AllowsEachAsksOnceForEveryAppAndAnswersAsAllowsDoes(t *testing.T) {
	ctx := context.Background()
	const other, third, group = "app_other", "app_third", "grp_team"
	s := newStore()
	s.userStatus[alice] = "active"
	s.userStatus[bob] = "active"
	s.control[app] = []authz.Grant{
		{ID: "gr_1", Plane: "control", PrincipalKind: "user", PrincipalID: alice, RoleID: authz.RoleOwner},
	}
	s.control[other] = []authz.Grant{
		{ID: "gr_2", Plane: "control", PrincipalKind: "group", PrincipalID: group, RoleID: authz.RoleViewer},
	}
	s.install = []authz.Grant{
		{ID: "gr_3", Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAppViewer},
	}
	apps := []string{app, other, third}
	verbs := []authz.Verb{authz.AppView, authz.AppGrantsManage}

	for name, p := range map[string]authz.Principal{
		"owner of one, viewer of another through a group": {Kind: authz.KindUser, ID: alice, UserID: alice, Status: "active", Groups: []string{group}},
		"app viewer install-wide":                         activeUser(bob),
		"anonymous":                                       authz.Anonymous(),
	} {
		reads := 0
		b := &batching{counting: &counting{store: s}}
		a := authz.New(b, countingPolicy{reads: &reads}, &recorder{})
		got, err := a.AllowsEach(ctx, p, apps, verbs...)
		require.NoError(t, err, name)
		require.LessOrEqual(t, b.batches, 1, "%s: every app's grants in one read", name)
		require.Zero(t, b.control, "%s: no app's grants read one at a time", name)
		require.LessOrEqual(t, b.install, 1, name)
		require.LessOrEqual(t, b.status, 1, name)
		require.LessOrEqual(t, reads, 1, name)

		single := authz.New(s, countingPolicy{reads: new(int)}, &recorder{})
		for _, appID := range apps {
			for _, verb := range verbs {
				want, err := single.Allows(ctx, p, appID, verb)
				require.NoError(t, err)
				require.Equal(t, want, got[appID][verb], "%s: %s on %s", name, verb, appID)
			}
		}
	}

	// A store without the batch read is asked app by app, with the same
	// answers.
	plain := &counting{store: s}
	got, err := authz.New(plain, nil, &recorder{}).AllowsEach(ctx, activeUser(alice), apps, authz.AppView)
	require.NoError(t, err)
	require.Equal(t, 3, plain.control)
	require.True(t, got[app][authz.AppView])
	require.False(t, got[third][authz.AppView])

	// The system principal holds everything; an install verb is refused; a
	// failed batch read is a failure, not a denial.
	sys, err := authz.New(s, nil, nil).AllowsEach(ctx, authz.System(), apps, authz.AppDelete)
	require.NoError(t, err)
	require.True(t, sys[third][authz.AppDelete])
	_, err = authz.New(s, nil, nil).AllowsEach(ctx, activeUser(alice), apps, authz.InstallView)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	failing := &batching{counting: &counting{store: s}, fail: true}
	_, err = authz.New(failing, nil, nil).AllowsEach(ctx, activeUser(alice), apps, authz.AppView)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}
