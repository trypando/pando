package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/reference"
)

// referenceDataCmd writes docs/cli.json and docs/adapters.json, for
// `make reference`.
//
// Here rather than in cmd/gen-reference because both are read from things only
// this package has: the whole command tree, server commands included, and the
// list of adapter kinds the server registers. A generator that rebuilt either
// would be a second list to keep in step with the first. Hidden: nobody runs it
// but the Makefile.
func referenceDataCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "reference-data <dir>",
		Short:  "Write the CLI and adapter reference as JSON",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			adapters, err := reference.Adapters(allKinds())
			if err != nil {
				return err
			}
			for name, v := range map[string]any{
				"cli.json":      reference.CLI(cmd.Root()),
				"adapters.json": adapters,
			} {
				body, err := reference.JSON(v)
				if err != nil {
					return err
				}
				path := filepath.Join(args[0], name)
				if err := os.WriteFile(path, body, 0o644); err != nil { //nolint:gosec // G306: documentation, world-readable on purpose.
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "wrote", path)
			}
			return nil
		},
	}
}

// allKinds is every adapter kind this build registers, identity providers
// included.
func allKinds() []adapterapi.KindInfo {
	kinds := adapterKinds()
	identity := identityKinds()
	names := make([]string, 0, len(identity))
	for name := range identity {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		kinds = append(kinds, identity[name].Info)
	}
	return kinds
}
