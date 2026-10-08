//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// Lists that cost a page, not the install (issue #72, O-53).

type pagedTotal struct {
	Total             int    `json:"total"`
	TotalIsLowerBound bool   `json:"total_is_lower_bound"`
	NextCursor        string `json:"next_cursor"`
}

// TestO53_AListPastTheCapSaysItsTotalIsALowerBound asserts O-53 at the API:
// a list holding more than 10,000 matches says 10,000 and that it is a lower
// bound, and one holding fewer says exactly how many and that it is not.
func TestO53_AListPastTheCapSaysItsTotalIsALowerBound(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	_, err := i.db.Exec(context.Background(), `
		INSERT INTO groups (id, name)
		SELECT 'grp_bulk' || lpad(i::text, 6, '0'), 'bulk ' || i FROM generate_series(1, $1) i`, state.TotalCap+3)
	require.NoError(t, err)

	var groups pagedTotal
	got := i.do(admin, http.MethodGet, "/groups?limit=1", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &groups)
	require.Equal(t, state.TotalCap, groups.Total)
	require.True(t, groups.TotalIsLowerBound, "10,000+")

	got = i.do(admin, http.MethodGet, "/groups?limit=1&q=bulk+1000", nil)
	got.JSON(t, &groups)
	require.Equal(t, 5, groups.Total, "bulk 1000 and 10000 to 10003")
	require.False(t, groups.TotalIsLowerBound)

	var users pagedTotal
	i.do(admin, http.MethodGet, "/users?limit=1", nil).JSON(t, &users)
	require.Equal(t, 1, users.Total)
	require.False(t, users.TotalIsLowerBound)
	var apps pagedTotal
	i.do(admin, http.MethodGet, "/apps?limit=1", nil).JSON(t, &apps)
	require.Zero(t, apps.Total)
	require.False(t, apps.TotalIsLowerBound)
}

// TestR264_TheLauncherSaysWhichAppsYouCanAlsoManage asserts that GET /me/apps
// carries, for each app, whether the caller can administer it — the control
// plane's answer beside the data plane's list (R-070, R-071) — and pages.
func TestR264_TheLauncherSaysWhichAppsYouCanAlsoManage(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	pat := i.user("pat")
	patID := i.userID(pat)

	useOnly := i.createApp(admin, "use-only")
	both := i.createApp(admin, "both")
	for _, g := range []map[string]any{
		{"app": useOnly, "plane": "data"},
		{"app": both, "plane": "data"},
		{"app": both, "plane": "control", "role_id": "role_operator"},
	} {
		body := map[string]any{"plane": g["plane"], "principal_kind": "user", "principal_id": patID}
		if role, ok := g["role_id"]; ok {
			body["role_id"] = role
		}
		got := i.do(admin, http.MethodPost, "/apps/"+g["app"].(string)+"/grants", body)
		require.Equal(t, http.StatusCreated, got.Code, got.String())
	}

	type launcher struct {
		Apps []struct {
			ID        string `json:"id"`
			CanManage bool   `json:"can_manage"`
		} `json:"apps"`
		NextCursor string `json:"next_cursor"`
	}
	var first, second launcher
	got := i.do(pat, http.MethodGet, "/me/apps?limit=1", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &first)
	require.Len(t, first.Apps, 1)
	require.NotEmpty(t, first.NextCursor)
	i.do(pat, http.MethodGet, "/me/apps?limit=1&cursor="+first.NextCursor, nil).JSON(t, &second)
	require.Len(t, second.Apps, 1)
	require.Empty(t, second.NextCursor)

	manage := map[string]bool{first.Apps[0].ID: first.Apps[0].CanManage, second.Apps[0].ID: second.Apps[0].CanManage}
	require.Equal(t, map[string]bool{both: true, useOnly: false}, manage,
		"named by name, both first; can_manage only where pat holds a control grant")

	var mine launcher
	i.do(admin, http.MethodGet, "/me/apps?q=use", nil).JSON(t, &mine)
	require.Len(t, mine.Apps, 1)
	require.Equal(t, useOnly, mine.Apps[0].ID)
	require.True(t, mine.Apps[0].CanManage, "an administrator manages every app")

	bad := i.do(pat, http.MethodGet, "/me/apps?limit=0", nil)
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())
}

// TestR071_SharedListsArePaged walks the lists that grew with an app's or a
// person's reach — an account's apps, a group's apps, an app's grants and the
// subscriptions list — a page at a time.
func TestR071_SharedListsArePaged(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	pat := i.user("pat")
	patID := i.userID(pat)

	created := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "team", "members": []string{patID}})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var team struct {
		ID string `json:"id"`
	}
	created.JSON(t, &team)

	var apps []string
	for _, name := range []string{"alpha", "beta", "gamma"} {
		appID := i.createApp(admin, name)
		apps = append(apps, appID)
		for _, who := range []map[string]any{
			{"principal_kind": "user", "principal_id": patID},
			{"principal_kind": "group", "principal_id": team.ID},
		} {
			who["plane"] = "data"
			got := i.do(admin, http.MethodPost, "/apps/"+appID+"/grants", who)
			require.Equal(t, http.StatusCreated, got.Code, got.String())
		}
	}

	walk := func(path string) (ids []string, pages int) {
		cursor := ""
		for range 10 {
			got := i.do(admin, http.MethodGet, path+"limit=2&cursor="+cursor, nil)
			require.Equal(t, http.StatusOK, got.Code, got.String())
			var out struct {
				Apps []struct {
					AppID     string `json:"app_id"`
					CanManage bool   `json:"can_manage"`
					Data      []any  `json:"data"`
				} `json:"apps"`
				NextCursor string `json:"next_cursor"`
			}
			got.JSON(t, &out)
			pages++
			for _, a := range out.Apps {
				require.True(t, a.CanManage)
				ids = append(ids, a.AppID)
			}
			if out.NextCursor == "" {
				return ids, pages
			}
			cursor = out.NextCursor
		}
		t.Fatal("the cursor never ran out")
		return nil, 0
	}
	ids, pages := walk("/users/" + patID + "/apps?")
	require.Equal(t, apps, ids, "by name, each once")
	require.Equal(t, 2, pages)
	ids, pages = walk("/groups/" + team.ID + "/apps?")
	require.Equal(t, apps, ids)
	require.Equal(t, 2, pages)

	var grants struct {
		Grants []struct {
			PrincipalKind string `json:"principal_kind"`
		} `json:"grants"`
		NextCursor    string `json:"next_cursor"`
		PublicSharing string `json:"public_sharing"`
	}
	got := i.do(admin, http.MethodGet, "/apps/"+apps[0]+"/grants?limit=1", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &grants)
	require.Len(t, grants.Grants, 1)
	require.NotEmpty(t, grants.NextCursor)
	require.NotEmpty(t, grants.PublicSharing)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/apps/"+apps[0]+"/grants?cursor=nonsense", nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/users/"+patID+"/apps?limit=x", nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/groups/"+team.ID+"/apps?limit=x", nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/groups/"+team.ID+"/members?limit=x", nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/subscriptions?limit=x", nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/apps/"+apps[0]+"/grants?limit=x", nil).Code)
}
