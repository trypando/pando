package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type recordingRegistryImages struct {
	deleted []string
	err     error
}

func (r *recordingRegistryImages) DeleteApp(_ context.Context, appID string) error {
	r.deleted = append(r.deleted, appID)
	return r.err
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
