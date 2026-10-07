package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// imageRegistryCmd reads and changes the install's image registry (issue #72):
// GET, PUT and DELETE /image-registry, as the console and the MCP tools do.
func imageRegistryCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image-registry",
		Short: "See and set the registry builds are pushed to when a runtime pulls",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the registry in effect, which settings are fixed at startup, and whether a password is set",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/image-registry", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	set := &cobra.Command{
		Use:   "set",
		Short: "Change the stored registry; settings not given are unchanged",
		Long: "Changes the registry stored on the server. The password is read from the terminal\n" +
			"(--password) and never shown again. Settings made in the server's startup configuration\n" +
			"(PANDO_REGISTRY_*) win and cannot be changed here. Every replica uses the change at its\n" +
			"next push or pull.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := imageRegistryChange(cmd)
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("PUT", "/image-registry", body, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	}
	set.Flags().String("url", "", "the registry and an optional path, such as https://registry.internal:5000")
	set.Flags().String("username", "", "the username Pando pushes and pulls with (the access key ID for ecr)")
	set.Flags().Bool("password", false, "read the password (the secret access key for ecr) from the terminal")
	set.Flags().Bool("remove-password", false, "remove the stored password")
	set.Flags().String("kind", "", "basic or ecr")
	set.Flags().String("layout", "", "per_app or single")
	set.Flags().Bool("insecure", false, "allow plain HTTP to the registry")
	set.Flags().Bool("always", false, "send every build through the registry, even on a runtime that imports")
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:   "clear",
		Short: "Remove the stored registry and its password; startup settings still apply",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/image-registry", nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed. Settings in the startup configuration still apply.")
			return nil
		},
	})
	return cmd
}

// imageRegistryChange is the PUT body for the flags given: only those, so a
// setting not mentioned is left as it is.
func imageRegistryChange(cmd *cobra.Command) (map[string]any, error) {
	f := cmd.Flags()
	body := map[string]any{}
	for _, name := range []string{"url", "username", "kind", "layout"} {
		if f.Changed(name) {
			v, _ := f.GetString(name)
			body[name] = v
		}
	}
	for _, name := range []string{"insecure", "always"} {
		if f.Changed(name) {
			v, _ := f.GetBool(name)
			body[name] = v
		}
	}
	ask, _ := f.GetBool("password")
	remove, _ := f.GetBool("remove-password")
	switch {
	case ask && remove:
		return nil, fmt.Errorf("give either --password or --remove-password, not both")
	case ask:
		pass, err := promptSecret(cmd, "Registry password: ")
		if err != nil {
			return nil, err
		}
		if pass == "" {
			return nil, fmt.Errorf("no password was entered; use --remove-password to remove the stored one")
		}
		body["password"] = pass
	case remove:
		body["password"] = ""
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("give at least one setting to change, such as --url or --password")
	}
	return body, nil
}
