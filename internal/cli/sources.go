package cli

import (
	"fmt"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// sourceCmd is the installation's source connections (R-091, issue #127), as
// the console's Adapters screen has them. A connection is added with
// `pando adapter add source/<kind>`; this is what is particular to one.
func sourceCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "See and authorize the connections private repositories are read with",
		Long: "A source connection is how Pando reads private repositories on GitHub, GitLab, Azure DevOps,\n" +
			"Bitbucket, Gitea or any git host. Add one with `pando adapter add source/<kind>` — `pando adapter\n" +
			"kinds` lists the kinds — and every app whose repository it covers is read with it.",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show the source connections and whether each is ready",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Sources []struct {
					ID           string `json:"id"`
					Name         string `json:"name"`
					Kind         string `json:"kind"`
					Problem      string `json:"problem"`
					Capabilities struct {
						Method     string `json:"method"`
						Host       string `json:"host"`
						Scope      string `json:"scope"`
						Authorized bool   `json:"authorized"`
					} `json:"capabilities"`
				} `json:"sources"`
			}
			if err := c.Do("GET", "/sources", nil, &out); err != nil {
				return err
			}
			if len(out.Sources) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No source connections. Add one with `pando adapter add source/<kind>`; `pando adapter kinds` lists the kinds.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tCOVERS\tSIGNS IN WITH\tSTATE")
			for _, s := range out.Sources {
				covers := s.Capabilities.Host
				if s.Capabilities.Scope != "" {
					covers += "/" + s.Capabilities.Scope
				}
				state := "ready"
				switch {
				case s.Problem != "":
					state = s.Problem
				case !s.Capabilities.Authorized:
					state = "not authorized: pando source authorize " + s.ID
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, orDashString(covers), orDashString(s.Capabilities.Method), state)
			}
			return w.Flush()
		},
	})

	var query string
	var limit int
	repos := &cobra.Command{
		Use:   "repos <connection>",
		Short: "List the repositories a connection can read",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if query != "" {
				q.Set("q", query)
			}
			if limit > 0 {
				q.Set("limit", fmt.Sprint(limit))
			}
			var out struct {
				Repositories []struct {
					URL           string `json:"url"`
					FullName      string `json:"full_name"`
					DefaultBranch string `json:"default_branch"`
					Private       bool   `json:"private"`
				} `json:"repositories"`
			}
			path := "/sources/" + url.PathEscape(args[0]) + "/repositories"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			if err := c.Do("GET", path, nil, &out); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "REPOSITORY\tBRANCH\tVISIBILITY\tADDRESS")
			for _, r := range out.Repositories {
				vis := "public"
				if r.Private {
					vis = "private"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.FullName, orDashString(r.DefaultBranch), vis, r.URL)
			}
			return w.Flush()
		},
	}
	repos.Flags().StringVar(&query, "query", "", "only repositories whose name contains this")
	repos.Flags().IntVar(&limit, "limit", 0, "at most this many (default 100)")
	cmd.AddCommand(repos)

	cmd.AddCommand(&cobra.Command{
		Use:   "branches <connection> <repository-url>",
		Short: "List a repository's branches through a connection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Branches []string `json:"branches"`
			}
			if err := c.Do("GET", "/sources/"+url.PathEscape(args[0])+"/branches?url="+url.QueryEscape(args[1]), nil, &out); err != nil {
				return err
			}
			for _, b := range out.Branches {
				fmt.Fprintln(cmd.OutOrStdout(), b)
			}
			return nil
		},
	})

	var web bool
	var wait time.Duration
	authorize := &cobra.Command{
		Use:   "authorize <connection>",
		Short: "Sign a connection in with OAuth",
		Long: "Shows a code to enter on the provider's site, then waits until you have. Nothing needs to be\n" +
			"able to reach this Pando, so it works on a laptop. --web prints an address to open in a browser\n" +
			"instead, which returns to Pando's own address.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			mode := "device"
			if web {
				mode = "web"
			}
			var a struct {
				UserCode        string `json:"user_code"`
				VerificationURL string `json:"verification_url"`
				AuthorizeURL    string `json:"authorize_url"`
				IntervalSeconds int    `json:"interval_seconds"`
			}
			base := "/sources/" + url.PathEscape(args[0]) + "/authorize"
			if err := c.Do("POST", base, map[string]string{"mode": mode}, &a); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if web {
				fmt.Fprintf(out, "Open this address in a browser signed in to Pando, and approve the connection:\n\n  %s\n", a.AuthorizeURL)
				return nil
			}
			fmt.Fprintf(out, "Go to %s and enter the code %s\nWaiting for you to approve it...\n", a.VerificationURL, a.UserCode)
			interval := time.Duration(a.IntervalSeconds) * time.Second
			if interval <= 0 {
				interval = 5 * time.Second
			}
			deadline := time.Now().Add(wait)
			for time.Now().Before(deadline) {
				time.Sleep(interval)
				var st struct {
					Status   string `json:"status"`
					SlowDown bool   `json:"slow_down"`
				}
				if err := c.Do("POST", base+"/poll", nil, &st); err != nil {
					return err
				}
				if st.Status == "authorized" {
					fmt.Fprintf(out, "Authorized. %s is ready: apps from repositories it covers are read with it.\n", args[0])
					return nil
				}
				if st.SlowDown {
					interval += 5 * time.Second
				}
			}
			return fmt.Errorf("the code was not approved within %s; run `pando source authorize %s` again", wait, args[0])
		},
	}
	authorize.Flags().BoolVar(&web, "web", false, "authorize in a browser instead of with a code")
	authorize.Flags().DurationVar(&wait, "timeout", 15*time.Minute, "how long to wait for the code to be approved")
	cmd.AddCommand(authorize)

	cmd.AddCommand(&cobra.Command{
		Use:   "remove <connection>",
		Short: "Disconnect a source connection and delete its stored credential",
		Long: "Apps read with the connection keep running. Their next deploy fails, saying the connection is\n" +
			"gone, until another connection covers their repository.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/sources/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s.\n", args[0])
			return nil
		},
	})
	return cmd
}

func orDashString(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
