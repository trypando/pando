package upgrade_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/upgrade"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type failingRuntime struct{ *fakeRuntime }

func (f *failingRuntime) StartHelper(context.Context, api.HelperSpec) (string, error) {
	return "", errors.New("daemon refused")
}

// Each way an upgrade can fail before Pando stops leaves Pando running and
// says what happened, and nothing is half-started.
func TestR359_AnUpgradeThatCannotStartLeavesPandoRunningAndSaysWhy(t *testing.T) {
	ctx := context.Background()

	t.Run("the backup fails", func(t *testing.T) {
		h := newHarness(t)
		h.svc.Backup = func(context.Context, authz.Principal, secret.Value) (string, error) {
			return "", errs.New(errs.AdapterFailed, "The backup destination is full.")
		}
		_, err := h.svc.Start(ctx, admin, upgrade.Request{Version: "0.3.2", Passphrase: secret.New("a long passphrase")})
		require.ErrorContains(t, err, "The backup taken before the upgrade failed")
		require.ErrorContains(t, err, "destination is full")
		require.Empty(t, h.rt.helpers)
		require.Equal(t, []string{"upgrade.refused"}, h.actions())
	})

	t.Run("the helper does not start", func(t *testing.T) {
		h := newHarness(t)
		rt := &failingRuntime{fakeRuntime: h.rt}
		h.svc.Runtime = func(context.Context) (api.RuntimeAdapter, error) { return rt, nil }
		o, err := h.svc.Start(ctx, admin, upgrade.Request{Version: "0.3.2", SkipBackup: true})
		require.Error(t, err)
		require.Equal(t, upgrade.StateFailed, o.State)
		require.Contains(t, o.Reason, "nothing was changed")
		require.Contains(t, h.actions(), "upgrade.failed")
		last, _ := h.svc.Last(ctx)
		require.True(t, last.Recorded, "recorded now: no later Pando will find it running")
	})

	t.Run("the runtime cannot be reached", func(t *testing.T) {
		h := newHarness(t)
		h.svc.Runtime = func(context.Context) (api.RuntimeAdapter, error) { return nil, errors.New("socket gone") }
		p, err := h.svc.PlanFor(ctx, "0.3.2")
		require.NoError(t, err)
		require.False(t, p.Possible)
		require.Contains(t, p.Reasons[len(p.Reasons)-1], "could not reach its runtime")
	})

	t.Run("Pando cannot say which container it is", func(t *testing.T) {
		h := newHarness(t)
		h.rt.selfErr = errs.New(errs.ValidInvalid, "Pando is not running in a container this Docker daemon manages.")
		p, _ := h.svc.PlanFor(ctx, "0.3.2")
		require.Contains(t, p.Reasons[len(p.Reasons)-1], "not running in a container")
	})

	t.Run("policy cannot be read", func(t *testing.T) {
		h := newHarness(t)
		h.svc.Policy = func(context.Context) (policy.Document, error) { return policy.Document{}, errors.New("db down") }
		_, err := h.svc.PlanFor(ctx, "0.3.2")
		require.Error(t, err)
		h.svc.Tick(ctx) // the loop logs and carries on
	})
}

// The copy is kept while a version other than the one upgraded to is running,
// and kept for the next pass when dropping it fails.
func TestR359_TheCopyIsKeptUnlessTheUpgradedVersionIsRunning(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.updates.st.Available = false
	require.NoError(t, upgrade.WriteOutcome(upgrade.OutcomePath(h.svc.WorkDir), upgrade.Attempt{
		ID: "upg_1", From: "0.3.0", To: "0.3.2", State: upgrade.StateSucceeded, Recorded: true,
		SnapshotAt: h.clock.Now(), FinishedAt: h.clock.Now(),
	}))
	h.clock.Advance(48 * time.Hour)
	h.svc.Tick(ctx) // running 0.3.1, not 0.3.2
	require.Zero(t, h.dropped)

	h.svc.DropSnapshot = func(context.Context) error { return errors.New("in use") }
	h.svc.Version = "0.3.2"
	h.svc.Tick(ctx)
	o, _ := h.svc.Last(ctx)
	require.False(t, o.SnapshotGone)
}
