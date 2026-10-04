package cli_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func possiblePlan(breaking bool) map[string]any {
	p := map[string]any{
		"current": "0.3.1", "target": "0.4.0", "possible": true, "reasons": []string{},
		"tag": "trypando/pando:latest", "note": "Every app is unreachable while Pando restarts.",
		"breaking": []any{},
	}
	if breaking {
		p["breaking"] = []map[string]any{{"version": "0.4.0", "notes": "### Upgrade notes\n\n- A variable is renamed.\n"}}
	}
	return p
}

// `pando upgrade` asks what the console asks, in the same order (R-356 –
// R-360), and sends the same request.
func TestR358_PandoUpgradeAsksForThePassphraseOrAnExplicitSkip(t *testing.T) {
	t.Run("latest, with a passphrase", func(t *testing.T) {
		api := newAPI(t).
			reply("GET /updates", map[string]any{"available": true, "latest": "0.4.0"}).
			reply("GET /upgrade", possiblePlan(false)).
			reply("POST /upgrade", map[string]any{"to": "0.4.0", "state": "running"})
		r := run(t, api, "a long passphrase\n", "upgrade")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "Upgrade Pando from 0.3.1 to 0.4.0.")
		require.Contains(t, r.out, "unreachable while Pando restarts")
		require.Contains(t, r.errOut, "doesn't keep this passphrase")
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(api.bodyFor("POST /upgrade")), &body))
		require.Equal(t, "0.4.0", body["version"])
		require.Equal(t, "a long passphrase", body["passphrase"])
		require.Nil(t, body["skip_backup"])
		require.True(t, api.sawPath("/upgrade?version=0.4.0"))
	})

	t.Run("skipping the backup, explicitly", func(t *testing.T) {
		api := newAPI(t).
			reply("GET /upgrade", possiblePlan(false)).
			reply("POST /upgrade", map[string]any{"to": "0.4.0"})
		r := run(t, api, "", "upgrade", "v0.4.0", "--skip-backup")
		require.NoError(t, r.err)
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(api.bodyFor("POST /upgrade")), &body))
		require.Equal(t, true, body["skip_backup"])
		require.Nil(t, body["passphrase"])
	})

	t.Run("already up to date", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{"available": false})
		r := run(t, api, "", "upgrade")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "up to date")
		require.False(t, api.sawPath("/upgrade"))
	})
}

// TestR360_PandoUpgradeShowsTheNotesAndNeedsTheVersionTyped asserts R-360 on
// the CLI.
func TestR360_PandoUpgradeShowsTheNotesAndNeedsTheVersionTyped(t *testing.T) {
	api := newAPI(t).reply("GET /upgrade", possiblePlan(true))
	r := run(t, api, "yes\n", "upgrade", "0.4.0", "--skip-backup")
	require.ErrorContains(t, r.err, "not upgraded")
	require.Contains(t, r.out, "A variable is renamed.")
	for _, c := range api.calls {
		require.NotEqual(t, "POST", c.method, "nothing is started without the version typed")
	}

	api = newAPI(t).reply("GET /upgrade", possiblePlan(true)).reply("POST /upgrade", map[string]any{"to": "0.4.0"})
	r = run(t, api, "0.4.0\n", "upgrade", "0.4.0", "--skip-backup")
	require.NoError(t, r.err)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(api.bodyFor("POST /upgrade")), &body))
	require.Equal(t, "0.4.0", body["confirm_breaking"])
}

// TestR355_PandoUpgradeSaysEveryReasonItCannot asserts R-355 on the CLI.
func TestR355_PandoUpgradeSaysEveryReasonItCannot(t *testing.T) {
	api := newAPI(t).reply("GET /upgrade", map[string]any{
		"current": "0.3.1", "target": "0.4.0", "possible": false,
		"reasons": []string{"In-place upgrades are off.", "Pando's image is pinned to trypando/pando:0.3.1."},
	})
	r := run(t, api, "", "upgrade", "0.4.0")
	require.ErrorContains(t, r.err, "not upgraded")
	require.Contains(t, r.out, "In-place upgrades are off.")
	require.Contains(t, r.out, "pinned to trypando/pando:0.3.1")
}

func TestPandoUpgradeLastSaysHowItWent(t *testing.T) {
	api := newAPI(t).reply("GET /upgrade/last", map[string]any{"upgrade": map[string]any{
		"from": "0.3.1", "to": "0.4.0", "state": "rolled_back", "automatic": true,
		"started_at": "2026-10-04T02:00:00Z", "reason": "0.4.0 did not start.", "logs": "panic: migration 43",
	}})
	r := run(t, api, "", "upgrade", "last")
	require.NoError(t, r.err)
	require.Contains(t, r.out, "0.3.1 to 0.4.0: rolled back, started 2026-10-04 02:00 UTC by the maintenance schedule.")
	require.Contains(t, r.out, "0.4.0 did not start.")
	require.Contains(t, r.out, "panic: migration 43")

	api = newAPI(t).reply("GET /upgrade/last", map[string]any{"upgrade": nil})
	r = run(t, api, "", "upgrade", "last")
	require.NoError(t, r.err)
	require.Contains(t, r.out, "has not upgraded itself")
}
