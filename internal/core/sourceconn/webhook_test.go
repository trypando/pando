package sourceconn_test

import (
	"context"
	"net/url"
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
	require.NoError(t, s.CompleteInstallation(ctx(), "src_fake", "7", beginWeb(t, s)))
	require.Equal(t, "good", store["src_fake"]["access_token"].Reveal(), "what the adapter returned is stored")

	err := s.CompleteInstallation(ctx(), "src_gone", "7", "src_gone.x")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	s.Credentials = nil
	err = s.CompleteInstallation(ctx(), "src_fake", "7", beginWeb(t, s))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
}

// TestR091_AnInstallationIsKeptOnlyForTheBrowserPandoSentThere asserts that
// the setup address a provider returns to after an installation changes the
// connection only with the state Pando opened the installation page with: a
// link somebody else sends an administrator, with no state or a made-up one,
// or one for another connection, changes nothing.
func TestR091_AnInstallationIsKeptOnlyForTheBrowserPandoSentThere(t *testing.T) {
	store := creds{}
	s := service(configs{
		row("src_fake", "fake", map[string]any{"host": "forge.example"}),
		row("src_other", "fake", map[string]any{"host": "forge.example"}),
	}, store, new(int))

	for _, state := range []string{"", "no-dot", "src_fake.forged"} {
		err := s.CompleteInstallation(ctx(), "src_fake", "7", state)
		require.Error(t, err, "%q, with no installation started", state)
	}
	beginWeb(t, s)
	for _, state := range []string{"", "no-dot", "src_fake.forged"} {
		err := s.CompleteInstallation(ctx(), "src_fake", "7", state)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), state)
	}
	other, err := s.BeginAuthorization(ctx(), "src_other", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
	require.NoError(t, err)
	err = s.CompleteInstallation(ctx(), "src_fake", "7", stateOf(other.AuthorizeURL))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "another connection's state")
	require.Empty(t, store["src_fake"], "nothing was kept")

	state := beginWeb(t, s)
	require.NoError(t, s.CompleteInstallation(ctx(), "src_fake", "7", state))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(s.CompleteInstallation(ctx(), "src_fake", "7", state)),
		"a state is used once")
}

// beginWeb starts a browser authorization of src_fake and returns its state.
func beginWeb(t *testing.T, s *sourceconn.Service) string {
	t.Helper()
	a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
	if err != nil {
		require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
		return "src_fake.none"
	}
	return stateOf(a.AuthorizeURL)
}

// stateOf is the state an address carries.
func stateOf(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
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
		if want == "" {
			require.Empty(t, got, next)
			continue
		}
		// Carried on under a state of its own, which the installation's
		// setup address must be returned with.
		u, err := url.Parse(got)
		require.NoError(t, err)
		installState := u.Query().Get("state")
		require.True(t, strings.HasPrefix(installState, "src_fake."), got)
		require.NotEqual(t, state, installState, "a new state for the next step")
		u.RawQuery = ""
		require.Equal(t, want, u.String(), next)
		require.NoError(t, s.CompleteInstallation(ctx(), "src_fake", "7", installState))
	}
}

// failingCreds is a credential store whose writes of one field fail — or,
// with second set, whichever is written second — and whose deletes fail too
// when stuck is set.
type failingCreds struct {
	creds
	fails  string
	second bool
	stuck  bool
	puts   *int
}

func (f failingCreds) Put(ctx context.Context, id, field string, v secret.Value) error {
	if f.puts != nil {
		*f.puts++
	}
	if field == f.fails || (f.second && *f.puts == 2) {
		return errs.New(errs.Internal, "the store is down")
	}
	return f.creds.Put(ctx, id, field, v)
}

func (f failingCreds) Delete(ctx context.Context, id, field string) error {
	if f.stuck {
		return errs.New(errs.Internal, "the store is down")
	}
	return f.creds.Delete(ctx, id, field)
}

// TestAnAuthorizationsCredentialsAreStoredAllOrNone asserts that a store
// failing partway through what an authorization returned leaves the
// connection's credentials as they were, rather than half of a GitHub App.
func TestAnAuthorizationsCredentialsAreStoredAllOrNone(t *testing.T) {
	for _, field := range []string{"access_token", "refresh_token"} {
		store := creds{"src_fake": {"access_token": secret.New("old")}}
		s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, new(int))
		s.Credentials = failingCreds{creds: store, fails: field}
		a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
		require.NoError(t, err)
		state := a.AuthorizeURL[strings.Index(a.AuthorizeURL, "state=")+len("state="):]
		_, _, err = s.CompleteWebAuthorization(ctx(), state, "code")
		require.Error(t, err, field)
		require.Equal(t, map[string]secret.Value{"access_token": secret.New("old")}, store["src_fake"], field)
	}
}

// TestADeclaredConnectionIsNotAuthorized asserts that a connection the
// configuration file declares is refused before the provider is asked:
// what authorizing returns would have nowhere to be stored.
func TestADeclaredConnectionIsNotAuthorized(t *testing.T) {
	s := service(configs{}, creds{}, new(int))
	s.Declared = map[string]api.SourceAdapter{"src_file": s.New("fake")}
	_, err := s.BeginAuthorization(ctx(), "src_file", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "declared in the configuration file")
}

// TestACredentialStoreThatCannotBePutBackSaysTheConnectionNeedsRepair asserts
// that when the fields already written cannot be restored either, the error
// says so and what to do, rather than the bare store failure.
func TestACredentialStoreThatCannotBePutBackSaysTheConnectionNeedsRepair(t *testing.T) {
	store := creds{}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, new(int))
	s.Credentials = failingCreds{creds: store, second: true, stuck: true, puts: new(int)}
	a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
	require.NoError(t, err)
	state := a.AuthorizeURL[strings.Index(a.AuthorizeURL, "state=")+len("state="):]
	_, _, err = s.CompleteWebAuthorization(ctx(), state, "code")
	require.Error(t, err)
	require.Contains(t, err.Error(), "nor put back")
	require.Contains(t, errs.As(err).Remedy, "Disconnect the connection")
}

// fakeNextBare is fakeNext answering with no credentials, or failing.
type fakeNextBare struct {
	fakeNext
	fail bool
}

func (f fakeNextBare) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if f.fail {
		return api.AuthorizationResult{}, errs.New(errs.AdapterFailed, "the provider refused")
	}
	res, err := f.fakeNext.CompleteAuthorization(ctx, req)
	res.Credentials = nil
	return res, err
}

// TestR091_ANextStepWithoutAnythingToKeepIsRefused asserts that a provider
// answering a web authorization with a next step but nothing to keep, or
// refusing it, leaves no installation step open.
func TestR091_ANextStepWithoutAnythingToKeepIsRefused(t *testing.T) {
	for _, fail := range []bool{false, true} {
		s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, creds{}, new(int))
		inner := s.New
		s.New = func(kind string) api.SourceAdapter {
			return fakeNextBare{fakeNext: fakeNext{fake: inner(kind).(*fake), next: "https://forge.example/apps/p/installations/new"}, fail: fail}
		}
		a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, sourceconn.Addresses{Callback: "https://pando.example/cb"})
		require.NoError(t, err)
		_, next, err := s.CompleteWebAuthorization(ctx(), stateOf(a.AuthorizeURL), "code")
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), "fail=%v", fail)
		require.Empty(t, next)
	}
}
