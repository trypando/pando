//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// refusingRuntime plans like egressRuntime and cannot start anything.
type refusingRuntime struct{ egressRuntime }

func (refusingRuntime) Apply(context.Context, adapterapi.BundlePlan) (adapterapi.BundleHandle, error) {
	return adapterapi.BundleHandle{}, errs.New(errs.Internal, "The runtime could not create the app's containers.")
}

// deployAndWait starts a deploy of the given revision and waits for it to end.
func (i *install) deployAndWait(s *session, appID string, revision int) state.Deployment {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/apps/"+appID+"/deployments", map[string]any{"spec_revision": revision})
	require.Equal(i.t, http.StatusAccepted, got.Code, got.String())
	var dep state.Deployment
	got.JSON(i.t, &dep)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got = i.do(s, http.MethodGet, fmt.Sprintf("/apps/%s/deployments/%s", appID, dep.ID), nil)
		require.Equal(i.t, http.StatusOK, got.Code, got.String())
		got.JSON(i.t, &dep)
		if dep.Status == state.DeploySucceeded || dep.Status == state.DeployFailed {
			return dep
		}
		time.Sleep(100 * time.Millisecond)
	}
	i.t.Fatalf("deployment %s never finished; last status %s", dep.ID, dep.Status)
	return dep
}

func (i *install) appStateOf(s *session, appID string) string {
	i.t.Helper()
	var app struct {
		State string `json:"state"`
	}
	i.do(s, http.MethodGet, "/apps/"+appID, nil).JSON(i.t, &app)
	return app.State
}

// TestR146_ADeployThatFailsBeforeTouchingTheRuntimeLeavesTheAppAsItWas asserts
// that a deploy which stops before anything is started puts back the state it
// found. The app was moved to deploying when the deploy began, and nothing
// moved it back: it stayed deploying for good — a state the reconciler never
// looks at, so the app was neither restored nor ever allowed to reach failed.
func TestR146_ADeployThatFailsBeforeTouchingTheRuntimeLeavesTheAppAsItWas(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	withEgressAdapters(t, i)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	before := i.appStateOf(admin, appID)
	require.Equal(t, state.StateProposed, before)

	// Uploaded source that was never uploaded: fetching it fails, before any
	// build, image or container.
	s := minimalSpec()
	s["source"] = map[string]any{"type": "upload"}
	rev := i.writeSpec(admin, appID, s)

	dep := i.deployAndWait(admin, appID, rev)
	require.Equal(t, state.DeployFailed, dep.Status)
	require.Equal(t, before, i.appStateOf(admin, appID),
		"a deploy that changed nothing leaves the app's state as it found it (R-146)")
}

// TestR151_ADeployThatCannotStartTheAppLeavesItFailedNotDeploying asserts the
// other half: once the runtime has been asked to start the app and could not,
// the app is failed (design 05 §1.2) — never left deploying, where nothing
// would look at it again.
func TestR151_ADeployThatCannotStartTheAppLeavesItFailedNotDeploying(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	reg := i.Server.Registry
	require.NoError(t, reg.Register("rt_docker", refusingRuntime{}))
	require.NoError(t, reg.SetDefault(adapterapi.CategoryRuntime, "rt_docker"))
	require.NoError(t, reg.Register("rte_loopback", subdomainRouting{}))
	require.NoError(t, reg.SetDefault(adapterapi.CategoryRouting, "rte_loopback"))

	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")

	dep := i.deployAndWait(admin, appID, 1)
	require.Equal(t, state.DeployFailed, dep.Status)
	require.Equal(t, state.StateFailed, i.appStateOf(admin, appID))
}
