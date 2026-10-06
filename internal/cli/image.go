package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Apps that run a prebuilt image, and the credential a private one is pulled
// with (issue #41).

// registryCredentialFlags adds the flags that describe a registry credential.
// The secret half is never a flag: a value passed as an argument is in the
// shell history and in /proc for every process on the machine to read, so it
// is read from the terminal, without echo, or from a pipe.
func registryCredentialFlags(cmd *cobra.Command) {
	cmd.Flags().String("registry-username", "",
		"pull a private image as this user; the password or token is read from the terminal")
	cmd.Flags().String("ecr-access-key-id", "",
		"pull from AWS ECR with this access key; the secret access key is read from the terminal")
	cmd.Flags().String("ecr-region", "", "with --ecr-access-key-id, the region (default: read from the registry host)")
}

// registryCredential reads the credential the flags describe, prompting for
// its secret half. Nil when no credential flag was given.
func registryCredential(cmd *cobra.Command) (map[string]string, error) {
	user, _ := cmd.Flags().GetString("registry-username")
	keyID, _ := cmd.Flags().GetString("ecr-access-key-id")
	region, _ := cmd.Flags().GetString("ecr-region")

	switch {
	case user != "" && keyID != "":
		return nil, fmt.Errorf("give either --registry-username or --ecr-access-key-id, not both")
	case user != "":
		pass, err := promptSecret(cmd, "Password or access token for "+user+": ")
		if err != nil {
			return nil, err
		}
		return map[string]string{"kind": "basic", "username": user, "password": pass}, nil
	case keyID != "":
		secretKey, err := promptSecret(cmd, "AWS secret access key: ")
		if err != nil {
			return nil, err
		}
		c := map[string]string{"kind": "ecr", "access_key_id": keyID, "secret_access_key": secretKey}
		if region != "" {
			c["region"] = region
		}
		return c, nil
	case region != "":
		return nil, fmt.Errorf("--ecr-region needs --ecr-access-key-id")
	}
	return nil, nil
}

// nameFromImage names an app after its image: ghcr.io/acme/web:1.4 is web.
func nameFromImage(ref string) string {
	ref = strings.TrimSpace(ref)
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if slash := strings.LastIndex(ref, "/"); slash >= 0 {
		ref = ref[slash+1:]
	}
	if colon := strings.Index(ref, ":"); colon >= 0 {
		ref = ref[:colon]
	}
	if ref == "" {
		return "app"
	}
	return ref
}

func registryCredentialCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry-credential",
		Short: "Manage the credential an app's private image is pulled with",
	}

	set := &cobra.Command{
		Use:   "set <app>",
		Short: "Set the credential, replacing any the app had",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			cred, err := registryCredential(cmd)
			if err != nil {
				return err
			}
			if cred == nil {
				return fmt.Errorf("give --registry-username for a username and token, or --ecr-access-key-id for AWS ECR")
			}
			if err := c.Do("PUT", "/apps/"+args[0]+"/registry-credential", cred, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Set. The next deploy pulls the image with it.")
			return nil
		},
	}
	registryCredentialFlags(set)
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:   "show <app>",
		Short: "Say whether the app has a credential, and of which kind, without its secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/apps/"+args[0]+"/registry-credential", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "remove <app>",
		Short: "Remove the credential, so the image is pulled anonymously",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/apps/"+args[0]+"/registry-credential", nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed.")
			return nil
		},
	})
	return cmd
}
