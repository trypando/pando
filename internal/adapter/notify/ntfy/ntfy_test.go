package ntfy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR374_NtfyPublishesToItsTopic asserts R-374: ntfy publishes to the
// configured topic with the subject as its title, and keeps the topic and
// token out of the stored configuration.
func TestR374_NtfyPublishesToItsTopic(t *testing.T) {
	var path, title, auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, title, auth = r.URL.Path, r.Header.Get("Title"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()

	a := New()
	require.Equal(t, api.AudienceChannel, a.Capabilities().Audience)
	require.ErrorContains(t, a.Configure(context.Background(), json.RawMessage(`{"topic":"x"}`)), "set it as a credential")
	require.ErrorContains(t, a.Configure(context.Background(), nil), "no topic")

	raw, _ := json.Marshal(map[string]any{
		"server_url":  srv.URL,
		"credentials": map[string]any{"topic": "pando-alerts", "access_token": "tk_1"},
	})
	require.NoError(t, a.Configure(context.Background(), raw))
	require.NoError(t, a.Notify(context.Background(), api.Notification{
		Subject: "billing is degraded\n(was running)", Body: "Health checks are failing.",
		Fields: []api.NotificationField{{Label: "From", Value: "running"}},
	}))
	require.Equal(t, "/pando-alerts", path)
	require.Equal(t, "billing is degraded (was running)", title)
	require.Equal(t, "Bearer tk_1", auth)
	require.Contains(t, body, "From: running")
	require.NoError(t, Info().Validate())
}
