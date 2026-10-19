package sourceconn_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR142_AWebhookIsOfferedOnlyWhereAProviderCanReachIt asserts which of
// Pando's addresses count as public: a provider refuses a webhook on any
// other, and a GitHub App with it.
func TestR142_AWebhookIsOfferedOnlyWhereAProviderCanReachIt(t *testing.T) {
	for addr, public := range map[string]bool{
		"https://pando.acme.com/api/v1/sources/x/webhook": true,
		"http://203.0.113.9:8080/hook":                    true,
		"http://localhost:8080/hook":                      false,
		"http://127.0.0.1/hook":                           false,
		"http://10.1.2.3/hook":                            false,
		"http://192.168.1.4/hook":                         false,
		"http://[fe80::1]/hook":                           false,
		"http://pando/hook":                               false,
		"http://pando.local/hook":                         false,
		"https://pando.corp.internal/hook":                false,
		"ftp://pando.acme.com/hook":                       false,
		"":                                                false,
	} {
		require.Equal(t, public, sourceconn.Public(addr), addr)
	}
}

// TestAConnectionWithoutAWebhookHasNoSecret asserts WebhookSecret answers
// "none" rather than an error for a connection that has no webhook, one that
// does not exist, and one declared in the configuration file.
func TestAConnectionWithoutAWebhookHasNoSecret(t *testing.T) {
	store := creds{"src_fake": {api.CredentialWebhookSecret: secret.New("whsec")}}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, new(int))

	for _, id := range []string{"src_fake", "src_gone"} {
		_, found, err := s.WebhookSecret(ctx(), id)
		require.NoError(t, err, id)
		require.False(t, found, "%s has no webhook of its own", id)
	}
	var none *sourceconn.Service
	_, found, err := none.WebhookSecret(ctx(), "src_fake")
	require.NoError(t, err)
	require.False(t, found)
}

// TestAnInstallationIsKeptOnlyForAStoredConnection asserts that finishing an
// installation is refused for a connection Pando cannot write to, and passes
// on what the adapter refuses.
func TestAnInstallationIsKeptOnlyForAStoredConnection(t *testing.T) {
	store := creds{}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, new(int))
	require.NoError(t, s.CompleteInstallation(ctx(), "src_fake", "7"))
	require.Equal(t, "good", store["src_fake"]["access_token"].Reveal(), "what the adapter returned is stored")

	err := s.CompleteInstallation(ctx(), "src_gone", "7")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	s.Credentials = nil
	err = s.CompleteInstallation(ctx(), "src_fake", "7")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
}

// fakeNext is the fake adapter, whose web authorization has a next step at
// the address it is given.
type fakeNext struct {
	*fake
	next string
}

func (f fakeNext) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	res, err := f.fake.CompleteAuthorization(ctx, req)
	res.ContinueURL = f.next
	return res, err
}

// TestANextStepIsOnlyEverOnTheConnectionsOwnHost asserts the callback is
// handed a provider's next step — a GitHub App's installation — only when it
// is on the host the connection is for, never anywhere else.
func TestANextStepIsOnlyEverOnTheConnectionsOwnHost(t *testing.T) {
	for next, want := range map[string]string{
		"https://forge.example/apps/p/installations/new": "https://forge.example/apps/p/installations/new",
		"https://evil.example/phish":                     "",
		"javascript:alert(1)":                            "",
		"/relative":                                      "",
	} {
		s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, creds{}, new(int))
		inner := s.New
		s.New = func(kind string) api.SourceAdapter {
			return fakeNext{fake: inner(kind).(*fake), next: next}
		}
		a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
		require.NoError(t, err)
		state := a.AuthorizeURL[strings.Index(a.AuthorizeURL, "state=")+len("state="):]
		_, got, err := s.CompleteWebAuthorization(ctx(), state, "code")
		require.NoError(t, err)
		require.Equal(t, want, got, next)
	}
}
