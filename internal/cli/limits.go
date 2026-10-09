package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// Idle apps and app limits (issue #131): the CLI's half of
// /apps/{id}/idle, /users/{id}/app-limit and /groups/{id}/app-limit, so a
// script or an agent can do what the console does (R-261).

// The idle flags of pando app idle.
const (
	flagStopDays   = "stop-days"
	flagDeleteDays = "delete-days"
)

// appIdleCmd reads or changes an app's idle settings (R-393 – R-397).
func appIdleCmd(client func() (*Client, error)) *cobra.Command {
	var stop, del string
	cmd := &cobra.Command{
		Use:   "idle <app>",
		Short: "Show or set when Pando stops or deletes an app nobody uses",
		Long: "With no flags, shows the app's idle settings: its own, the installation's, the ones in\n" +
			"force, when it was last used, and when Pando would stop or delete it.\n\n" +
			"--stop-days and --delete-days take a number of days, never, or default for the\n" +
			"installation's setting. A flag left out keeps what the app has. Both count from the\n" +
			"app's last use, deploy or start, so deleting has to come later than stopping.\n" +
			"These are app settings, not spec: nothing is deployed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			path := "/apps/" + url.PathEscape(args[0]) + "/idle"
			var report map[string]any
			if !cmd.Flags().Changed(flagStopDays) && !cmd.Flags().Changed(flagDeleteDays) {
				if err := c.Do("GET", path, nil, &report); err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), report)
			}

			// A PUT replaces both, so the one not given is what the app has.
			var current struct {
				StopDays   *int `json:"stop_days"`
				DeleteDays *int `json:"delete_days"`
			}
			if err := c.Do("GET", path, nil, &current); err != nil {
				return err
			}
			body := map[string]any{"stop_days": current.StopDays, "delete_days": current.DeleteDays}
			for flag, field := range map[string]string{flagStopDays: "stop_days", flagDeleteDays: "delete_days"} {
				if !cmd.Flags().Changed(flag) {
					continue
				}
				v, _ := cmd.Flags().GetString(flag)
				days, err := idleDays(flag, v)
				if err != nil {
					return err
				}
				body[field] = days
			}
			if err := c.Do("PUT", path, body, &report); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), report)
		},
	}
	cmd.Flags().StringVar(&stop, flagStopDays, "", "days without use before Pando stops the app: a number, never, or default")
	cmd.Flags().StringVar(&del, flagDeleteDays, "", "days without use before Pando deletes the app: a number, never, or default")
	return cmd
}

// idleDays reads a flag's value: nil for the installation's, 0 for never.
func idleDays(flag, v string) (*int, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "default":
		return nil, nil
	case "never":
		zero := 0
		return &zero, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("--%s is %q; use a number of days such as 30, never, or default for the installation's setting", flag, v)
	}
	return &n, nil
}

// appLimitCmd reads or sets the app limit on an account or a group (R-244).
// what is "user" or "group".
func appLimitCmd(client func() (*Client, error), what string) *cobra.Command {
	var remove bool
	short := "Show how many apps an account may own and where that comes from, or set the account's own limit"
	long := "With only the account, shows the limit in force, where it comes from (the account, a\n" +
		"group, or host policy) and how many apps the account owns.\n\n" +
		"With a number, sets the account's own limit, which applies whatever its groups say.\n" +
		"0 is unlimited. --clear removes it, so its groups' or the installation's applies."
	if what == "group" {
		short = "Show or set how many apps each person in a group may own"
		long = "With only the group, shows its limit. With a number, sets it; 0 is unlimited.\n" +
			"--clear removes it. Somebody in several groups gets the most generous, and a limit on\n" +
			"their own account replaces every group's."
	}
	cmd := &cobra.Command{
		Use:   "app-limit <" + what + "-id> [apps]",
		Short: short,
		Long:  long,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			path := "/" + what + "s/" + url.PathEscape(args[0]) + "/app-limit"
			var out map[string]any
			switch {
			case remove && len(args) == 2:
				return fmt.Errorf("give a number or --clear, not both")
			case remove:
				err = c.Do("PUT", path, map[string]any{"max_apps": nil}, &out)
			case len(args) == 2:
				n, convErr := strconv.Atoi(args[1])
				if convErr != nil || n < 0 {
					return fmt.Errorf("%q is not a number of apps; use 1 or more, 0 for unlimited, or --clear", args[1])
				}
				err = c.Do("PUT", path, map[string]any{"max_apps": n}, &out)
			default:
				err = c.Do("GET", path, nil, &out)
			}
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	}
	cmd.Flags().BoolVar(&remove, "clear", false, "remove the "+what+"'s own limit")
	return cmd
}
