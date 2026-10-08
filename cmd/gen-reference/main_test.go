package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/audit/ocsf"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/reference"
)

// The docs are generated so they cannot drift from the code (R-261), and
// `TestR261_EveryRouteIsDocumented` holds the route table to the router. What
// nothing held was the rendering: a route could be in the Document, pass that
// test, and be dropped on the way to the page somebody reads.

// TestR261_EveryRouteReachesThePage asserts R-261.
func TestR261_EveryRouteReachesThePage(t *testing.T) {
	doc := httpapi.Reference()
	page := reference.APIMarkdown(doc)

	require.NotEmpty(t, doc.API.Routes)
	for _, route := range doc.API.Routes {
		require.Contains(t, page, "`"+route.Method+" "+route.Path+"`",
			"%s %s is in the reference and not on the page", route.Method, route.Path)
		require.Contains(t, page, route.Summary)
	}

	// Each group gets a heading, and no group is rendered twice.
	seen := map[string]bool{}
	for _, route := range doc.API.Routes {
		group := route.Group
		if seen[group] {
			continue
		}
		seen[group] = true
		require.Equal(t, 1, strings.Count(page, "### "+group+"\n"), group)
	}

	// Every error code, because the envelope is what a client branches on.
	require.NotEmpty(t, doc.Errors)
	for _, e := range doc.Errors {
		require.Contains(t, page, "`"+string(e.Code)+"`")
	}
}

// TestR384_EveryActionReachesTheFormatsPage asserts R-384: docs/audit-formats.md
// lists every catalogued action's OCSF class, and every native column.
func TestR384_EveryActionReachesTheFormatsPage(t *testing.T) {
	page := reference.AuditFormatsMarkdown()
	for _, r := range ocsf.Table() {
		require.Contains(t, page, fmt.Sprintf("| `%s` | %d %s | %d %s | %d |", r.Action, r.ClassUID, r.ClassName, r.ActivityID, r.ActivityName, r.TypeUID()))
	}
	require.Len(t, ocsf.Table(), len(audit.Actions))
	for _, col := range []string{"`occurred_at`", "`schema_version`", "`actor_email`"} {
		require.Contains(t, page, col)
	}
}
