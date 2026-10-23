package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// appNotice is one entry of GET /apps/{id}/notices (R-412).
type appNotice struct {
	Key     string `json:"key"`
	Message string `json:"message"`
	Ignored bool   `json:"ignored"`
}

// appNoticesCmd is what an app's Overview asks of somebody, and ignoring one
// until it changes (R-412).
func appNoticesCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notices",
		Short: "See what an app asks of you, and ignore a notice until it changes",
	}

	var all bool
	list := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's notices",
		Long: "Lists what the app's Overview asks of somebody: a configuration not yet\n" +
			"deployed, variables with no value, a backup that was not taken, a security\n" +
			"score below this installation's minimum, the spec's warnings. Notices you\n" +
			"have ignored are left out unless --all is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Notices []appNotice `json:"notices"`
			}
			if err := c.Do("GET", noticesPath(args[0]), nil, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			shown, hidden := 0, 0
			t := table(w, "KEY", "IGNORED", "NOTICE")
			for _, n := range out.Notices {
				if n.Ignored && !all {
					hidden++
					continue
				}
				shown++
				ignored := ""
				if n.Ignored {
					ignored = "yes"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\n", n.Key, ignored, n.Message)
			}
			if shown == 0 {
				fmt.Fprintln(w, "Nothing needs your attention on this app.")
			} else if err := t.Flush(); err != nil {
				return err
			}
			if hidden > 0 {
				fmt.Fprintf(w, "%d ignored; run with --all to list them.\n", hidden)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include notices you have ignored")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use:   "ignore <app> <key>",
		Short: "Ignore a notice until what it says changes",
		Long: "Hides a notice from you until what it says changes. Only you: everyone else\n" +
			"still sees it. A deploy the notice warned about is still refused for the same\n" +
			"reason.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("PUT", noticesPath(args[0])+"/"+url.PathEscape(args[1])+"/ignore", nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Ignored %s until it changes.\n", args[1])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "restore <app> <key>",
		Short: "Show a notice you ignored again",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", noticesPath(args[0])+"/"+url.PathEscape(args[1])+"/ignore", nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored %s.\n", args[1])
			return nil
		},
	})
	return cmd
}

func noticesPath(app string) string { return "/apps/" + url.PathEscape(app) + "/notices" }
