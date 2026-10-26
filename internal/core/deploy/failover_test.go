package deploy

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
)

// TestR422_ADeployTellsTheRuntimeWhetherStandbysMayGoPastTheReservation
// asserts that a deploy reads host policy allow_failover_oversubscription
// for the runtime placing the app's standby, and that any doubt — no
// planner, or a policy that cannot be read — answers false, so the standby
// is placed only where it would start. The bundle carries the spec's
// failover priority either way.
func TestR422_ADeployTellsTheRuntimeWhetherStandbysMayGoPastTheReservation(t *testing.T) {
	ctx := context.Background()
	s := &spec.AppSpec{AppID: "app_1", Availability: spec.Availability{Priority: 3}}

	doc := policy.Default()
	doc.AllowFailoverOversubscription = true
	require.True(t, (&Runner{planner: planner.New(nil, policy.Static(doc), nil)}).failoverOversubscription(ctx, s))
	require.False(t, (&Runner{planner: planner.New(nil, policy.Static(policy.Default()), nil)}).failoverOversubscription(ctx, s))
	require.False(t, (&Runner{}).failoverOversubscription(ctx, s))
	failing := &Runner{planner: planner.New(nil, policy.New(func(context.Context) (policy.Document, error) {
		return policy.Document{}, errors.New("unreadable")
	}), nil)}
	require.False(t, failing.failoverOversubscription(ctx, s))

	shape, err := BundlePlanShape(s, "img", nil, api.EgressRules{})
	require.NoError(t, err)
	require.Equal(t, 3, shape.Failover.Priority)
	require.False(t, shape.Failover.AllowOversubscription, "what the reconciler sends")
}
