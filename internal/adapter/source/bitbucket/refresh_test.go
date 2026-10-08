package bitbucket

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

func TestRefresh(t *testing.T) {
	srv := oauthServer(t)
	defer srv.Close()
	ctx := context.Background()

	tokenConn := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tok}}, "")
	if got, err := tokenConn.Refresh(ctx); got != nil || err != nil {
		t.Fatalf("token Refresh = %v, %v", got, err)
	}
	cfg := func(expires time.Time) map[string]any {
		return map[string]any{"method": "oauth", "client_id": "key-1", "api_url": srv.URL,
			"credentials": map[string]string{"client_secret": clientSecret, "access_token": "access-1",
				"refresh_token": "refresh-1", "token_expires_at": expires.Format(time.RFC3339)}}
	}
	if got, err := mustConfigure(t, cfg(fixedNow.Add(time.Hour)), srv.URL).Refresh(ctx); got != nil || err != nil {
		t.Fatalf("unexpired Refresh = %v, %v", got, err)
	}
	expired := mustConfigure(t, cfg(fixedNow.Add(-time.Hour)), srv.URL)
	got, err := expired.Refresh(ctx)
	if err != nil || got["access_token"].Reveal() != "access-2" || got["refresh_token"].Reveal() != "refresh-1" {
		t.Fatalf("expired Refresh = %v, %v", got, err)
	}

	// A listing never refreshes on its own: an expired token is an error.
	_, err = expired.ListRepositories(ctx, api.ListRepositoriesRequest{})
	if errs.CodeOf(err) != errs.StateInvalid || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("listing with an expired token: %v", err)
	}
	noSecret(t, err)
}
