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
// In the CLI rather than the server because that is where the credential is: an
// agent runs this on its own machine with its own token, and the token is what
// makes it a principal. A server-side MCP endpoint would need its own
// authentication and would be a second way in.
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
				Call: func(_ context.Context, method, path string, body, out any) error {
					if raw, ok := body.(mcp.Bytes); ok {
						return c.sendBytes(method, path, raw.ContentType, raw.Data, out)
					}
					return c.Do(method, path, body, out)
				},
			}
			// Stdout is the protocol stream. Anything else written there
			// corrupts it, which is why every diagnostic in this package goes
			// to stderr — including cobra's own error output.
			fmt.Fprintf(os.Stderr, "pando mcp: connected to %s\n", c.BaseURL)
			return s.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
