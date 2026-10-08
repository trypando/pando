package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR182_EgressWrittenBeforeIssue79IsReadInTodaysTerms asserts R-182's
// amendment: an old spec keeps meaning what its author wrote, and never
// loosens in the reading — an old allowlist becomes the app's own list,
// which narrows, where it used to replace the installation's.
func TestR182_EgressWrittenBeforeIssue79IsReadInTodaysTerms(t *testing.T) {
	cases := []struct {
		name string
		in   spec.Egress
		want spec.Egress
	}{
		{"empty is inherit", spec.Egress{}, spec.Egress{Mode: spec.EgressInherit}},
		{"allow_all is inherit", spec.Egress{Mode: spec.EgressAllowAll}, spec.Egress{Mode: spec.EgressInherit}},
		{"block_private is the switch",
			spec.Egress{Mode: spec.EgressBlockPrivate},
			spec.Egress{Mode: spec.EgressInherit, BlockPrivate: ptr(true)}},
		{"block_private keeps an explicit switch",
			spec.Egress{Mode: spec.EgressBlockPrivate, BlockPrivate: ptr(false)},
			spec.Egress{Mode: spec.EgressInherit, BlockPrivate: ptr(false)}},
		{"allowlist field becomes the list",
			spec.Egress{Mode: spec.EgressAllowlist, Allowlist: []string{"api.example.com"}},
			spec.Egress{Mode: spec.EgressAllowlist, List: []string{"api.example.com"}}},
		{"the list wins over the old field",
			spec.Egress{Mode: spec.EgressAllowlist, List: []string{"a.example"}, Allowlist: []string{"b.example"}},
			spec.Egress{Mode: spec.EgressAllowlist, List: []string{"a.example"}}},
		{"today's shape is left alone",
			spec.Egress{Mode: spec.EgressDenylist, List: []string{"x.example"}, Add: []string{"y.example"}, Remove: []string{"z.example"}, BlockPrivate: ptr(true)},
			spec.Egress{Mode: spec.EgressDenylist, List: []string{"x.example"}, Add: []string{"y.example"}, Remove: []string{"z.example"}, BlockPrivate: ptr(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in
			got.Normalize()
			require.Equal(t, tc.want, got)

			again := got
			again.Normalize()
			require.Equal(t, got, again, "Normalize is idempotent")
		})
	}
}

// TestR184_AnUnchangedEgressIsTheSameHoweverItIsWritten asserts what the
// verb gate starts from (R-184): saving an old spec again has not changed
// its egress, and a real change is seen.
func TestR184_AnUnchangedEgressIsTheSameHoweverItIsWritten(t *testing.T) {
	require.True(t, spec.Egress{}.Same(spec.Egress{Mode: spec.EgressInherit}))
	require.True(t, spec.Egress{Mode: spec.EgressAllowAll}.Same(spec.Egress{}))
	require.True(t, spec.Egress{Mode: spec.EgressAllowlist, Allowlist: []string{"a.example"}}.
		Same(spec.Egress{Mode: spec.EgressAllowlist, List: []string{"a.example"}}))
	require.True(t, spec.Egress{Mode: spec.EgressBlockPrivate}.Same(spec.Egress{BlockPrivate: ptr(true)}))

	require.False(t, spec.Egress{}.Same(spec.Egress{Add: []string{"a.example"}}))
	require.False(t, spec.Egress{}.Same(spec.Egress{Remove: []string{"a.example"}}))
	require.False(t, spec.Egress{}.Same(spec.Egress{BlockPrivate: ptr(false)}), "an explicit off differs from inheriting")
	require.False(t, spec.Egress{BlockPrivate: ptr(true)}.Same(spec.Egress{BlockPrivate: ptr(false)}))
	require.False(t, spec.Egress{Mode: spec.EgressAllowlist, List: []string{"a.example"}}.
		Same(spec.Egress{Mode: spec.EgressDenylist, List: []string{"a.example"}}))
}

// TestR185_EgressEntriesAreValidated asserts R-185: every entry an app
// writes names a destination, and a list says how it is read.
func TestR185_EgressEntriesAreValidated(t *testing.T) {
	ok := []spec.Egress{
		{},
		{Mode: spec.EgressInherit, Add: []string{"api.example.com", "*.example.com", "203.0.113.7", "10.0.0.0/8", "api.example.com:443", "[2001:db8::1]:443", "*"}},
		{Mode: spec.EgressAllowlist, List: []string{"api.example.com"}},
		{Mode: spec.EgressDenylist, List: []string{"2001:db8::/32"}, Remove: []string{"evil.example"}},
		{Mode: spec.EgressAllowlist, Allowlist: []string{"api.example.com"}},
		{Mode: spec.EgressBlockPrivate},
	}
	for _, e := range ok {
		s := valid()
		s.Egress = e
		require.NoError(t, spec.Validate(s), "%+v", e)
	}

	bad := []struct {
		egress spec.Egress
		says   string
	}{
		{spec.Egress{Mode: "everything"}, "everything"},
		{spec.Egress{List: []string{"api.example.com"}}, "allowlist"},
		{spec.Egress{Mode: spec.EgressAllowlist, List: []string{"not a host"}}, "egress.list"},
		{spec.Egress{Add: []string{"10.0.0.0/33"}}, "egress.add"},
		{spec.Egress{Remove: []string{"*.*.example"}}, "egress.remove"},
		{spec.Egress{Add: []string{"api.example.com:99999"}}, "egress.add"},
		{spec.Egress{Add: []string{""}}, "egress.add"},
	}
	for _, tc := range bad {
		s := valid()
		s.Egress = tc.egress
		err := spec.Validate(s)
		require.Error(t, err, "%+v", tc.egress)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
		e := errs.As(err)
		require.Contains(t, e.Message+" "+e.Remedy, tc.says, "R-105: the refusal names what to fix")
	}
}

// TestR158_AnAppThatRequiresApprovalCannotAlsoAutoDeploy asserts R-158 for
// the requirement an app makes of itself: the two settings in one spec are
// refused, and the refusal says why.
func TestR158_AnAppThatRequiresApprovalCannotAlsoAutoDeploy(t *testing.T) {
	s := valid()
	s.Deploy.RequireApproval = true
	s.Deploy.AutoDeploy = spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerBranchUpdated}

	err := spec.Validate(s)
	require.Error(t, err)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, "approved")
	require.NotEmpty(t, e.Remedy)
	require.NotContains(t, e.Message, "Error:")

	s.Deploy.AutoDeploy.Enabled = false
	require.NoError(t, spec.Validate(s), "approval alone is fine")

	s.Deploy.RequireApproval = false
	s.Deploy.AutoDeploy.Enabled = true
	require.NoError(t, spec.Validate(s), "auto-deploy alone is fine")
}

// TestR141_AutoDeploySettingsAreChecked asserts that a trigger Pando does not
// know and a tag pattern that is not one are each refused, saying what to
// change.
func TestR141_AutoDeploySettingsAreChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*spec.AppSpec)
		says string
	}{
		{"unknown trigger", func(s *spec.AppSpec) { s.Deploy.AutoDeploy.Trigger = "on_tuesdays" }, "release_tagged"},
		{"bad pattern", func(s *spec.AppSpec) { s.Deploy.AutoDeploy.TagPattern = "release-[" }, "release-*"},
	} {
		s := valid()
		tc.edit(s)
		err := spec.Validate(s)
		require.Error(t, err, tc.name)
		e := errs.As(err)
		require.Contains(t, e.Message+" "+e.Remedy, tc.says, tc.name)
	}

	s := valid()
	s.Deploy.AutoDeploy = spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerReleaseTagged, TagPattern: "release-*"}
	require.NoError(t, spec.Validate(s))
}

// TestR184_ReDetectingKeepsTheAppsEgressAndApproval asserts that accepting a
// re-detection does not move an app's egress or drop its own approval
// requirement: neither is read from the repository, and losing either would
// change what the app may reach, or whether its deploys are approved,
// without anybody deciding it (R-184, R-154).
func TestR184_ReDetectingKeepsTheAppsEgressAndApproval(t *testing.T) {
	pinned := pinnedApp()
	pinned.Egress = spec.Egress{Mode: spec.EgressAllowlist, List: []string{"api.stripe.com"}, BlockPrivate: ptr(true)}
	pinned.Deploy.RequireApproval = true

	got := spec.Carry(pinned, redetected())
	require.Equal(t, pinned.Egress, got.Egress)
	require.True(t, got.Deploy.RequireApproval)
}
