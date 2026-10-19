//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// TestR105_TwoProvidersCreatedWithOneIDGetTheTakenRefusal asserts that a
// create racing another with the same chosen ID, past the service's check,
// is refused as a taken ID rather than as a server fault.
func TestR105_TwoProvidersCreatedWithOneIDGetTheTakenRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providers := state.NewIdentityProviders(connected(t))
	chosen := id.New(id.IdentityAdpt)
	first, second := "First", "Second"

	_, err := providers.Create(ctx, "oidc", state.ProviderSettings{ID: chosen, Name: &first})
	require.NoError(t, err)
	_, err = providers.Create(ctx, "oidc", state.ProviderSettings{ID: chosen, Name: &second})
	e := errs.As(err)
	require.NotNil(t, e, "%v", err)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Equal(t, "Another identity provider has the ID "+chosen+".", e.Message)
	require.Contains(t, e.Remedy, "it was saved")
}
