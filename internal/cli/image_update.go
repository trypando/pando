package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// imageCheck is GET /apps/{id}/image-update (R-143a).
type imageCheck struct {
	Image     string `json:"image"`
	Current   string `json:"current_digest"`
	Latest    string `json:"latest_digest"`
	Available bool   `json:"update_available"`
}

func imageUpdateCmd(client func() (*Client, error)) *cobra.Command {
	var upgrade bool

	cmd := &cobra.Command{
		Use:   "image-update <app>",
		Short: "Check whether an image app's tag names a newer build, and upgrade to it",
		Long: "Asks the registry which build the app's image tag names now and compares it\n" +
			"with the build the app runs. Nothing changes unless --upgrade is given, which\n" +
			"writes a revision running that build and deploys it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			path := "/apps/" + args[0] + "/image-update"
			var check imageCheck
			if err := c.Do("GET", path, nil, &check); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if !check.Available {
				fmt.Fprintf(w, "%s is up to date.\n", check.Image)
				return nil
			}
			if !upgrade {
				fmt.Fprintf(w, "A newer build of %s is available (%s).\n", check.Image, shortDigest(check.Latest))
				fmt.Fprintf(w, "Run `pando app image-update %s --upgrade` to deploy it.\n", args[0])
				return nil
			}

			// The digest the check found, so the upgrade ships that build and no later one.
			var out waitingDeploy
			if err := c.Do("POST", path, map[string]any{"digest": check.Latest}, &out); err != nil {
				return err
			}
			if out.Status == "awaiting_approval" {
				printAwaiting(w, args[0], out)
				return nil
			}
			fmt.Fprintf(w, "Upgrading to %s.\n", shortDigest(check.Latest))
			return nil
		},
	}
	cmd.Flags().BoolVar(&upgrade, "upgrade", false, "deploy the newer build when there is one")
	return cmd
}

// shortDigest is a digest the way Docker shows one: twelve hex characters.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
