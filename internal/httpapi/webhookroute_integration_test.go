//go:build integration

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/httpapi"
)

func noodleRoute() map[string]any {
	return map[string]any{
		"name": "noodle", "method": "POST", "path": "/api/v1/webhook/noodle",
		"verification": map[string]any{"kind": "hmac_sha256", "header": "X-Hub-Signature-256", "prefix": "sha256="},
	}
}

// TestR432_WebhookRoutesAreManagedThroughTheAPI asserts the whole life of a
// route: added with a generated secret shown once, listed without it, replaced
// both ways, and removed.
func TestR432_WebhookRoutesAreManagedThroughTheAPI(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, minimalSpec()))
	base := "/apps/" + appID + "/webhook-routes"

	added := i.do(admin, http.MethodPost, base, noodleRoute())
	require.Equal(t, http.StatusCreated, added.Code, added.String())
	var saved struct {
		Revision int    `json:"revision"`
		Secret   string `json:"secret"`
	}
	added.JSON(t, &saved)
	require.True(t, strings.HasPrefix(saved.Secret, "pdwr_"), added.String())

	listed := i.do(admin, http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	require.NotContains(t, listed.String(), saved.Secret, "shown once (R-427)")
	var view struct {
		Routes []struct {
			Name      string `json:"name"`
			SecretSet bool   `json:"secret_set"`
			Deployed  bool   `json:"deployed"`
		} `json:"routes"`
	}
	listed.JSON(t, &view)
	require.Len(t, view.Routes, 1)
	require.True(t, view.Routes[0].SecretSet)
	require.False(t, view.Routes[0].Deployed)

	generated := i.do(admin, http.MethodPost, base+"/noodle/secret", nil)
	require.Equal(t, http.StatusCreated, generated.Code, generated.String())
	supplied := i.do(admin, http.MethodPost, base+"/noodle/secret", map[string]any{"secret": "whsec_from_the_provider"})
	require.Equal(t, http.StatusNoContent, supplied.Code, supplied.String())
	missing := i.do(admin, http.MethodPost, base+"/nothing/secret", nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.String())

	removed := i.do(admin, http.MethodDelete, base+"/noodle", nil)
	require.Equal(t, http.StatusOK, removed.Code, removed.String())
	listed = i.do(admin, http.MethodGet, base, nil)
	listed.JSON(t, &view)
	require.Empty(t, view.Routes)
}

// TestR432_RoutesNeedTheirVerbsAndGoodJSON asserts nobody without a credential
// reaches them, and that a body that cannot be read says what to send.
func TestR432_RoutesNeedTheirVerbsAndGoodJSON(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	base := "/apps/" + appID + "/webhook-routes"

	for _, call := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodPost, base},
		{http.MethodDelete, base + "/noodle"},
		{http.MethodPost, base + "/noodle/secret"},
	} {
		got := i.anon(call.method, call.path, noodleRoute())
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, got.Code,
			"%s %s: %s", call.method, call.path, got.String())
	}

	for _, path := range []string{base, base + "/noodle/secret"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, strings.NewReader("{not json"))
		req.Header.Set("Cookie", httpapi.SessionCookie+"="+admin.cookie)
		rec := httptest.NewRecorder()
		i.handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	unconfigured := i.do(admin, http.MethodPost, base, noodleRoute())
	require.Equal(t, http.StatusBadRequest, unconfigured.Code, unconfigured.String())
	require.Contains(t, unconfigured.String(), "isn't configured yet")
}
