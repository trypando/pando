//go:build integration

package reconciler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

// TestR194_ARestoredWorkloadIsPulledWithTheInstallRegistrysCredential asserts
// that when the reconciler re-creates a workload — on a runtime that pulls
// rather than imports — the pull carries the credential the install registry
// gives for that image, resolved for this one apply (issue #72, PR 5), and
// that an image it gives none for is pulled without one.
func TestR194_ARestoredWorkloadIsPulledWithTheInstallRegistrysCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.setObserved(api.ObservedBundle{Exists: true})

	var asked []string
	h.rec.BuiltImageAuth = func(_ context.Context, ref string) *api.RegistryAuth {
		asked = append(asked, ref)
		return &api.RegistryAuth{Registry: "registry.internal:5000", Username: "pando",
			Password: secret.New("registry-password-9")}
	}
	h.rec.Tick(context.Background())

	h.runtime.mu.Lock()
	plan := h.runtime.applied
	h.runtime.mu.Unlock()
	require.Equal(t, []string{"example/app:1"}, asked, "asked once, for the workload's own image")
	require.Len(t, plan.Workloads, 1)
	require.NotNil(t, plan.Workloads[0].PullAuth)
	require.Equal(t, "registry-password-9", plan.Workloads[0].PullAuth.Password.Reveal())

	h2 := newHarness(t, state.StateRunning)
	h2.runtime.setObserved(api.ObservedBundle{Exists: true})
	h2.rec.BuiltImageAuth = func(context.Context, string) *api.RegistryAuth { return nil }
	h2.rec.Tick(context.Background())
	h2.runtime.mu.Lock()
	defer h2.runtime.mu.Unlock()
	require.Equal(t, 1, h2.runtime.applies)
	require.Nil(t, h2.runtime.applied.Workloads[0].PullAuth, "not Pando's build, an anonymous pull")
}
