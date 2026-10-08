package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

func TestRefreshIsNilForEveryMethodButOAuth(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"method": "token", "credentials": map[string]string{"token": tokenSecret}},
		{"method": "token", "token_type": "deploy_token", "username": "u", "credentials": map[string]string{"token": tokenSecret}},
		{"method": "ssh", "credentials": map[string]string{"ssh_private_key": testKey(t)}},
	} {
		a := mustConfigure(t, cfg)
		got, err := a.Refresh(context.Background())
		if got != nil || err != nil {
			t.Errorf("%v: Refresh = %v, %v", cfg["method"], got, err)
		}
	}
}

func TestRefreshRenewsOnlyAnExpiredOAuthToken(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		if r.URL.Path != "/oauth/token" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != refreshSecret {
			t.Errorf("refresh request = %s %v", r.URL.Path, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":7200}`)
	}))
	defer srv.Close()

	build := func(expires time.Time) *Adapter {
		a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id", "credentials": map[string]string{
			"access_token": accessSecret, "refresh_token": refreshSecret, "token_expires_at": expires.Format(time.RFC3339),
		}})
		point(a, srv)
		a.now = func() time.Time { return now }
		return a
	}

	got, err := build(now.Add(time.Hour)).Refresh(context.Background())
	if got != nil || err != nil || calls != 0 {
		t.Fatalf("a good token: Refresh = %v, %v after %d calls", got, err, calls)
	}

	a := build(now.Add(-time.Minute))
	got, err = a.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got["access_token"].Reveal() != "new-access" || got["refresh_token"].Reveal() != "new-refresh" ||
		got["token_expires_at"].Reveal() != now.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("refreshed = %v after %d calls", got, calls)
	}

	// Listing never refreshes on its own: with the token still expired it
	// says to authorize again, and makes no call.
	_, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	e := errs.As(err)
	if e == nil || e.Code != errs.StateInvalid || !strings.Contains(e.Remedy, "Authorize the connection again") || calls != 1 {
		t.Fatalf("listing with an expired token: %v after %d calls", err, calls)
	}
	noSecret(t, err)
}

func TestRefreshWithNoRefreshTokenSaysToAuthorizeAgain(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id", "credentials": map[string]string{
		"access_token": accessSecret, "token_expires_at": now.Add(-time.Hour).Format(time.RFC3339),
	}})
	a.now = func() time.Time { return now }
	got, err := a.Refresh(context.Background())
	if got != nil || errs.CodeOf(err) != errs.StateInvalid {
		t.Fatalf("Refresh = %v, %v", got, err)
	}
	noSecret(t, err)

	unauthorized := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id"})
	if _, err := unauthorized.Refresh(context.Background()); errs.CodeOf(err) != errs.StateInvalid {
		t.Fatalf("an unauthorized connection: %v", err)
	}
}
