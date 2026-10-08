//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type inboxPage struct {
	Notifications []struct {
		ID      string  `json:"id"`
		Kind    string  `json:"kind"`
		Subject string  `json:"subject"`
		AppName string  `json:"app_name"`
		ReadAt  *string `json:"read_at"`
	} `json:"notifications"`
	Unread int `json:"unread"`
}

func (i *install) inbox(s *session) inboxPage {
	i.t.Helper()
	got := i.do(s, http.MethodGet, "/me/notifications", nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	var page inboxPage
	got.JSON(i.t, &page)
	return page
}

// TestR377_TheInboxShowsWhatPandoToldYouOnTheConsole asserts R-377: what the
// console adapter records is listed to the person it is for, counted while
// unread, and marked read by them alone.
func TestR377_TheInboxShowsWhatPandoToldYouOnTheConsole(t *testing.T) {
	i := newInstall(t)
	appID := i.createApp(i.admin(), "billing")
	carol, dave := i.user("carol"), i.user("dave")

	for _, who := range []*session{carol, dave} {
		got := i.do(who, http.MethodPut, "/notification-preferences", map[string]any{
			"choices": []map[string]any{{"kind": "app_shared", "channel": "ntf_console", "enabled": true}},
		})
		require.Equal(t, http.StatusOK, got.Code, got.String())
		got = i.do(i.admin(), http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
			"plane": "data", "principal_kind": "user", "principal_id": i.userID(who),
		})
		require.Equal(t, http.StatusCreated, got.Code, got.String())
	}

	page := i.inbox(carol)
	require.Len(t, page.Notifications, 1)
	require.Equal(t, 1, page.Unread)
	require.Equal(t, "app_shared", page.Notifications[0].Kind)
	require.Equal(t, "billing", page.Notifications[0].AppName)
	carolsID := page.Notifications[0].ID

	// Dave cannot mark Carol's read, and is not told it exists.
	got := i.do(dave, http.MethodPost, "/me/notifications/"+carolsID+"/read", nil)
	require.Equal(t, http.StatusNoContent, got.Code)
	require.Equal(t, 1, i.inbox(carol).Unread)

	got = i.do(carol, http.MethodPost, "/me/notifications/"+carolsID+"/read", nil)
	require.Equal(t, http.StatusNoContent, got.Code)
	page = i.inbox(carol)
	require.Zero(t, page.Unread)
	require.NotNil(t, page.Notifications[0].ReadAt)

	got = i.do(dave, http.MethodPost, "/me/notifications/read", nil)
	require.Equal(t, http.StatusNoContent, got.Code)
	require.Zero(t, i.inbox(dave).Unread)

	// A token that is not a person has no inbox.
	got = i.do(i.serviceToken(i.admin()), http.MethodGet, "/me/notifications", nil)
	require.Equal(t, http.StatusForbidden, got.Code)
}

func (i *install) unread(s *session) int {
	i.t.Helper()
	got := i.do(s, http.MethodGet, "/me/notifications/unread", nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	var count struct {
		Unread *int `json:"unread"`
	}
	got.JSON(i.t, &count)
	require.NotNil(i.t, count.Unread, got.String())
	return *count.Unread
}

// TestR377_TheBellCountsUnreadWithoutListingThem asserts R-377: the count of
// unread notifications beside Settings on every screen is answered on its
// own, for that person alone, and moves as they read.
func TestR377_TheBellCountsUnreadWithoutListingThem(t *testing.T) {
	i := newInstall(t)
	carol, dave := i.user("carol"), i.user("dave")
	for _, name := range []string{"billing", "ledger"} {
		appID := i.createApp(i.admin(), name)
		got := i.do(carol, http.MethodPut, "/notification-preferences", map[string]any{
			"choices": []map[string]any{{"kind": "app_shared", "channel": "ntf_console", "enabled": true}},
		})
		require.Equal(t, http.StatusOK, got.Code, got.String())
		got = i.do(i.admin(), http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
			"plane": "data", "principal_kind": "user", "principal_id": i.userID(carol),
		})
		require.Equal(t, http.StatusCreated, got.Code, got.String())
	}

	require.Equal(t, 2, i.unread(carol))
	require.Equal(t, i.inbox(carol).Unread, i.unread(carol), "the same count the list carries")
	require.Zero(t, i.unread(dave), "another person's notifications are not counted")

	got := i.do(carol, http.MethodPost, "/me/notifications/"+i.inbox(carol).Notifications[0].ID+"/read", nil)
	require.Equal(t, http.StatusNoContent, got.Code)
	require.Equal(t, 1, i.unread(carol))

	got = i.do(i.serviceToken(i.admin()), http.MethodGet, "/me/notifications/unread", nil)
	require.Equal(t, http.StatusForbidden, got.Code, "a token that is not a person has no inbox")
}

// TestR378_AnAppShowsItsOwnEvents asserts R-378: anyone who can see an app
// reads its recent events, described for a person, and nobody else does.
func TestR378_AnAppShowsItsOwnEvents(t *testing.T) {
	i := newInstall(t)
	appID := i.createApp(i.admin(), "billing")
	_, err := i.db.Exec(context.Background(), `UPDATE apps SET state = 'degraded' WHERE id = $1`, appID)
	require.NoError(t, err)

	got := i.do(i.admin(), http.MethodGet, "/apps/"+appID+"/events", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var feed struct {
		Events []struct {
			Type    string `json:"type"`
			Subject string `json:"subject"`
			Link    string `json:"link"`
		} `json:"events"`
	}
	got.JSON(t, &feed)
	require.GreaterOrEqual(t, len(feed.Events), 2)
	require.Equal(t, "app.state_changed", feed.Events[0].Type, "newest first")
	require.Contains(t, feed.Events[0].Subject, "billing is degraded")
	require.Equal(t, "app.created", feed.Events[len(feed.Events)-1].Type)

	stranger := i.user("stranger")
	require.Equal(t, http.StatusNotFound, i.do(stranger, http.MethodGet, "/apps/"+appID+"/events", nil).Code)
}
