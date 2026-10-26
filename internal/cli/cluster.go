package cli

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// clusterCmd manages the install's cluster: its hosts and the tokens they join
// with (R-414, R-415). The console's Hosts screen does the same (R-261).
func clusterCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "cluster", Short: "The hosts this installation runs on, and joining new ones"}
	cmd.AddCommand(clusterHostsCmd(client), clusterTokensCmd(client))
	return cmd
}

type hostRow struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Roles      []string   `json:"roles"`
	Address    string     `json:"address"`
	Version    string     `json:"version"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

type joinTokenRow struct {
	ID        string     `json:"id"`
	Roles     []string   `json:"roles"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func clusterHostsCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "hosts", Short: "The cluster's hosts"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the cluster's hosts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Hosts []hostRow `json:"hosts"`
			}
			if err := c.Do("GET", "/cluster/hosts", nil, &out); err != nil {
				return err
			}
			t := table(cmd.OutOrStdout(), "NAME", "ROLES", "ADDRESS", "VERSION", "LAST SEEN", "ID")
			for _, h := range out.Hosts {
				seen := "never"
				if h.LastSeenAt != nil {
					seen = h.LastSeenAt.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, strings.Join(h.Roles, ","), h.Address, h.Version, seen, h.ID)
			}
			return t.Flush()
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "remove <host-id>",
		Short: "Remove a host from the cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/cluster/hosts/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed the host.")
			return nil
		},
	})
	return cmd
}

func clusterTokensCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "tokens", Short: "Tokens new hosts join with"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List join tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Tokens []joinTokenRow `json:"tokens"`
			}
			if err := c.Do("GET", "/cluster/join-tokens", nil, &out); err != nil {
				return err
			}
			t := table(cmd.OutOrStdout(), "ID", "ROLES", "USES", "EXPIRES", "REVOKED")
			for _, tok := range out.Tokens {
				revoked := ""
				if tok.RevokedAt != nil {
					revoked = tok.RevokedAt.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(t, "%s\t%s\t%d/%d\t%s\t%s\n", tok.ID, strings.Join(tok.Roles, ","), tok.Uses, tok.MaxUses,
					tok.ExpiresAt.UTC().Format(time.RFC3339), revoked)
			}
			return t.Flush()
		},
	})

	var roles []string
	var uses, hours int
	var address string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a join token. It is shown once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if len(roles) > 0 {
				body["roles"] = roles
			}
			if uses > 0 {
				body["max_uses"] = uses
			}
			if hours > 0 {
				body["expires_hours"] = hours
			}
			if address != "" {
				body["address"] = address
			}
			var out struct {
				Token joinTokenRow `json:"token"`
				Join  string       `json:"join"`
			}
			if err := c.Do("POST", "/cluster/join-tokens", body, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, out.Join)
			fmt.Fprintf(w, "\nRoles %s, %d use, expires %s. Shown once.\n",
				strings.Join(out.Token.Roles, ","), out.Token.MaxUses, out.Token.ExpiresAt.UTC().Format(time.RFC3339))
			return nil
		},
	}
	create.Flags().StringSliceVar(&roles, "role", nil, "control, app, or both (repeat or comma-separate). Default: both while the cluster has fewer than three hosts, then app")
	create.Flags().IntVar(&uses, "uses", 0, "How many hosts may join with it (default 1)")
	create.Flags().IntVar(&hours, "expires-hours", 0, "How many hours it lasts (default 24)")
	create.Flags().StringVar(&address, "address", "", "The installation's address the joining host reaches (default this server's advertise address)")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a join token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/cluster/join-tokens/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Revoked the join token.")
			return nil
		},
	})
	return cmd
}
