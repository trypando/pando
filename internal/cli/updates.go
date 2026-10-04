package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trypando/pando/internal/core/update"
)

// updatesCmd shows whether a newer Pando is released (R-351) — the same answer
// GET /updates gives the console — and how to upgrade the server (R-352).
func updatesCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "updates",
		Short: "Show whether a newer Pando is released, what changed, and how to upgrade",
		Long: "Shows the version the server runs, the latest release on the update channel, and the\n" +
			"changelog of every version in between, with security fixes and breaking changes marked.\n" +
			"Then the command that upgrades the server. The check is host policy: disable_update_check\n" +
			"turns it off and update_channel chooses stable or prerelease (`pando policy set`).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var st update.Status
			if err := c.Do("GET", "/updates", nil, &st); err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			current := st.Current
			if st.Development {
				current = "a development build"
			}
			fmt.Fprintf(w, "Running: %s\n", current)
			if !st.Enabled {
				fmt.Fprintln(w, "The update check is off in host policy (disable_update_check), so Pando has not looked.")
				return nil
			}
			if st.Error != "" {
				fmt.Fprintln(w, st.Error)
			}
			if st.CheckedAt == nil {
				if st.Error == "" {
					fmt.Fprintln(w, "Pando has not checked for releases yet. It checks at startup and every six hours.")
				}
				return nil
			}
			fmt.Fprintf(w, "Latest:  %s (%s channel, checked %s)\n", orNone(st.Latest), st.Channel,
				st.CheckedAt.UTC().Format("2006-01-02 15:04 UTC"))
			if !st.Available {
				if !st.Development {
					fmt.Fprintln(w, "Pando is up to date.")
				}
				return nil
			}

			for _, r := range st.Releases {
				var marks []string
				if r.Security {
					marks = append(marks, "security fix")
				}
				if r.Breaking {
					marks = append(marks, "may break what the version before it did")
				}
				head := r.Version
				if !r.PublishedAt.IsZero() {
					head += ", " + r.PublishedAt.Format("2006-01-02")
				}
				if len(marks) > 0 {
					head += " — " + strings.Join(marks, "; ")
				}
				fmt.Fprintf(w, "\n## %s\n\n%s\n", head, strings.TrimSpace(r.Notes))
			}
			if st.Upgrade != nil {
				fmt.Fprintf(w, "\nTo upgrade:\n\n  %s\n\n%s\n", st.Upgrade.Command, st.Upgrade.Instructions)
			}
			return nil
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "none published"
	}
	return s
}
