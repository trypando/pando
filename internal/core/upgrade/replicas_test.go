package upgrade_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR355_AnInstallOfSeveralReplicasIsNotUpgradedInPlace asserts that an
// in-place upgrade, which replaces only the container it runs in, is refused
// with what to do instead when several replicas serve the install (R-355,
// R-256, issue #72).
func TestR355_AnInstallOfSeveralReplicasIsNotUpgradedInPlace(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t)
	h.svc.Replicas = func(context.Context) (int, error) { return 3, nil }
	p, err := h.svc.PlanFor(ctx, "0.3.2")
	require.NoError(t, err)
	require.False(t, p.Possible)
	require.Len(t, p.Reasons, 1)
	require.Contains(t, p.Reasons[0], "runs 3 Pando replicas")
	require.Contains(t, p.Reasons[0], "kubectl rollout", "the reason says how to upgrade instead")

	for name, count := range map[string]func(context.Context) (int, error){
		"one replica":         func(context.Context) (int, error) { return 1, nil },
		"the count is unread": func(context.Context) (int, error) { return 0, errors.New("database away") },
	} {
		h := newHarness(t)
		h.svc.Replicas = count
		p, err := h.svc.PlanFor(ctx, "0.3.2")
		require.NoError(t, err, name)
		require.True(t, p.Possible, "%s: %v", name, p.Reasons)
	}
}
