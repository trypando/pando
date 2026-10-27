package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

type fixedHold struct {
	hold state.Hold
	err  error
}

func (f fixedHold) Held(context.Context, string) (state.Hold, error) { return f.hold, f.err }

func (f fixedHold) HostHeld(_ context.Context, host string) (state.Hold, error) {
	h := f.hold
	h.Home = host
	return h, f.err
}

// TestR421_WhatHoldsAnAppSaysWhy asserts each hold's reason names the host
// and what Pando does about it (R-105), that a hold that cannot be read
// holds, and that nothing holds without Holds.
func TestR421_WhatHoldsAnAppSaysWhy(t *testing.T) {
	ctx := context.Background()
	r := &Reconciler{}
	require.Empty(t, r.held(ctx, "app_a"))
	require.False(t, r.localFenced())

	for hold, want := range map[state.Hold]string{
		{Promoting: true, Home: "app-2"}:  "being started on app-2",
		{HomeLost: true, Home: "app-1"}:   "app-1, is marked lost",
		{HomeFenced: true, Home: "app-1"}: "app-1, is cut off from most of the cluster",
		{Home: "app-1"}:                   "",
	} {
		r.Holds = fixedHold{hold: hold}
		if want == "" {
			require.Empty(t, r.held(ctx, "app_a"))
			continue
		}
		require.Contains(t, r.held(ctx, "app_a"), want)
	}
	r.Holds = fixedHold{err: errors.New("database away")}
	require.Contains(t, r.held(ctx, "app_a"), "could not read")
	require.Contains(t, r.heldOn(ctx, "app-1"), "could not read")
	r.Holds = fixedHold{hold: state.Hold{}}
	require.Empty(t, r.heldOn(ctx, ""), "a runtime with one place names no host")
	r.Holds = fixedHold{hold: state.Hold{HomeFenced: true}}
	require.Contains(t, r.heldOn(ctx, "app-3"), "app-3, is cut off")
	r.LocalFenced = func() bool { return true }
	require.True(t, r.localFenced())
}
