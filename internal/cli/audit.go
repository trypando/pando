package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// auditCmd reads the audit log (R-027), with the same filters as the console
// and the API, which it had none of: GET /audit was reachable from the console
// alone (R-261).
func auditCmd(client func() (*Client, error)) *cobra.Command {
	var action, actor, actorKind, app, targetKind, target, involving, since, until, before string
	var limit int

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the audit log",
		Long: "Lists what was done on this installation, newest first. The filters combine.\n\n" +
			"--since and --until take a time (2026-09-21T09:00:00Z) or a duration back from now\n" +
			"(24h, 30m), so --since 24h is the last day.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			q := url.Values{}
			for key, v := range map[string]string{
				"action": action, "principal_id": actor, "principal_kind": actorKind, "app_id": app,
				"target_kind": targetKind, "target_id": target, "involving": involving, "before": before,
			} {
				if v != "" {
					q.Set(key, v)
				}
			}
			for key, v := range map[string]string{"since": since, "until": until} {
				if v == "" {
					continue
				}
				at, err := whenFlag(v)
				if err != nil {
					return fmt.Errorf("--%s: %w", key, err)
				}
				q.Set(key, at)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}

			var out struct {
				Events []struct {
					OccurredAt time.Time `json:"occurred_at"`
					Action     string    `json:"action"`
					Principal  string    `json:"principal_id"`
					OnBehalfOf string    `json:"on_behalf_of"`
					TargetKind string    `json:"target_kind"`
					TargetID   string    `json:"target_id"`
				} `json:"events"`
				NextBefore string `json:"next_before"`
			}
			path := "/audit"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			if err := c.Do("GET", path, nil, &out); err != nil {
				return err
			}

			t := table(cmd.OutOrStdout(), "WHEN", "ACTION", "WHO", "TARGET")
			for _, e := range out.Events {
				who := e.Principal
				if e.OnBehalfOf != "" && e.OnBehalfOf != e.Principal {
					who += " for " + e.OnBehalfOf
				}
				target := e.TargetID
				if e.TargetKind != "" {
					target = e.TargetKind + " " + target
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", e.OccurredAt.Local().Format(time.DateTime), e.Action, who, target)
			}
			if err := t.Flush(); err != nil {
				return err
			}
			// The same filters, one page further back: the API pages on a cursor,
			// and the command that reads the next page is the useful thing to say.
			if out.NextBefore != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "\nOlder events: add --before %s\n", out.NextBefore)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&action, "action", "", "actions starting with this, e.g. app. or grant.delete")
	f.StringVar(&actor, "actor", "", "who did it: a user or token ID, or system, reconciler or detection")
	f.StringVar(&actorKind, "actor-kind", "", "what kind of actor: user, token, system or anonymous")
	f.StringVar(&app, "app", "", "events on this app")
	f.StringVar(&targetKind, "target-kind", "", "what kind of thing it was done to, e.g. user, role, app")
	f.StringVar(&target, "target", "", "the ID of the thing it was done to")
	f.StringVar(&involving, "involving", "", "events where this ID is the actor or the target, e.g. a user ID")
	f.StringVar(&since, "since", "", "from this time, or this long ago (24h)")
	f.StringVar(&until, "until", "", "up to this time, or this long ago")
	f.IntVar(&limit, "limit", 0, "how many events (default 100, at most 500)")
	f.StringVar(&before, "before", "", "the page before this cursor, as printed after a full page")

	cmd.AddCommand(auditArchivesCmd(client), auditTailCmd(client), auditExportCmd(client), auditSinksCmd(client))
	return cmd
}

// auditArchive is one archived month, as GET /audit/archives lists it.
type auditArchive struct {
	ID         string    `json:"id"`
	Month      string    `json:"month"`
	AdapterRef string    `json:"adapter_ref"`
	RowCount   int64     `json:"row_count"`
	FirstAt    time.Time `json:"first_at"`
	LastAt     time.Time `json:"last_at"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
}

// auditArchivesCmd lists and downloads the months retention has archived
// (R-347).
func auditArchivesCmd(client func() (*Client, error)) *cobra.Command {
	list := func(c *Client) ([]auditArchive, error) {
		var out struct {
			Archives []auditArchive `json:"archives"`
		}
		err := c.Do("GET", "/audit/archives", nil, &out)
		return out.Archives, err
	}

	cmd := &cobra.Command{
		Use:   "archives",
		Short: "List the months of the audit log archived past retention",
		Long: "Months older than host policy's audit_retention_months are archived, checked, and removed\n" +
			"from the live log, so `pando audit` no longer finds them. This lists the archives; download\n" +
			"one with `pando audit archives download <id>`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			archives, err := list(c)
			if err != nil {
				return err
			}
			if len(archives) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No months have been archived.")
				return nil
			}
			t := table(cmd.OutOrStdout(), "ID", "MONTH", "EVENTS", "SIZE", "KEPT BY", "SHA-256")
			for _, a := range archives {
				where := "Pando"
				if a.AdapterRef != "" {
					where = a.AdapterRef
				}
				fmt.Fprintf(t, "%s\t%s\t%d\t%s\t%s\t%s\n", a.ID, a.Month, a.RowCount, size(a.SizeBytes), where, a.SHA256)
			}
			return t.Flush()
		},
	}

	var output string
	download := &cobra.Command{
		Use:   "download <archive-id>",
		Short: "Download one archived month, checking it against its digest",
		Long: "Saves the archive — gzipped JSON lines, one audit event per line — to --output, or to\n" +
			"audit-<month>.jsonl.gz in the current directory. The SHA-256 is checked against the archive's\n" +
			"manifest as it arrives, and a file that does not match is removed rather than kept.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			archives, err := list(c)
			if err != nil {
				return err
			}
			var rec *auditArchive
			for i := range archives {
				if archives[i].ID == args[0] {
					rec = &archives[i]
				}
			}
			if rec == nil {
				return fmt.Errorf("there is no audit archive %q; `pando audit archives` lists them", args[0])
			}

			body, err := c.Stream("GET", "/audit/archives/"+url.PathEscape(rec.ID), nil)
			if err != nil {
				return err
			}
			defer func() { _ = body.Close() }()

			path := output
			if path == "" {
				path = "audit-" + rec.Month + ".jsonl.gz"
			}
			dst := cmd.OutOrStdout()
			var file *os.File
			if path != "-" {
				if file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
					return err
				}
				dst = file
			}
			digest := sha256.New()
			n, err := io.Copy(io.MultiWriter(dst, digest), body)
			if file != nil {
				if cerr := file.Close(); err == nil {
					err = cerr
				}
			}
			if err == nil && hex.EncodeToString(digest.Sum(nil)) != rec.SHA256 {
				err = fmt.Errorf("the download's SHA-256 does not match the archive's manifest (%s); try again", rec.SHA256)
			}
			if err != nil {
				if file != nil {
					_ = os.Remove(path)
				}
				return err
			}
			if file != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Saved %s: %d events from %s, %s, SHA-256 %s.\n",
					path, rec.RowCount, rec.Month, size(n), rec.SHA256)
			}
			return nil
		},
	}
	download.Flags().StringVarP(&output, "output", "o", "", "where to save it; - writes to standard output")
	cmd.AddCommand(download)
	return cmd
}

// whenFlag turns a time or a duration-ago into RFC 3339.
func whenFlag(v string) (string, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d).UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("%q is neither a time like 2026-09-21T09:00:00Z nor a duration like 24h", v)
}

// auditGet makes a GET that honors ctx and hands back the response, headers
// and all, for the stream and the export: c.Do has no context, so Ctrl-C could
// not end a long poll, and c.Stream has no headers, which the export reads.
// A failure is decoded into the same APIError c.Do returns.
func auditGet(ctx context.Context, c *Client, hc *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1"+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching %s: %w", c.BaseURL, err)
	}
	warnSkew(c.Warn, c.BaseURL, resp)
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		apiErr := &APIError{Status: resp.StatusCode}
		raw, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(raw, apiErr); err != nil || apiErr.Message == "" {
			apiErr.Message = fmt.Sprintf("The server returned %d.", resp.StatusCode)
		}
		return nil, apiErr
	}
	return resp, nil
}

// auditFilter adds --format, --action and --exclude to a query.
func auditFilter(q url.Values, format string, actions, exclude []string) {
	if format != "" {
		q.Set("format", format)
	}
	for _, a := range actions {
		q.Add("action", a)
	}
	for _, e := range exclude {
		q.Add("exclude", e)
	}
}

// streamWait is how many seconds `pando audit tail --follow` asks the server
// to hold a request open for the next event: under the API's 60-second cap,
// and short enough that a proxy in between does not drop the connection.
const streamWait = 30

// auditTailCmd reads the audit log in commit order from a cursor (R-381,
// design 12 §4).
func auditTailCmd(client func() (*Client, error)) *cobra.Command {
	var after, format string
	var follow bool
	var actions, exclude []string
	var limit int

	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Print the audit log in commit order, one JSON event per line",
		Long: "Prints audit events oldest first, one JSON object per line, from --after: a cursor a previous\n" +
			"run printed, or now for only what happens from here on. Without --after it starts at the oldest\n" +
			"event in the live log. Without --follow it stops once it has caught up; with --follow it keeps\n" +
			"waiting for new events until interrupted.\n\n" +
			"When it stops, for any reason, it prints the last cursor to standard error as\n" +
			"`cursor: c1.…`. Pass that back as --after to carry on where it left off. Delivery is at least\n" +
			"once: an event can be printed again after a resume, and its id is the key to drop it by.",
		Example: "  pando audit tail --follow --after now\n" +
			"  pando audit tail --after c1.AAAA --action grant. --exclude grant.view 2>cursor.txt >>events.jsonl",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// What to print on the way out: the cursor given, until the server
			// answers with one. "now" is not a position to resume from.
			cursor := ""
			if after != "now" {
				cursor = after
			}
			defer func() {
				if cursor != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "cursor: %s\n", cursor)
				}
			}()

			out := cmd.OutOrStdout()
			next := after
			for {
				q := url.Values{}
				if next != "" {
					q.Set("after", next)
				}
				if limit > 0 {
					q.Set("limit", strconv.Itoa(limit))
				}
				if follow {
					q.Set("wait", strconv.Itoa(streamWait))
				}
				auditFilter(q, format, actions, exclude)

				var page struct {
					Events   []json.RawMessage `json:"events"`
					Cursor   string            `json:"cursor"`
					CaughtUp bool              `json:"caught_up"`
				}
				resp, err := auditGet(ctx, c, c.HTTP, "/audit/stream?"+q.Encode())
				if err == nil {
					err = json.NewDecoder(resp.Body).Decode(&page)
					_ = resp.Body.Close()
				}
				if err != nil {
					if ctx.Err() != nil {
						return nil // interrupted: a clean stop, cursor and all
					}
					return err
				}

				var lines bytes.Buffer
				for _, e := range page.Events {
					if err := json.Compact(&lines, e); err != nil {
						return err
					}
					lines.WriteByte('\n')
				}
				if _, err := out.Write(lines.Bytes()); err != nil {
					return err
				}
				// Only once the page is written: a cursor past events nobody
				// saw would lose them on a resume.
				if page.Cursor != "" {
					cursor, next = page.Cursor, page.Cursor
				}
				if (!follow && page.CaughtUp) || ctx.Err() != nil {
					return nil
				}
			}
		},
	}

	f := cmd.Flags()
	f.StringVar(&after, "after", "", "start after this cursor, as a previous run printed it, or now")
	f.BoolVarP(&follow, "follow", "f", false, "keep waiting for new events until interrupted")
	f.StringVar(&format, "format", "", "native (the archive's line format, the default) or ocsf")
	f.StringArrayVar(&actions, "action", nil, "only actions starting with this, e.g. grant.; repeatable")
	f.StringArrayVar(&exclude, "exclude", nil, "leave out actions starting with this; repeatable")
	f.IntVar(&limit, "limit", 0, "events per request (default 500, at most 1000)")
	return cmd
}

// auditExportCmd writes a range of the live log as gzipped JSON lines (R-387,
// design 12 §7).
func auditExportCmd(client func() (*Client, error)) *cobra.Command {
	var since, until, format, output string
	var actions, exclude []string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a range of the audit log as gzipped JSON lines",
		Long: "Writes the audit events between --since and --until, in commit order, as gzipped JSON lines:\n" +
			"to --output, or to standard output when that is not a terminal. --since and --until take a time\n" +
			"(2026-09-21T09:00:00Z) or a duration back from now (24h); left out, the range is open at that end.\n\n" +
			"The export covers the live log only. When the range reaches back before it, a note on standard\n" +
			"error says where the live log starts; `pando audit archives` has the months before that.",
		Example: "  pando audit export --since 720h > audit.jsonl.gz\n" +
			"  pando audit export --since 2026-09-01T00:00:00Z --until 2026-10-01T00:00:00Z --format ocsf -o september.jsonl.gz",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for key, v := range map[string]string{"since": since, "until": until} {
				if v == "" {
					continue
				}
				at, err := whenFlag(v)
				if err != nil {
					return fmt.Errorf("--%s: %w", key, err)
				}
				q.Set(key, at)
			}
			auditFilter(q, format, actions, exclude)

			toStdout := output == "" || output == "-"
			if f, ok := cmd.OutOrStdout().(*os.File); ok && toStdout && term.IsTerminal(int(f.Fd())) {
				return errors.New("pando audit export writes gzip, which a terminal cannot show. " +
					"Redirect it to a file (pando audit export ... > audit.jsonl.gz) or pass -o audit.jsonl.gz")
			}

			c, err := client()
			if err != nil {
				return err
			}
			path := "/audit/export"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			// No timeout: an export of a year is streamed, and takes as long as it takes.
			resp, err := auditGet(cmd.Context(), c, nil, path)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()

			dst := cmd.OutOrStdout()
			var file *os.File
			if !toStdout {
				if file, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
					return err
				}
				dst = file
			}
			events, err := copyCheckedGzip(dst, resp.Body)
			if file != nil {
				if cerr := file.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					_ = os.Remove(output)
				}
			}
			if err != nil {
				return err
			}

			stderr := cmd.ErrOrStderr()
			if file != nil {
				fmt.Fprintf(stderr, "Saved %s: %d events.\n", output, events)
			}
			if from := resp.Header.Get("Pando-Audit-Live-From"); from != "" {
				at := from
				if t, err := time.Parse(time.RFC3339Nano, from); err == nil {
					at = t.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(stderr, "The live log starts at %s, so the export starts there. "+
					"Events before it are archived: `pando audit archives` lists the months, and "+
					"`pando audit archives download <id>` saves one.\n", at)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&since, "since", "", "from this time, or this long ago (720h)")
	f.StringVar(&until, "until", "", "up to this time, or this long ago")
	f.StringVar(&format, "format", "", "native (the archive's line format, the default) or ocsf")
	f.StringArrayVar(&actions, "action", nil, "only actions starting with this, e.g. grant.; repeatable")
	f.StringArrayVar(&exclude, "exclude", nil, "leave out actions starting with this; repeatable")
	f.StringVarP(&output, "output", "o", "", "the file to write; - or left out writes to standard output")
	return cmd
}

// copyCheckedGzip copies a gzip body to dst, decompressing it on the way to
// count its lines and to check it is whole. The server has sent 200 before it
// starts, so an export that ends early can only show as a truncated gzip
// (design 12 §7); this is where that becomes an error rather than a file that
// looks complete.
func copyCheckedGzip(dst io.Writer, body io.Reader) (int, error) {
	tee := io.TeeReader(body, dst)
	gz, err := gzip.NewReader(tee)
	if err == nil {
		var lines lineCounter
		//nolint:gosec // G110: decompressed only to count lines, never held; the bytes kept are the compressed ones.
		if _, err = io.Copy(&lines, gz); err == nil {
			// Anything after the gzip stream still belongs in the output.
			_, err = io.Copy(io.Discard, tee)
			return lines.n, err
		}
	}
	return 0, fmt.Errorf("the export ended before it was complete (%v). Nothing is wrong with the audit log; "+
		"run the export again, and if it keeps ending early, narrow the range with --since and --until", err)
}

type lineCounter struct{ n int }

func (l *lineCounter) Write(p []byte) (int, error) {
	l.n += bytes.Count(p, []byte{'\n'})
	return len(p), nil
}

// auditSink is one audit sink, as GET /audit/sinks lists it.
type auditSink struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Enabled        bool       `json:"enabled"`
	Transport      string     `json:"transport"`
	Endpoint       string     `json:"endpoint"`
	Backlog        int        `json:"backlog"`
	BacklogCapped  bool       `json:"backlog_capped"`
	Unusable       string     `json:"unusable"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	LastError      string     `json:"last_error"`
	FailingSince   *time.Time `json:"failing_since"`
	DisabledAt     *time.Time `json:"disabled_at"`
	DisabledReason string     `json:"disabled_reason"`
	GapFrom        *time.Time `json:"gap_from"`
	GapTo          *time.Time `json:"gap_to"`
	Disclosure     string     `json:"disclosure"`
}

// auditSinksCmd lists where the audit log is pushed and how far each
// destination has got (R-383, R-385).
func auditSinksCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "sinks",
		Short: "List where the audit log is sent, and how far each destination has got",
		Long: "An audit sink is a destination, such as a SIEM's syslog or HTTPS collector, that Pando pushes\n" +
			"the audit log to as it is written. This lists each one with what it sends where, the events\n" +
			"waiting to go (BACKLOG), when it last delivered, and its last error. Below the table, each sink's\n" +
			"line says exactly what leaves the installation, and any range of events it missed.\n\n" +
			"Audit sinks are adapters: add one with `pando adapter add audit_sink/<kind>`.",
		Example: "  pando audit sinks",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Sinks []auditSink `json:"audit_sinks"`
			}
			if err := c.Do("GET", "/audit/sinks", nil, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(out.Sinks) == 0 {
				fmt.Fprintln(w, "No audit sinks are configured. Add one with `pando adapter add audit_sink/<kind>`; "+
					"`pando adapter kinds` lists the kinds.")
				return nil
			}

			when := func(t *time.Time, none string) string {
				if t == nil {
					return none
				}
				return t.Local().Format(time.DateTime)
			}
			t := table(w, "ID", "KIND", "ENABLED", "SENDS TO", "BACKLOG", "LAST DELIVERED", "LAST ERROR")
			for _, s := range out.Sinks {
				enabled := "yes"
				if !s.Enabled || s.DisabledAt != nil {
					enabled = "no"
				}
				to := s.Endpoint
				if to == "" {
					to = "-"
				} else if s.Transport != "" {
					to += " (" + s.Transport + ")"
				}
				backlog := strconv.Itoa(s.Backlog)
				if s.BacklogCapped {
					backlog += "+"
				}
				lastErr := strings.Join(strings.Fields(s.LastError), " ")
				if lastErr == "" {
					lastErr = "-"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.ID, s.Kind, enabled, to, backlog, when(s.DeliveredAt, "never"), lastErr)
			}
			if err := t.Flush(); err != nil {
				return err
			}

			fmt.Fprintln(w)
			for _, s := range out.Sinks {
				if s.Disclosure != "" {
					fmt.Fprintf(w, "%s: %s\n", s.ID, s.Disclosure)
				}
				if s.Unusable != "" {
					fmt.Fprintf(w, "%s cannot run with its settings: %s\n", s.ID, s.Unusable)
				}
				if s.DisabledAt != nil {
					fmt.Fprintf(w, "%s was turned off by Pando at %s: %s\n", s.ID, when(s.DisabledAt, ""), s.DisabledReason)
				}
				if s.FailingSince != nil {
					fmt.Fprintf(w, "%s has been failing since %s.\n", s.ID, when(s.FailingSince, ""))
				}
				if s.GapFrom != nil && s.GapTo != nil {
					fmt.Fprintf(w, "%s missed the events from %s to %s. `pando audit export --since %s --until %s` has them.\n",
						s.ID, when(s.GapFrom, ""), when(s.GapTo, ""),
						s.GapFrom.UTC().Format(time.RFC3339), s.GapTo.UTC().Format(time.RFC3339))
				}
			}
			return nil
		},
	}
}
