package cli_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/trypando/pando/internal/cli"
)

// failingWriter refuses every write, as a closed pipe on standard output does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// runAudit runs `pando audit <args>` writing to out, with server as --server
// when it is set, and nothing stored to fall back on.
func runAudit(t *testing.T, out io.Writer, server string, args ...string) (string, error) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(cli.EnvServer, "")
	t.Setenv(cli.EnvToken, "")
	var cmd *cobra.Command
	for _, c := range cli.Commands() {
		if c.Name() == "audit" {
			cmd = c
		}
	}
	require.NotNil(t, cmd)
	var errOut bytes.Buffer
	cmd.SetOut(out)
	cmd.SetErr(&errOut)
	if server != "" {
		args = append(args, "--server", server)
	}
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return errOut.String(), err
}

// TestR261_AuditCommandsNeedAServer asserts tail, export and sinks with no
// server to talk to say how to name one, rather than failing on a request.
func TestR261_AuditCommandsNeedAServer(t *testing.T) {
	for _, args := range [][]string{{"tail"}, {"export"}, {"sinks"}} {
		var out bytes.Buffer
		_, err := runAudit(t, &out, "", args...)
		require.ErrorIs(t, err, cli.ErrNotLoggedIn, "audit %v", args)
	}
}

// TestR381_TailSendsTheTokenAndReadsAnyErrorBody asserts the stream's request
// carries the token, a refusal that is not the error envelope (a proxy's HTML
// page) still becomes a readable error, and a URL that cannot make a request
// is refused before one is attempted.
func TestR381_TailSendsTheTokenAndReadsAnyErrorBody(t *testing.T) {
	var auth string
	api := serverAt(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})
	isolateConfig(t)
	t.Setenv(cli.EnvToken, "tok_tail_1")
	var cmd *cobra.Command
	for _, c := range cli.Commands() {
		if c.Name() == "audit" {
			cmd = c
		}
	}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"tail", "--server", api.url})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	require.EqualError(t, err, "The server returned 502.")
	require.Equal(t, "Bearer tok_tail_1", auth)
	require.Empty(t, out.String())

	_, err = runAudit(t, &out, "http://bad\x7fhost", "tail")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid control character in URL")
}

// TestR381_TailStopsWhenStandardOutputFails asserts a tail whose output can
// no longer be written stops with that error, and does not print a cursor past
// events nobody saw.
func TestR381_TailStopsWhenStandardOutputFails(t *testing.T) {
	api := newAPI(t).reply("GET /audit/stream", map[string]any{
		"events": []map[string]any{{"id": 1}}, "cursor": "c1.after", "caught_up": true,
	})
	errOut, err := runAudit(t, failingWriter{}, api.url, "tail", "--after", "c1.before")
	require.EqualError(t, err, "broken pipe")
	require.Equal(t, "cursor: c1.before\n", errOut, "the cursor printed is the last one whose events were written")
}

// TestR387_ExportRefusalsLeaveNoFile asserts a refused export is the server's
// message and creates no file, and that a body that is not gzip at all is
// refused like a truncated one.
func TestR387_ExportRefusalsLeaveNoFile(t *testing.T) {
	refused := newAPI(t).fail("GET /audit/export", 403, map[string]any{"code": "FORBIDDEN", "message": "You need install.audit.read."})
	out := filepath.Join(t.TempDir(), "audit.jsonl.gz")
	var stdout bytes.Buffer
	_, err := runAudit(t, &stdout, refused.url, "export", "-o", out)
	require.EqualError(t, err, "You need install.audit.read.")
	require.NoFileExists(t, out)

	plain := serverAt(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":1}` + "\n")) })
	_, err = runAudit(t, &stdout, plain.url, "export", "-o", out)
	require.ErrorContains(t, err, "the export ended before it was complete")
	require.ErrorContains(t, err, "narrow the range with --since and --until")
	require.NoFileExists(t, out)

	_, err = runAudit(t, &stdout, plain.url, "export", "--until", "next week")
	require.ErrorContains(t, err, "--until")
}

// TestR387_ExportRefusesToWriteGzipToATerminal asserts R-387's export, with
// standard output a terminal and no -o, is refused before any request with
// the two ways to save it.
func TestR387_ExportRefusesToWriteGzipToATerminal(t *testing.T) {
	tty, closeTTY, err := openTerminal()
	if err != nil {
		t.Skipf("no pseudo-terminal to stand in for one: %v", err)
	}
	t.Cleanup(closeTTY)
	require.True(t, term.IsTerminal(int(tty.Fd())))

	api := newAPI(t)
	_, err = runAudit(t, tty, api.url, "export")
	require.ErrorContains(t, err, "pando audit export writes gzip, which a terminal cannot show")
	require.ErrorContains(t, err, "-o audit.jsonl.gz")
	require.Empty(t, api.calls, "refused before any request")

	_, err = runAudit(t, tty, api.url, "export", "-o", "-")
	require.ErrorContains(t, err, "which a terminal cannot show", "- is standard output too")
}

// TestR383_SinksShowsAnUnusableSinkAndAMissingEndpoint asserts R-383: a sink
// whose settings cannot run says so below the table, and one with no
// endpoint shows a dash rather than an empty column.
func TestR383_SinksShowsAnUnusableSinkAndAMissingEndpoint(t *testing.T) {
	api := newAPI(t).reply("GET /audit/sinks", map[string]any{"audit_sinks": []map[string]any{{
		"id": "as_bad", "kind": "syslog", "enabled": true, "backlog": 3,
		"unusable": "syslog: no address is set. Set address to the collector's host and port, such as siem.example.com:6514",
	}}})
	var out bytes.Buffer
	_, err := runAudit(t, &out, api.url, "sinks")
	require.NoError(t, err)
	var row string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "as_bad ") {
			row = l
			break
		}
	}
	require.Equal(t, []string{"as_bad", "syslog", "yes", "-", "3", "never", "-"}, strings.Fields(row))
	require.Contains(t, out.String(), "as_bad cannot run with its settings: syslog: no address is set.")

	_, err = runAudit(t, failingWriter{}, api.url, "sinks")
	require.EqualError(t, err, "broken pipe")
}
