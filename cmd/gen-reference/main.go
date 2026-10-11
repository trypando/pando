// gen-reference writes docs/api.md, docs/cli.md and docs/mcp.md from the
// binary that serves them.
//
// The same document `GET /api/v1/reference` returns and AI answers from, in
// Markdown, so the repository's documentation of its own API cannot describe a
// version of Pando that no longer exists. `make reference` writes them and
// `make check` fails on a diff — the same shape as the generated API types and
// the requirements index, and for the same reason: a file that has to be
// updated by remembering is a file that is wrong.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/reference"
)

func main() {
	dir := "docs"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	doc := httpapi.Reference()

	for name, body := range map[string]string{
		"api.md":    reference.APIMarkdown(doc),
		"cli.md":    reference.CLIMarkdown(doc),
		"mcp.md":    reference.MCPMarkdown(doc),
		"events.md": reference.EventsMarkdown(doc),

		"audit-formats.md": reference.AuditFormatsMarkdown(),
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // G306: documentation, world-readable on purpose.
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", path)
	}

	// The same, machine-readable. cli.json and adapters.json are written by
	// `pando reference-data`, which has the whole command tree and the kinds
	// the server registers; this package cannot import either.
	for name, v := range map[string]any{
		"mcp.json":    reference.MCP(),
		"config.json": reference.Config(),
		"errors.json": reference.Errors(),
		"events.json": reference.Events(),
	} {
		body, err := reference.JSON(v)
		if err == nil {
			path := filepath.Join(dir, name)
			err = os.WriteFile(path, body, 0o644) //nolint:gosec // G306: documentation, world-readable on purpose.
			if err == nil {
				fmt.Println("wrote", path)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
