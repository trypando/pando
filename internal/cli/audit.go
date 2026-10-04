package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
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

	cmd.AddCommand(auditArchivesCmd(client))
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
