package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// autoDeployCmd is `pando app auto-deploy`: the same endpoints the console's
// deploy settings use (R-141, R-142, R-261).
func autoDeployCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto-deploy",
		Short: "Deploy an app automatically when its branch or releases change",
		Long: "Pando checks the app's repository every few minutes and deploys what it finds.\n" +
			"Off by default. A change here is saved as a new configuration revision and takes\n" +
			"effect at the app's next deploy.",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "show <app>",
		Short: "Show the settings, the last check and the webhook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/apps/"+args[0]+"/auto-deploy", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	set := &cobra.Command{
		Use:   "set <app>",
		Short: "Turn automatic deploys on, following a branch or new releases",
		Long: "With --trigger branch (the default), each new commit on the branch deploys. Without\n" +
			"--branch, that is the branch the app was deployed from.\n\n" +
			"With --trigger release, each new release tag deploys. A release is a tag such as\n" +
			"v1.2.3, or one matching --tag-pattern, a pattern such as release-* where * matches\n" +
			"any characters.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			trigger, _ := cmd.Flags().GetString("trigger")
			switch trigger {
			case "branch", "branch_updated":
				trigger = "branch_updated"
			case "release", "release_tagged":
				trigger = "release_tagged"
			default:
				return fmt.Errorf("--trigger is branch or release, not %q", trigger)
			}
			branch, _ := cmd.Flags().GetString("branch")
			pattern, _ := cmd.Flags().GetString("tag-pattern")
			return saveAutoDeploy(cmd, c, args[0], map[string]any{
				"enabled": true, "trigger": trigger, "branch": branch, "tag_pattern": pattern,
			})
		},
	}
	set.Flags().String("trigger", "branch", "what deploys: branch (each new commit) or release (each new release tag)")
	set.Flags().String("branch", "", "the branch to follow; empty follows the branch the app was deployed from")
	set.Flags().String("tag-pattern", "", "which tags are releases, such as release-*; empty means tags like v1.2.3")
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:   "off <app>",
		Short: "Turn automatic deploys off",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			// The rest of the settings are kept, so turning it on again
			// follows what it followed before.
			var current struct {
				Settings map[string]any `json:"settings"`
			}
			if err := c.Do("GET", "/apps/"+args[0]+"/auto-deploy", nil, &current); err != nil {
				return err
			}
			if current.Settings == nil {
				current.Settings = map[string]any{}
			}
			current.Settings["enabled"] = false
			return saveAutoDeploy(cmd, c, args[0], current.Settings)
		},
	})

	secret := &cobra.Command{
		Use:   "webhook-secret <app>",
		Short: "Make the app a new webhook secret, and print it with the webhook URL",
		Long: "A webhook lets the git host tell Pando about a push or a release as it happens,\n" +
			"instead of Pando finding out at its next check. It needs Pando to be reachable from\n" +
			"the git host. Paste the URL and secret into the repository's webhook settings, with\n" +
			"content type application/json. The secret is printed this once; making a new one\n" +
			"replaces it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if remove, _ := cmd.Flags().GetBool("remove"); remove {
				if err := c.Do("DELETE", "/apps/"+args[0]+"/auto-deploy/webhook-secret", nil, nil); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Removed. The webhook no longer starts a check; Pando still checks every few minutes.")
				return nil
			}
			var out struct {
				URL    string `json:"webhook_url"`
				Secret string `json:"webhook_secret"`
			}
			if err := c.Do("POST", "/apps/"+args[0]+"/auto-deploy/webhook-secret", nil, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Webhook URL:    %s\nWebhook secret: %s\n\nThe secret is not shown again.\n", out.URL, out.Secret)
			return nil
		},
	}
	secret.Flags().Bool("remove", false, "remove the secret instead, which turns the webhook off")
	cmd.AddCommand(secret)
	return cmd
}

func saveAutoDeploy(cmd *cobra.Command, c *Client, appID string, settings map[string]any) error {
	var out struct {
		Changed  bool `json:"changed"`
		Revision int  `json:"revision"`
	}
	if err := c.Do("PUT", "/apps/"+appID+"/auto-deploy", settings, &out); err != nil {
		return err
	}
	if !out.Changed {
		fmt.Fprintln(cmd.OutOrStdout(), "Nothing to change.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Saved as revision %d. It takes effect at the app's next deploy.\n", out.Revision)
	return nil
}
