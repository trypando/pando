package cli_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR412_NoticesFromTheCLI asserts R-412 and R-261: an app's notices can be
// listed, ignored and restored from the CLI, each subcommand the one API call
// it names.
func TestR412_NoticesFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_1/notices", map[string]any{
		"notices": []map[string]any{
			{"key": "variables.unset", "message": "A_KEY is declared with no value.", "fingerprint": "f1", "ignored": false},
			{"key": "config.unshipped", "message": "The configuration has changed.", "fingerprint": "f2", "ignored": true},
		},
	})

	got := run(t, api, "", "app", "notices", "list", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "variables.unset")
	require.Contains(t, got.out, "A_KEY is declared with no value.")
	require.NotContains(t, got.out, "config.unshipped", "an ignored notice is left out")
	require.Contains(t, got.out, "1 ignored; run with --all")

	got = run(t, api, "", "app", "notices", "list", "app_1", "--all")
	require.NoError(t, got.err, got.errOut)
	require.Regexp(t, `config\.unshipped\s+yes\s+The configuration has changed\.`, got.out)

	got = run(t, api, "", "app", "notices", "ignore", "app_1", "variables.unset")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Ignored variables.unset until it changes.")
	api.bodyFor("PUT /apps/app_1/notices/variables.unset/ignore")

	got = run(t, api, "", "app", "notices", "restore", "app_1", "variables.unset")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Restored variables.unset.")
	api.bodyFor("DELETE /apps/app_1/notices/variables.unset/ignore")
}

func TestNoticesListSaysWhenThereIsNothing(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_1/notices", map[string]any{"notices": []any{}})
	got := run(t, api, "", "app", "notices", "list", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Nothing needs your attention on this app.")
}

func TestNoticeCommandsReportTheServersRefusal(t *testing.T) {
	api := newAPI(t)
	for _, key := range []string{
		"GET /apps/app_1/notices", "PUT /apps/app_1/notices/backup.not_taken/ignore",
		"DELETE /apps/app_1/notices/backup.not_taken/ignore",
	} {
		api.fail(key, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "There is no app with that ID."})
	}
	for _, args := range [][]string{
		{"list", "app_1"}, {"ignore", "app_1", "backup.not_taken"}, {"restore", "app_1", "backup.not_taken"},
	} {
		got := run(t, api, "", "app", append([]string{"notices"}, args...)...)
		require.Error(t, got.err, args)
	}
}
