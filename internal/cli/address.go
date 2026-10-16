package cli

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// appAddressCmd reads or changes where an app is reached: the CLI's half of
// GET and PUT /apps/{id}/routing, so a script or an agent can do what the
// console's Change address does (R-261).
func appAddressCmd(client func() (*Client, error)) *cobra.Command {
	flags := map[string]string{
		"adapter":  "adapter_ref",
		"mode":     "mode",
		"hostname": "hostname",
		"domain":   "domain",
		"path":     "path_prefix",
	}
	cmd := &cobra.Command{
		Use:   "address <app>",
		Short: "Show or change where an app is reached",
		Long: "With no flags, shows the app's routing and address, where it moves at the next deploy\n" +
			"if a change is saved, and the routing adapters it could use with the domains each serves.\n\n" +
			"With flags, changes it. A flag left out keeps what the app has, or takes the adapter's\n" +
			"default when moving to another adapter. --domain puts the app in another of the\n" +
			"adapter's domains, such as a second Cloudflare zone: alone it makes <app>.<domain>, and\n" +
			"with --hostname docs it makes docs.<domain>.\n\n" +
			"A change is saved as a spec revision and takes effect at the next deploy, when the old\n" +
			"address stops working. Without --confirm nothing is changed and Pando says what would move.",
		Example: "  pando app address notes\n" +
			"  pando app address notes --domain trypando.ai --confirm\n" +
			"  pando app address notes --hostname docs.example.com --confirm",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			path := "/apps/" + url.PathEscape(args[0]) + "/routing"
			body := map[string]any{}
			for flag, field := range flags {
				if cmd.Flags().Changed(flag) {
					v, _ := cmd.Flags().GetString(flag)
					body[field] = v
				}
			}
			if len(body) == 0 {
				var out map[string]any
				if err := c.Do("GET", path, nil, &out); err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), out)
			}
			confirm, _ := cmd.Flags().GetBool("confirm")
			return setAddress(cmd, c, args[0], path, body, confirm)
		},
	}
	cmd.Flags().String("adapter", "", "the routing adapter, such as rte_cloudflare")
	cmd.Flags().String("mode", "", "subdomain, path or port")
	cmd.Flags().String("hostname", "", "with subdomain mode: the hostname, or with --domain a single name such as docs")
	cmd.Flags().String("domain", "", "with subdomain mode: which of the adapter's domains, such as trypando.ai")
	cmd.Flags().String("path", "", "with path mode: the path, such as /team/notes")
	cmd.Flags().Bool("confirm", false, "make the change; without it Pando only says what would move")
	return cmd
}

// setAddress sends the change and says where the app will be.
func setAddress(cmd *cobra.Command, c *Client, app, path string, body map[string]any, confirm bool) error {
	body["confirm"] = confirm
	var out map[string]any
	if err := c.Do("PUT", path, body, &out); err != nil {
		// The API asks for confirm: true; here that is --confirm.
		var apiErr *APIError
		if !confirm && errors.As(err, &apiErr) && apiErr.Details["to"] != nil {
			return fmt.Errorf("%s\nAdd --confirm to go ahead", apiErr.Message)
		}
		return err
	}
	if changed, _ := out["changed"].(bool); !changed {
		fmt.Fprintf(cmd.OutOrStdout(), "%s is already at %v. Nothing changed.\n", app, out["address"])
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Saved as revision %v. %s moves to %v at the next deploy: `pando deploy %s`.\n",
		out["revision"], app, out["address"], app)
	return nil
}
