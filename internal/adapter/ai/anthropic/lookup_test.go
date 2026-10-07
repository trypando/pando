package anthropic_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

// TestO54_AnthropicLooksPeopleUpBeforeDrafting asserts O-54 at the adapter:
// it says it looks things up, offers the lookup tools the request carries,
// runs a lookup through core's callback, hands the result back, and reads
// the draft submitted after it.
func TestO54_AnthropicLooksPeopleUpBeforeDrafting(t *testing.T) {
	a, fake := withFake(t,
		message("tool_use", toolUse("t1", "find_people", `{"query":"ada"}`)),
		message("tool_use", toolUse("t2", "submit", `{"group":{"name":"Release","members":["usr_ada"]},"reply":"Drafted."}`)))

	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, caps.LooksUp)

	var asked []string
	got, err := a.DraftAccess(context.Background(), api.AccessRequest{
		Description: "Ada ships releases",
		Lookup: &api.Lookup{People: func(_ context.Context, q string) ([]api.PersonInfo, error) {
			asked = append(asked, q)
			return []api.PersonInfo{{ID: "usr_ada", Username: "ada"}}, nil
		}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"usr_ada"}, got.Group.Members)
	require.Equal(t, []string{"ada"}, asked)

	require.Len(t, fake.bodies, 2)
	var names []string
	for _, tool := range fake.bodies[0]["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	require.Equal(t, []string{"submit", "find_people"}, names, "only the lookups the request carries")

	msgs := fake.bodies[1]["messages"].([]any)
	result := msgs[len(msgs)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
	require.Equal(t, "tool_result", result["type"])
	body, _ := json.Marshal(result["content"])
	require.Contains(t, string(body), "usr_ada")
}

// TestO54_AnthropicLookupLoopIsBounded asserts a model that never stops
// looking things up is cut off: at most aikit.MaxLookups lookups run, and the
// conversation ends after a bounded number of rounds with an error.
func TestO54_AnthropicLookupLoopIsBounded(t *testing.T) {
	var replies []string
	for range 24 {
		replies = append(replies, message("tool_use", toolUse("t", "find_apps", `{"query":"blog"}`)))
	}
	a, fake := withFake(t, replies...)

	calls := 0
	_, err := a.SearchAudit(context.Background(), api.AuditSearchRequest{
		Question: "what happened to blog",
		Lookup: &api.Lookup{Apps: func(context.Context, string) ([]api.AppInfo, error) {
			calls++
			return []api.AppInfo{{ID: "app_blog", Name: "blog"}}, nil
		}},
	})
	require.ErrorContains(t, err, "without submitting")
	require.Equal(t, aikit.MaxLookups, calls)
	require.Len(t, fake.bodies, 24)
}

// A request without a Lookup offers the submit tool alone.
func TestAnthropicOffersNoLookupWithoutOne(t *testing.T) {
	a, fake := withFake(t, message("tool_use", toolUse("t1", "submit", `{"filter":{},"note":""}`)))
	_, err := a.SearchAudit(context.Background(), api.AuditSearchRequest{Question: "anything"})
	require.NoError(t, err)
	require.Len(t, fake.bodies[0]["tools"].([]any), 1)
}

// A canceled request ends the loop before the next round.
func TestAnthropicSubmitHonorsCancellation(t *testing.T) {
	a, _ := withFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.DraftPolicy(ctx, api.PolicyRequest{Description: "x"})
	require.ErrorIs(t, err, context.Canceled)
}
