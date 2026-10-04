package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// `pando updates` prints what GET /updates says, in every state it can say it
// (R-351), and the command that upgrades the server (R-352).
func TestR351_PandoUpdatesShowsEachStateOfTheCheck(t *testing.T) {
	t.Run("an update with a security fix and a breaking bump", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{
			"current": "0.3.1", "enabled": true, "channel": "stable", "checked_at": "2026-10-04T06:00:00Z",
			"latest": "0.4.1", "available": true, "security": true, "breaking": true,
			"releases": []map[string]any{
				{"version": "0.4.1", "published_at": "2026-10-03T00:00:00Z", "security": true,
					"notes": "### Security\n\n- GHSA-1234 (high).\n"},
				{"version": "0.4.0", "breaking": true, "notes": "### Upgrade notes\n\n- Renamed a variable.\n"},
			},
			"upgrade": map[string]any{
				"version": "0.4.1", "command": "curl -fsSLO https://example/docker-compose.yml && docker compose up -d",
				"instructions": "Download the docker-compose.yml.",
			},
		})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "Running: 0.3.1")
		require.Contains(t, r.out, "Latest:  0.4.1 (stable channel, checked 2026-10-04 06:00 UTC)")
		require.Contains(t, r.out, "## 0.4.1, 2026-10-03 — security fix")
		require.Contains(t, r.out, "## 0.4.0 — may break what the version before it did")
		require.Contains(t, r.out, "GHSA-1234")
		require.Contains(t, r.out, "docker compose up -d")
		require.Contains(t, r.out, "Download the docker-compose.yml.")
	})

	t.Run("up to date", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{
			"current": "0.4.1", "enabled": true, "channel": "prerelease", "checked_at": "2026-10-04T06:00:00Z",
			"latest": "0.4.1", "releases": []any{},
		})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "prerelease channel")
		require.Contains(t, r.out, "Pando is up to date.")
	})

	t.Run("a development build", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{
			"current": "dev", "development": true, "enabled": true, "channel": "stable",
			"checked_at": "2026-10-04T06:00:00Z", "latest": "0.4.1",
		})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "Running: a development build")
		require.NotContains(t, r.out, "up to date")
	})

	t.Run("the check is off", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{"current": "0.3.1", "enabled": false})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "disable_update_check")
	})

	t.Run("not checked yet", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{"current": "0.3.1", "enabled": true})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "has not checked for releases yet")
	})

	t.Run("GitHub unreachable, nothing found before", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{
			"current": "0.3.1", "enabled": true, "error": "Pando could not reach GitHub.",
		})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "Pando could not reach GitHub.")
		require.NotContains(t, r.out, "has not checked")
	})

	t.Run("nothing published on the channel", func(t *testing.T) {
		api := newAPI(t).reply("GET /updates", map[string]any{
			"current": "0.3.1", "enabled": true, "channel": "stable", "checked_at": "2026-10-04T06:00:00Z",
		})
		r := run(t, api, "", "updates")
		require.NoError(t, r.err)
		require.Contains(t, r.out, "Latest:  none published")
	})

	t.Run("refused", func(t *testing.T) {
		api := newAPI(t).fail("GET /updates", 403, map[string]any{"code": "AUTHZ_DENIED", "message": "You need install.view."})
		r := run(t, api, "", "updates")
		require.ErrorContains(t, r.err, "install.view")
	})
}
