//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func specWithMemory(bytes int64) map[string]any {
	s := minimalSpec()
	s["resources"] = map[string]any{"memory_bytes": bytes, "overridden": true}
	return s
}

func specWithWorkloadLimit(bytes int64) map[string]any {
	s := minimalSpec()
	s["workloads"].([]map[string]any)[0]["resources"] = map[string]any{"memory_bytes": bytes}
	return s
}

// TestR241_ChangingResourceLimitsNeedsTheOverrideVerb asserts R-241 and
// R-241a through POST /apps/{id}/specs: Operator, who may edit the app's
// configuration, may not change its limits — the app's or a workload's own —
// and may change anything else; Owner may; and so may the Administrator,
// through install.apps.resources.override, on an app it holds no grant on.
func TestR241_ChangingResourceLimitsNeedsTheOverrideVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	carol := i.user("carol")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+i.userID(carol)+"/role", map[string]string{"role_id": "role_creator"}).Code)
	appID := i.appWithSpec(carol, "notes")

	olive := i.user("olive")
	i.grantControl(appID, olive, "role_operator")

	// Anything else: Operator may.
	other := minimalSpec()
	other["workloads"].([]map[string]any)[0]["image"] = "nginx:1.27-alpine"
	i.writeSpec(olive, appID, other)

	// Raising the app's memory, or giving a workload a limit of its own: not
	// Operator's, and the refusal says what and which verb.
	for name, body := range map[string]map[string]any{
		"app memory":     specWithMemory(2 << 30),
		"workload limit": specWithWorkloadLimit(256 << 20),
	} {
		refused := i.do(olive, http.MethodPost, "/apps/"+appID+"/specs", body)
		require.Equal(t, http.StatusForbidden, refused.Code, "%s: %s", name, refused.String())
		require.Equal(t, "PERM_VERB_REQUIRED", refused.ErrorCode(), name)
		msg, remedy := refused.envelope(t)
		require.Contains(t, msg, "resource limits", name)
		require.Contains(t, msg, "app.resources.override", name)
		require.NotEmpty(t, remedy, name)

		// A dry run answers the same.
		dry := i.do(olive, http.MethodPost, "/apps/"+appID+"/specs?dry_run=true", body)
		require.Equal(t, "PERM_VERB_REQUIRED", dry.ErrorCode(), name)
	}

	// The owner may.
	i.writeSpec(carol, appID, specWithMemory(2<<30))

	// Keeping the limits the newest revision sets is not changing them.
	i.writeSpec(olive, appID, specWithMemory(2<<30))

	// The administrator may, without a grant on the app (install.apps.*).
	i.writeSpec(admin, appID, specWithWorkloadLimit(256<<20))
	i.writeSpec(admin, appID, specWithMemory(4<<30))
}
