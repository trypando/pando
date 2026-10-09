package ocsf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestR384_OCSFRefusesALineItCannotRead asserts a line that is not a native
// audit line is an error saying so, not an empty event.
func TestR384_OCSFRefusesALineItCannotRead(t *testing.T) {
	for _, line := range []string{`{not json`, `[]`, `{"id":"seven"}`, `{"detail":"a string"}`} {
		out, err := Encode(json.RawMessage(line))
		require.ErrorContains(t, err, "ocsf: decode audit line", line)
		require.Nil(t, out)
	}
}

// TestR384_OCSFOddLines asserts R-384 for lines with fields missing or
// unknown: an outcome this version does not know falls back to the action's
// name, a line with no target or app is about the installation, a line with
// no principal at all is by anonymous, a system actor and a user actor are
// typed, an actor with only on_behalf_of is kept, and an app that is not the
// target is a second resource.
func TestR384_OCSFOddLines(t *testing.T) {
	unknown := encode(t, `{"id":1,"occurred_at":"2026-10-08T12:00:00Z","principal_kind":"system","principal_id":"reconciler",
		"action":"app.failed","outcome":"exploded","detail":{}}`)
	assert.Equal(t, "failed", unknown["status_detail"], "an unknown outcome is read from the action")
	assert.Equal(t, json.Number("4"), unknown["severity_id"])
	assert.Equal(t, "app.failed by reconciler on install install", unknown["message"])
	assert.Equal(t, []any{map[string]any{"type": "install", "uid": "install"}}, unknown["resources"])
	assert.Equal(t, map[string]any{"uid": "reconciler", "type_id": json.Number("3"), "type": "System"},
		unknown["actor"].(map[string]any)["user"])
	assert.Equal(t, map[string]any{}, unknown["unmapped"].(map[string]any)["detail"], "an empty detail is kept as written")

	nobody := encode(t, `{"id":2,"occurred_at":"2026-10-08T12:00:00Z","action":"session.denied"}`)
	assert.Equal(t, "session.denied by anonymous on install install", nobody["message"])
	assert.NotContains(t, nobody, "actor")
	assert.Equal(t, "denied", nobody["status_detail"])
	assert.NotContains(t, nobody["unmapped"], "detail", "no detail is no detail, not null")

	user := encode(t, `{"id":3,"occurred_at":"2026-10-08T12:00:00Z","principal_kind":"user","principal_id":"usr_A",
		"action":"grant.delete","outcome":"success","target_kind":"grant","target_id":"gr_1","app_id":"app_B"}`)
	assert.Equal(t, "grant.delete by usr_A on grant gr_1", user["message"], "with no name, the principal's ID")
	assert.Equal(t, map[string]any{"uid": "usr_A", "type_id": json.Number("1"), "type": "User"}, user["actor"].(map[string]any)["user"])
	assert.Equal(t, []any{
		map[string]any{"type": "grant", "uid": "gr_1"},
		map[string]any{"type": "app", "uid": "app_B"},
	}, user["resources"])

	same := encode(t, `{"id":4,"occurred_at":"2026-10-08T12:00:00Z","principal_kind":"user","principal_id":"usr_A",
		"action":"app.deploy","target_kind":"app","target_id":"app_B","app_id":"app_B"}`)
	assert.Equal(t, []any{map[string]any{"type": "app", "uid": "app_B"}}, same["resources"], "the app is not listed twice")

	kindOnly := encode(t, `{"id":5,"occurred_at":"2026-10-08T12:00:00Z","principal_kind":"anonymous",
		"action":"install.update","target_kind":"policy"}`)
	assert.Equal(t, "install.update by anonymous on policy", kindOnly["message"], "a target with no ID is named by its kind")
	assert.Equal(t, []any{map[string]any{"type": "policy"}}, kindOnly["resources"])

	invoked := encode(t, `{"id":6,"occurred_at":"2026-10-08T12:00:00Z","principal_kind":"anonymous","on_behalf_of":"usr_C",
		"action":"app.use"}`)
	assert.Equal(t, map[string]any{"invoked_by": "usr_C"}, invoked["actor"], "on_behalf_of alone still makes an actor")
}
