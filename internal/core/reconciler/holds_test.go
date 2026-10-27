package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

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
// and what Pando does about it (R-105), that a hold that cannot be read is an
// error rather than a reason, and that nothing holds without Holds or on an
// install of one host.
func TestR421_WhatHoldsAnAppSaysWhy(t *testing.T) {
	ctx := context.Background()
	must := func(why string, err error) string { require.NoError(t, err); return why }
	r := &Reconciler{}
	require.Empty(t, must(r.held(ctx, "app_a")))
	require.False(t, r.localFenced())

	for hold, want := range map[state.Hold]string{
		{Promoting: true, Home: "app-2"}:  "being started on app-2",
		{HomeLost: true, Home: "app-1"}:   "app-1, is marked lost",
		{HomeFenced: true, Home: "app-1"}: "app-1, is cut off from most of the cluster",
		{Home: "app-1"}:                   "",
	} {
		r.Holds = fixedHold{hold: hold}
		if want == "" {
			require.Empty(t, must(r.held(ctx, "app_a")))
			continue
		}
		require.Contains(t, must(r.held(ctx, "app_a")), want)
	}
	r.Holds = fixedHold{err: errors.New("database away")}
	why, err := r.held(ctx, "app_a")
	require.Error(t, err)
	require.Empty(t, why, "an error is not a hold")
	_, err = r.heldOn(ctx, "app-1")
	require.Error(t, err)
	r.Holds = fixedHold{hold: state.Hold{}}
	require.Empty(t, must(r.heldOn(ctx, "")), "a runtime with one place names no host")
	r.Holds = fixedHold{hold: state.Hold{HomeFenced: true}}
	require.Contains(t, must(r.heldOn(ctx, "app-3")), "app-3, is cut off")
	r.LocalFenced = func() bool { return true }
	require.True(t, r.localFenced())
}

// TestAHostCountThatCannotBeReadStillConsultsHolds asserts that the install
// is taken for one host only when its hosts are counted: a count that fails
// is said, and holds are asked as on a cluster.
func TestAHostCountThatCannotBeReadStillConsultsHolds(t *testing.T) {
	ctx := context.Background()
	must := func(why string, err error) string { require.NoError(t, err); return why }
	core, logs := observer.New(zap.ErrorLevel)
	r := &Reconciler{Holds: fixedHold{hold: state.Hold{HomeLost: true, Home: "app-1"}}, Logger: zap.New(core)}
	r.Hosts = func(context.Context) (int, error) { return 1, nil }
	r.noteHosts(ctx)
	require.Empty(t, must(r.held(ctx, "app_a")), "one host: nothing is asked")
	r.Hosts = func(context.Context) (int, error) { return 0, errors.New("database away") }
	r.noteHosts(ctx)
	require.Contains(t, must(r.held(ctx, "app_a")), "marked lost")
	require.Equal(t, 1, logs.Len())
}
