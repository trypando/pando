package policy_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/policy"
)

func env(key, value string) policy.Setting {
	return policy.Setting{Key: key, Value: value, Source: policy.Source{Kind: "env", Name: "PANDO_POLICY_" + key}}
}

// R-271: host policy may be set at startup. What is set there overrides the
// stored document and reads the way its field does.
func TestR271_StartupPolicyOverridesTheStoredDocument(t *testing.T) {
	o, err := policy.NewOverlay([]policy.Setting{
		env("min_security_score", "0"),
		env("disabled_verbs", "app.exec, app.secrets.read"),
		env("allow_anonymous_grants", "false"),
		{Key: "max_token_lifetime_days", Value: 30, Source: policy.Source{Kind: "file", Name: "/etc/pando.yaml", Key: "policy.max_token_lifetime_days"}},
	})
	require.NoError(t, err)

	yes := true
	stored := policy.Document{MinSecurityScore: 70, AllowAnonymousGrants: &yes, EgressAllowlist: []string{"api.example.com"}}
	got := o.Apply(stored)

	require.Equal(t, 0, got.MinSecurityScore, "a zero from the startup config still overrides")
	require.Equal(t, []string{"app.exec", "app.secrets.read"}, got.DisabledVerbs)
	require.NotNil(t, got.AllowAnonymousGrants)
	require.False(t, *got.AllowAnonymousGrants)
	require.Equal(t, 30, got.MaxTokenLifetimeDays)
	require.Equal(t, []string{"api.example.com"}, got.EgressAllowlist, "what is not fixed is the stored value")
}

func TestR271_AStartupPolicyThatIsNotOneStopsStartup(t *testing.T) {
	_, err := policy.NewOverlay([]policy.Setting{env("min_security_scor", "70")})
	require.ErrorContains(t, err, "not a host policy setting")
	require.ErrorContains(t, err, "min_security_score", "the message lists the real names")

	_, err = policy.NewOverlay([]policy.Setting{env("min_security_score", "high")})
	require.ErrorContains(t, err, "PANDO_POLICY_min_security_score")
	require.ErrorContains(t, err, "not a whole number")
}

// A fixed field cannot be changed from the API, but sending back the value it
// already has is not a change, and a save never stores it.
func TestR271_AFixedFieldIsNeitherChangedNorStored(t *testing.T) {
	o, err := policy.NewOverlay([]policy.Setting{env("min_security_score", "80")})
	require.NoError(t, err)

	_, changed := o.Changes(policy.Document{MinSecurityScore: 80, EgressAllowlist: []string{"x"}})
	require.False(t, changed)

	f, changed := o.Changes(policy.Document{MinSecurityScore: 50})
	require.True(t, changed)
	require.Equal(t, "min_security_score", f.Key)
	require.Equal(t, "PANDO_POLICY_min_security_score", f.Source.Name)

	saved := o.Restore(policy.Document{MinSecurityScore: 80, EgressAllowlist: []string{"x"}}, policy.Document{MinSecurityScore: 60})
	require.Equal(t, 60, saved.MinSecurityScore, "the stored value is kept, not the startup one")
	require.Equal(t, []string{"x"}, saved.EgressAllowlist)

	var none *policy.Overlay
	require.Equal(t, policy.Document{MinSecurityScore: 5}, none.Apply(policy.Document{MinSecurityScore: 5}))
}

// TestR242_OversubscriptionCanBeAllowedAtStartup asserts the R-242 amendment's
// toggles are ordinary policy fields, settable from the environment like any
// other (R-271), and that there is no disk counterpart to set.
func TestR242_OversubscriptionCanBeAllowedAtStartup(t *testing.T) {
	o, err := policy.NewOverlay([]policy.Setting{
		env("allow_cpu_oversubscription", "true"),
		env("allow_memory_oversubscription", "false"),
	})
	require.NoError(t, err)
	got := o.Apply(policy.Default())
	require.True(t, got.AllowCPUOversubscription)
	require.False(t, got.AllowMemoryOversubscription)

	_, err = policy.NewOverlay([]policy.Setting{env("allow_disk_oversubscription", "true")})
	require.ErrorContains(t, err, "not a host policy setting", "disk is never oversubscribed")
}
