package cli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

// call is one request the fake server saw.
type call struct {
	method, path string
	body         string
}

// fakeAPI is a Pando server that answers from a routing table keyed by
// "METHOD /path", so a test states only the responses its command needs.
type fakeAPI struct {
	t         *testing.T
	responses map[string]any
	status    map[string]int
	calls     []call
	url       string
}

func newAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, responses: map[string]any{}, status: map[string]int{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/v1")
		f.calls = append(f.calls, call{method: r.Method, path: r.URL.RequestURI(), body: string(raw)})

		if code, ok := f.status[key]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
		}
		body, ok := f.responses[key]
		// A reply that differs from one call to the next: what a restart looks
		// like from outside, where the same request answers differently once
		// the process is back.
		if fn, dynamic := body.(func() any); dynamic {
			body = fn()
		}
		if !ok {
			body = map[string]any{}
		}
		if s, isString := body.(string); isString {
			_, _ = io.WriteString(w, s)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeAPI) reply(key string, body any) *fakeAPI { f.responses[key] = body; return f }

// handle answers a key from a function, for a reply that changes between
// calls.
func (f *fakeAPI) handle(key string, body func() any) *fakeAPI {
	f.responses[key] = body
	return f
}
func (f *fakeAPI) fail(key string, code int, body any) *fakeAPI {
	f.status[key] = code
	f.responses[key] = body
	return f
}

func (f *fakeAPI) sawPath(path string) bool {
	for _, c := range f.calls {
		if c.path == "/api/v1"+path {
			return true
		}
	}
	return false
}

func (f *fakeAPI) bodyFor(key string) string {
	f.t.Helper()
	for _, c := range f.calls {
		if c.method+" "+strings.TrimPrefix(strings.Split(c.path, "?")[0], "/api/v1") == key {
			return c.body
		}
	}
	f.t.Fatalf("no %s call was made; saw %v", key, f.calls)
	return ""
}

// result is what running a command produced.
type result struct {
	out, errOut string
	err         error
}

// run executes one top-level command from Commands() with args, pointing it at
// the fake server. stdin is whatever the test supplies, which is how the
// prompting commands are driven.
func run(t *testing.T, api *fakeAPI, stdin string, name string, args ...string) result {
	t.Helper()
	isolateConfig(t)

	var cmd *cobra.Command
	for _, c := range cli.Commands() {
		if c.Name() == name {
			cmd = c
			break
		}
	}
	require.NotNil(t, cmd, "no %q command", name)

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	// A fresh reader per run: `buffered` keys its cache on the reader, so two
	// runs sharing one would have the second read the first's leftovers.
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append(args, "--server", api.url))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

// Every command is a wrapper over an endpoint (R-261). A command that did
// something the API cannot do would be a capability the console and MCP could
// never have — so the set is worth asserting directly.
func TestR261_EveryCommandIsPresentAndDocumented(t *testing.T) {
	names := map[string]bool{}
	for _, c := range cli.Commands() {
		names[c.Name()] = true
		require.NotEmpty(t, c.Short, "%s has no summary", c.Name())
		require.NotNil(t, c.Flag("server"), "%s cannot be pointed at another install", c.Name())
	}

	for _, want := range []string{
		"login", "app", "deploy", "exec", "slot", "plan", "logs",
		"secret", "grant", "rollback", "export", "backup", "policy", "token", "mcp",
		"approvals",
	} {
		require.True(t, names[want], "missing the %s command", want)
	}
}

// A flag belongs to the subcommand it documents.
//
// cobra keeps Commands() in alphabetical order rather than insertion order, so
// attaching a flag to "the one just added" by indexing that slice lands it on
// whichever name sorts last. That put --name on `app list`, where it did
// nothing, and left `app add --name` rejected as an unknown flag.
func TestEachFlagIsOnTheSubcommandThatReadsIt(t *testing.T) {
	subs := map[string]*cobra.Command{}
	for _, c := range cli.Commands() {
		if c.Name() == "app" {
			for _, sub := range c.Commands() {
				subs[sub.Name()] = sub
			}
		}
	}

	require.NotNil(t, subs["add"].Flags().Lookup("name"))
	require.Nil(t, subs["list"].Flags().Lookup("name"))
	require.NotNil(t, subs["delete"].Flags().Lookup("discard-data"))
	require.Nil(t, subs["show"].Flags().Lookup("discard-data"))
}

func TestAppList(t *testing.T) {
	api := newAPI(t).reply("GET /apps", map[string]any{
		"apps": []map[string]any{
			{"id": "app_01HQ8", "name": "notes", "state": "running", "access": "passcode",
				"owner_name": "Ada", "address": "https://notes.example.com", "notice_count": 2},
			{"id": "app_01HQ9", "name": "wiki", "state": "failed", "access": "public"},
			{"id": "app_01HQA", "name": "crm", "state": "running", "access": "private"},
			{"id": "app_01HQB", "name": "docs", "state": "running", "access": "shared"},
		},
	})

	got := run(t, api, "", "app", "list")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "NAME")
	require.Contains(t, got.out, "notes")
	require.Contains(t, got.out, "running")
	require.Contains(t, got.out, "app_01HQ9")
	// Who can open each app, in the console's Access words, and who owns it
	// and where it is (discussion #262).
	require.Contains(t, got.out, "public with passcode")
	require.Contains(t, got.out, "private – shared")
	require.Regexp(t, `wiki\s+failed\s+-\s+public\s`, got.out)
	require.Regexp(t, `crm\s+running\s+-\s+private\s`, got.out)
	// How many notices the caller has not ignored (R-412).
	require.Regexp(t, `notes\s+running\s+2\s+public with passcode`, got.out)
	require.Contains(t, got.out, "Ada")
	require.Contains(t, got.out, "https://notes.example.com")
	// "public" is never left to stand alone (R-077).
	require.Contains(t, got.out, "Anyone on the internet can open a public app without signing in")
}

func TestAppListSaysNothingAboutPublicAppsWhenThereAreNone(t *testing.T) {
	api := newAPI(t).reply("GET /apps", map[string]any{
		"apps": []map[string]string{{"id": "app_01HQA", "name": "crm", "state": "running", "access": "private"}},
	})
	got := run(t, api, "", "app", "list")
	require.NoError(t, got.err)
	require.NotContains(t, got.out, "Anyone on the internet")
}

func TestAppAddNamesTheAppAfterTheRepositoryUnlessTold(t *testing.T) {
	api := newAPI(t).reply("POST /apps", map[string]any{"id": "app_01HQ8"})

	got := run(t, api, "", "app", "add", "https://github.com/ben/notes.git")
	require.NoError(t, got.err)
	require.JSONEq(t,
		`{"name":"notes","source":{"type":"git","url":"https://github.com/ben/notes.git"}}`,
		api.bodyFor("POST /apps"))

	// 202: the app exists in draft and detection is queued. Saying so is the
	// difference between waiting and wondering.
	require.Contains(t, got.out, "app_01HQ8")
	require.Contains(t, got.out, "working out how to run it")
}

func TestAppAddTakesAnExplicitName(t *testing.T) {
	api := newAPI(t).reply("POST /apps", map[string]any{"id": "app_01HQ8"})
	got := run(t, api, "", "app", "add", "https://github.com/ben/notes.git", "--name", "field-notes")
	require.NoError(t, got.err)
	require.Contains(t, api.bodyFor("POST /apps"), `"field-notes"`)
	require.Contains(t, got.out, "field-notes")
}

func TestAppShowPrintsTheAppAsJSON(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8", map[string]any{"id": "app_01HQ8", "state": "running"})

	got := run(t, api, "", "app", "show", "app_01HQ8")
	require.NoError(t, got.err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(got.out), &decoded))
	require.Equal(t, "running", decoded["state"])
}

// R-205: a non-interactive delete keeps a copy of the app's storage unless
// told not to. The CLI is where that default is chosen.
func TestR205_DeleteBacksUpByDefaultAndDiscardsOnlyWhenAsked(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "app", "delete", "app_01HQ8")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/apps/app_01HQ8?backup=true"))
	require.Contains(t, got.out, "backed up")

	api = newAPI(t)
	got = run(t, api, "", "app", "delete", "app_01HQ8", "--discard-data")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/apps/app_01HQ8?force=true"))
	require.Contains(t, got.out, "discarded")
}

func TestDeployOfAnExistingApp(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_01HQ8/deployments", map[string]any{"id": "dep_01HQ8"})

	got := run(t, api, "", "deploy", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "pando logs app_01HQ8 -f")
}

func TestPlanPrintsThePlanWithoutDeploying(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_01HQ8/plan", map[string]any{"steps": []string{"build", "start"}})

	got := run(t, api, "", "plan", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "build")
	require.False(t, api.sawPath("/apps/app_01HQ8/deployments"), "plan does not deploy")
}

func TestExportPrintsTheSpec(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/export", map[string]any{"version": 1})
	got := run(t, api, "", "export", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, `"version"`)
}

func TestLogsStreamTheBodyThrough(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/logs", "first line\nsecond line\n")

	got := run(t, api, "", "logs", "app_01HQ8")
	require.NoError(t, got.err)
	require.Equal(t, "first line\nsecond line\n", got.out)
	require.True(t, api.sawPath("/apps/app_01HQ8/logs"))

	api = newAPI(t).reply("GET /apps/app_01HQ8/logs", "tailing\n")
	got = run(t, api, "", "logs", "app_01HQ8", "-f")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/apps/app_01HQ8/logs?follow=true"))
}

func TestRollback(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "rollback", "app_01HQ8")
	require.NoError(t, got.err)
	require.JSONEq(t, `{}`, api.bodyFor("POST /apps/app_01HQ8/deployments/rollback"),
		"with no revision named, the server picks the previous one")
	require.Contains(t, got.out, "Rolling back")

	api = newAPI(t)
	require.NoError(t, run(t, api, "", "rollback", "app_01HQ8", "--to", "3").err)
	require.JSONEq(t, `{"spec_revision":3}`, api.bodyFor("POST /apps/app_01HQ8/deployments/rollback"))
}

func TestSlotSetRequiresExactlyOneMode(t *testing.T) {
	api := newAPI(t)

	got := run(t, api, "", "slot", "set", "app_01HQ8", "REDIS_URL")
	require.ErrorContains(t, got.err, "exactly one")

	got = run(t, newAPI(t), "", "slot", "set", "app_01HQ8", "REDIS_URL",
		"--provision", "--literal", "redis://elsewhere")
	require.ErrorContains(t, got.err, "exactly one")
}

func TestSlotSetSendsTheChosenMode(t *testing.T) {
	api := newAPI(t)
	require.NoError(t, run(t, api, "", "slot", "set", "app_01HQ8", "REDIS_URL", "--provision").err)
	require.JSONEq(t, `{"key":"REDIS_URL","mode":"provision"}`,
		api.bodyFor("PUT /apps/app_01HQ8/slots/REDIS_URL"))

	api = newAPI(t)
	require.NoError(t, run(t, api, "", "slot", "set", "app_01HQ8", "DB_URL", "--bind", "svc_01HQ8").err)
	require.JSONEq(t, `{"key":"DB_URL","mode":"bind","target":"svc_01HQ8"}`,
		api.bodyFor("PUT /apps/app_01HQ8/slots/DB_URL"))

	api = newAPI(t)
	got := run(t, api, "", "slot", "set", "app_01HQ8", "DB_URL", "--literal", "postgres://x")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"key":"DB_URL","mode":"literal","value":"postgres://x"}`,
		api.bodyFor("PUT /apps/app_01HQ8/slots/DB_URL"))
	require.Contains(t, got.out, "Set DB_URL")
}

// A secret is never taken from a flag: an argument is in the shell history and
// in /proc for every process on the machine to read.
func TestSecretSetReadsTheValueFromTheTerminal(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "hunter2\n", "secret", "set", "app_01HQ8", "API_KEY")
	require.NoError(t, got.err)

	require.JSONEq(t, `{"value":"hunter2"}`, api.bodyFor("PUT /apps/app_01HQ8/secrets/API_KEY"))
	require.Contains(t, got.errOut, "not hidden",
		"reading from a pipe says so, because a secret nobody realized was echoed has leaked")
	require.Contains(t, got.out, "Set API_KEY")

	for _, c := range cli.Commands() {
		if c.Name() == "secret" {
			for _, sub := range c.Commands() {
				require.Nil(t, sub.Flags().Lookup("value"), "a secret never comes from a flag")
			}
		}
	}
}

func TestSecretListShowsWhichAreSetWithoutValues(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/secrets",
		map[string]any{"secrets": []map[string]string{{"key": "API_KEY"}}})

	got := run(t, api, "", "secret", "list", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "API_KEY")
}

// Sharing an app normally means letting someone use it, not letting them
// redeploy it. The dangerous plane has to be asked for.
func TestGrantAddDefaultsToTheDataPlane(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "grant", "add", "app_01HQ8", "--user", "usr_01HQ9")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"plane":"data","principal_kind":"user","principal_id":"usr_01HQ9"}`,
		api.bodyFor("POST /apps/app_01HQ8/grants"))
	require.Contains(t, got.out, "Shared.")

	api = newAPI(t)
	require.NoError(t, run(t, api, "", "grant", "add", "app_01HQ8",
		"--user", "usr_01HQ9", "--plane", "control", "--role", "role_deployer").err)
	require.JSONEq(t,
		`{"plane":"control","principal_kind":"user","principal_id":"usr_01HQ9","role_id":"role_deployer"}`,
		api.bodyFor("POST /apps/app_01HQ8/grants"))
}

func TestGrantAddSaysWhatIsMissing(t *testing.T) {
	got := run(t, newAPI(t), "", "grant", "add", "app_01HQ8")
	require.ErrorContains(t, got.err, "--user")
}

func TestGrantList(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/grants",
		map[string]any{"grants": []map[string]string{{"principal_id": "usr_01HQ9"}}})
	got := run(t, api, "", "grant", "list", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "usr_01HQ9")
}

func TestTokenListAndRevoke(t *testing.T) {
	revoked := "2026-09-01T00:00:00Z"
	api := newAPI(t).reply("GET /tokens", map[string]any{
		"tokens": []map[string]any{
			{"id": "tok_01HQ8", "name": "laptop CLI", "revoked_at": nil},
			{"id": "tok_01HQ9", "name": "old laptop", "revoked_at": revoked},
		},
	})

	got := run(t, api, "", "token", "list")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "active")
	require.Contains(t, got.out, "revoked")

	api = newAPI(t)
	got = run(t, api, "", "token", "revoke", "tok_01HQ8")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/tokens/tok_01HQ8"))
	require.Contains(t, got.out, "immediately")
}

func TestPolicyShow(t *testing.T) {
	api := newAPI(t).reply("GET /policy", map[string]any{"exec_enabled": false})
	got := run(t, api, "", "policy", "show")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "exec_enabled")
}

// Replaces rather than merges: a merge would make it impossible to remove a rule.
func TestPolicySetReplacesTheDocumentFromStdin(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, `{"exec_enabled":true}`, "policy", "set")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"exec_enabled":true}`, api.bodyFor("PUT /policy"))
	require.Contains(t, got.out, "already running are unchanged")
}

func TestPolicySetRefusesSomethingThatIsNotJSON(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "exec_enabled = true", "policy", "set")
	require.ErrorContains(t, got.err, "not valid JSON")
	require.False(t, api.sawPath("/policy"), "nothing is sent")
}

func TestBackupList(t *testing.T) {
	api := newAPI(t).reply("GET /backups", map[string]any{
		"backups": []map[string]any{
			{"id": "bk_01HQ8", "adapter_ref": "bk_local", "created_at": "2026-09-01T00:00:00Z", "size_bytes": 1024},
		},
	})
	got := run(t, api, "", "backup", "list")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "DESTINATION")
	require.Contains(t, got.out, "bk_local")
}

// R-214, said before the passphrase is chosen rather than after.
func TestR214_BackupCreateWarnsThatThePassphraseCannotBeRecovered(t *testing.T) {
	api := newAPI(t).reply("POST /backups", map[string]any{"id": "bk_01HQ8"})

	got := run(t, api, "correct horse\ncorrect horse\n", "backup", "create")
	require.NoError(t, got.err)
	require.Contains(t, got.errOut, "Pando doesn't keep this passphrase")
	require.JSONEq(t, `{"passphrase":"correct horse"}`, api.bodyFor("POST /backups"))
	require.Contains(t, got.out, "bk_01HQ8")
}

func TestBackupCreateRefusesMistypedPassphrases(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "one thing\nanother thing\n", "backup", "create")
	require.ErrorContains(t, got.err, "different")
	require.False(t, api.sawPath("/backups"), "no backup is taken under a passphrase nobody can retype")
}

func TestBackupVerify(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "correct horse\n", "backup", "verify", "bk_01HQ8")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"passphrase":"correct horse"}`, api.bodyFor("POST /backups/bk_01HQ8/verify"))
	require.Contains(t, got.out, "complete and can be restored")
}

// Typed confirmation, like the console. A --yes flag alone would make the most
// destructive action in the system fit in a shell alias.
func TestBackupRestoreRequiresTypedConfirmation(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "yes\n", "backup", "restore", "bk_01HQ8")
	require.ErrorContains(t, got.err, "not confirmed")
	require.False(t, api.sawPath("/backups/bk_01HQ8/restore"))
	require.Contains(t, got.errOut, "every app, every account")

	api = newAPI(t)
	got = run(t, api, "replace\ncorrect horse\n", "backup", "restore", "bk_01HQ8")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"passphrase":"correct horse","confirm":true}`,
		api.bodyFor("POST /backups/bk_01HQ8/restore"))
	require.Contains(t, got.out, "Restored.")
}

func TestBackupRestoreSkipsTheConfirmationForScripts(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "correct horse\n", "backup", "restore", "bk_01HQ8", "--yes")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/backups/bk_01HQ8/restore"))
}

// The server's error text reaches the operator as written (R-105).
func TestAServerErrorReachesTheOperatorUnchanged(t *testing.T) {
	api := newAPI(t).fail("GET /apps", http.StatusForbidden, map[string]string{
		"code":    "PERM_DENIED",
		"message": "You do not have access to any apps on this install.",
		"remedy":  "Ask an administrator to share one with you.",
	})

	got := run(t, api, "", "app", "list")
	require.ErrorContains(t, got.err, "You do not have access to any apps on this install.")
	require.ErrorContains(t, got.err, "Ask an administrator to share one with you.")
}

func TestEveryCommandReportsAnUnreachableServer(t *testing.T) {
	dead := &fakeAPI{t: t, url: "http://127.0.0.1:1", responses: map[string]any{}, status: map[string]int{}}

	for _, args := range [][]string{
		{"app", "list"}, {"app", "show", "app_01HQ8"}, {"app", "delete", "app_01HQ8"},
		{"plan", "app_01HQ8"}, {"export", "app_01HQ8"}, {"logs", "app_01HQ8"},
		{"rollback", "app_01HQ8"}, {"token", "list"}, {"policy", "show"}, {"backup", "list"},
	} {
		got := run(t, dead, "", args[0], args[1:]...)
		require.Error(t, got.err, "%v", args)
	}
}

// --- deploying a directory (R-262) -----------------------------------------

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return dir
}

func TestPackDirectoryTakesEveryRegularFile(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"main.go":           "package main",
		"src/app.js":        "console.log(1)",
		"nested/deep/x.txt": "x",
	})

	archive, count, err := cli.PackDirectory(dir)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	require.NotEmpty(t, archive)
	require.Equal(t, []string{"main.go", "nested/deep/x.txt", "src/app.js"}, entries(t, archive))
}

// Not a .gitignore parser — the short list of things that are always wrong to
// send. Guessing which of someone's other files matter is how an upload
// silently omits the one that did.
func TestPackDirectorySkipsWhatABuildRecreates(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"main.go":                    "package main",
		".git/config":                "[core]",
		"node_modules/left-pad/i.js": "module.exports=1",
		"vendor/x/y.go":              "package y",
		"__pycache__/m.pyc":          "\x00",
		".DS_Store":                  "\x00",
		".vscode/settings.json":      "{}",
		".env":                       "SECRET=1",
	})

	archive, count, err := cli.PackDirectory(dir)
	require.NoError(t, err)

	got := entries(t, archive)
	require.Equal(t, []string{".env", "main.go"}, got,
		"everything not on the list goes, including dotfiles a build may need")
	require.Equal(t, 2, count)
}

func TestPackDirectorySkipsSymlinks(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})
	require.NoError(t, os.Symlink(filepath.Join(dir, "main.go"), filepath.Join(dir, "link.go")))

	archive, count, err := cli.PackDirectory(dir)
	require.NoError(t, err)
	require.Equal(t, 1, count, "a symlink is a question the server should not have to answer")
	require.Equal(t, []string{"main.go"}, entries(t, archive))
}

func TestPackDirectoryRefusesWhatIsNotADirectory(t *testing.T) {
	file := filepath.Join(writeTree(t, map[string]string{"main.go": "x"}), "main.go")

	_, _, err := cli.PackDirectory(file)
	require.ErrorContains(t, err, "not a directory")

	_, _, err = cli.PackDirectory(filepath.Join(t.TempDir(), "no-such-place"))
	require.ErrorContains(t, err, "no-such-place")
}

// R-262: something that has just generated an app cannot commit and push, but
// it can run a command. `pando deploy ./` is that path end to end.
func TestR262_DeployingADirectoryCreatesUploadsDetectsAndDeploys(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main", "go.mod": "module x"})

	api := newAPI(t).
		reply("POST /apps", map[string]any{"id": "app_01HQ8"}).
		reply("GET /apps/app_01HQ8/detection", map[string]any{
			"status": "succeeded",
			"detection": map[string]any{
				"questions":   []any{},
				"winning_bid": map[string]string{"detector": "Go", "strategy": "buildpack"},
			},
			"answers": map[string]string{},
		}).
		reply("POST /apps/app_01HQ8/deployments", map[string]any{"id": "dep_01HQ8"})

	got := run(t, api, "", "deploy", dir)
	require.NoError(t, got.err)

	require.True(t, api.sawPath("/apps"), "the app is created")
	require.True(t, api.sawPath("/apps/app_01HQ8/source"), "the directory is uploaded")
	require.True(t, api.sawPath("/apps/app_01HQ8/detection/rerun"),
		"R-022: detection is asked for, never re-run on its own")
	require.True(t, api.sawPath("/apps/app_01HQ8/detection/accept"))
	require.True(t, api.sawPath("/apps/app_01HQ8/deployments"))

	require.Contains(t, got.errOut, "Uploading 2 files")
	require.Contains(t, got.errOut, "Recognized it: Go, built with buildpack")
	require.Contains(t, got.out, "Deploying")

	require.JSONEq(t, `{"name":`+jsonString(filepath.Base(dir))+`,"source":{"type":"upload"}}`,
		api.bodyFor("POST /apps"))
}

func TestDeployingADirectoryAsAnExistingAppSkipsCreation(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})

	api := newAPI(t).
		reply("GET /apps/app_01HQ8/detection", map[string]any{
			"status":    "succeeded",
			"detection": map[string]any{"winning_bid": map[string]string{"detector": "Go", "strategy": "buildpack"}},
		}).
		reply("POST /apps/app_01HQ8/deployments", map[string]any{"id": "dep_01HQ8"})

	got := run(t, api, "", "deploy", dir, "--app", "app_01HQ8")
	require.NoError(t, got.err)

	for _, c := range api.calls {
		require.NotEqual(t, "/api/v1/apps", c.path, "no app is created when one was named")
	}
	require.True(t, api.sawPath("/apps/app_01HQ8/source"))
}

// pinnedSpecs is GET /specs for an app whose pinned revision deploys from
// sourceType.
func pinnedSpecs(sourceType string) map[string]any {
	return map[string]any{
		"pinned_spec_id": "spec_01HQ8",
		"revisions": []map[string]any{
			{"id": "spec_01HQ7", "body": map[string]any{"source": map[string]string{"type": "git"}}},
			{"id": "spec_01HQ8", "body": map[string]any{"source": map[string]string{"type": sourceType}}},
		},
	}
}

// R-022: an app already set up to deploy its upload keeps that setup. The new
// files are uploaded and deployed; detecting again would build and try them
// only to propose the setup the app already has.
func TestR022_RedeployingADirectoryKeepsTheAppsSetup(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})

	api := newAPI(t).
		reply("GET /apps/app_01HQ8/specs", pinnedSpecs("upload")).
		reply("POST /apps/app_01HQ8/deployments", map[string]any{"id": "dep_01HQ8"})

	got := run(t, api, "", "deploy", dir, "--app", "app_01HQ8")
	require.NoError(t, got.err)

	require.True(t, api.sawPath("/apps/app_01HQ8/source"), "the directory is uploaded")
	require.False(t, api.sawPath("/apps/app_01HQ8/detection/rerun"), "detection is not run")
	require.False(t, api.sawPath("/apps/app_01HQ8/detection/accept"), "the setup is not replaced")
	require.True(t, api.sawPath("/apps/app_01HQ8/deployments"))
	require.Contains(t, got.errOut, "Keeping its setup.")
	require.Contains(t, got.out, "Deploying")
}

// --detect is the explicit re-detection R-022 asks for, and replacing a setup
// is what it means, so the accept confirms it.
func TestR022_DetectWorksOutASetUpAppsSetupAgain(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})

	api := newAPI(t).
		reply("GET /apps/app_01HQ8/specs", pinnedSpecs("upload")).
		reply("GET /apps/app_01HQ8/detection", map[string]any{
			"status":    "succeeded",
			"detection": map[string]any{"winning_bid": map[string]string{"detector": "Go", "strategy": "buildpack"}},
		}).
		reply("POST /apps/app_01HQ8/deployments", map[string]any{"id": "dep_01HQ8"})

	got := run(t, api, "", "deploy", dir, "--app", "app_01HQ8", "--detect", "--env", "A=1")
	require.NoError(t, got.err)

	require.True(t, api.sawPath("/apps/app_01HQ8/detection/rerun"))
	require.JSONEq(t, `{"values":[{"key":"A","value":"1"}],"confirm":true}`, api.bodyFor("POST /apps/app_01HQ8/detection/accept"))
	require.True(t, api.sawPath("/apps/app_01HQ8/deployments"))
}

// An app set up from a repository or an image deploys that whatever is
// uploaded, so uploading to it without --detect would ship nothing new.
func TestDeployingADirectoryToAnAppSetUpFromElsewhereNeedsDetect(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})

	for sourceType, noun := range map[string]string{"git": "repository", "image": "image", "": "source"} {
		api := newAPI(t).reply("GET /apps/app_01HQ8/specs", pinnedSpecs(sourceType))

		got := run(t, api, "", "deploy", dir, "--app", "app_01HQ8")
		require.ErrorContains(t, got.err, "set up to deploy from its "+noun)
		require.ErrorContains(t, got.err, "--detect")
		require.False(t, api.sawPath("/apps/app_01HQ8/source"), "nothing is uploaded")
		require.False(t, api.sawPath("/apps/app_01HQ8/deployments"))
	}
}

// --env is written into a setup as it is accepted; an app keeping its setup
// accepts none, so the variables would be dropped without a word.
func TestDeployingADirectoryRefusesEnvForAnAppKeepingItsSetup(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})
	api := newAPI(t).reply("GET /apps/app_01HQ8/specs", pinnedSpecs("upload"))

	got := run(t, api, "", "deploy", dir, "--app", "app_01HQ8", "--env", "A=1")
	require.ErrorContains(t, got.err, "app app_01HQ8 keeps its setup")
	require.False(t, api.sawPath("/apps/app_01HQ8/source"))
}

func TestDeployingADirectoryReportsAnAppThatCannotBeRead(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})
	api := newAPI(t).fail("GET /apps/app_01HQ8/specs", http.StatusNotFound,
		map[string]string{"code": "NOT_FOUND", "message": "There is no app app_01HQ8."})

	got := run(t, api, "", "deploy", dir, "--app", "app_01HQ8")
	require.ErrorContains(t, got.err, "There is no app app_01HQ8.")
	require.False(t, api.sawPath("/apps/app_01HQ8/source"))
}

// The questions are printed verbatim (R-105): they are written to be
// self-contained and pasteable into the assistant that wrote the app, and
// paraphrasing here would undo that at the last step.
func TestR105_UnansweredDetectionQuestionsArePrintedVerbatimAndStopTheDeploy(t *testing.T) {
	dir := writeTree(t, map[string]string{"server.js": "require('http')"})

	question := "This app appears to be a Node.js service. Pando could not determine which port " +
		"it serves HTTP on. Valid answer: a port number such as 3000."

	api := newAPI(t).
		reply("POST /apps", map[string]any{"id": "app_01HQ8"}).
		reply("GET /apps/app_01HQ8/detection", map[string]any{
			"status": "succeeded",
			"detection": map[string]any{
				"questions": []map[string]any{
					// `prompt`, as the server writes it (detect.Question). The
					// CLI read `question`, which nothing sends, and printed
					// blank lines where the questions should be.
					{"key": "port", "prompt": question},
					{"key": "later", "prompt": "Something deferred.", "deferred": true},
					{"key": "answered", "prompt": "Already handled."},
				},
			},
			"answers": map[string]string{"answered": "yes"},
		})

	got := run(t, api, "", "deploy", dir)

	require.ErrorContains(t, got.err, "1 question(s) still to answer")
	require.Contains(t, got.errOut, question, "printed exactly as the server wrote it")
	require.NotContains(t, got.errOut, "Something deferred.", "a deferred question does not block")
	require.NotContains(t, got.errOut, "Already handled.")
	require.Contains(t, got.errOut, "paste a question into whatever wrote this app")

	require.False(t, api.sawPath("/apps/app_01HQ8/detection/accept"))
	require.False(t, api.sawPath("/apps/app_01HQ8/deployments"))
}

func TestDeployStopsWhenDetectionFailed(t *testing.T) {
	dir := writeTree(t, map[string]string{"README.md": "# nothing runnable"})

	api := newAPI(t).
		reply("POST /apps", map[string]any{"id": "app_01HQ8"}).
		reply("GET /apps/app_01HQ8/detection", map[string]any{"status": "failed"})

	got := run(t, api, "", "deploy", dir)
	require.ErrorContains(t, got.err, "could not work out how to run this directory")
	require.False(t, api.sawPath("/apps/app_01HQ8/deployments"))
}

func TestDeployReportsAnUploadThatWasRefused(t *testing.T) {
	dir := writeTree(t, map[string]string{"main.go": "package main"})

	api := newAPI(t).
		reply("POST /apps", map[string]any{"id": "app_01HQ8"}).
		fail("POST /apps/app_01HQ8/source", http.StatusRequestEntityTooLarge,
			map[string]string{"code": "VALID_INVALID", "message": "That upload is too large."})

	got := run(t, api, "", "deploy", dir)
	require.ErrorContains(t, got.err, "That upload is too large.")
	require.False(t, api.sawPath("/apps/app_01HQ8/deployments"))
}

// The upload line reports a size a person can read. Exercised through deploy,
// which is humanBytes' only caller.
func TestTheUploadLineReportsAReadableSize(t *testing.T) {
	for _, tc := range []struct {
		bytes int
		want  string
	}{
		{64, " B)"},
		{8 << 10, " KB)"},
		{3 << 20, " MB)"},
	} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blob"), incompressible(tc.bytes), 0o644))

		api := newAPI(t).
			reply("POST /apps", map[string]any{"id": "app_01HQ8"}).
			reply("GET /apps/app_01HQ8/detection", map[string]any{
				"status":    "succeeded",
				"detection": map[string]any{"winning_bid": map[string]string{"detector": "Static", "strategy": "static"}},
			})

		got := run(t, api, "", "deploy", dir)
		require.NoError(t, got.err)
		require.Contains(t, got.errOut, "Uploading 1 files")
		require.Contains(t, got.errOut, tc.want)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// incompressible returns n bytes gzip cannot shrink, so the archive the CLI
// reports the size of is about as large as the file that went into it.
func incompressible(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// entries lists the paths inside a packed archive, sorted.
func entries(t *testing.T, archive []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	defer func() { require.NoError(t, gz.Close()) }()

	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return names
}
