//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/notice"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type listedNotice struct {
	Key         string `json:"key"`
	Message     string `json:"message"`
	Fingerprint string `json:"fingerprint"`
	Ignored     bool   `json:"ignored"`
}

// notices is GET /apps/{id}/notices for one caller, by key.
func (i *install) notices(s *session, appID string) map[string]listedNotice {
	i.t.Helper()
	got := i.do(s, http.MethodGet, "/apps/"+appID+"/notices", nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	var body struct {
		Notices []listedNotice `json:"notices"`
	}
	got.JSON(i.t, &body)
	require.NotNil(i.t, body.Notices, "an empty list is [], not null")
	out := map[string]listedNotice{}
	for _, n := range body.Notices {
		out[n.Key] = n
	}
	return out
}

// specWithEnv is minimalSpec with these variables on its one workload.
func specWithEnv(env ...map[string]any) map[string]any {
	body := minimalSpec()
	body["workloads"].([]map[string]any)[0]["env"] = env
	return body
}

func emptyVar(key string) map[string]any { return map[string]any{"key": key, "value": ""} }

// TestR412_IgnoringHidesANoticeForThatPersonOnly asserts R-412: a notice one
// person ignores is still shown, unignored, to everyone else who can see the
// app — and the ignore is kept by Pando, so the API reads it back.
func TestR412_IgnoringHidesANoticeForThatPersonOnly(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, specWithEnv(emptyVar("B_KEY"), emptyVar("A_KEY"))))

	other := i.user("colleague")
	granted := i.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": i.userID(other), "role_id": authz.RoleViewer,
	})
	require.Equal(t, http.StatusCreated, granted.Code, granted.String())

	before := i.notices(admin, appID)
	unset, ok := before[notice.KeyUnsetVars]
	require.True(t, ok, "the empty variables are a notice: %v", before)
	require.Equal(t, "2 variables are declared with no value: B_KEY, A_KEY.", unset.Message)
	require.False(t, unset.Ignored)

	ignored := i.do(admin, http.MethodPut, "/apps/"+appID+"/notices/"+notice.KeyUnsetVars+"/ignore", nil)
	require.Equal(t, http.StatusOK, ignored.Code, ignored.String())
	var n listedNotice
	ignored.JSON(t, &n)
	require.True(t, n.Ignored)
	require.Equal(t, unset.Fingerprint, n.Fingerprint)

	require.True(t, i.notices(admin, appID)[notice.KeyUnsetVars].Ignored, "ignored for the person who ignored it")
	require.False(t, i.notices(other, appID)[notice.KeyUnsetVars].Ignored, "and for nobody else")

	// The other person can ignore it for themselves with only app.view.
	require.Equal(t, http.StatusOK,
		i.do(other, http.MethodPut, "/apps/"+appID+"/notices/"+notice.KeyUnsetVars+"/ignore", nil).Code)
	require.True(t, i.notices(other, appID)[notice.KeyUnsetVars].Ignored)

	// A notice the app does not have now cannot be ignored: there is nothing
	// to record what it said.
	missing := i.do(admin, http.MethodPut, "/apps/"+appID+"/notices/backup.not_taken/ignore", nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.String())
	require.Equal(t, string(errs.NotFound), missing.ErrorCode())

	// Somebody who cannot see the app sees none of its notices.
	stranger := i.user("stranger")
	denied := i.do(stranger, http.MethodGet, "/apps/"+appID+"/notices", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())
	deniedIgnore := i.do(stranger, http.MethodPut, "/apps/"+appID+"/notices/"+notice.KeyUnsetVars+"/ignore", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, deniedIgnore.Code, deniedIgnore.String())
}

// TestR412_AnIgnoredNoticeComesBackWhenItChanges asserts R-412: an ignore is
// keyed on what the notice says, so another empty variable, or another edit
// to the configuration, brings it back.
func TestR412_AnIgnoredNoticeComesBackWhenItChanges(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, specWithEnv(emptyVar("A_KEY"))))

	// A revision that is not deployed yet.
	i.writeSpec(admin, appID, specWithEnv(emptyVar("A_KEY")))
	got := i.notices(admin, appID)
	require.Contains(t, got, notice.KeyUnshipped)
	require.Equal(t, "The configuration has changed since this app was last deployed. Deploy to apply it.",
		got[notice.KeyUnshipped].Message)
	require.Equal(t, "A_KEY is declared with no value.", got[notice.KeyUnsetVars].Message)

	for _, key := range []string{notice.KeyUnshipped, notice.KeyUnsetVars} {
		require.Equal(t, http.StatusOK, i.do(admin, http.MethodPut, "/apps/"+appID+"/notices/"+key+"/ignore", nil).Code)
	}
	got = i.notices(admin, appID)
	require.True(t, got[notice.KeyUnshipped].Ignored)
	require.True(t, got[notice.KeyUnsetVars].Ignored)

	// Another edit, and another empty variable once it is pinned: both say
	// something new, so both come back.
	rev := i.writeSpec(admin, appID, specWithEnv(emptyVar("A_KEY"), emptyVar("B_KEY")))
	got = i.notices(admin, appID)
	require.False(t, got[notice.KeyUnshipped].Ignored, "a newer revision brings the notice back")
	require.True(t, got[notice.KeyUnsetVars].Ignored, "the pinned spec has not changed")

	i.pinSpec(admin, appID, rev)
	got = i.notices(admin, appID)
	require.NotContains(t, got, notice.KeyUnshipped, "the newest revision is pinned")
	require.False(t, got[notice.KeyUnsetVars].Ignored, "another empty variable brings it back")
	require.Equal(t, "2 variables are declared with no value: A_KEY, B_KEY.", got[notice.KeyUnsetVars].Message)

	// Filling them all makes the notice go, whatever was ignored.
	filled := specWithEnv(map[string]any{"key": "A_KEY", "value": "a"}, map[string]any{"key": "B_KEY", "value": "b"})
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, filled))
	require.NotContains(t, i.notices(admin, appID), notice.KeyUnsetVars)
}

// TestR412_RestoringBringsItBack asserts R-412: an ignored notice can be
// shown again, and restoring one that is not ignored is not an error.
func TestR412_RestoringBringsItBack(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, specWithEnv(emptyVar("A_KEY"))))

	path := "/apps/" + appID + "/notices/" + notice.KeyUnsetVars + "/ignore"
	require.Equal(t, http.StatusOK, i.do(admin, http.MethodPut, path, nil).Code)
	require.True(t, i.notices(admin, appID)[notice.KeyUnsetVars].Ignored)

	for range 2 {
		got := i.do(admin, http.MethodDelete, path, nil)
		require.Equal(t, http.StatusNoContent, got.Code, got.String())
	}
	require.False(t, i.notices(admin, appID)[notice.KeyUnsetVars].Ignored)

	// Anonymous callers have nothing to ignore with.
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, i.anon(http.MethodPut, path, nil).Code)
}

// An installation wired without notices says so on every notice route rather
// than answering with an empty list.
func TestNoticesUnavailableSaySo(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	i.Server.Notices = nil

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		path := "/apps/" + appID + "/notices"
		if method != http.MethodGet {
			path += "/" + notice.KeyUnsetVars + "/ignore"
		}
		got := i.do(admin, method, path, nil)
		require.Equal(t, string(errs.AdapterUnavailable), got.ErrorCode(), "%s %s: %s", method, path, got.String())
	}
}

// noticeScanner is a configured scanner; the test records the scan itself.
type noticeScanner struct{}

func (s *noticeScanner) Kind() string                                     { return "fixed" }
func (s *noticeScanner) Category() adapterapi.Category                    { return adapterapi.CategoryScanner }
func (s *noticeScanner) Configure(context.Context, json.RawMessage) error { return nil }
func (s *noticeScanner) HealthCheck(context.Context) error                { return nil }
func (s *noticeScanner) ScannerCapabilities() adapterapi.ScannerCapabilities {
	return adapterapi.ScannerCapabilities{ScansImages: true, ScansSource: true}
}
func (s *noticeScanner) Scan(context.Context, adapterapi.ScanRequest) (adapterapi.ScanResult, error) {
	return adapterapi.ScanResult{Scanner: "fixed 1.0", Ran: time.Now().UTC()}, nil
}

// TestR412_IgnoringDoesNotUnblockADeploy asserts R-412: ignoring the notice
// that an app's security score is below the installation's minimum hides the
// reminder and nothing more. The deploy gate (R-314) is asked exactly as a
// deploy asks it, after the ignore, and still refuses.
func TestR412_IgnoringDoesNotUnblockADeploy(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, minimalSpec()))
	app, found, err := i.Apps.ByID(context.Background(), appID)
	require.NoError(t, err)
	require.True(t, found)

	require.NoError(t, i.Server.Registry.Register("scn_fixed", &noticeScanner{}))
	require.NoError(t, i.Server.Registry.SetDefault(adapterapi.CategoryScanner, "scn_fixed"))
	ctx := context.Background()
	doc, err := i.PolicyStore.Load(ctx)
	require.NoError(t, err)
	doc.MinSecurityScore = 80
	require.NoError(t, i.PolicyStore.Save(ctx, doc, i.AdminID))

	score := 40
	_, err = state.NewScans(i.db).Record(ctx, state.Scan{
		AppID: appID, SpecID: app.PinnedSpecID, Image: "sha256:" + "ab", ScannerRef: "scn_fixed",
		Score: &score, ScoreFixable: &score, RanAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	got := i.notices(admin, appID)
	require.Contains(t, got, notice.KeySecurity)
	require.Contains(t, got[notice.KeySecurity].Message, "security score is 40, below this installation's requirement of 80")

	ignored := i.do(admin, http.MethodPut, "/apps/"+appID+"/notices/"+notice.KeySecurity+"/ignore", nil)
	require.Equal(t, http.StatusOK, ignored.Code, ignored.String())
	require.True(t, i.notices(admin, appID)[notice.KeySecurity].Ignored)

	// What the deploy runner asks before it deploys (deploy.Runner.allowed).
	gate := i.Server.Security.(*security.Service)
	standing, err := gate.Allows(ctx, appID, app.PinnedSpecID)
	require.NoError(t, err)
	require.False(t, standing.Deployable(), "an ignored notice does not make the app deployable")
	require.Equal(t, errs.PlanSecurityBelowThreshold, errs.CodeOf(security.Refusal(standing, nil)))

	// And the security report still says so, ignored or not.
	report := i.do(admin, http.MethodGet, "/apps/"+appID+"/security", nil)
	require.Equal(t, http.StatusOK, report.Code, report.String())
	require.Contains(t, report.String(), `"verdict":"insecure"`)
}
