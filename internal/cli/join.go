package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// joinCmd joins a new Pando to a cluster (R-415), as the setup screen's Join a
// cluster does: POST /setup/join on that Pando. Before setup there is no
// account to sign in with, so it needs no stored token; it needs the setup
// token that Pando printed to its log, as setting it up does (R-046).
func joinCmd(server *string) *cobra.Command {
	var name, setupToken string
	cmd := &cobra.Command{
		Use:   "join <token>",
		Short: "Join a new Pando to a cluster with a join token",
		Long: "Joins the Pando at --server, which must be new, with no account or app, to the\n" +
			"cluster that minted the token (pando cluster tokens create). The Pando checks the\n" +
			"cluster's certificate authority against the token, joins, and restarts into the\n" +
			"cluster. --setup-token is the setup_token that Pando printed to its log at startup.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base := "http://localhost:8080"
			if *server != "" {
				base = strings.TrimSuffix(*server, "/")
			}
			anon := &Client{BaseURL: base, HTTP: &http.Client{Timeout: 2 * time.Minute}}
			var out struct {
				Roles  []string `json:"roles"`
				HostID string   `json:"host_id"`
				Name   string   `json:"name"`
			}
			body := map[string]string{"setup_token": setupToken, "token": args[0], "name": name}
			if err := anon.Do("POST", "/setup/join", body, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined the cluster as %s (%s), roles %s. Pando is restarting into the cluster.\n",
				out.Name, out.HostID, strings.Join(out.Roles, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "this host's name in the cluster (default: the machine's hostname)")
	cmd.Flags().StringVar(&setupToken, "setup-token", "", "the setup token from the joining Pando's log (setup_token)")
	_ = cmd.MarkFlagRequired("setup-token")
	return cmd
}
