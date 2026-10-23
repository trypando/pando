package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

// The commands under `pando app spec`, one per spec revision endpoint
// (R-261): listing, reading, writing, pinning and comparing. Revisions are
// append-only (R-152), so changing a configured app is writing a new one and
// pinning it.

func specPath(appID, rest string) string {
	return "/apps/" + url.PathEscape(appID) + "/specs" + rest
}

func appSpecCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "spec", Short: "Read and write an app's spec revisions"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list <app>",
		Short: "List spec revisions, and which one is pinned",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", specPath(args[0], ""), nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "show <app> <revision>",
		Short: "Print one spec revision",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", specPath(args[0], "/"+url.PathEscape(args[1])), nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	cmd.AddCommand(specCreateCmd(client))

	cmd.AddCommand(&cobra.Command{
		Use:   "pin <app> <revision>",
		Short: "Pin a spec revision as what the app runs",
		Long: "Pins a revision: what the app converges to, and what the next deploy ships.\nThis does not deploy: `pando deploy <app>` does that.\n\n" +
			"On an app that has been deployed, pinning changes what it runs, so it needs\napp.deploy, and a revision whose deploy needs approval is refused: deploy it instead.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("POST", specPath(args[0], "/"+url.PathEscape(args[1])+"/pin"), map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pinned revision %s. Nothing is deployed yet.\n", args[1])
			fmt.Fprintf(cmd.OutOrStdout(), "Next: `pando deploy %s`.\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "diff <app> <from> <to>",
		Short: "Compare two spec revisions",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			path := specPath(args[0], "/"+url.PathEscape(args[1])+"/diff/"+url.PathEscape(args[2]))
			if err := c.Do("GET", path, nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})
	return cmd
}

func specCreateCmd(client func() (*Client, error)) *cobra.Command {
	var file string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "create <app>",
		Short: "Write a new spec revision from a JSON file",
		Long: "Writes a whole spec, read as JSON from --file or stdin, as a new revision.\n" +
			"`pando export <app>` prints the pinned spec to start from. This does not pin\n" +
			"or deploy: `pando app spec pin <app> <revision>` and `pando deploy <app>` do.\n" +
			"--dry-run checks the spec and writes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := readSpecInput(cmd, file)
			if err != nil {
				return err
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("that is not valid JSON: %w", err)
			}
			c, err := client()
			if err != nil {
				return err
			}
			path := specPath(args[0], "")
			if dryRun {
				var out map[string]any
				if err := c.Do("POST", path+"?dry_run=true", doc, &out); err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), out)
			}
			var out struct {
				Revision int `json:"revision"`
			}
			if err := c.Do("POST", path, doc, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote revision %d. It is not pinned yet.\n", out.Revision)
			fmt.Fprintf(cmd.OutOrStdout(), "Next: `pando app spec pin %s %d`, then `pando deploy %s`.\n",
				args[0], out.Revision, args[0])
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "the spec as a JSON file (default: read stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check the spec without writing it")
	return cmd
}

func readSpecInput(cmd *cobra.Command, file string) ([]byte, error) {
	if file == "" || file == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(file)
}
