//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// portSpec is minimalSpec reached on port on the loopback adapter.
func portSpec(port int) map[string]any {
	s := minimalSpec()
	s["routing"] = map[string]any{"adapter_ref": "rte_loopback", "mode": "port", "port": port}
	return s
}

// portOfApp is the port an app was given through PUT /routing, pinned.
func (i *install) portOfApp(s *session, appID string) int {
	i.t.Helper()
	moved := i.do(s, http.MethodPut, "/apps/"+appID+"/routing", map[string]any{"adapter_ref": "rte_loopback", "confirm": true})
	require.Equal(i.t, http.StatusCreated, moved.Code, moved.String())
	require.Equal(i.t, http.StatusOK, i.pinNewest(s, appID).Code)
	port := i.newestRouting(s, appID).Port
	require.Positive(i.t, port)
	return port
}

// TestR165_AnAppIsNeverReachedAtAnotherAppsPort asserts R-165's one app per
// address for ports: a spec naming a port another app holds is refused when
// it is pinned, for someone who can edit only their own app, and the port
// still reaches the app that holds it (issue #348).
func TestR165_AnAppIsNeverReachedAtAnotherAppsPort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i, admin, _, notes := withRouting(t)
	held := i.portOfApp(admin, notes)

	// Operator on one app of her own, and nothing on notes.
	eve := i.user("eve")
	mine := i.appWithSpec(admin, "eves-app")
	i.grantControl(mine, eve, "role_operator")
	// A spec is written whole, past PUT /routing's check, as a path is; the
	// pin decides (TestR167_ACustomPathBelongsToOneApp).
	written := i.do(eve, http.MethodPost, "/apps/"+mine+"/specs", portSpec(held))
	require.Equal(t, http.StatusCreated, written.Code, written.String())
	refused := i.pinNewest(eve, mine)
	require.Equal(t, http.StatusConflict, refused.Code, "refused when pinned: %s", refused.String())
	require.Equal(t, "STATE_ADDRESS_TAKEN", refused.ErrorCode())

	// And at the pin, for a revision written before the port was taken.
	other := i.appWithSpec(admin, "wiki")
	rev := i.writeSpec(admin, other, portSpec(held+1))
	require.Equal(t, held+1, i.portOfApp(admin, i.appWithSpec(admin, "blog")), "the next free port goes to blog")
	pinned := i.do(admin, http.MethodPost, "/apps/"+other+"/specs/"+strconv.Itoa(rev)+"/pin", map[string]any{})
	require.Equal(t, http.StatusConflict, pinned.Code, "refused when pinned: %s", pinned.String())
	require.Equal(t, "STATE_ADDRESS_TAKEN", pinned.ErrorCode())

	app, _, found, err := i.Apps.ByRouting(ctx, "port", strconv.Itoa(held))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, notes, app.ID, "the port still reaches the app that holds it")
}

// TestR166_ASpecThatNamesAPortIsServedThere asserts R-166's "an app given a
// port is served there": a port written into a spec, rather than allocated
// through PUT /routing, becomes the app's allocation when pinned, so the proxy
// listens on it. A port outside the install's range, which nothing publishes,
// is refused.
func TestR166_ASpecThatNamesAPortIsServedThere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i, admin, _, appID := withRouting(t)

	i.pinSpec(admin, appID, i.writeSpec(admin, appID, portSpec(9012)))
	listening, err := state.NewPorts(i.db).InUse(ctx)
	require.NoError(t, err)
	require.Contains(t, listening, 9012, "the proxy listens on the port the spec names")

	// Moving to another port releases the first.
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, portSpec(9013)))
	listening, err = state.NewPorts(i.db).InUse(ctx)
	require.NoError(t, err)
	require.Contains(t, listening, 9013)
	require.NotContains(t, listening, 9012, "the app's old port is released")

	// Re-pinning the port the app holds is not a new claim.
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, portSpec(9013)))

	outside := i.writeSpec(admin, appID, portSpec(12000))
	refused := i.do(admin, http.MethodPost, "/apps/"+appID+"/specs/"+strconv.Itoa(outside)+"/pin", map[string]any{})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "9000")
}
