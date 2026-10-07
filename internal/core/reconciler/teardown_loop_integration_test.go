//go:build integration

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// signallingRuntime reports each Destroy on a channel.
type signallingRuntime struct {
	api.RuntimeAdapter
	destroyed chan string
}

func (r *signallingRuntime) Destroy(_ context.Context, ref api.BundleRef, _ api.DestroyOptions) error {
	r.destroyed <- ref.BundleID
	return nil
}

// stuckScores is a security pass that does not finish until released.
type stuckScores struct {
	entered chan struct{}
	release chan struct{}
}

func (s *stuckScores) Configured() (string, bool) { return "scn", true }
func (s *stuckScores) Place(ctx context.Context, _ map[string]security.Scores) (map[string]security.Placed, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return map[string]security.Placed{}, nil
}

// TestR224_ADeleteIsTornDownWhileTheSlowPassRuns asserts that teardown is
// served on a loop of its own (issue #72): a delete is acted on while the
// GC's slow pass — here, a security pass that does not finish — is still
// running, rather than after it, which over thousands of apps was an hour.
func TestR224_ADeleteIsTornDownWhileTheSlowPassRuns(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, _ := statetest.Connect(t)
	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID
	apps := state.NewApps(db)

	app, err := apps.Create(ctx, "gone-"+id.New(id.App)[4:12], id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID,
		Source:    spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
		Build:     spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true}},
		Runtime:   spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
	}, spec.OriginManual, owner)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))

	rt := &signallingRuntime{destroyed: make(chan string, 1)}
	scores := &stuckScores{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(scores.release)
	teardownNow := make(chan struct{}, 1)
	gc := &GC{
		Apps:          apps,
		Logger:        zap.NewNop(),
		Registry:      fakeRegistry{runtime: map[string]api.RuntimeAdapter{"rt_fake": rt}},
		TeardownNow:   teardownNow,
		TeardownEvery: time.Hour,
		Security:      scores,
		SecurityState: newFakeState(state.SecurityState{AppID: "app_other", PinnedSpecID: "spec_1"}),
		PolicyStore:   fakePolicy{doc: policy.Document{MinSecurityScore: 70}},
	}
	go gc.Run(ctx)

	select {
	case <-scores.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow pass never started")
	}

	// The slow pass is stuck. The app is deleted, and asks for its teardown.
	require.NoError(t, apps.Archive(ctx, app.ID))
	teardownNow <- struct{}{}

	select {
	case got := <-rt.destroyed:
		require.Equal(t, app.ID, got)
	case <-time.After(10 * time.Second):
		t.Fatal("the delete waited for the slow pass")
	}
}
