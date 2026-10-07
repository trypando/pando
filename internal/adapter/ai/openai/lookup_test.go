package openai_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestO54_OpenAILooksGroupsUpBeforeDrafting asserts O-54 for OpenAI: it says
// it looks things up, runs a lookup through core's callback, hands the
// result back as the call's output, and reads the draft submitted after it.
func TestO54_OpenAILooksGroupsUpBeforeDrafting(t *testing.T) {
	a, fake := withFake(t,
		response("resp_1", call("c1", "find_groups", `{"query":"ops"}`)),
		response("resp_2", call("c2", "submit", `{"reply":"Ops already exists, so nothing was drafted."}`)))

	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, caps.LooksUp)

	got, err := a.DraftAccess(context.Background(), api.AccessRequest{
		Description: "An ops group",
		Lookup: &api.Lookup{Groups: func(_ context.Context, q string) ([]api.GroupInfo, error) {
			require.Equal(t, "ops", q)
			return []api.GroupInfo{{ID: "grp_ops", Name: "Ops"}}, nil
		}},
	})
	require.NoError(t, err)
	require.Contains(t, got.Reply, "Ops already exists")

	require.Len(t, fake.bodies, 2)
	require.Len(t, fake.bodies[0]["tools"].([]any), 2, "submit and find_groups")
	body, _ := json.Marshal(fake.bodies[1]["input"])
	require.Contains(t, string(body), "grp_ops")
}
