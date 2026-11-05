package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// instructionsCmd reads and sets the instructions for agents (R-424): what
// Pando's MCP server sends every agent when it connects.
func instructionsCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instructions",
		Short: "Read and set the instructions every agent is given",
		Long: "Agents connecting over MCP are given Pando's guidance for building apps here, then the\n" +
			"company's own instructions, which win where the two differ.",
	}

	var companyOnly bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the instructions agents are given",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var d struct {
				Text    string `json:"text"`
				Company string `json:"company"`
			}
			if err := c.Do("GET", "/instructions", nil, &d); err != nil {
				return err
			}
			text := d.Text
			if companyOnly {
				text = d.Company
			}
			if text != "" {
				fmt.Fprintln(cmd.OutOrStdout(), text)
			}
			return nil
		},
	}
	show.Flags().BoolVar(&companyOnly, "company", false, "print only the company's instructions")
	cmd.AddCommand(show)

	cmd.AddCommand(&cobra.Command{
		Use:   "set",
		Short: "Replace the company's instructions with text read from stdin",
		Long: "Reads the company's instructions as text on stdin, at most 32 KB, and replaces the current\n" +
			"ones. Empty input clears them. Needs install.policy.manage.\n\n" +
			"  pando instructions set < instructions.md",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			var d struct {
				Company string `json:"company"`
			}
			if err := c.Do("PUT", "/instructions", map[string]string{"company": string(raw)}, &d); err != nil {
				return err
			}
			if d.Company == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "Company instructions cleared. Agents get Pando's guidance alone.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Company instructions saved. Agents get them the next time they connect.")
			return nil
		},
	})
	return cmd
}
