//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newApp(name string) map[string]any {
	return map[string]any{
		"name": name, "source": map[string]string{"type": "git", "url": "https://github.com/acme/" + name},
	}
}

// TestR244_ACreatePastTheLimitIsRefusedAndSaysWhy asserts R-244 and R-105
// through the API: a group's limit applies to its members, the refusal names
// it, a value on the account beats it, and only an administrator sets one.
func TestR244_ACreatePastTheLimitIsRefusedAndSaysWhy(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	maker := i.user("limited-maker")
	var me struct {
		UserID string `json:"user_id"`
	}
	i.do(maker, http.MethodGet, "/me", nil).JSON(t, &me)
	granted := i.do(admin, http.MethodPut, "/users/"+me.UserID+"/role", map[string]any{"role_id": "role_creator"})
	require.Less(t, granted.Code, 300, granted.String())

	var finance struct {
		ID string `json:"id"`
	}
	created := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "Finance"})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	created.JSON(t, &finance)
	added := i.do(admin, http.MethodPut, "/groups/"+finance.ID+"/members/"+me.UserID, nil)
	require.Less(t, added.Code, 300, added.String())

	set := i.do(admin, http.MethodPut, "/groups/"+finance.ID+"/app-limit", map[string]any{"max_apps": 1})
	require.Equal(t, http.StatusOK, set.Code, set.String())
	require.JSONEq(t, `{"max_apps": 1}`, i.do(admin, http.MethodGet, "/groups/"+finance.ID+"/app-limit", nil).String())

	i.createApp(maker, "ledger")
	refused := i.do(maker, http.MethodPost, "/apps", newApp("budget"))
	require.Equal(t, http.StatusForbidden, refused.Code, refused.String())
	require.Equal(t, "POLICY_APP_LIMIT_REACHED", refused.ErrorCode())
	assert.Contains(t, refused.String(), "You own 1 app, and 1 is the limit set for the Finance group")
	assert.Contains(t, refused.String(), "Ask an administrator to raise the limit")

	var limit struct {
		Limit   int    `json:"limit"`
		Source  string `json:"source"`
		GroupID string `json:"group_id"`
		Owned   int    `json:"owned"`
	}
	i.do(maker, http.MethodGet, "/users/"+me.UserID+"/app-limit", nil).JSON(t, &limit)
	assert.Equal(t, 1, limit.Limit)
	assert.Equal(t, "group", limit.Source)
	assert.Equal(t, finance.ID, limit.GroupID)
	assert.Equal(t, 1, limit.Owned)

	// Nobody raises their own limit.
	own := i.do(maker, http.MethodPut, "/users/"+me.UserID+"/app-limit", map[string]any{"max_apps": 0})
	require.Equal(t, http.StatusForbidden, own.Code, own.String())

	// An administrator makes an exception: 0 on the account is unlimited,
	// whatever the group says.
	exception := i.do(admin, http.MethodPut, "/users/"+me.UserID+"/app-limit", map[string]any{"max_apps": 0})
	require.Equal(t, http.StatusOK, exception.Code, exception.String())
	i.createApp(maker, "budget")

	// Cleared, the group applies again.
	cleared := i.do(admin, http.MethodPut, "/users/"+me.UserID+"/app-limit", map[string]any{"max_apps": nil})
	require.Equal(t, http.StatusOK, cleared.Code, cleared.String())
	again := i.do(maker, http.MethodPost, "/apps", newApp("forecast"))
	require.Equal(t, http.StatusForbidden, again.Code, again.String())
	assert.Contains(t, again.String(), "You own 2 apps", "lowering a limit removes nothing")

	bad := i.do(admin, http.MethodPut, "/groups/"+finance.ID+"/app-limit", map[string]any{"max_apps": -1})
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())
}

// TestR244_HostPolicySetsTheDefault asserts R-244: max_apps_per_user applies
// to anybody without a value of their own or a group's.
func TestR244_HostPolicySetsTheDefault(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	var doc map[string]any
	i.do(admin, http.MethodGet, "/policy", nil).JSON(t, &doc)
	doc["max_apps_per_user"] = 1
	saved := i.do(admin, http.MethodPut, "/policy", doc)
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	i.createApp(admin, "first")
	refused := i.do(admin, http.MethodPost, "/apps", newApp("second"))
	require.Equal(t, http.StatusForbidden, refused.Code, refused.String())
	assert.Contains(t, refused.String(), "the limit this installation sets for each person")
}

// TestR397_AnAppsIdleSettingsAreItsOwn asserts R-393 and R-397 through the
// API: the installation's days apply until the app sets its own, turning one
// off writes no spec revision, and a setting that cannot work is refused.
func TestR397_AnAppsIdleSettingsAreItsOwn(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	var doc map[string]any
	i.do(admin, http.MethodGet, "/policy", nil).JSON(t, &doc)
	doc["idle_stop_days"] = 30
	saved := i.do(admin, http.MethodPut, "/policy", doc)
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	app := i.createApp(admin, "rarely")
	type report struct {
		StopDays          *int   `json:"stop_days"`
		EffectiveStopDays int    `json:"effective_stop_days"`
		InstallStopDays   int    `json:"install_stop_days"`
		LastActivityAt    string `json:"last_activity_at"`
		StoppedForIdle    bool   `json:"stopped_for_idle"`
	}
	var got report
	read := i.do(admin, http.MethodGet, "/apps/"+app+"/idle", nil)
	require.Equal(t, http.StatusOK, read.Code, read.String())
	read.JSON(t, &got)
	assert.Nil(t, got.StopDays)
	assert.Equal(t, 30, got.EffectiveStopDays)
	assert.NotEmpty(t, got.LastActivityAt)

	off := i.do(admin, http.MethodPut, "/apps/"+app+"/idle", map[string]any{"stop_days": 0, "delete_days": nil})
	require.Equal(t, http.StatusOK, off.Code, off.String())
	off.JSON(t, &got)
	require.NotNil(t, got.StopDays)
	assert.Equal(t, 0, got.EffectiveStopDays)
	assert.Equal(t, 30, got.InstallStopDays)

	var specs struct {
		Revisions []any `json:"revisions"`
	}
	i.do(admin, http.MethodGet, "/apps/"+app+"/specs", nil).JSON(t, &specs)
	assert.Empty(t, specs.Revisions, "settings, not spec")

	refused := i.do(admin, http.MethodPut, "/apps/"+app+"/idle", map[string]any{"stop_days": nil, "delete_days": 20})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	assert.Contains(t, refused.String(), "the installation's idle_stop_days is 30")

	stranger := i.user("idle-stranger")
	require.Equal(t, http.StatusNotFound, i.do(stranger, http.MethodGet, "/apps/"+app+"/idle", nil).Code)
	require.Equal(t, http.StatusNotFound, i.do(stranger, http.MethodPut, "/apps/"+app+"/idle", map[string]any{"stop_days": 1}).Code)

	// A body that is not the settings says what to send.
	garbled := i.do(admin, http.MethodPut, "/apps/"+app+"/idle", "thirty days")
	require.Equal(t, http.StatusBadRequest, garbled.Code, garbled.String())
	assert.Contains(t, garbled.String(), "Use 0 to turn either off")
}

// TestR105_AnAppLimitRequestThatCannotWorkSaysWhy asserts R-244 and R-105:
// a missing account or group, a body that is not a limit, and a caller who
// may not read somebody else's limit are each answered, not guessed at.
func TestR105_AnAppLimitRequestThatCannotWorkSaysWhy(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	for _, path := range []string{"/users/usr_missing/app-limit", "/groups/grp_missing/app-limit"} {
		got := i.do(admin, http.MethodPut, path, map[string]any{"max_apps": 3})
		require.Equal(t, http.StatusNotFound, got.Code, path+": "+got.String())
	}
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodGet, "/users/usr_missing/app-limit", nil).Code)
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodGet, "/groups/grp_missing/app-limit", nil).Code)

	garbled := i.do(admin, http.MethodPut, "/users/"+i.AdminID+"/app-limit", "five")
	require.Equal(t, http.StatusBadRequest, garbled.Code, garbled.String())
	assert.Contains(t, garbled.String(), "to clear it")
	negative := i.do(admin, http.MethodPut, "/users/"+i.AdminID+"/app-limit", map[string]any{"max_apps": -2})
	require.Equal(t, http.StatusBadRequest, negative.Code, negative.String())

	other := i.user("limit-onlooker")
	require.Equal(t, http.StatusForbidden, i.do(other, http.MethodGet, "/users/"+i.AdminID+"/app-limit", nil).Code)
	require.Equal(t, http.StatusForbidden, i.do(other, http.MethodGet, "/groups/grp_missing/app-limit", nil).Code)
	require.Equal(t, http.StatusForbidden, i.do(other, http.MethodPut, "/groups/grp_missing/app-limit", map[string]any{"max_apps": 1}).Code)
}
