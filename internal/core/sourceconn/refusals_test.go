package sourceconn_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/generic"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// limited is the fake with the capabilities a test sets, and an
// authorization that can finish without a token.
type limited struct {
	*fake
	caps  api.SourceCapabilities
	empty bool
}

func (l *limited) SourceCapabilities() api.SourceCapabilities {
	c := l.caps
	c.Host = l.cfg.Host
	return c
}

func (l *limited) CompleteAuthorization(context.Context, api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if l.empty {
		return api.AuthorizationResult{}, nil
	}
	return api.AuthorizationResult{Credentials: map[string]secret.Value{"access_token": secret.New("good")}}, nil
}

// withKind adds one adapter kind to a service built by service().
func withKind(s *sourceconn.Service, kind string, build func() api.SourceAdapter) *sourceconn.Service {
	next := s.New
	s.New = func(k string) api.SourceAdapter {
		if k == kind {
			return build()
		}
		return next(k)
	}
	return s
}

// unopenable is a credential store whose credentials cannot be opened.
type unopenable struct{ creds }

func (unopenable) Resolve(context.Context, string) (map[string]secret.Value, error) {
	return nil, errors.New("the key that sealed these is gone")
}

func gitRow(id string) state.AdapterConfig {
	return row(id, generic.Kind, map[string]any{"method": "token", "host": "git.example.com"})
}

// TestR091_SavingAConnectionIsCheckedWithTheCredentialsItWouldHave asserts
// that Validate configures the connection as it would be saved: the stored
// credentials kept unless replaced, an empty one removing what is stored.
func TestR091_SavingAConnectionIsCheckedWithTheCredentialsItWouldHave(t *testing.T) {
	store := creds{"src_git": {"token": secret.New("t1")}}
	s := service(configs{gitRow("src_git")}, store, new(int))
	cfg := json.RawMessage(`{"method":"token","host":"git.example.com"}`)

	require.NoError(t, s.Validate(ctx(), "src_git", generic.Kind, cfg, nil),
		"the stored token is kept when the settings are saved without one")
	require.NoError(t, s.Validate(ctx(), "src_git", generic.Kind, cfg,
		map[string]secret.Value{"token": secret.New("t2")}))

	err := s.Validate(ctx(), "src_git", generic.Kind, cfg, map[string]secret.Value{"token": {}})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "needs a token", "an empty token removes the stored one")

	err = s.Validate(ctx(), "", generic.Kind, cfg, nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "a new connection has nothing stored")

	err = s.Validate(ctx(), "", "sourcehut", cfg, nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), `no source adapter of kind "sourcehut"`)

	err = s.Validate(ctx(), "", generic.Kind, json.RawMessage(`[1]`), nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "could not be read")

	// An adapter refusing with a plain error is reported without its text.
	err = s.Validate(ctx(), "", "fake", json.RawMessage(`{"host":5}`), nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "settings were refused")

	// A build that cannot run source connections has nothing to check.
	require.NoError(t, (&sourceconn.Service{}).Validate(ctx(), "", "anything", nil, nil))
}

func TestAConnectionThatDoesNotExistIsNotFound(t *testing.T) {
	s := service(configs{gitRow("src_git")}, creds{"src_git": {"token": secret.New("t")}}, new(int))
	_, err := s.Get(ctx(), "src_gone")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.Contains(t, err.Error(), `"src_gone"`)
	require.NotEmpty(t, errs.As(err).Remedy)
}

// TestR091_ListingConnectionsSaysWhyOneCannotBeUsed asserts that every
// enabled source connection is listed, declared ones included, and that one
// which cannot be built carries a reason rather than its credential.
func TestR091_ListingConnectionsSaysWhyOneCannotBeUsed(t *testing.T) {
	declared := generic.New()
	require.NoError(t, declared.Configure(ctx(),
		json.RawMessage(`{"method":"token","host":"git.example.com","credentials":{"token":"t"}}`)))
	fileOnly := generic.New()
	require.NoError(t, fileOnly.Configure(ctx(),
		json.RawMessage(`{"method":"token","host":"other.example.com","credentials":{"token":"t"}}`)))

	disabled := gitRow("src_off")
	disabled.Enabled = false
	notSource := gitRow("rt_docker")
	notSource.Category = "runtime"
	badJSON := gitRow("src_badjson")
	badJSON.Config = json.RawMessage(`[1]`)
	declaredRow := gitRow("src_decl")
	declaredRow.Name = "Declared"

	s := service(configs{
		disabled, notSource, badJSON, declaredRow,
		row("src_unknown", "sourcehut", nil),
		row("src_refused", "fake", map[string]any{"host": 5}),
		row("src_stored", generic.Kind, map[string]any{"method": "token", "host": "git.example.com"}),
	}, creds{"src_stored": {"token": secret.New("ghp_do_not_show")}}, new(int))
	s.Declared = map[string]api.SourceAdapter{"src_decl": declared, "src_file": fileOnly}

	all, err := s.List(ctx())
	require.NoError(t, err)
	byID := map[string]sourceconn.Connection{}
	var names []string
	for _, c := range all {
		byID[c.ID] = c
		names = append(names, c.Name)
	}
	require.IsIncreasing(t, names, "ordered by name")
	require.NotContains(t, byID, "src_off", "a disabled connection is not listed")
	require.NotContains(t, byID, "rt_docker", "another category is not a connection")

	require.True(t, byID["src_decl"].Usable(), "a declared connection is the adapter built at startup")
	require.Equal(t, "Declared", byID["src_decl"].Name)
	require.Same(t, declared, byID["src_decl"].Adapter())
	require.True(t, byID["src_file"].Usable(), "a connection only the file declares is listed too")
	require.Equal(t, "src_file", byID["src_file"].Name)

	require.Contains(t, byID["src_badjson"].Problem, "could not be read")
	require.Contains(t, byID["src_unknown"].Problem, "no source adapter of kind sourcehut")
	require.Contains(t, byID["src_refused"].Problem, "were refused")
	require.Nil(t, byID["src_refused"].Adapter())
	require.True(t, byID["src_stored"].Usable())

	// Credentials that cannot be opened are a reason, not an error.
	s.Credentials = unopenable{}
	all, err = s.List(ctx())
	require.NoError(t, err)
	for _, c := range all {
		if c.ID == "src_stored" {
			require.Contains(t, c.Problem, "could not be opened")
			require.NotContains(t, c.Problem, "the key that sealed")
		}
	}

	// A build without source adapters lists each with that reason.
	s.New = nil
	all, err = s.List(ctx())
	require.NoError(t, err)
	for _, c := range all {
		if c.ID == "src_stored" {
			require.Contains(t, c.Problem, "cannot run source connections")
		}
	}

	var none *sourceconn.Service
	all, err = none.List(ctx())
	require.NoError(t, err)
	require.Empty(t, all)
}

// TestR091_AnAuthorizationThatCannotRunSaysWhatWould asserts the refusals
// of BeginAuthorization, each with a remedy that names what can be done.
func TestR091_AnAuthorizationThatCannotRunSaysWhatWould(t *testing.T) {
	calls := new(int)
	s := service(configs{
		gitRow("src_git"),
		row("src_unknown", "sourcehut", nil),
		row("src_device", "device-only", map[string]any{"host": "forge.example"}),
		row("src_web", "web-only", map[string]any{"host": "forge.example"}),
		row("src_none", "no-flow", map[string]any{"host": "forge.example"}),
	}, creds{"src_git": {"token": secret.New("t")}}, calls)
	withKind(s, "device-only", func() api.SourceAdapter {
		return &limited{fake: &fake{calls: calls}, caps: api.SourceCapabilities{Method: "oauth", DeviceAuthorization: true}}
	})
	withKind(s, "web-only", func() api.SourceAdapter {
		return &limited{fake: &fake{calls: calls}, caps: api.SourceCapabilities{Method: "oauth", WebAuthorization: true}}
	})
	withKind(s, "no-flow", func() api.SourceAdapter {
		return &limited{fake: &fake{calls: calls}, caps: api.SourceCapabilities{Method: "oauth"}}
	})

	for _, c := range []struct {
		id     string
		mode   api.AuthorizationMode
		says   string
		remedy string
	}{
		{"src_git", api.AuthorizationDevice, "with a device code", "signs in with a token; there is nothing to authorize"},
		{"src_git", api.AuthorizationWeb, "in the browser", "nothing to authorize"},
		{"src_device", api.AuthorizationWeb, "in the browser", "with a device code instead"},
		{"src_web", api.AuthorizationDevice, "with a device code", "in the browser instead"},
		{"src_none", api.AuthorizationDevice, "with a device code", "client ID and secret"},
	} {
		_, err := s.BeginAuthorization(ctx(), c.id, c.mode, "")
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), c.id)
		require.Contains(t, err.Error(), c.says, c.id)
		require.Contains(t, errs.As(err).Remedy, c.remedy, c.id)
	}

	_, err := s.BeginAuthorization(ctx(), "src_device", "sms", "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "Valid answers: device, web")

	_, err = s.BeginAuthorization(ctx(), "src_unknown", api.AuthorizationDevice, "")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "cannot be authorized")

	_, err = s.BeginAuthorization(ctx(), "src_gone", api.AuthorizationDevice, "")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	s.Authorizations = nil
	_, err = s.BeginAuthorization(ctx(), "src_device", api.AuthorizationDevice, "")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "no secrets adapter")
}

// TestR091_AnAuthorizationInProgressIsFinishedOnlyTheWayItStarted asserts
// that a browser authorization is not polled, a device one is not finished
// by a callback, an expired one is forgotten, and one the provider finishes
// without a token stores nothing.
func TestR091_AnAuthorizationInProgressIsFinishedOnlyTheWayItStarted(t *testing.T) {
	calls := new(int)
	store := creds{}
	s := service(configs{
		row("src_fake", "fake", map[string]any{"host": "forge.example"}),
		row("src_empty", "empty", map[string]any{"host": "forge.example"}),
	}, store, calls)
	withKind(s, "empty", func() api.SourceAdapter {
		return &limited{fake: &fake{calls: calls}, empty: true,
			caps: api.SourceCapabilities{Method: "oauth", DeviceAuthorization: true}}
	})
	c := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	s.Clock = c

	a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, "https://pando.example/cb")
	require.NoError(t, err)
	require.Equal(t, c.Now().Add(15*time.Minute), a.ExpiresAt, "an authorization with no expiry of its own gets fifteen minutes")
	_, err = s.PollAuthorization(ctx(), "src_fake")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "in the browser")

	_, err = s.CompleteWebAuthorization(ctx(), "no-dot-here", "code")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "not one Pando started")

	// A device authorization is not finished by a browser callback.
	_, err = s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationDevice, "")
	require.NoError(t, err)
	_, err = s.CompleteWebAuthorization(ctx(), "src_fake.anything", "code")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	c.Advance(16 * time.Minute)
	_, err = s.PollAuthorization(ctx(), "src_fake")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "expired")
	_, err = s.PollAuthorization(ctx(), "src_fake")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "No authorization", "an expired authorization is forgotten")
	require.Empty(t, store["src_fake"])

	_, err = s.BeginAuthorization(ctx(), "src_empty", api.AuthorizationDevice, "")
	require.NoError(t, err)
	_, err = s.PollAuthorization(ctx(), "src_empty")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, err.Error(), "without a token")
	require.Empty(t, store["src_empty"])

	_, err = s.PollAuthorization(ctx(), "src_gone")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	s.Authorizations = nil
	_, err = s.PollAuthorization(ctx(), "src_fake")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "No authorization is in progress")
}

// TestR091_PickingThroughAConnectionWithoutAnAPISaysToTypeTheAddress asserts
// the browse refusals: a connection with no API, one not authorized, one not
// for the repository's host, and a repository policy refuses (R-092).
func TestR091_PickingThroughAConnectionWithoutAnAPISaysToTypeTheAddress(t *testing.T) {
	calls := new(int)
	s := service(configs{
		gitRow("src_git"),
		row("src_app", "app", map[string]any{"host": "forge.example"}),
		row("src_fake", "fake", map[string]any{"host": "forge.example"}),
		row("src_unknown", "sourcehut", nil),
	}, creds{"src_git": {"token": secret.New("t")}}, calls)
	withKind(s, "app", func() api.SourceAdapter {
		return &limited{fake: &fake{calls: calls}, caps: api.SourceCapabilities{Method: "app", Authorized: true}}
	})

	_, err := s.ListRepositories(ctx(), "src_git", api.ListRepositoriesRequest{})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "signs in with a token")
	require.Contains(t, errs.As(err).Remedy, "address")

	_, err = s.ListBranches(ctx(), "src_git", "https://git.example.com/acme/api.git")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "cannot list branches")

	_, err = s.ListBranches(ctx(), "src_app", "https://forge.example/acme/api")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "signs in with a app")

	_, err = s.ListBranches(ctx(), "src_git", "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "which repository")

	_, err = s.ListRepositories(ctx(), "src_fake", api.ListRepositoriesRequest{})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "not been authorized")

	_, err = s.ListRepositories(ctx(), "src_unknown", api.ListRepositoriesRequest{})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "no source adapter of kind sourcehut")

	_, err = s.ListRepositories(ctx(), "src_gone", api.ListRepositoriesRequest{})
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	s.Policy = policy(func(spec.Source) error { return errs.New(errs.PolicySourceNotAllowed, "no") })
	_, err = s.ListBranches(ctx(), "src_app", "https://forge.example/acme/api")
	require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err))
}

// TestR190_AnAddressThatIsNotAURLCarriesNoCredential asserts that an
// scp-like address, and one with a user but no password, pass the check for
// a credential in the address, and that a source other than git is left as
// it is.
func TestR190_AnAddressThatIsNotAURLCarriesNoCredential(t *testing.T) {
	s := service(configs{}, creds{}, new(int))

	ref, err := s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "git@github.com:acme/api.git"})
	require.NoError(t, err)
	require.Empty(t, ref)

	ref, err = s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://bob@github.com/acme/api.git"})
	require.NoError(t, err)
	require.Empty(t, ref)

	ref, err = s.Check(ctx(), spec.Source{Type: spec.SourceImage, CredentialRef: "kept"})
	require.NoError(t, err)
	require.Equal(t, "kept", ref)
}
