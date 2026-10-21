//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR261_ADeployOfThePinnedRevisionSaysWhenANewerOneExists asserts R-261 for
// a deploy after an edit. Editing writes a revision and leaves the pinned one
// alone (R-152), and the console's Deploy names the newest; a CLI or MCP deploy
// that names none ships the pinned one, and the reply says a newer one is
// there rather than leaving the old version shipped quietly.
func TestR261_ADeployOfThePinnedRevisionSaysWhenANewerOneExists(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	withEgressAdapters(t, i)
	admin := i.admin()

	appID := i.appWithSpec(admin, "notes")
	edited := minimalSpec()
	edited["name"] = "notes, edited"
	newest := i.writeSpec(admin, appID, edited)
	require.Equal(t, 2, newest)

	got := i.do(admin, http.MethodPost, "/apps/"+appID+"/deployments", map[string]any{})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var dep state.Deployment
	got.JSON(t, &dep)
	require.Equal(t, newest, dep.NewerRevision, "the reply names the edit that was not shipped")
	require.Equal(t, 1, dep.SpecRevision, "and what shipped is still the pinned revision")
}

// A deploy that names the newest revision, or one with nothing newer than the
// pinned revision, has nothing to say about it.
func TestR261_ADeployWithNothingNewerSaysNothing(t *testing.T) {
	t.Parallel()
	for _, named := range []bool{true, false} {
		i := newInstall(t)
		withEgressAdapters(t, i)
		admin := i.admin()
		appID := i.appWithSpec(admin, "notes")
		body := map[string]any{}
		if named {
			body["spec_revision"] = i.writeSpec(admin, appID, minimalSpec())
		}

		got := i.do(admin, http.MethodPost, "/apps/"+appID+"/deployments", body)
		require.Equal(t, http.StatusAccepted, got.Code, got.String())
		require.NotContains(t, got.String(), "newer_revision")
	}
}

// TestR193_SettingASecretSaysWhetherTheAppReadsIt asserts R-193's limit: a
// rotated secret reaches an app that reads it, and only a reference in the
// spec makes it read. Setting one nothing references used to answer 204 like
// any other, and a deploy after it changed nothing; the reply now says so.
func TestR193_SettingASecretSaysWhetherTheAppReadsIt(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodPut, "/apps/"+appID+"/secrets/NTFY_TOPIC", map[string]any{"value": "t"})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var reply struct {
		ReadBySpec bool   `json:"read_by_spec"`
		Note       string `json:"note"`
	}
	got.JSON(t, &reply)
	require.False(t, reply.ReadBySpec)
	require.Contains(t, reply.Note, `{"key": "NTFY_TOPIC", "secret_ref": "NTFY_TOPIC"}`)

	reads := minimalSpec()
	reads["workloads"].([]map[string]any)[0]["env"] = []map[string]any{{"key": "NTFY_TOPIC", "secret_ref": "NTFY_TOPIC"}}
	i.writeSpec(admin, appID, reads)

	got = i.do(admin, http.MethodPut, "/apps/"+appID+"/secrets/NTFY_TOPIC", map[string]any{"value": "u"})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	reply = struct {
		ReadBySpec bool   `json:"read_by_spec"`
		Note       string `json:"note"`
	}{}
	got.JSON(t, &reply)
	require.True(t, reply.ReadBySpec, "the newest revision reads it")
	require.Empty(t, reply.Note)
}
