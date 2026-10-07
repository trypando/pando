package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
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
// install registry: the teardown step that forgets a deleted app's build
// cache and upload also deletes its builds from the registry, and a registry
// that cannot be reached leaves the app for the next pass rather than marking
// it done.
func TestR224_TeardownDeletesADeletedAppsRegistryImages(t *testing.T) {
	images := &recordingRegistryImages{}
	g := &GC{RegistryImages: images}
	require.NoError(t, g.forgetFiles(context.Background(), state.TeardownTarget{AppID: "app_1"}))
	require.Equal(t, []string{"app_1"}, images.deleted)

	images.err = errors.New("registry unreachable")
	require.Error(t, g.forgetFiles(context.Background(), state.TeardownTarget{AppID: "app_2"}))

	require.NoError(t, (&GC{}).forgetFiles(context.Background(), state.TeardownTarget{AppID: "app_3"}),
		"an install with no registry has nothing there")
}
