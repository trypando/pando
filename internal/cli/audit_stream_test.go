package cli_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

// queryOf is the query string of the n-th call the fake server saw.
func queryOf(t *testing.T, api *fakeAPI, n int) url.Values {
	t.Helper()
	require.Greater(t, len(api.calls), n)
	u, err := url.Parse(api.calls[n].path)
	require.NoError(t, err)
	return u.Query()
}

// serverAt is a fakeAPI over a handler of the test's own, for a reply that
// needs headers or has to block, which the routing table cannot express.
func serverAt(t *testing.T, h http.HandlerFunc) *fakeAPI {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fakeAPI{t: t, url: srv.URL, responses: map[string]any{}, status: map[string]int{}}
}

// TestR381_TailReadsUntilCaughtUpAndPrintsTheCursor asserts R-381 from the
// CLI: pages are read in order, each from the cursor the last one returned,
// every event is one line on stdout, and the last cursor is on stderr so a
// script can resume (design 12 §4).
func TestR381_TailReadsUntilCaughtUpAndPrintsTheCursor(t *testing.T) {
	pages := []map[string]any{
		{"events": []map[string]any{{"id": 1, "action": "app.create"}, {"id": 2, "action": "grant.create"}}, "cursor": "c1.a", "caught_up": false},
		{"events": []map[string]any{{"id": 3, "action": "app.delete"}}, "cursor": "c1.b", "caught_up": true},
	}
	n := 0
	api := newAPI(t).handle("GET /audit/stream", func() any { p := pages[n]; n++; return p })

	got := run(t, api, "", "audit", "tail", "--after", "c1.start", "--format", "ocsf",
		"--action", "app.", "--action", "grant.", "--exclude", "grant.view", "--limit", "2")
	require.NoError(t, got.err, got.errOut)

	require.Equal(t, `{"action":"app.create","id":1}`+"\n"+`{"action":"grant.create","id":2}`+"\n"+`{"action":"app.delete","id":3}`+"\n", got.out,
		"one compact JSON line per event, in order")
	require.Equal(t, "cursor: c1.b\n", got.errOut)

	require.Len(t, api.calls, 2, "it stops once caught up")
	first, second := queryOf(t, api, 0), queryOf(t, api, 1)
	require.Equal(t, "c1.start", first.Get("after"))
	require.Equal(t, "c1.a", second.Get("after"), "each page starts where the last ended")
	require.Equal(t, []string{"app.", "grant."}, first["action"])
	require.Equal(t, []string{"grant.view"}, first["exclude"])
	require.Equal(t, "ocsf", first.Get("format"))
	require.Equal(t, "2", first.Get("limit"))
	require.Empty(t, first.Get("wait"), "without --follow nothing waits")
}

// TestR381_TailFollowLongPollsAndStopsCleanlyWhenInterrupted asserts that
// --follow keeps long-polling from the latest cursor, and that being stopped
// mid-wait is a clean exit that still prints the cursor to resume from.
func TestR381_TailFollowLongPollsAndStopsCleanlyWhenInterrupted(t *testing.T) {
	isolateConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	var queries []url.Values
	api := serverAt(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"events":[{"id":7}],"cursor":"c1.seven","caught_up":true}`))
		case 2:
			_, _ = w.Write([]byte(`{"events":[],"cursor":"c1.seven","caught_up":true}`))
		default:
			cancel() // Ctrl-C, while the request waits for an event
			<-r.Context().Done()
		}
	})

	var cmd *cobra.Command
	for _, c := range cli.Commands() {
		if c.Name() == "audit" {
			cmd = c
		}
	}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"tail", "--follow", "--after", "now", "--server", api.url})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err, "an interrupted tail is a clean stop")
	case <-time.After(10 * time.Second):
		t.Fatal("tail --follow did not stop when its context was canceled")
	}

	require.Equal(t, `{"id":7}`+"\n", out.String())
	require.Equal(t, "cursor: c1.seven\n", errOut.String())
	require.Len(t, queries, 3, "an empty page is not the end when following")
	require.Equal(t, "now", queries[0].Get("after"))
	require.Equal(t, "c1.seven", queries[1].Get("after"))
	require.Equal(t, "30", queries[1].Get("wait"))
}

// TestR381_TailPrintsTheCursorItWasGivenWhenTheServerRefuses asserts that a
// failed run still says where to resume, and that "now" is not a cursor to
// print.
func TestR381_TailPrintsTheCursorItWasGivenWhenTheServerRefuses(t *testing.T) {
	refused := newAPI(t).fail("GET /audit/stream", 403, map[string]any{"message": "You need install.audit.read."})

	got := run(t, refused, "", "audit", "tail", "--after", "c1.kept")
	require.ErrorContains(t, got.err, "install.audit.read")
	require.Equal(t, "cursor: c1.kept\n", got.errOut)

	got = run(t, refused, "", "audit", "tail", "--after", "now")
	require.Error(t, got.err)
	require.Empty(t, got.errOut, "now is not a position to resume from")
}

// gzipOf is lines gzipped, as the export sends them.
func gzipOf(t *testing.T, lines ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, l := range lines {
		_, err := gz.Write([]byte(l + "\n"))
		require.NoError(t, err)
	}
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestR387_ExportWritesTheGzipAndPointsAtTheArchives asserts R-387 from the
// CLI: the range and filters reach the API, the gzip is saved as sent, a range
// reaching before the live log says where the rest is, and an export that
// ended early is refused rather than left looking complete.
func TestR387_ExportWritesTheGzipAndPointsAtTheArchives(t *testing.T) {
	body := gzipOf(t, `{"id":1}`, `{"id":2}`)
	var queries []url.Values
	reply := body
	api := serverAt(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/audit/export", r.URL.Path)
		queries = append(queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Pando-Audit-Live-From", "2026-07-01T00:00:00.123Z")
		_, _ = w.Write(reply)
	})
	out := filepath.Join(t.TempDir(), "audit.jsonl.gz")

	got := run(t, api, "", "audit", "export", "--since", "2026-06-01T00:00:00Z", "--until", "24h",
		"--format", "ocsf", "--action", "grant.", "--exclude", "grant.view", "-o", out)
	require.NoError(t, got.err, got.errOut)
	saved, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, body, saved)
	require.Contains(t, got.errOut, "Saved "+out+": 2 events.")
	require.Contains(t, got.errOut, "The live log starts at 2026-07-01T00:00:00Z")
	require.Contains(t, got.errOut, "pando audit archives")

	q := queries[0]
	require.Equal(t, "2026-06-01T00:00:00Z", q.Get("since"))
	until, err := time.Parse(time.RFC3339, q.Get("until"))
	require.NoError(t, err, "a duration is sent as a time")
	require.WithinDuration(t, time.Now().Add(-24*time.Hour), until, time.Minute)
	require.Equal(t, "ocsf", q.Get("format"))
	require.Equal(t, []string{"grant."}, q["action"])
	require.Equal(t, []string{"grant.view"}, q["exclude"])

	// To standard output, which is not a terminal here.
	got = run(t, api, "", "audit", "export")
	require.NoError(t, got.err, got.errOut)
	require.Equal(t, string(body), got.out)
	require.Empty(t, queries[1], "no range is the whole live log")

	// A file already there is not overwritten.
	got = run(t, api, "", "audit", "export", "-o", out)
	require.Error(t, got.err)

	// An export cut short is an error, and leaves no file behind.
	reply = body[:len(body)-6]
	short := filepath.Join(t.TempDir(), "short.jsonl.gz")
	got = run(t, api, "", "audit", "export", "-o", short)
	require.ErrorContains(t, got.err, "ended before it was complete")
	require.NoFileExists(t, short)

	// A time that is neither form.
	got = run(t, api, "", "audit", "export", "--since", "last tuesday")
	require.ErrorContains(t, got.err, "--since")
}

// TestR383_SinksListsWhatEachSendsWhereAndHowFarItHasGot asserts R-383 from
// the CLI: each sink's destination, backlog, last delivery and last error,
// its disclosure sentence, and any range it missed.
func TestR383_SinksListsWhatEachSendsWhereAndHowFarItHasGot(t *testing.T) {
	api := newAPI(t).reply("GET /audit/sinks", map[string]any{"audit_sinks": []map[string]any{
		{
			"id": "as_siem", "kind": "syslog", "enabled": true, "transport": "syslog", "endpoint": "siem.internal:6514",
			"backlog": 10000, "backlog_capped": true, "delivered_at": "2026-10-01T09:00:00Z", "delivered_count": 41,
			"last_error": "connection refused\nby peer", "failing_since": "2026-10-01T09:05:00Z",
			"gap_from": "2026-09-01T00:00:00Z", "gap_to": "2026-09-02T00:00:00Z",
			"disclosure": "Every audit event is sent to siem.internal:6514 over syslog.",
		},
		{
			"id": "as_hook", "kind": "https", "enabled": false, "transport": "https", "endpoint": "https://collector.example/ingest",
			"backlog": 0, "disabled_at": "2026-10-02T00:00:00Z", "disabled_reason": "The collector answered 401 for 24 hours.",
			"disclosure": "Audit events matching grant. are sent to https://collector.example/ingest over HTTPS.",
		},
	}})

	got := run(t, api, "", "audit", "sinks")
	require.NoError(t, got.err, got.errOut)
	for _, want := range []string{
		"ID", "KIND", "ENABLED", "SENDS TO", "BACKLOG", "LAST DELIVERED", "LAST ERROR",
		"as_siem", "siem.internal:6514 (syslog)", "10000+", "connection refused by peer",
		"as_hook", "https://collector.example/ingest (https)", "never",
		"as_siem: Every audit event is sent to siem.internal:6514 over syslog.",
		"as_hook was turned off by Pando", "The collector answered 401 for 24 hours.",
		"as_siem has been failing since",
		"as_siem missed the events from", "pando audit export --since 2026-09-01T00:00:00Z --until 2026-09-02T00:00:00Z",
	} {
		require.Contains(t, got.out, want)
	}
	row := func(id string) string {
		for _, l := range strings.Split(got.out, "\n") {
			if strings.HasPrefix(l, id+" ") {
				return l
			}
		}
		return ""
	}
	require.Contains(t, row("as_hook"), " no ")
	require.Contains(t, row("as_siem"), " yes ")

	empty := run(t, newAPI(t).reply("GET /audit/sinks", map[string]any{"audit_sinks": []any{}}), "", "audit", "sinks")
	require.NoError(t, empty.err)
	require.Contains(t, empty.out, "No audit sinks are configured.")

	refused := newAPI(t).fail("GET /audit/sinks", 403, map[string]any{"message": "You need install.audit.read."})
	got = run(t, refused, "", "audit", "sinks")
	require.ErrorContains(t, got.err, "install.audit.read")
}
