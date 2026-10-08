//go:build integration

package planner_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// countingInventory is the store, counting the apps each preview reads.
type countingInventory struct {
	apps *state.Apps
	read int
}

func (c *countingInventory) LiveApps(ctx context.Context, f planner.InventoryFilter) ([]planner.InventoryApp, error) {
	got, err := c.apps.LiveApps(ctx, f)
	c.read += len(got)
	return got, err
}

// TestR092_APolicyPreviewReadsOnlyTheAppsItCouldBlockAndSaysTheSame asserts
// design 05 §3's preview after issue #72: reading only the apps a candidate
// policy could block — chosen in the query, from what the candidate sets —
// reports exactly what checking every live app reports, for the source
// allowlist (R-092), public sharing (R-076), egress (R-183, R-186) and the
// isolation floors (R-024, R-114), alone and together. And a policy that can
// block nothing reads nothing.
func TestR092_APolicyPreviewReadsOnlyTheAppsItCouldBlockAndSaysTheSame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))
	digest, err := hash.New(secret.New("correct-password"))
	require.NoError(t, err)
	alice, err := users.Create(ctx, state.LocalAdapterID, "alice", "alice@corp.com", "alice", digest, false)
	require.NoError(t, err)
	apps := state.NewApps(db)

	// Two runtimes and two builders, unlike each other in every way a policy
	// can tell apart.
	weak := capableRuntime() // container, cannot enforce egress
	strong := capableRuntime()
	strong.caps.IsolationClass = spec.IsolationVM
	strong.caps.SupportsEgressRestriction = true
	sandbox := capableBuilder()
	sandbox.caps.IsolationClass = spec.IsolationSandboxed
	reg := registry(t, weak, capableRouting(), capableBuilder())
	require.NoError(t, reg.Register("rt_vm", strong))
	require.NoError(t, reg.Register("bld_sandbox", sandbox))

	sources := []spec.Source{
		{Type: spec.SourceGit, URL: "https://github.com/acme/notes", Ref: "main"},
		{Type: spec.SourceGit, URL: "https://gitlab.com/acme/wiki", Ref: "main"},
		{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"},
		{Type: spec.SourceUpload},
	}
	off := false
	egresses := []spec.Egress{
		{},
		{Mode: spec.EgressAllowAll},
		{Mode: spec.EgressAllowlist, List: []string{"api.example.com"}},
		{Mode: spec.EgressInherit, Add: []string{"api.example.com"}},
		{Mode: spec.EgressInherit, Remove: []string{"bad.example.com"}},
		{Mode: spec.EgressInherit, BlockPrivate: &off},
	}
	builds := []spec.Build{
		{Strategy: spec.BuildDockerfile, AdapterRef: "bld_buildkit"},
		{Strategy: spec.BuildDockerfile, AdapterRef: "bld_sandbox"},
		{Strategy: spec.BuildDockerfile, AdapterRef: "bld_buildkit", IsolationFloor: spec.IsolationSandboxed},
		{},
	}

	const n = 48
	for i := range n {
		s := plannableSpec()
		s.Source = sources[i%len(sources)]
		s.Egress = egresses[i%len(egresses)]
		s.Build = builds[(i/2)%len(builds)]
		s.Runtime = spec.RuntimeRef{AdapterRef: []string{"rt_docker", "rt_vm"}[(i/3)%2], IsolationFloor: spec.IsolationContainer}
		if i%7 == 0 {
			s.Runtime.IsolationFloor = spec.IsolationSandboxed
		}
		s.Routing.Port = 9000 + i
		app, err := apps.Create(ctx, fmt.Sprintf("app %02d", i), fmt.Sprintf("app-%02d", i), alice.ID, alice.ID, s.Source)
		require.NoError(t, err)
		s.AppID = app.ID
		rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginManual, alice.ID)
		require.NoError(t, err)
		require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, alice.ID))
		switch i % 5 {
		case 1, 3:
			_, err = db.Exec(ctx, `INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, created_by)
				VALUES ($1, $2, 'data', 'anonymous', NULL, $3)`, id.New(id.Grant), app.ID, alice.ID)
			require.NoError(t, err)
			if i%5 == 3 {
				_, err = db.Exec(ctx, `UPDATE grants SET passcode_hash = 'x' WHERE app_id = $1 AND principal_kind = 'anonymous'`, app.ID)
				require.NoError(t, err)
			}
		}
	}

	// Every live app, read the way the preview read them before issue #72.
	all, err := apps.LiveApps(ctx, planner.InventoryFilter{All: true})
	require.NoError(t, err)
	require.Len(t, all, n)

	with := func(edit func(*policy.Document)) policy.Document {
		doc := policy.Default()
		edit(&doc)
		return doc
	}
	candidates := map[string]policy.Document{
		"default":       policy.Default(),
		"allowlist":     with(func(d *policy.Document) { d.SourceAllowlist = []string{"github.com", "upload"} }),
		"no sharing":    with(func(d *policy.Document) { d.PublicSharing = policy.PublicSharingNone }),
		"passcode only": with(func(d *policy.Document) { d.PublicSharing = policy.PublicSharingPasscodeOnly }),
		"egress allow":  with(func(d *policy.Document) { d.EgressMode, d.EgressList = spec.EgressAllowlist, []string{"x.example.com"} }),
		"egress deny": with(func(d *policy.Document) {
			d.EgressMode, d.EgressList = spec.EgressDenylist, []string{"bad.example.com"}
		}),
		"block private": with(func(d *policy.Document) { d.EgressBlockPrivate = true }),
		"no loosening":  with(func(d *policy.Document) { d.EgressLoosening = policy.EgressLooseningForbidden }),
		"deny, no loosen": with(func(d *policy.Document) {
			d.EgressMode, d.EgressList, d.EgressLoosening = spec.EgressDenylist, []string{"bad.example.com"}, policy.EgressLooseningForbidden
		}),
		"vm runtime":      with(func(d *policy.Document) { d.MinRuntimeIsolation = spec.IsolationVM }),
		"sandboxed build": with(func(d *policy.Document) { d.MinBuildIsolation = spec.IsolationSandboxed }),
		"everything": with(func(d *policy.Document) {
			d.SourceAllowlist = []string{"github.com"}
			d.PublicSharing = policy.PublicSharingNone
			d.EgressMode, d.EgressList = spec.EgressAllowlist, []string{"x.example.com"}
			d.EgressLoosening = policy.EgressLooseningForbidden
			d.MinRuntimeIsolation, d.MinBuildIsolation = spec.IsolationVM, spec.IsolationSandboxed
		}),
	}

	codes := map[string]bool{}
	for name, doc := range candidates {
		full, err := planner.New(reg, policy.Static(policy.Default()), fixedAllocations{}).
			WithInventory(staticInventory(all)).PreviewPolicy(ctx, doc)
		require.NoError(t, err, name)

		inv := &countingInventory{apps: apps}
		narrowed, err := planner.New(reg, policy.Static(policy.Default()), fixedAllocations{}).
			WithInventory(inv).PreviewPolicy(ctx, doc)
		require.NoError(t, err, name)

		require.Equal(t, full, narrowed, "%s: the narrowed preview says exactly what the full one says", name)
		require.LessOrEqual(t, inv.read, n, name)
		switch name {
		case "default", "no sharing", "passcode only", "vm runtime", "sandboxed build":
			require.Less(t, inv.read, n, "%s: a policy that cannot block every app does not read every app", name)
		}
		for _, v := range full {
			codes[v.Code] = true
		}
		t.Logf("%s: %d violations, %d of %d apps read", name, len(full), inv.read, n)
	}
	require.GreaterOrEqual(t, len(codes), 4, "the fixture exercises several kinds of block: %v", codes)
}
