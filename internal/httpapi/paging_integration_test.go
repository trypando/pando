//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	corepolicy "github.com/trypando/pando/internal/core/policy"
)

// TestR154_TheWaitingListPagesAndShowsOnlyAppsTheCallerSees asserts that GET
// /approvals pages on a cursor (issue #72) and that paging never shows a
// request on an app the caller cannot view (R-154).
func TestR154_TheWaitingListPagesAndShowsOnlyAppsTheCallerSees(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	first := a.appWithSpec(admin, "first")
	hidden := a.appWithSpec(admin, "hidden")
	last := a.appWithSpec(admin, "last")
	dev := a.owner(admin, first, "dev")
	got := a.do(admin, http.MethodPost, "/apps/"+last+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": a.userID(dev), "role_id": "role_owner",
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	a.policy(func(d *corepolicy.Document) {
		d.DeployApprovalApps = []string{first, hidden, last}
	})
	want := []string{a.deploy(admin, first).ID, a.deploy(admin, hidden).ID, a.deploy(admin, last).ID}

	walk := func(s *session) []string {
		var ids []string
		cursor := ""
		for range 10 {
			got := a.do(s, http.MethodGet, "/approvals?limit=1&cursor="+cursor, nil)
			require.Equal(t, http.StatusOK, got.Code, got.String())
			var out struct {
				Approvals  []deployView `json:"approvals"`
				NextCursor string       `json:"next_cursor"`
			}
			got.JSON(t, &out)
			require.LessOrEqual(t, len(out.Approvals), 1)
			for _, d := range out.Approvals {
				ids = append(ids, d.ID)
			}
			if out.NextCursor == "" {
				return ids
			}
			cursor = out.NextCursor
		}
		t.Fatal("the cursor never ran out")
		return nil
	}
	require.Equal(t, want, walk(admin), "oldest first, each once")
	require.Equal(t, []string{want[0], want[2]}, walk(dev), "never the app dev cannot see")

	bad := a.do(admin, http.MethodGet, "/approvals?cursor=nonsense", nil)
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())
}

// TestTheAccountsGroupsAndAppsListsPage asserts the list endpoints answer with
// next_cursor and total, honor limit, and refuse a bad limit readably.
func TestTheAccountsGroupsAndAppsListsPage(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	i.user("one")
	i.user("two")
	i.appWithSpec(admin, "a")
	i.appWithSpec(admin, "b")

	for _, path := range []string{"/users", "/groups", "/apps"} {
		got := i.do(admin, http.MethodGet, path+"?limit=1", nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		var out map[string]any
		got.JSON(t, &out)
		require.Contains(t, out, "next_cursor", path)
		require.Contains(t, out, "total", path)

		bad := i.do(admin, http.MethodGet, path+"?limit=none", nil)
		require.Equal(t, http.StatusBadRequest, bad.Code, path)
	}

	got := i.do(admin, http.MethodGet, "/users?limit=1", nil)
	var users struct {
		Users      []map[string]any `json:"users"`
		NextCursor string           `json:"next_cursor"`
		Total      int              `json:"total"`
	}
	got.JSON(t, &users)
	require.Len(t, users.Users, 1)
	require.NotEmpty(t, users.NextCursor)
	require.GreaterOrEqual(t, users.Total, 3)

	got = i.do(admin, http.MethodGet, "/apps?limit=1", nil)
	var apps struct {
		Apps       []map[string]any `json:"apps"`
		NextCursor string           `json:"next_cursor"`
		Total      int              `json:"total"`
	}
	got.JSON(t, &apps)
	require.Len(t, apps.Apps, 1)
	require.Equal(t, 2, apps.Total)
	got = i.do(admin, http.MethodGet, "/apps?limit=1&cursor="+apps.NextCursor, nil)
	var rest struct {
		Apps       []map[string]any `json:"apps"`
		NextCursor string           `json:"next_cursor"`
	}
	got.JSON(t, &rest)
	require.Len(t, rest.Apps, 1)
	require.NotEqual(t, apps.Apps[0]["id"], rest.Apps[0]["id"])
	require.Empty(t, rest.NextCursor)

	// A group in the list counts its members; the group itself lists them.
	created := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "team", "members": []string{users.Users[0]["id"].(string)}})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var group struct {
		ID string `json:"id"`
	}
	created.JSON(t, &group)
	got = i.do(admin, http.MethodGet, "/groups?q=tea", nil)
	require.Contains(t, got.String(), `"member_count":1`)
	require.NotContains(t, got.String(), `"members"`)
	got = i.do(admin, http.MethodGet, "/groups/"+group.ID, nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, got.String(), users.Users[0]["id"].(string))
}
