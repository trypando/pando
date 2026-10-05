package events

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// The same rule the events table's CHECK applies to a name.
var nameShape = regexp.MustCompile(`^[a-z][a-z_]*(\.[a-z][a-z_]*)+$`)

// TestR364_TheCatalogIsOneListOfStableNames asserts R-364: every event has a
// unique name the database accepts, a summary, and a source; an audit-sourced
// event names the actions it is copied from, and no action feeds two events.
func TestR364_TheCatalogIsOneListOfStableNames(t *testing.T) {
	seen := map[string]bool{}
	actions := map[string]string{}
	for _, d := range Catalog() {
		require.False(t, seen[d.Name], "%s is listed twice", d.Name)
		seen[d.Name] = true
		require.Regexp(t, nameShape, d.Name)
		require.NotEmpty(t, d.Summary, d.Name)
		require.Contains(t, []Scope{ScopeApp, ScopeInstall}, d.Scope, d.Name)
		if d.Source == SourceAudit {
			require.NotEmpty(t, d.Actions, "%s is copied from the audit log but names no action", d.Name)
		}
		for _, a := range d.Actions {
			require.Empty(t, actions[a], "%s feeds both %s and %s", a, actions[a], d.Name)
			actions[a] = d.Name
		}
		fields := map[string]bool{}
		for _, f := range d.Fields {
			require.False(t, fields[f.Name], "%s lists %s twice", d.Name, f.Name)
			fields[f.Name] = true
			require.NotEmpty(t, f.Description, "%s.%s", d.Name, f.Name)
		}
	}
	// The constants core and the triggers write are catalogued.
	for _, n := range []string{AppStateChanged, DeploySucceeded, DeployFailed, BackupCreated, BackupFailed,
		AdapterUnhealthy, AdapterRecovered, SubscriptionTest, SubscriptionDisable} {
		_, ok := Lookup(n)
		require.True(t, ok, n)
	}
}

func TestPatterns(t *testing.T) {
	require.True(t, Match("*", "deploy.failed"))
	require.True(t, Match("deploy.*", "deploy.failed"))
	require.False(t, Match("deploy.*", "deployment.failed"), "a prefix matches whole segments")
	require.False(t, Match("deploy.failed", "deploy.succeeded"))

	require.True(t, ValidPattern("*"))
	require.True(t, ValidPattern("security.*"))
	require.True(t, ValidPattern("deploy.failed"))
	require.False(t, ValidPattern("deploy.fialed"))
	require.False(t, ValidPattern("nothing.*"))
}

// TestR194_AnEventCarriesOnlyItsCataloguedFields asserts R-194 for events: a
// field an audit detail carries and the catalog does not list never reaches a
// subscriber.
func TestR194_AnEventCarriesOnlyItsCataloguedFields(t *testing.T) {
	d, ok := ForAction("token.create")
	require.True(t, ok)
	data := d.Data(map[string]any{"name": "ci", "kind": "account", "secret": "tok_plaintext", "token": "x"})
	require.Equal(t, map[string]any{"name": "ci", "kind": "account"}, data)
}
