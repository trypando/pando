//go:build integration

package deploy

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// singleHostDocker behaves as single-host Docker does where it matters here:
// a loaded build moves pando/<app>:latest, and its content-addressed ID is
// what ImportImage returns.
type singleHostDocker struct {
	api.RuntimeAdapter
	mu      sync.Mutex
	loads   int
	tags    map[string]string
	applied []string
}

func (d *singleHostDocker) Category() api.Category { return api.CategoryRuntime }

func (d *singleHostDocker) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{
		SupportsPrivateNetwork: true,
		ImageDelivery:          []api.ImageDelivery{api.ImageDeliveryImport},
	}, nil
}

func (d *singleHostDocker) ImportImage(_ context.Context, r io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, r)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loads++
	id := fmt.Sprintf("sha256:%064d", d.loads)
	d.tags["pando/app:latest"] = id
	return id, nil
}

func (d *singleHostDocker) Apply(_ context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applied = append(d.applied, p.Workloads[0].Image)
	return api.BundleHandle{}, nil
}

func (d *singleHostDocker) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.applied) == 0 {
		return api.ObservedBundle{}, nil
	}
	return api.ObservedBundle{Workloads: []api.ObservedWorkload{{
		Name: "web", Running: true, ImageDigest: d.applied[len(d.applied)-1],
	}}}, nil
}

type taggingBuilder struct {
	api.BuilderAdapter
	tags []string
}

func (b *taggingBuilder) Category() api.Category { return api.CategoryBuilder }
func (b *taggingBuilder) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return api.BuilderCapabilities{Strategies: []api.BuildStrategy{spec.BuildDockerfile}}, nil
}
func (b *taggingBuilder) Build(_ context.Context, req api.BuildRequest) (api.BuildResult, error) {
	b.tags = append(b.tags, req.Tag)
	_, _ = io.WriteString(req.ImageSink, "image-bytes")
	return api.BuildResult{ImageRef: "pando/app:" + req.Tag}, nil
}

type routeAnything struct{ api.RoutingAdapter }

func (routeAnything) Category() api.Category { return api.CategoryRouting }
func (routeAnything) Ensure(context.Context, api.RouteRequest) (api.RouteHandle, error) {
	return api.RouteHandle{}, nil
}

type noSecrets struct{}

func (noSecrets) Resolve(context.Context, string) (map[string]secret.Value, error) { return nil, nil }
func (noSecrets) Versions(context.Context, string) (map[string]int, error)         { return nil, nil }

// TestR146_ARefusedBuildIsNotWhatTheReconcilerRestores asserts R-146 and the
// security scan's matching contract (R-314) on single-host Docker: a build
// that succeeded and was then refused leaves the running app alone, and that
// includes what the reconciler restores afterwards. The refused build has been
// loaded and moved pando/<app>:latest; the image the reconciler would recreate
// a removed workload from is still the one the last successful deploy ran,
// because what a deployment records is the image's ID and not a tag.
func TestR146_ARefusedBuildIsNotWhatTheReconcilerRestores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, commit := gitRepo(t)

	db, _ := statetest.Connect(t)
	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID
	apps := state.NewApps(db)
	deploys := state.NewDeployments(db)
	src := spec.Source{Type: spec.SourceGit, URL: repo, Ref: "master", Commit: commit}
	app, err := apps.Create(ctx, "refused", "refused-build", owner, owner, src)
	require.NoError(t, err)

	docker := &singleHostDocker{tags: map[string]string{}}
	builder := &taggingBuilder{}
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("rt", docker))
	require.NoError(t, reg.Register("bld", builder))
	require.NoError(t, reg.Register("rte", routeAnything{}))

	sec := &fakeSecurity{standing: security.Standing{Verdict: security.VerdictOK}}
	r := NewRunner(reg, nil, apps, deploys, noSecrets{}, nil, NewLogStore(), nil, "http://pando:8080").
		WithSources(source.Sources{WorkDir: t.TempDir()}).
		WithSecurity(sec)

	s := &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
		Build:     spec.Build{Strategy: spec.BuildDockerfile, AdapterRef: "bld"},
		Runtime:   spec.RuntimeRef{AdapterRef: "rt"},
		Routing:   spec.Routing{AdapterRef: "rte", Mode: spec.RoutingPort, Port: 8080},
		Workloads: []spec.Workload{{Name: "web", Primary: true, Exposed: true}},
	}
	deployOnce := func() (state.Deployment, error) {
		rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginManual, owner)
		require.NoError(t, err)
		dep, err := deploys.Create(ctx, app.ID, rev.ID, state.TriggerManual, owner)
		require.NoError(t, err)
		return dep, r.Run(ctx, dep, rev)
	}

	// The first deploy succeeds and records the image it ran.
	good, err := deployOnce()
	require.NoError(t, err)
	ran := docker.applied[0]
	require.Equal(t, fmt.Sprintf("sha256:%064d", 1), ran)

	// The second builds, is loaded, and is refused by the scan.
	sec.standing = security.Standing{Verdict: security.VerdictInsecure, Threshold: 60}
	refused, err := deployOnce()
	require.Equal(t, errs.PlanSecurityBelowThreshold, errs.CodeOf(err))
	require.Len(t, docker.applied, 1, "the refused build was never applied")
	refusedID := fmt.Sprintf("sha256:%064d", 2)
	require.Equal(t, refusedID, docker.tags["pando/app:latest"],
		"the refused build is on the host, under the tag a build used to be recorded by")

	// Each build was named by its own deployment, never by a moving tag.
	require.Equal(t, []string{good.ID, refused.ID}, builder.tags)

	// What the reconciler restores a removed workload from.
	require.NoError(t, apps.SetState(ctx, app.ID, state.StateRunning))
	claimed, err := state.NewReconciles(db).Lease().Claim(ctx, time.Now(), time.Now().Add(time.Minute), 10, time.Minute)
	require.NoError(t, err)
	var restored *state.Reconcilable
	for i := range claimed {
		if claimed[i].ID == app.ID {
			restored = &claimed[i]
		}
	}
	require.NotNil(t, restored)
	require.Equal(t, ran, restored.ImageRef, "the reconciler restores what the last successful deploy ran")
	plan, err := BundlePlanShape(s, restored.ImageRef, nil, api.EgressRules{})
	require.NoError(t, err)
	require.Equal(t, ran, plan.Workloads[0].Image)
	require.NotEqual(t, refusedID, plan.Workloads[0].Image, "and never the refused build")
}
