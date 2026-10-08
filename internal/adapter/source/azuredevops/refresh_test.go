package azuredevops

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

func TestRefresh(t *testing.T) {
	e := &entra{}
	srv := httptest.NewServer(e.handler(t))
	defer srv.Close()
	ctx := context.Background()

	tokenConn := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "credentials": map[string]string{"token": pat}}, "")
	if got, err := tokenConn.Refresh(ctx); got != nil || err != nil {
		t.Fatalf("token Refresh = %v, %v", got, err)
	}
	sp := mustConfigure(t, map[string]any{"method": "service_principal", "scope": "acme", "client_id": "app-1", "tenant_id": "t1",
		"credentials": map[string]string{"client_secret": clientSecret}}, srv.URL)
	if got, err := sp.Refresh(ctx); got != nil || err != nil {
		t.Fatalf("service principal Refresh = %v, %v", got, err)
	}

	good := mustConfigure(t, map[string]any{"method": "oauth", "scope": "acme", "client_id": "app-1", "tenant_id": "t1",
		"credentials": map[string]string{"access_token": "access-0", "refresh_token": "refresh-0",
			"token_expires_at": fixedNow.Add(time.Hour).Format(time.RFC3339)}}, srv.URL)
	if got, err := good.Refresh(ctx); got != nil || err != nil {
		t.Fatalf("unexpired Refresh = %v, %v", got, err)
	}

	expired := mustConfigure(t, map[string]any{"method": "oauth", "scope": "acme", "client_id": "app-1", "tenant_id": "t1", "api_url": srv.URL,
		"credentials": map[string]string{"access_token": "access-0", "refresh_token": "refresh-0",
			"token_expires_at": fixedNow.Add(-time.Hour).Format(time.RFC3339)}}, srv.URL)
	got, err := expired.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["access_token"].Reveal() != "access-2" || got["refresh_token"].Reveal() != "refresh-2" {
		t.Fatal("Refresh did not return the renewed tokens")
	}

	// A listing never refreshes on its own: an expired token is an error.
	_, err = expired.ListRepositories(ctx, api.ListRepositoriesRequest{})
	if errs.CodeOf(err) != errs.StateInvalid || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("listing with an expired token: %v", err)
	}
	noSecret(t, err)
}
