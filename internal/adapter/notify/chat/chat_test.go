package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

func configure(t *testing.T, a *Adapter, webhook string) error {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"credentials": map[string]any{"webhook_url": webhook}})
	return a.Configure(context.Background(), raw)
}

var sample = api.Notification{
	Kind:    "deploy.failed",
	Subject: "billing: a deploy failed",
	Body:    "The build exited with status 1.",
	Fields:  []api.NotificationField{{Label: "Error code", Value: "BUILD_FAILED"}},
	Link:    "https://pando.example.com/admin/apps/app_1",
}

// TestR374_ChatAdaptersAreChannelsAndKeepTheURLSecret asserts R-374 and
// R-373: Slack, Teams and Discord post to a channel, so Pando's own
// person-addressed notifications never go to them; and the webhook URL, being
// the credential, is refused in the stored configuration.
func TestR374_ChatAdaptersAreChannelsAndKeepTheURLSecret(t *testing.T) {
	for _, p := range []Platform{Slack, Teams, Discord} {
		a := New(p)
		require.Equal(t, api.AudienceChannel, a.Capabilities().Audience, p.Kind)

		err := a.Configure(context.Background(), json.RawMessage(`{"webhook_url":"https://hooks.slack.com/x"}`))
		require.ErrorContains(t, err, "set it as a credential", p.Kind)

		require.ErrorContains(t, a.Configure(context.Background(), nil), "no webhook URL", p.Kind)
		require.ErrorContains(t, configure(t, a, "http://insecure.example.com/x"), "not an https address", p.Kind)
		require.NoError(t, p.Info().Validate(), p.Kind)
	}
}

func TestPlatformsRefuseAnotherPlatformsURL(t *testing.T) {
	require.ErrorContains(t, configure(t, New(Slack), "https://discord.com/api/webhooks/1/abc"), "not a Slack address")
	require.ErrorContains(t, configure(t, New(Discord), "https://hooks.slack.com/services/T/B/x"), "not a Discord address")
	require.NoError(t, configure(t, New(Slack), "https://hooks.slack.com/services/T/B/x"))
	require.NoError(t, configure(t, New(Discord), "https://discord.com/api/webhooks/1/abc"))
}

func TestTeamsPostsAnAdaptiveCard(t *testing.T) {
	var got map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(b, &got))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := New(Teams)
	require.NoError(t, configure(t, a, srv.URL+"/workflows/1"))
	a.client = srv.Client()
	require.NoError(t, a.Notify(context.Background(), sample))

	card := got["attachments"].([]any)[0].(map[string]any)
	require.Equal(t, "application/vnd.microsoft.card.adaptive", card["contentType"])
	encoded, _ := json.Marshal(card)
	require.Contains(t, string(encoded), "BUILD_FAILED")
	require.Contains(t, string(encoded), "Open in Pando")
}

func TestAFailedPostDoesNotRevealTheURL(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such hook", http.StatusNotFound)
	}))
	defer srv.Close()
	a := New(Teams)
	require.NoError(t, configure(t, a, srv.URL+"/workflows/secret-token"))
	a.client = srv.Client()
	err := a.Notify(context.Background(), sample)
	require.ErrorContains(t, err, "404")
	require.NotContains(t, err.Error(), "secret-token")
}

func TestSlackEscapesAndDiscordMentionsNobody(t *testing.T) {
	n := sample
	n.Subject = "<!channel> & friends"
	msg := slackMessage(n).(map[string]any)
	require.Equal(t, "&lt;!channel&gt; &amp; friends", msg["text"], "the preview is escaped too")
	section := msg["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)
	require.Contains(t, section["text"], "&lt;!channel&gt; &amp; friends")

	d, _ := json.Marshal(discordMessage(n))
	require.Contains(t, string(d), `"allowed_mentions":{"parse":[]}`)
}

func TestClipKeepsRunesWhole(t *testing.T) {
	s := strings.Repeat("é", 10)
	out := clip(s, 7)
	require.LessOrEqual(t, len(out), 7)
	require.True(t, strings.HasSuffix(out, "…"))
	require.Equal(t, "short", clip("short", 10))
}
