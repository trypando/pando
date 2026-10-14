//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	routingcloudflare "github.com/trypando/pando/internal/adapter/routing/cloudflare"
	routingloopback "github.com/trypando/pando/internal/adapter/routing/loopback"
	"github.com/trypando/pando/internal/core/state"
)

// TestR252_MakingAnAdapterTheDefaultHandsTheDefaultOver asserts issue #220:
// saving an adapter as the default of a category that has one makes it the
// default in place of that one, says which, and audits both in one event,
// rather than failing on the one-default-per-category index.
func TestR252_MakingAnAdapterTheDefaultHandsTheDefaultOver(t *testing.T) {
	i := newInstall(t)
	admin := i.admin()
	require.NoError(t, i.Adapters.Upsert(context.Background(), state.AdapterConfig{
		ID: "rte_loopback", Category: "routing", Kind: routingloopback.Kind, Name: "Localhost",
		IsDefault: true, Enabled: true,
	}))

	got := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "rte_cloudflare", "category": "routing", "kind": routingcloudflare.Kind, "name": "Cloudflare",
		"is_default": true,
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	var saved struct {
		Replaced string `json:"replaced_default"`
		Handover string `json:"handover"`
		Note     string `json:"note"`
	}
	got.JSON(t, &saved)
	require.Equal(t, "rte_loopback", saved.Replaced)
	require.Equal(t, "This makes Cloudflare the default routing adapter in place of Localhost. "+
		"Apps already added keep the routing adapter they were added with; apps added from now on use Cloudflare.",
		saved.Handover)
	require.Contains(t, saved.Note, saved.Handover)

	defaults := map[string]bool{}
	rows, err := i.Adapters.List(context.Background())
	require.NoError(t, err)
	for _, r := range rows {
		if r.Category == "routing" {
			defaults[r.ID] = r.IsDefault
		}
	}
	require.Equal(t, map[string]bool{"rte_loopback": false, "rte_cloudflare": true}, defaults,
		"one default, and it is the new one")

	events := i.eventsFor(admin, "adapter.configure")
	require.NotEmpty(t, events)
	require.Equal(t, "rte_cloudflare", events[0].TargetID)
	require.Equal(t, "rte_loopback", events[0].Detail["replaced_default"])

	// Saving the default again hands nothing over.
	got = i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "rte_cloudflare", "category": "routing", "kind": routingcloudflare.Kind, "name": "Cloudflare",
		"is_default": true,
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	require.NotContains(t, got.String(), "replaced_default")
}
