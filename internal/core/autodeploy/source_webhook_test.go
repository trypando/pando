package autodeploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type sourceSecrets map[string]string

func (s sourceSecrets) WebhookSecret(_ context.Context, id string) (secret.Value, bool, error) {
	v, ok := s[id]
	return secret.New(v), ok, nil
}

func sign(key, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestR142_ASourceWebhookIsVerifiedWithTheConnectionsSecret asserts a
// delivery to a connection's webhook is refused the same way whatever is
// wrong with it — no such connection, no webhook, a wrong signature — and
// that one with nothing to act on is answered without looking for apps.
func TestR142_ASourceWebhookIsVerifiedWithTheConnectionsSecret(t *testing.T) {
	s := &Service{Sources: sourceSecrets{"src_gh": "whsec"}}
	body := `{"zen":"Design for failure."}`

	for name, tc := range map[string]struct{ id, sig string }{
		"wrong secret":     {"src_gh", sign("other", body)},
		"no signature":     {"src_gh", ""},
		"not hex":          {"src_gh", "sha256=zz"},
		"other connection": {"src_other", sign("whsec", body)},
		"no connection":    {"", sign("whsec", body)},
	} {
		_, err := s.DeliverForSource(context.Background(), tc.id, Delivery{Event: "ping", Signature: tc.sig, Body: []byte(body)})
		require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), name)
	}
	_, err := (&Service{}).DeliverForSource(context.Background(), "src_gh",
		Delivery{Event: "ping", Signature: sign("whsec", body), Body: []byte(body)})
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), "an installation with no source connections")

	out, err := s.DeliverForSource(context.Background(), "src_gh", Delivery{Event: "ping", Signature: sign("whsec", body), Body: []byte(body)})
	require.NoError(t, err)
	require.False(t, out.Checking)
	require.Contains(t, out.Message, "test delivery")

	push := `{"ref":"refs/heads/main"}`
	out, err = s.DeliverForSource(context.Background(), "src_gh", Delivery{Event: "push", Signature: sign("whsec", push), Body: []byte(push)})
	require.NoError(t, err)
	require.False(t, out.Checking)
	require.Contains(t, out.Message, "names no repository")
}

func TestRepositoriesCompareByHostAndPath(t *testing.T) {
	for _, addr := range []string{
		"https://github.com/Acme/API.git",
		"https://github.com/acme/api",
		"https://github.com/acme/api/",
		"git@github.com:acme/api.git",
		"ssh://git@github.com/acme/api",
		"https://x-access-token@github.com/acme/api.git",
	} {
		require.Equal(t, "github.com/acme/api", repoKey(addr), addr)
	}
	for _, addr := range []string{"", "github.com", "https://github.com/", "not a url at all", "::"} {
		require.Empty(t, repoKey(addr), addr)
	}
}
