package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// webhookRouteCmd is `pando app webhook-route`: the same endpoints the console's
// app settings use (R-432, R-261).
func webhookRouteCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook-route",
		Short: "Let verified webhook deliveries reach one path of an app",
		Long: "A route lets a delivery with no Pando credential through to one method and path,\n" +
			"once it is verified. The rest of a private app stays private. A change is saved as a\n" +
			"new configuration revision and takes effect at the app's next deploy.",
	}
	cmd.AddCommand(webhookRouteListCmd(client), webhookRouteAddCmd(client),
		webhookRouteRemoveCmd(client), webhookRouteSecretCmd(client))
	return cmd
}

func webhookRouteListCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's webhook routes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/apps/"+args[0]+"/webhook-routes", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	}
}

func webhookRouteAddCmd(client func() (*Client, error)) *cobra.Command {
	add := &cobra.Command{
		Use:   "add <app> <name> <path>",
		Short: "Add a route, and print its secret if Pando generated one",
		Long: "With --verify hmac_sha256 (the default), --header holds the hex HMAC-SHA256 of the\n" +
			"body, after any --prefix. With --verify header, --header holds the secret itself.\n\n" +
			"Without --paste-secret, Pando generates one and prints it this once.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			method, _ := cmd.Flags().GetString("method")
			kind, _ := cmd.Flags().GetString("verify")
			header, _ := cmd.Flags().GetString("header")
			prefix, _ := cmd.Flags().GetString("prefix")
			repeats, _ := cmd.Flags().GetBool("allow-repeats")
			key, err := suppliedSecret(cmd)
			if err != nil {
				return err
			}
			var out struct {
				Revision int    `json:"revision"`
				Secret   string `json:"secret"`
			}
			err = c.Do("POST", "/apps/"+args[0]+"/webhook-routes", map[string]any{
				"name": args[1], "method": strings.ToUpper(method), "path": args[2],
				"verification":  map[string]any{"kind": kind, "header": header, "prefix": prefix},
				"allow_repeats": repeats, "secret": key,
			}, &out)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved as revision %d. It takes effect at the app's next deploy.\n", out.Revision)
			if out.Secret != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Secret: %s\n\nThe secret is not shown again.\n", out.Secret)
			}
			return nil
		},
	}
	add.Flags().String("method", "POST", "POST, PUT or PATCH")
	add.Flags().String("verify", "hmac_sha256", "hmac_sha256 or header")
	add.Flags().String("header", "", "the header holding the signature or the secret")
	add.Flags().String("prefix", "", "what comes before the signature, such as sha256=")
	add.Flags().Bool("allow-repeats", false, "turn the replay check off, for a sender that repeats identical bodies")
	add.Flags().Bool("paste-secret", false, "ask for the secret the provider issued instead of generating one")
	_ = add.MarkFlagRequired("header")
	return add
}

func webhookRouteRemoveCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <app> <name>",
		Short: "Remove a route",
		Long:  "The route's secret goes at once, so deliveries are refused from now on.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/apps/"+args[0]+"/webhook-routes/"+args[1], nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed.")
			return nil
		},
	}
}

func webhookRouteSecretCmd(client func() (*Client, error)) *cobra.Command {
	set := &cobra.Command{
		Use:   "secret <app> <name>",
		Short: "Replace a route's secret, and print it if Pando generated it",
		Long:  "The old secret stops working at once.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			key, err := suppliedSecret(cmd)
			if err != nil {
				return err
			}
			var body any
			if key != "" {
				body = map[string]any{"secret": key}
			}
			var out struct {
				Secret string `json:"secret"`
			}
			if err := c.Do("POST", "/apps/"+args[0]+"/webhook-routes/"+args[1]+"/secret", body, &out); err != nil {
				return err
			}
			if out.Secret == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "Saved.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret: %s\n\nThe secret is not shown again.\n", out.Secret)
			return nil
		},
	}
	set.Flags().Bool("paste-secret", false, "ask for the secret the provider issued instead of generating one")
	return set
}

// suppliedSecret asks for a provider's secret when --paste-secret says there is
// one. Asked for, not a flag's value, which would reach shell history and the
// process list (R-194).
func suppliedSecret(cmd *cobra.Command) (string, error) {
	if paste, _ := cmd.Flags().GetBool("paste-secret"); !paste {
		return "", nil
	}
	return promptSecret(cmd, "Secret: ")
}
