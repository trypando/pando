package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/reference"
)

// TestR261_TheCLIReferenceIsTheWholeTree asserts R-261 for docs/cli.json: it
// is written from the command tree `pando` runs, server commands included, and
// leaves out the hidden plumbing — itself among it.
func TestR261_TheCLIReferenceIsTheWholeTree(t *testing.T) {
	dir := t.TempDir()
	root := rootCmd()
	root.SetArgs([]string{"reference-data", dir})
	root.SetOut(io.Discard)
	require.NoError(t, root.Execute())

	body, err := os.ReadFile(filepath.Join(dir, "cli.json"))
	require.NoError(t, err)
	// Read by users here, in docs/cli.md and in --help: no requirement or
	// decision IDs anywhere in a command's or a flag's text.
	require.NotRegexp(t, traceID, string(body))
	var cli reference.CLIData
	require.NoError(t, json.Unmarshal(body, &cli))
	paths := map[string]bool{}
	for _, c := range cli.Commands {
		paths[c.Path] = true
	}
	require.Equal(t, "pando", cli.Commands[0].Path)
	for _, want := range []string{"pando serve", "pando admin setup-token", "pando app add", "pando login"} {
		require.True(t, paths[want], want)
	}
	for _, hidden := range []string{"pando reference-data", "pando egress-gateway", "pando upgrade-helper", "pando help"} {
		require.False(t, paths[hidden], hidden)
	}

	body, err = os.ReadFile(filepath.Join(dir, "adapters.json"))
	require.NoError(t, err)
	var adapters reference.AdaptersData
	require.NoError(t, json.Unmarshal(body, &adapters))
	kinds := 0
	for _, c := range adapters.Categories {
		kinds += len(c.Kinds)
	}
	require.Equal(t, len(allKinds()), kinds)
}
