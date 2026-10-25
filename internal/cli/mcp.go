package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/trypando/pando/internal/mcp"
)

// MCPCommand runs the MCP server over stdio (R-262, design 04 §3).
//
// Pando also serves the same server itself at /api/v1/mcp (R-413), which a
// client adds by address and signs in to with OAuth. This is for a client
// that runs a program instead: it uses the token `pando login` stored, on the
// agent's own machine.
func MCPCommand() *cobra.Command {
	var server string

	return &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server over stdio, for agents",
		Long: "Exposes Pando's API as MCP tools, using the token from `pando login`.\n\n" +
			"An agent is a principal like any other: nothing here bypasses authorization, and\n" +
			"every action lands in the audit log under the token's owner. The most dangerous\n" +
			"actions — running commands in apps, reading secret values, changing who has access —\n" +
			"are refused by host policy for tokens by default, and are not offered as tools.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := New(server)
			if err != nil {
				return err
			}

			s := &mcp.Server{
				Version: Version,
				Call:    mcpCall(c),
			}
			// Stdout is the protocol stream. Anything else written there
			// corrupts it, which is why every diagnostic in this package goes
			// to stderr — including cobra's own error output.
			fmt.Fprintf(os.Stderr, "pando mcp: connected to %s\n", c.BaseURL)
			return s.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// mcpCall is the MCP server's way to the API: the CLI's own client, so an agent
// reaches nothing a person at the terminal could not.
func mcpCall(c *Client) func(ctx context.Context, method, path string, body, out any) error {
	return func(_ context.Context, method, path string, body, out any) error {
		if raw, ok := body.(mcp.Bytes); ok {
			return c.sendBytes(method, path, raw.ContentType, raw.Data, out)
		}
		return c.Do(method, path, body, out)
	}
}
