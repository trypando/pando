package subscription

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// names is an ActorNames that counts its lookups.
type names struct {
	users   map[string]state.User
	lookups int
}

func (n *names) ByID(_ context.Context, id string) (state.User, bool, error) {
	n.lookups++
	u, ok := n.users[id]
	return u, ok, nil
}

// TestR379_TheFeedNamesWhoActed asserts R-379's actor names reach an app's
// feed: a person by their display name, falling back to their username; a
// token by the person it acted for; Pando by nothing; and each person looked up
// once a page.
func TestR379_TheFeedNamesWhoActed(t *testing.T) {
	ctx := context.Background()
	lookup := &names{users: map[string]state.User{
		"usr_ada": {ID: "usr_ada", DisplayName: "Ada Lovelace", ExternalID: "ada"},
		"usr_sam": {ID: "usr_sam", ExternalID: "sam"},
	}}
	s := &Service{Names: lookup}
	seen := map[string]string{}

	require.Equal(t, "Ada Lovelace", s.actorName(ctx, Actor{Kind: "user", ID: "usr_ada"}, seen))
	require.Equal(t, "Ada Lovelace", s.actorName(ctx, Actor{Kind: "user", ID: "usr_ada"}, seen))
	require.Equal(t, 1, lookup.lookups, "a person is looked up once a page")

	require.Equal(t, "sam", s.actorName(ctx, Actor{Kind: "user", ID: "usr_sam"}, seen), "no display name falls back to the username")
	require.Equal(t, "Ada Lovelace", s.actorName(ctx, Actor{Kind: "token", ID: "tok_1", OnBehalfOf: "usr_ada"}, seen))
	require.Empty(t, s.actorName(ctx, Actor{Kind: "system"}, seen))
	require.Empty(t, s.actorName(ctx, Actor{Kind: "token", ID: "tok_svc"}, seen), "a service token acting for nobody has no name")
	require.Empty(t, s.actorName(ctx, Actor{Kind: "user", ID: "usr_gone"}, seen), "a person who no longer exists has no name")

	require.Empty(t, (&Service{}).actorName(ctx, Actor{Kind: "user", ID: "usr_ada"}, seen), "no lookup wired leaves names out")
}
