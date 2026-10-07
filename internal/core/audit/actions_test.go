package audit

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/events"
)

// written are the shapes an action is written in across the tree: an Event's
// Action field, an action variable, a helper that takes one, a constant.
var written = []*regexp.Regexp{
	regexp.MustCompile(`Action:\s+"([a-z_.]+)"`),
	regexp.MustCompile(`\baction\s*:?=\s*"([a-z_.]+)"`),
	regexp.MustCompile(`auditApp\([^,]+,\s*[^,]+,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`principalEvent\(\w+,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`\.audit\(ctx,\s*\w+,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`auditSecurity\(ctx,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`const Action\w*\s*=\s*"([a-z_.]+)"`),
}

// TestEveryActionWrittenIsCatalogued asserts O-54's catalog keeps up with the
// code: every literal action written anywhere in internal/ or cmd/ is in
// Actions, so an audit search is offered every name the log can hold.
func TestEveryActionWrittenIsCatalogued(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	found := map[string]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range written {
				for _, m := range re.FindAllStringSubmatch(string(body), -1) {
					// "upgrade." + state is completed below; a bare prefix is
					// not an action.
					if !strings.HasSuffix(m[1], ".") {
						found[m[1]] = path
					}
				}
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.Greater(t, len(found), 50, "the scan found the tree's actions")
	for action, path := range found {
		assert.Contains(t, Actions, action, "%s writes %q, which is not in audit.Actions", path, action)
	}
}

// TestActionsCatalogIsSortedAndCoversEveryMirroredAndAIAction asserts the
// catalog is sorted without repeats, holds every action the event catalog
// mirrors, and the ai.<function> action of each administrative function.
func TestActionsCatalogIsSortedAndCoversEveryMirroredAndAIAction(t *testing.T) {
	assert.True(t, slices.IsSorted(Actions))
	assert.Len(t, slices.Compact(slices.Clone(Actions)), len(Actions))
	for _, def := range events.Catalog() {
		for _, a := range def.Actions {
			assert.Contains(t, Actions, a, "event %s mirrors %s", def.Name, a)
		}
	}
	for _, fn := range []api.AIFunction{api.AIFunctionDraftAccess, api.AIFunctionDraftPolicy,
		api.AIFunctionSearchAudit, api.AIFunctionAnswerReference} {
		assert.Contains(t, Actions, "ai."+string(fn))
	}
	for _, state := range []string{"succeeded", "failed", "rolled_back"} {
		assert.Contains(t, Actions, "upgrade."+state)
	}
}
