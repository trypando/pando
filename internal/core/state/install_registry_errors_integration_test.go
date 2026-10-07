//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
)

// TestR105_TheStoredImageRegistrySaysWhatFailedWhenTheDatabaseIsGone asserts
// that the install registry's store reports a lost database as an error saying
// what it was doing — never as "no registry configured", which would send a
// build to the runtime by import, or refuse a pull-only runtime, for the wrong
// reason.
func TestR105_TheStoredImageRegistrySaysWhatFailedWhenTheDatabaseIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, password := statetest.Database(t)
	db, err := state.ConnectCopy(ctx, ownerURL, password)
	require.NoError(t, err)
	db.Close()
	store := state.NewInstallRegistry(db, nil, "")

	_, found, err := store.Load(ctx)
	require.False(t, found)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, "Could not read the install registry's settings.", errs.As(err).Message)

	err = store.Save(ctx, state.StoredRegistry{URL: "https://registry.internal"}, "")
	require.Equal(t, "Could not store the install registry's settings.", errs.As(err).Message)

	err = store.Clear(ctx)
	require.Equal(t, "Could not remove the install registry's settings.", errs.As(err).Message)

	_, set, err := store.Password(ctx)
	require.False(t, set)
	require.Error(t, err)
}
