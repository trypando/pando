package planner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

type staticInventory []planner.InventoryApp

// LiveApps ignores the filter: every app is read, which is the full
// evaluation a filtered read must agree with.
func (s staticInventory) LiveApps(context.Context, planner.InventoryFilter) ([]planner.InventoryApp, error) {
	return s, nil
}

func appNamed(t *testing.T, name, sourceURL string, anonymous bool) planner.InventoryApp {
	t.Helper()
	s := plannableSpec()
	s.Source.URL = sourceURL
	return planner.InventoryApp{
		AppID: "app_" + name, Name: name, Spec: s, AnonymousGrant: anonymous,
	}
}

func previewer(t *testing.T, inv staticInventory) *planner.Planner {
	t.Helper()
	return planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(policy.Default()),
		fixedAllocations{},
	).WithInventory(inv)
}

// TestO10_APolicyNamesTheAppsItWouldBlockBeforeItIsSaved asserts design 05 §3.
//
// O-10 resolved policy application to "report now, block on next deploy". That
// is the right behavior and an unusable experience on its own: an admin
// tightening a source allowlist is entitled to know it blocks four apps before
// they save it, not one deploy at a time.
func TestO10_APolicyNamesTheAppsItWouldBlockBeforeItIsSaved(t *testing.T) {
	inv := staticInventory{
		appNamed(t, "notes", "https://github.com/acme/notes", false),
		appNamed(t, "wiki", "https://gitlab.com/acme/wiki", false),
		appNamed(t, "blog", "https://github.com/acme/blog", false),
	}

	tightened := policy.Default()
	tightened.SourceAllowlist = []string{"github.com"}

	violations, err := previewer(t, inv).PreviewPolicy(context.Background(), tightened)
	require.NoError(t, err)

	require.Len(t, violations, 1, "only the GitLab app is blocked")
	require.Equal(t, "wiki", violations[0].AppName)
	require.NotEmpty(t, violations[0].Message, "R-105: the console shows what the deploy will say")
	require.NotEmpty(t, violations[0].Remedy)
}

// A policy that breaks nothing says so, and says it the same way.
//
// An empty list is the answer an admin most wants and must not be
// indistinguishable from a failure.
func TestAPolicyThatBlocksNothingReturnsAnEmptyList(t *testing.T) {
	inv := staticInventory{appNamed(t, "notes", "https://github.com/acme/notes", false)}

	violations, err := previewer(t, inv).PreviewPolicy(context.Background(), policy.Default())
	require.NoError(t, err)
	require.Empty(t, violations)
}

// TestR076_ForbiddingAnonymousGrantsNamesTheAppsAnyoneCanReach asserts R-076.
//
// There is no spec field to detect this in — the grant is the violation. An app
// that anyone on the internet can reach under a policy that says they may not
// is exactly the case "report now, block on next deploy" is least comfortable
// with, and the one an admin most needs named before they save.
func TestR076_ForbiddingAnonymousGrantsNamesTheAppsAnyoneCanReach(t *testing.T) {
	inv := staticInventory{
		appNamed(t, "status-page", "https://github.com/acme/status", true),
		appNamed(t, "admin", "https://github.com/acme/admin", false),
	}

	forbid := policy.Default()
	no := false
	forbid.AllowAnonymousGrants = &no

	violations, err := previewer(t, inv).PreviewPolicy(context.Background(), forbid)
	require.NoError(t, err)

	require.Len(t, violations, 1)
	require.Equal(t, "status-page", violations[0].AppName)
}

// TestR114_RaisingTheIsolationFloorNamesTheAppsThatCannotMeetIt asserts R-114.
func TestR114_RaisingTheIsolationFloorNamesTheAppsThatCannotMeetIt(t *testing.T) {
	inv := staticInventory{appNamed(t, "notes", "https://github.com/acme/notes", false)}

	raised := policy.Default()
	raised.MinRuntimeIsolation = spec.IsolationVM

	violations, err := previewer(t, inv).PreviewPolicy(context.Background(), raised)
	require.NoError(t, err)

	require.Len(t, violations, 1)
	require.Equal(t, "notes", violations[0].AppName)
}

// TestR114_ARaisedRuntimeFloorNamesImageAppsToo asserts R-114 for apps that
// are not built: an image runs on a runtime like any other app, the deploy
// refuses it below the floor, so the preview names it — with the words the
// deploy will use. It used to be skipped because it names no builder.
func TestR114_ARaisedRuntimeFloorNamesImageAppsToo(t *testing.T) {
	image := plannableSpec()
	image.Source = spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}
	image.Build = spec.Build{}
	inv := staticInventory{{AppID: "app_web", Name: "web", Spec: image}}

	raised := policy.Default()
	raised.MinRuntimeIsolation = spec.IsolationVM
	violations, err := previewer(t, inv).PreviewPolicy(context.Background(), raised)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	require.Equal(t, "web", violations[0].AppName)

	// The same refusal the deploy gives.
	p := planner.New(registry(t, capableRuntime(), capableRouting(), capableBuilder()), policy.Static(raised), fixedAllocations{})
	_, planErr := p.Check(context.Background(), image)
	require.Error(t, planErr)
	require.Equal(t, string(errs.CodeOf(planErr)), violations[0].Code)
	require.Equal(t, errs.As(planErr).Message, violations[0].Message)

	// And an image app on a runtime that meets the floor is not named.
	none, err := previewer(t, inv).PreviewPolicy(context.Background(), policy.Default())
	require.NoError(t, err)
	require.Empty(t, none)
}

// Previewing changes nothing — not the stored policy, and not any app.
//
// The whole value of the answer is that it is free to ask. A preview with a
// side effect is a save with a misleading name.
func TestPreviewSavesNothing(t *testing.T) {
	inv := staticInventory{appNamed(t, "notes", "https://gitlab.com/acme/notes", false)}
	p := previewer(t, inv)

	tightened := policy.Default()
	tightened.SourceAllowlist = []string{"github.com"}

	violations, err := p.PreviewPolicy(context.Background(), tightened)
	require.NoError(t, err)
	require.Len(t, violations, 1)

	// The planner still plans under the policy it was built with, not the one
	// it was just asked about.
	_, err = p.Check(context.Background(), inv[0].Spec)
	require.NoError(t, err, "the previewed policy was not adopted")
}

// A planner with no inventory refuses rather than reporting that nothing
// breaks. "Nothing breaks" and "nothing was checked" are opposite answers.
func TestPreviewWithoutAnInventoryRefuses(t *testing.T) {
	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(policy.Default()),
		fixedAllocations{},
	)
	_, err := p.PreviewPolicy(context.Background(), policy.Default())
	require.Error(t, err)
}

// filterInventory records the filter it was asked with.
type filterInventory struct {
	asked []planner.InventoryFilter
	apps  []planner.InventoryApp
}

func (f *filterInventory) LiveApps(_ context.Context, filter planner.InventoryFilter) ([]planner.InventoryApp, error) {
	f.asked = append(f.asked, filter)
	return f.apps, nil
}

// A policy preview asks the store only for what the candidate could block
// (issue #72), and with nothing configured that a policy could find wanting,
// asks for nothing at all.
func TestAPolicyPreviewAsksOnlyForWhatTheCandidateCouldBlock(t *testing.T) {
	ctx := context.Background()

	empty := &filterInventory{}
	none, err := planner.New(api.NewRegistry(), policy.Static(policy.Default()), fixedAllocations{}).
		WithInventory(empty).PreviewPolicy(ctx, policy.Default())
	require.NoError(t, err)
	require.Empty(t, none)
	require.Empty(t, empty.asked, "nothing to check against, so no app is read")

	weak := capableRuntime()
	inv := &filterInventory{}
	p := planner.New(registry(t, weak, capableRouting(), capableBuilder()), policy.Static(policy.Default()), fixedAllocations{}).
		WithInventory(inv)

	candidate := policy.Default()
	candidate.SourceAllowlist = []string{"github.com"}
	candidate.PublicSharing = policy.PublicSharingPasscodeOnly
	candidate.EgressBlockPrivate = true
	candidate.EgressLoosening = policy.EgressLooseningForbidden
	candidate.MinRuntimeIsolation = spec.IsolationVM
	_, err = p.PreviewPolicy(ctx, candidate)
	require.NoError(t, err)
	require.Len(t, inv.asked, 1)
	f := inv.asked[0]
	require.True(t, f.All, "an allowlist is matched against every source")
	require.True(t, f.Anonymous)
	require.True(t, f.AnonymousWithoutPasscode)
	require.True(t, f.OwnEgress, "loosening forbidden names apps with egress settings of their own")
	require.Equal(t, []string{"rt_docker"}, f.EgressRuntimes, "a runtime that cannot enforce egress rules")
	require.True(t, f.EgressRestrictsAll, "blocking private addresses restricts an app with no settings")
	require.Equal(t, map[string]spec.IsolationClass{"rt_docker": spec.IsolationContainer}, f.Runtimes)
	require.Equal(t, spec.IsolationVM, f.MinRuntime)
	require.Equal(t, map[string]spec.IsolationClass{"bld_buildkit": spec.IsolationContainer}, f.Builders)

	_, err = p.PreviewPolicy(ctx, policy.Default())
	require.NoError(t, err)
	f = inv.asked[1]
	require.False(t, f.All || f.Anonymous || f.OwnEgress || f.EgressRestrictsAll,
		"the default policy blocks no source, no public app and no loosening: %+v", f)
}
