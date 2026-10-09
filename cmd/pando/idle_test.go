package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/appdelete"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/subscription"
)

// fixedPolicy is a policy store that is only ever compared, never read.
type fixedPolicy struct{ name string }

func (*fixedPolicy) Load(context.Context) (corepolicy.Document, error) {
	return corepolicy.Document{}, nil
}

func (*fixedPolicy) Save(context.Context, corepolicy.Document, string) error { return nil }

// TestR394_TheProxyAndTheIdlePassShareOneActivityStore asserts R-394 and
// R-397 as wired: the recorder the proxy touches, the API's settings and the
// leader's pass all read and write the same activity, so what the proxy saw
// is what the pass counts from.
func TestR394_TheProxyAndTheIdlePassShareOneActivityStore(t *testing.T) {
	policy := &fixedPolicy{name: "host policy"}
	w := newLimitsWiring(nil, policy, zap.NewNop())

	require.NotNil(t, w.Recorder)
	assert.Same(t, w.Activity, w.Recorder.Store)
	assert.Same(t, w.Activity, w.Settings.Store)
	assert.Same(t, w.AppLimits, w.Limits.Store)
	assert.Equal(t, policy, w.Limits.Policy)

	asked := 0
	pass := w.idlePass(idlePassDeps{
		Apps: &state.Apps{}, Policy: policy, Notifier: subscription.Router{},
		TeardownNow: func() { asked++ }, Logger: zap.NewNop(),
	})
	assert.Same(t, w.Activity, pass.Store)
	assert.NotNil(t, pass.Clock, "real time, not none")

	// R-284: the pass deletes through the same service as a person, with
	// host policy and the same teardown.
	deleter, ok := pass.Deleter.(*appdelete.Service)
	require.True(t, ok)
	assert.Equal(t, policy, deleter.Policy)
	deleter.TeardownNow()
	assert.Equal(t, 1, asked)
}

// A teardown asked for twice while one waits is asked for once, and asking
// never blocks the delete that asked.
func TestSignalOnceNeverWaits(t *testing.T) {
	ch := make(chan struct{}, 1)
	signal := signalOnce(ch)
	signal()
	signal()
	assert.Len(t, ch, 1)
}
