//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

func routingDefaults(t *testing.T, a *state.Adapters) []string {
	t.Helper()
	rows, err := a.List(context.Background())
	require.NoError(t, err)
	var out []string
	for _, r := range rows {
		if r.Category == "routing" && r.IsDefault {
			out = append(out, r.ID)
		}
	}
	return out
}

// TestR252_SavingADefaultReplacesTheCategorysDefault asserts issue #220 at the
// store: the default is handed over in one transaction, the replaced adapter
// is returned, and saves racing to be the default leave exactly one.
func TestR252_SavingADefaultReplacesTheCategorysDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	adapters := state.NewAdapters(connected(t))

	replaced, err := adapters.Save(ctx, state.AdapterConfig{
		ID: "rte_loopback", Category: "routing", Kind: "loopback", Name: "Localhost", IsDefault: true, Enabled: true})
	require.NoError(t, err)
	require.Zero(t, replaced, "the first default replaces nothing")

	replaced, err = adapters.Save(ctx, state.AdapterConfig{
		ID: "rte_cf", Category: "routing", Kind: "cloudflare", Name: "Cloudflare", IsDefault: true, Enabled: true})
	require.NoError(t, err)
	require.Equal(t, state.Replaced{ID: "rte_loopback", Name: "Localhost"}, replaced)
	require.Equal(t, []string{"rte_cf"}, routingDefaults(t, adapters))

	// Not the default: saved beside it, replacing nothing.
	replaced, err = adapters.Save(ctx, state.AdapterConfig{
		ID: "rte_traefik", Category: "routing", Kind: "traefik", Name: "Traefik", Enabled: true})
	require.NoError(t, err)
	require.Zero(t, replaced)
	require.Equal(t, []string{"rte_cf"}, routingDefaults(t, adapters))

	// Racing saves each hand over in turn; none fails on the index.
	var wg sync.WaitGroup
	failures := make(chan error, 3)
	for _, id := range []string{"rte_loopback", "rte_cf", "rte_traefik"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := adapters.Save(ctx, state.AdapterConfig{
				ID: id, Category: "routing", Kind: "stub", Name: id, IsDefault: true, Enabled: true}); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	require.Len(t, routingDefaults(t, adapters), 1)
}

// TestR105_AnAdapterTheDatabaseRefusesSaysWhy asserts issue #220's third
// point: a constraint an adapter breaks reaches the client as a validation
// error naming the rule, never as an internal error.
func TestR105_AnAdapterTheDatabaseRefusesSaysWhy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	adapters := state.NewAdapters(connected(t))

	_, err := adapters.Save(ctx, state.AdapterConfig{
		ID: "ai_other", Category: "ai", Kind: "stub", Name: "Other",
		Config: json.RawMessage(`{"credentials": {"api_key": "x"}}`), Enabled: true})
	var e *errs.Error
	require.True(t, errors.As(err, &e), "%v", err)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.NotEmpty(t, e.Remedy)
	require.NotContains(t, e.Message, "Could not save the adapter configuration")

	_, err = adapters.Save(ctx, state.AdapterConfig{
		ID: "reg_main", Category: "image_registry", Kind: "oci", Name: "Registry",
		Config: json.RawMessage(`{"url": "https://bob:hunter2@registry.example.com"}`), Enabled: true})
	require.True(t, errors.As(err, &e), "%v", err)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Message, "username or password")
	require.NotContains(t, e.Message, "hunter2")
}
