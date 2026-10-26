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

// stamp is t in RFC 3339, or empty for none.
func stamp(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

// deleteByID is a command that DELETEs base/<id> and says done.
func deleteByID(client func() (*Client, error), use, short, base, done string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", base+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), done)
			return nil
		},
	}
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
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, strings.Join(h.Roles, ","), h.Address, h.Version, stamp(h.LastSeenAt, "never"), h.ID)
			}
			return t.Flush()
		},
	})
	cmd.AddCommand(deleteByID(client, "remove <host-id>", "Remove a host from the cluster", "/cluster/hosts/", "Removed the host."))
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
				fmt.Fprintf(t, "%s\t%s\t%d/%d\t%s\t%s\n", tok.ID, strings.Join(tok.Roles, ","), tok.Uses, tok.MaxUses,
					stamp(&tok.ExpiresAt, ""), stamp(tok.RevokedAt, ""))
			}
			return t.Flush()
		},
	})
	cmd.AddCommand(createJoinTokenCmd(client))
	cmd.AddCommand(deleteByID(client, "revoke <token-id>", "Revoke a join token", "/cluster/join-tokens/", "Revoked the join token."))
	return cmd
}

type joinTokenFlags struct {
	roles   []string
	uses    int
	hours   int
	address string
}

func (f joinTokenFlags) body() map[string]any {
	body := map[string]any{}
	if len(f.roles) > 0 {
		body["roles"] = f.roles
	}
	if f.uses > 0 {
		body["max_uses"] = f.uses
	}
	if f.hours > 0 {
		body["expires_hours"] = f.hours
	}
	if f.address != "" {
		body["address"] = f.address
	}
	return body
}

func createJoinTokenCmd(client func() (*Client, error)) *cobra.Command {
	var f joinTokenFlags
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a join token. It is shown once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Token joinTokenRow `json:"token"`
				Join  string       `json:"join"`
			}
			if err := c.Do("POST", "/cluster/join-tokens", f.body(), &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, out.Join)
			fmt.Fprintf(w, "\nRoles %s, %d use, expires %s. Shown once.\n",
				strings.Join(out.Token.Roles, ","), out.Token.MaxUses, stamp(&out.Token.ExpiresAt, ""))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&f.roles, "role", nil, "control, app, or both (repeat or comma-separate). Default: both while the cluster has fewer than three hosts, then app")
	cmd.Flags().IntVar(&f.uses, "uses", 0, "How many hosts may join with it (default 1)")
	cmd.Flags().IntVar(&f.hours, "expires-hours", 0, "How many hours it lasts (default 24)")
	cmd.Flags().StringVar(&f.address, "address", "", "The installation's address the joining host reaches (default this server's advertise address)")
	return cmd
}
