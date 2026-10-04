package cli

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
)

// upgradeCmd upgrades the server in place (R-355 – R-360): the plan, then the
// backup passphrase or an explicit skip, then — for a breaking upgrade — the
// version typed to confirm. The same questions the console asks, in the same
// order, against the same endpoints.
func upgradeCmd(client func() (*Client, error)) *cobra.Command {
	var skipBackup bool
	cmd := &cobra.Command{
		Use:   "upgrade [version]",
		Short: "Upgrade the Pando server in place to a newer release",
		Long: "Upgrades the server to the latest release, or to the version given. Pando verifies the\n" +
			"image's signature, takes a full backup (or not, with --skip-backup), and starts a helper\n" +
			"that replaces Pando's container and puts the previous version back if the new one does\n" +
			"not start. Every app is unreachable while Pando restarts. Needs install.upgrade, and\n" +
			"upgrade_in_place on in host policy. `pando upgrade last` shows how the last one went.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			w, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()

			version := ""
			if len(args) == 1 {
				version = strings.TrimPrefix(args[0], "v")
			} else {
				var st update.Status
				if err := c.Do("GET", "/updates", nil, &st); err != nil {
					return err
				}
				if !st.Available {
					fmt.Fprintln(w, "Pando is up to date.")
					return nil
				}
				version = st.Latest
			}

			var plan upgrade.Plan
			if err := c.Do("GET", "/upgrade?version="+url.QueryEscape(version), nil, &plan); err != nil {
				return err
			}
			if !plan.Possible {
				fmt.Fprintf(w, "Pando cannot upgrade itself from %s to %s:\n", plan.Current, plan.Target)
				for _, r := range plan.Reasons {
					fmt.Fprintf(w, "\n  %s\n", r)
				}
				return fmt.Errorf("not upgraded")
			}

			fmt.Fprintf(w, "Upgrade Pando from %s to %s.\n%s\n", plan.Current, plan.Target, plan.Note)
			body := map[string]any{"version": plan.Target}

			if len(plan.Breaking) > 0 {
				for _, r := range plan.Breaking {
					fmt.Fprintf(w, "\n%s may break something that works now:\n\n%s\n", r.Version, strings.TrimSpace(r.Notes))
				}
				typed, err := prompt(cmd, fmt.Sprintf("\nType %s to upgrade anyway: ", plan.Target))
				if err != nil {
					return err
				}
				if strings.TrimPrefix(strings.TrimSpace(typed), "v") != plan.Target {
					return fmt.Errorf("not upgraded: that is not %s", plan.Target)
				}
				body["confirm_breaking"] = plan.Target
			}

			if skipBackup {
				body["skip_backup"] = true
			} else {
				fmt.Fprintln(errw, "\nPando takes a full backup first. It doesn't keep this passphrase; if you lose\n"+
					"it, nothing in the backup can be read again.")
				passphrase, err := promptSecret(cmd, "Backup passphrase: ")
				if err != nil {
					return err
				}
				body["passphrase"] = passphrase
			}

			var o upgrade.Outcome
			if err := c.Do("POST", "/upgrade", body, &o); err != nil {
				return err
			}
			fmt.Fprintf(w, "\nUpgrading to %s. Pando restarts in a moment; run `pando upgrade last` once it is back.\n", o.To)
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipBackup, "skip-backup", false, "upgrade without taking a full backup first (recorded in the audit log)")

	cmd.AddCommand(&cobra.Command{
		Use:   "last",
		Short: "Show how the most recent in-place upgrade went",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Upgrade *upgrade.Outcome `json:"upgrade"`
			}
			if err := c.Do("GET", "/upgrade/last", nil, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			o := out.Upgrade
			if o == nil {
				fmt.Fprintln(w, "Pando has not upgraded itself in place.")
				return nil
			}
			states := map[string]string{
				upgrade.StateRunning:    "under way",
				upgrade.StateSucceeded:  "succeeded",
				upgrade.StateRolledBack: "rolled back",
				upgrade.StateFailed:     "failed",
			}
			fmt.Fprintf(w, "%s to %s: %s, started %s", o.From, o.To, states[o.State], o.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
			if o.Automatic {
				fmt.Fprint(w, " by the maintenance schedule")
			}
			fmt.Fprintln(w, ".")
			if o.Reason != "" {
				fmt.Fprintln(w, o.Reason)
			}
			if o.Logs != "" {
				fmt.Fprintf(w, "\nThe last lines %s wrote:\n\n%s\n", o.To, strings.TrimSpace(o.Logs))
			}
			return nil
		},
	})
	return cmd
}
