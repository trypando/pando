//go:build integration

package httpapi_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/httpapi"
)

// gitSpec is minimalSpec built from a repository, deploying automatically
// with ad.
func gitSpec(ad map[string]any) map[string]any {
	s := minimalSpec()
	s["source"] = map[string]any{"type": "git", "url": "https://github.com/acme/notes", "ref": "main", "commit": "aaaaaaa"}
	s["deploy"] = map[string]any{"strategy": "recreate", "auto_deploy": ad}
	return s
}

// autoDeployView is GET /auto-deploy, as much of it as the tests read.
type autoDeployView struct {
	Settings struct {
		Enabled    bool   `json:"enabled"`
		Trigger    string `json:"trigger"`
		Branch     string `json:"branch"`
		TagPattern string `json:"tag_pattern"`
	} `json:"settings"`
	Deployed struct {
		Enabled bool `json:"enabled"`
	} `json:"deployed"`
	Pending          bool   `json:"pending"`
	WebhookURL       string `json:"webhook_url"`
	WebhookSecretSet bool   `json:"webhook_secret_set"`
}

func (i *install) autoDeploy(s *session, appID string) autoDeployView {
	i.t.Helper()
	got := i.do(s, http.MethodGet, "/apps/"+appID+"/auto-deploy", nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	var v autoDeployView
	got.JSON(i.t, &v)
	return v
}

// webhook sends a delivery as GitHub would, signed with key.
func (i *install) webhook(appID, event, key, body string) reply {
	i.t.Helper()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+appID+"/auto-deploy/webhook", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, req)
	return reply{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

func (i *install) rotateWebhookSecret(s *session, appID string) string {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/apps/"+appID+"/auto-deploy/webhook-secret", nil)
	require.Equal(i.t, http.StatusCreated, got.Code, got.String())
	var body struct {
		Secret string `json:"webhook_secret"`
		URL    string `json:"webhook_url"`
	}
	got.JSON(i.t, &body)
	require.True(i.t, strings.HasPrefix(body.Secret, "pdwh_"), got.String())
	// With no external URL configured, the address this request came in on.
	require.Equal(i.t, "http://example.com/api/v1/apps/"+appID+"/auto-deploy/webhook", body.URL)
	return body.Secret
}

// checked is the next check a webhook started, or "" if none starts soon.
func (i *install) checked(wait time.Duration) string {
	select {
	case c := <-i.Checked.apps:
		return c
	case <-time.After(wait):
		return ""
	}
}

// TestR261_AutoDeploySettingsAreChangedThroughTheAPI asserts that turning
// auto-deploy on and choosing its trigger is one endpoint every client uses:
// a revision the next deploy ships, the same refusals as a spec save, and
// nothing written when nothing changes.
func TestR261_AutoDeploySettingsAreChangedThroughTheAPI(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, gitSpec(map[string]any{"enabled": false})))

	before := i.autoDeploy(admin, appID)
	require.False(t, before.Settings.Enabled, "R-141: off by default")
	require.False(t, before.Pending)

	set := map[string]any{"enabled": true, "trigger": "release_tagged", "tag_pattern": "release-*"}
	got := i.do(admin, http.MethodPut, "/apps/"+appID+"/auto-deploy", set)
	require.Equal(t, http.StatusCreated, got.Code, got.String())

	after := i.autoDeploy(admin, appID)
	require.True(t, after.Settings.Enabled)
	require.Equal(t, "release_tagged", after.Settings.Trigger)
	require.Equal(t, "release-*", after.Settings.TagPattern)
	require.False(t, after.Deployed.Enabled, "saved, not deployed: it does nothing until the next deploy")
	require.True(t, after.Pending)

	again := i.do(admin, http.MethodPut, "/apps/"+appID+"/auto-deploy", set)
	require.Equal(t, http.StatusOK, again.Code, again.String())
	require.Contains(t, again.String(), `"changed":false`)

	bad := i.do(admin, http.MethodPut, "/apps/"+appID+"/auto-deploy", map[string]any{"enabled": true, "tag_pattern": "release-["})
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())
	require.Contains(t, bad.String(), "release-*", "R-105: the refusal says what a pattern looks like")
}

// The settings and the secret are the app's: somebody who cannot see the app
// is refused each endpoint, and a malformed or premature change is refused
// saying what to send.
func TestAutoDeployEndpointsRefuseWhatTheyShould(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")

	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/auto-deploy"},
		{http.MethodPut, "/auto-deploy"},
		{http.MethodPost, "/auto-deploy/webhook-secret"},
		{http.MethodDelete, "/auto-deploy/webhook-secret"},
	} {
		got := i.anon(call.method, "/apps/"+appID+call.path, map[string]any{"enabled": true})
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, got.Code,
			"%s %s: %s", call.method, call.path, got.String())
	}

	unconfigured := i.do(admin, http.MethodPut, "/apps/"+appID+"/auto-deploy", map[string]any{"enabled": true})
	require.Equal(t, http.StatusBadRequest, unconfigured.Code, unconfigured.String())
	require.Contains(t, unconfigured.String(), "isn't configured yet")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/apps/"+appID+"/auto-deploy", strings.NewReader("{not json"))
	req.Header.Set("Cookie", httpapi.SessionCookie+"="+admin.cookie)
	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "branch_updated", "the refusal shows what to send")
}

// A verified webhook for an app that does not auto-deploy, or is not deployed
// yet, starts nothing and says why.
func TestR142_AWebhookForAnAppThatDoesNotAutoDeployStartsNothing(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	off := i.createApp(admin, "off")
	i.pinSpec(admin, off, i.writeSpec(admin, off, gitSpec(map[string]any{"enabled": false})))
	key := i.rotateWebhookSecret(admin, off)
	got := i.webhook(off, "push", key, `{"ref":"refs/heads/main"}`)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, got.String(), "does not deploy automatically")

	draft := i.createApp(admin, "draft")
	draftKey := i.rotateWebhookSecret(admin, draft)
	got = i.webhook(draft, "push", draftKey, `{"ref":"refs/heads/main"}`)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, got.String(), "isn't deployed yet")

	require.Equal(t, "", i.checked(100*time.Millisecond))
}

// TestR142_PollingIsTheDefaultAndAWebhookNeedsTheAppsSecret asserts that an
// app has no webhook until somebody makes it a secret — polling needs no
// inbound connection, so it is what an app gets — and that every refusal of
// a webhook is the same refusal.
func TestR142_PollingIsTheDefaultAndAWebhookNeedsTheAppsSecret(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, gitSpec(map[string]any{"enabled": true, "branch": "main"})))

	require.False(t, i.autoDeploy(admin, appID).WebhookSecretSet)

	push := `{"ref":"refs/heads/main"}`
	noSecret := i.webhook(appID, "push", "a-guess", push)
	require.Equal(t, http.StatusUnauthorized, noSecret.Code, noSecret.String())

	key := i.rotateWebhookSecret(admin, appID)
	wrong := i.webhook(appID, "push", key+"x", push)
	require.Equal(t, http.StatusUnauthorized, wrong.Code)
	unknown := i.webhook("app_01HQ8ZZZZZZZZZZZZZZZZZZZZZ", "push", key, push)
	require.Equal(t, http.StatusUnauthorized, unknown.Code)
	require.Equal(t, wrong.ErrorCode(), unknown.ErrorCode())
	require.Equal(t, "", i.checked(100*time.Millisecond), "nothing unverified starts a check")

	require.True(t, i.autoDeploy(admin, appID).WebhookSecretSet)
	require.NotContains(t, i.do(admin, http.MethodGet, "/apps/"+appID+"/auto-deploy", nil).String(), key,
		"R-194: the secret is shown once, when it is made")

	require.Equal(t, http.StatusNoContent, i.do(admin, http.MethodDelete, "/apps/"+appID+"/auto-deploy/webhook-secret", nil).Code)
	require.Equal(t, http.StatusUnauthorized, i.webhook(appID, "push", key, push).Code, "removing it turns the webhook off")
}

// TestR142_AVerifiedWebhookStartsACheckThroughTheSamePath asserts that a
// signed push to the branch an app follows asks the auto-deploy job to check
// it — the job reads the repository and deploys the ordinary way; the
// webhook deploys nothing itself — and that a push elsewhere, or a secret
// that has been replaced, starts nothing.
func TestR142_AVerifiedWebhookStartsACheckThroughTheSamePath(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, gitSpec(map[string]any{"enabled": true, "branch": "main"})))
	key := i.rotateWebhookSecret(admin, appID)

	ping := i.webhook(appID, "ping", key, `{"zen":"Keep it logically awesome."}`)
	require.Equal(t, http.StatusOK, ping.Code, ping.String())

	got := i.webhook(appID, "push", key, `{"ref":"refs/heads/main","after":"ffffffff"}`)
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	require.Equal(t, appID+" webhook", i.checked(5*time.Second))

	elsewhere := i.webhook(appID, "push", key, `{"ref":"refs/heads/feature"}`)
	require.Equal(t, http.StatusOK, elsewhere.Code, elsewhere.String())
	require.Contains(t, elsewhere.String(), "follows main")
	release := i.webhook(appID, "release", key, `{"action":"published"}`)
	require.Equal(t, http.StatusOK, release.Code, "this app follows a branch, not releases")
	require.Equal(t, "", i.checked(100*time.Millisecond))

	i.rotateWebhookSecret(admin, appID)
	require.Equal(t, http.StatusUnauthorized, i.webhook(appID, "push", key, `{"ref":"refs/heads/main"}`).Code,
		"a replaced secret stops verifying at once")
}
