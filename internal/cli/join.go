package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Where `pando join` reads its two secrets when they are not on the command
// line, which other users of the machine can read and shell history keeps
// (R-194).
const (
	EnvJoinToken  = "PANDO_JOIN_TOKEN"
	EnvSetupToken = "PANDO_SETUP_TOKEN"
)

// argumentWarning is said when a secret is given on the command line.
const argumentWarning = "Warning: a token on the command line can be read by other users of this machine and is kept in shell history. " +
	"Leave it off to be asked for it, or set " + EnvJoinToken + " and " + EnvSetupToken + ".\n"

// joinCmd joins a new Pando to a cluster (R-415), as the setup screen's Join a
// cluster does: POST /setup/join on that Pando. Before setup there is no
// account to sign in with, so it needs no stored token; it needs the setup
// token that Pando printed to its log, as setting it up does (R-046).
func joinCmd(server *string) *cobra.Command {
	var name, setupToken string
	cmd := &cobra.Command{
		Use:   "join [token]",
		Short: "Join a new Pando to a cluster with a join token",
		Long: "Joins the Pando at --server, which must be new, with no account or app, to the\n" +
			"cluster that minted the join token (pando cluster tokens create). The Pando checks the\n" +
			"cluster's certificate authority against the token, joins, and restarts into the\n" +
			"cluster. It also needs the setup_token that Pando printed to its log at startup.\n\n" +
			"Each token is read from " + EnvJoinToken + " and " + EnvSetupToken + ", or asked for\n" +
			"without echoing, or read from standard input one per line, join token first.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := joinSecret(cmd, args, EnvJoinToken, "Join token: ")
			if err != nil {
				return err
			}
			if setupToken == "" {
				if setupToken, err = joinSecret(cmd, nil, EnvSetupToken, "Setup token: "); err != nil {
					return err
				}
			} else {
				fmt.Fprint(cmd.ErrOrStderr(), argumentWarning)
			}
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
			body := map[string]string{"setup_token": setupToken, "token": token, "name": name}
			if err := anon.Do("POST", "/setup/join", body, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined the cluster as %s (%s), roles %s. Pando is restarting into the cluster.\n",
				out.Name, out.HostID, strings.Join(out.Roles, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "this host's name in the cluster (default: the machine's hostname)")
	cmd.Flags().StringVar(&setupToken, "setup-token", "", "the setup token from the joining Pando's log (setup_token); prefer "+EnvSetupToken+" or the prompt")
	return cmd
}

// joinSecret is a secret `pando join` needs: the argument when one was given,
// with a warning, else env, else asked for.
func joinSecret(cmd *cobra.Command, args []string, env, label string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		fmt.Fprint(cmd.ErrOrStderr(), argumentWarning)
		return args[0], nil
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	v, err := promptSecret(cmd, label)
	if v = strings.TrimSpace(v); v == "" {
		return "", errors.New("no " + strings.ToLower(strings.TrimSuffix(label, ": ")) + " was given. Set " + env + ", or type it when asked")
	}
	return v, err
}
