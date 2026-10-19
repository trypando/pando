package reconciler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type recordingRegistryImages struct {
	mu      sync.Mutex
	deleted []string
	err     error
}

func (r *recordingRegistryImages) DeleteApp(_ context.Context, appID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, appID)
	return r.err
}

// waitingRegistryImages answers no delete until every app's has arrived.
type waitingRegistryImages struct {
	all     sync.WaitGroup
	refused string
}

func (r *waitingRegistryImages) DeleteApp(_ context.Context, appID string) error {
	r.all.Done()
	r.all.Wait()
	if appID == r.refused {
		return errors.New("refused")
	}
	return nil
}

// TestR224_RegistryCleanupsRunSideBySide asserts that a deleted app's
// registry cleanup does not wait on another's: a pass with a slow registry
// costs one timeout, not one per app. Each error comes back for its own app.
func TestR224_RegistryCleanupsRunSideBySide(t *testing.T) {
	images := &waitingRegistryImages{refused: "app_2"}
	images.all.Add(3)
	g := &GC{RegistryImages: images, Logger: zap.NewNop()}
	targets := []state.TeardownTarget{{AppID: "app_1"}, {AppID: "app_2"}, {AppID: "app_3"}}

	done := make(chan []error)
	go func() { done <- g.forgetAllRegistryImages(context.Background(), targets) }()
	select {
	case got := <-done:
		require.NoError(t, got[0])
		require.Error(t, got[1])
		require.NoError(t, got[2])
	case <-time.After(10 * time.Second):
		t.Fatal("each cleanup waited for the one before it")
	}
}

// TestR224_TeardownDeletesADeletedAppsRegistryImages asserts R-224 for the
// install registry: teardown deletes a deleted app's builds from the
// registry, a registry that refuses is reported to the caller without
// failing the files step, and an install with no registry has nothing there.
func TestR224_TeardownDeletesADeletedAppsRegistryImages(t *testing.T) {
	images := &recordingRegistryImages{}
	g := &GC{RegistryImages: images, Logger: zap.NewNop()}
	require.NoError(t, g.forgetRegistryImages(context.Background(), state.TeardownTarget{AppID: "app_1"}))
	require.Equal(t, []string{"app_1"}, images.deleted)

	images.err = errors.New("registry unreachable")
	require.NoError(t, g.forgetFiles(context.Background(), state.TeardownTarget{AppID: "app_2"}),
		"the registry is not part of the files step, so it cannot fail it")
	require.Error(t, g.forgetRegistryImages(context.Background(), state.TeardownTarget{AppID: "app_2"}))

	require.NoError(t, (&GC{}).forgetRegistryImages(context.Background(), state.TeardownTarget{AppID: "app_3"}),
		"an install with no registry has nothing there")
}

// TestR105_ARegistryFailureOnTheAuditEventSaysWhatToDo asserts that what the
// teardown's audit event says about images left behind stands on its own: the
// registry's message, its remedy, and that Pando will not try again.
func TestR105_ARegistryFailureOnTheAuditEventSaysWhatToDo(t *testing.T) {
	got := registryFailure(errs.New(errs.AdapterFailed, "The registry ghcr.io refused to delete a deleted app's image.").
		WithRemedy("Give the token delete:packages."))
	require.Contains(t, got, "refused to delete")
	require.Contains(t, got, "does not try again")
	require.Contains(t, got, "delete:packages")

	require.Contains(t, registryFailure(errors.New("dial tcp: refused")), "dial tcp: refused")
}
