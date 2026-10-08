package sourceconn_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/generic"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func ctx() context.Context { return context.Background() }

type configs []state.AdapterConfig

func (c configs) List(context.Context) ([]state.AdapterConfig, error) { return c, nil }

// creds is a credential store in memory.
type creds map[string]map[string]secret.Value

func (c creds) Resolve(_ context.Context, id string) (map[string]secret.Value, error) {
	out := map[string]secret.Value{}
	for k, v := range c[id] {
		out[k] = v
	}
	return out, nil
}

func (c creds) Put(_ context.Context, id, field string, v secret.Value) error {
	if v.IsZero() {
		delete(c[id], field)
		return nil
	}
	if c[id] == nil {
		c[id] = map[string]secret.Value{}
	}
	c[id][field] = v
	return nil
}

func (c creds) Delete(_ context.Context, id, field string) error {
	delete(c[id], field)
	return nil
}

type policy func(spec.Source) error

func (p policy) AllowsSource(_ context.Context, src spec.Source) error { return p(src) }

type auditLog []audit.Event

func (a *auditLog) Write(_ context.Context, e audit.Event) error {
	*a = append(*a, e)
	return nil
}

func row(id, kind string, cfg map[string]any) state.AdapterConfig {
	raw, _ := json.Marshal(cfg)
	return state.AdapterConfig{ID: id, Category: "source", Kind: kind, Name: id, Config: raw, Enabled: true}
}

// fake is a source adapter that counts what it is asked and does what its
// configuration says.
type fake struct {
	cfg struct {
		Host        string                  `json:"host"`
		Credentials map[string]secret.Value `json:"credentials"`
	}
	calls *int
}

func (f *fake) Kind() string                      { return "fake" }
func (f *fake) Category() api.Category            { return api.CategorySource }
func (f *fake) HealthCheck(context.Context) error { return nil }
func (f *fake) Configure(_ context.Context, raw json.RawMessage) error {
	return json.Unmarshal(raw, &f.cfg)
}
func (f *fake) SourceCapabilities() api.SourceCapabilities {
	return api.SourceCapabilities{Method: "oauth", Host: f.cfg.Host, ListRepositories: true,
		DeviceAuthorization: true, WebAuthorization: true,
		Authorized: !f.cfg.Credentials["access_token"].IsZero()}
}
func (f *fake) Covers(u string) int {
	if strings.Contains(u, f.cfg.Host) {
		return 1
	}
	return 0
}
func (f *fake) GitCredential(context.Context, string) (api.GitCredential, error) {
	*f.calls++
	cred := api.GitCredential{Password: f.cfg.Credentials["access_token"]}
	if f.cfg.Credentials["access_token"].Reveal() == "expired" {
		cred.Password = secret.New("renewed")
		cred.Rotated = map[string]secret.Value{"access_token": secret.New("renewed"), "refresh_token": secret.New("r2")}
	}
	return cred, nil
}
func (f *fake) Refresh(context.Context) (map[string]secret.Value, error) {
	if f.cfg.Credentials["access_token"].Reveal() == "expired" {
		return map[string]secret.Value{"access_token": secret.New("renewed")}, nil
	}
	return nil, nil
}
func (f *fake) ListRepositories(context.Context, api.ListRepositoriesRequest) ([]api.Repository, error) {
	if f.cfg.Credentials["access_token"].Reveal() != "renewed" && f.cfg.Credentials["access_token"].Reveal() != "good" {
		return nil, errs.New(errs.StateInvalid, "the token is "+f.cfg.Credentials["access_token"].Reveal())
	}
	return []api.Repository{{URL: "https://" + f.cfg.Host + "/acme/api", FullName: "acme/api"},
		{URL: "https://" + f.cfg.Host + "/blocked/api", FullName: "blocked/api"}}, nil
}
func (f *fake) ListBranches(context.Context, string) ([]string, error) { return []string{"main"}, nil }
func (f *fake) BeginAuthorization(_ context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if req.Mode == api.AuthorizationWeb {
		return api.Authorization{Mode: req.Mode, AuthorizeURL: "https://idp/authorize?state=" + req.State, Flow: secret.New("verifier")}, nil
	}
	return api.Authorization{Mode: req.Mode, UserCode: "ABCD-1234", VerificationURL: "https://idp/device",
		Interval: 5 * time.Second, Flow: secret.New("device-code")}, nil
}
func (f *fake) CompleteAuthorization(_ context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	*f.calls++
	if req.Mode == api.AuthorizationDevice && *f.calls == 1 {
		return api.AuthorizationResult{Pending: true}, nil
	}
	return api.AuthorizationResult{Credentials: map[string]secret.Value{"access_token": secret.New("good"), "refresh_token": secret.New("r1")}}, nil
}

func service(rows configs, store creds, calls *int) *sourceconn.Service {
	return &sourceconn.Service{
		Configs:        rows,
		Credentials:    store,
		Authorizations: creds{},
		Clock:          clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)),
		New: func(kind string) api.SourceAdapter {
			switch kind {
			case generic.Kind:
				return generic.New()
			case "fake":
				return &fake{calls: calls}
			}
			return nil
		},
	}
}

// TestR091_ARepositoryIsReadWithTheConnectionThatCoversItMostClosely asserts
// O-3 as re-resolved: connections are the install's, and the narrowest one
// covering a repository is the one it is read with.
func TestR091_ARepositoryIsReadWithTheConnectionThatCoversItMostClosely(t *testing.T) {
	store := creds{
		"src_host": {"token": secret.New("t1")},
		"src_acme": {"token": secret.New("t2")},
	}
	s := service(configs{
		row("src_host", "git", map[string]any{"method": "token", "host": "git.example.com"}),
		row("src_acme", "git", map[string]any{"method": "token", "host": "git.example.com", "scope": "acme"}),
		row("src_broken", "git", map[string]any{"method": "token", "host": "git.example.com", "scope": "acme/x"}),
	}, store, new(int))

	c, ok, err := s.Match(ctx(), "https://git.example.com/acme/api.git")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "src_acme", c.ID)

	c, ok, err = s.Match(ctx(), "https://git.example.com/other/api.git")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "src_host", c.ID)

	_, ok, err = s.Match(ctx(), "https://github.com/acme/api.git")
	require.NoError(t, err)
	require.False(t, ok, "no connection covers another host")

	// A connection that cannot be built is listed with why, and never matched.
	all, err := s.List(ctx())
	require.NoError(t, err)
	require.Len(t, all, 3)
	for _, c := range all {
		if c.ID == "src_broken" {
			require.False(t, c.Usable())
			require.Contains(t, c.Problem, "token")
		}
	}
}

// TestR092_ABlockedSourceIsNeverGivenACredential asserts that the allowlist
// is evaluated before a connection is asked for a credential.
func TestR092_ABlockedSourceIsNeverGivenACredential(t *testing.T) {
	calls := 0
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})},
		creds{"src_fake": {"access_token": secret.New("good")}}, &calls)
	s.Policy = policy(func(spec.Source) error {
		return errs.New(errs.PolicySourceNotAllowed, "not allowed")
	})
	_, err := s.Access(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://forge.example/acme/api", CredentialRef: "src_fake"}, source.PurposeClone)
	require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err))
	require.Zero(t, calls, "no credential was minted for a blocked source")
}

// TestR091_ARefreshedTokenIsStoredAndEveryCloneIsAudited asserts that a token
// the provider rotated is kept (R-190: sealed, by the store) and that each
// clone with a connection is in the audit log without the credential, while a
// check for new commits is not.
func TestR091_ARefreshedTokenIsStoredAndEveryCloneIsAudited(t *testing.T) {
	calls := 0
	store := creds{"src_fake": {"access_token": secret.New("expired")}}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, &calls)
	log := &auditLog{}
	s.Audit = log

	src := spec.Source{Type: spec.SourceGit, URL: "https://forge.example/acme/api", CredentialRef: "src_fake"}
	a, err := s.Access(source.ForApp(ctx(), "app_01"), src, source.PurposeClone)
	require.NoError(t, err)
	require.Equal(t, "renewed", a.Credential.Password.Reveal())
	require.Equal(t, "renewed", store["src_fake"]["access_token"].Reveal())
	require.Equal(t, "r2", store["src_fake"]["refresh_token"].Reveal())

	require.Len(t, *log, 1)
	e := (*log)[0]
	require.Equal(t, "source.connection.use", e.Action)
	require.Equal(t, "app_01", e.AppID)
	require.Equal(t, "src_fake", e.TargetID)
	body, _ := json.Marshal(e.Detail)
	require.NotContains(t, string(body), "renewed", "R-194: no credential in the audit log")

	_, err = s.Access(ctx(), src, source.PurposeCheck)
	require.NoError(t, err)
	require.Len(t, *log, 1, "polling for commits is not a use worth a record")

	// Anonymous: no connection is asked at all.
	a, err = s.Access(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://forge.example/acme/api"}, source.PurposeClone)
	require.NoError(t, err)
	require.Nil(t, a)
}

// TestR091_AConnectionThatIsGoneOrUnusableSaysSo asserts R-105 messages for
// an app whose connection was removed or never authorized.
func TestR091_AConnectionThatIsGoneOrUnusableSaysSo(t *testing.T) {
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, creds{}, new(int))

	_, err := s.Access(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://forge.example/a/b", CredentialRef: "src_gone"}, source.PurposeClone)
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "no longer exists")

	_, err = s.Access(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://forge.example/a/b", CredentialRef: "src_fake"}, source.PurposeClone)
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "not been authorized")
}

type prober struct {
	got []spec.Source
	err error
}

func (p *prober) Probe(_ context.Context, src spec.Source) error {
	p.got = append(p.got, src)
	return p.err
}

// TestR091_CreatingAnAppChoosesItsConnectionAndChecksItCanRead asserts the
// check an app's creation makes.
func TestR091_CreatingAnAppChoosesItsConnectionAndChecksItCanRead(t *testing.T) {
	s := service(configs{row("src_acme", "git", map[string]any{"method": "token", "host": "git.example.com", "scope": "acme"})},
		creds{"src_acme": {"token": secret.New("t")}}, new(int))
	p := &prober{}
	s.Prober = p

	ref, err := s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://git.example.com/acme/api.git"})
	require.NoError(t, err)
	require.Equal(t, "src_acme", ref)
	require.Equal(t, "src_acme", p.got[0].CredentialRef, "the probe reads with the connection it chose")

	ref, err = s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://github.com/public/repo"})
	require.NoError(t, err)
	require.Empty(t, ref, "a repository nothing covers is read anonymously")

	p.err = errs.New(errs.SourceUnreadable, "private")
	_, err = s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://github.com/private/repo"})
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))

	_, err = s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://github.com/a/b", CredentialRef: "src_missing"})
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}

// TestR190_ACredentialInTheRepositoryAddressIsRefused asserts that a token
// pasted into an address — which the spec stores in the clear — is refused.
func TestR190_ACredentialInTheRepositoryAddressIsRefused(t *testing.T) {
	s := service(configs{}, creds{}, new(int))
	_, err := s.Check(ctx(), spec.Source{Type: spec.SourceGit, URL: "https://bob:ghp_secret@github.com/acme/api.git"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.NotContains(t, err.Error(), "ghp_secret")
}

// TestR091_DeviceAuthorizationStoresTheTokenItReturns asserts the OAuth
// device flow, which needs no address the provider can reach.
func TestR091_DeviceAuthorizationStoresTheTokenItReturns(t *testing.T) {
	calls := 0
	store := creds{}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, &calls)

	a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationDevice, "")
	require.NoError(t, err)
	require.Equal(t, "ABCD-1234", a.UserCode)
	require.Equal(t, 5, a.IntervalSeconds)

	st, err := s.PollAuthorization(ctx(), "src_fake")
	require.NoError(t, err)
	require.Equal(t, "pending", st.Status)

	st, err = s.PollAuthorization(ctx(), "src_fake")
	require.NoError(t, err)
	require.Equal(t, "authorized", st.Status)
	require.Equal(t, "good", store["src_fake"]["access_token"].Reveal())

	c, err := s.Get(ctx(), "src_fake")
	require.NoError(t, err)
	require.True(t, c.Usable(), "authorized, the connection can be used")

	_, err = s.PollAuthorization(ctx(), "src_fake")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "a finished authorization is forgotten")
}

// TestR091_WebAuthorizationChecksTheStateItStarted asserts that a callback
// must carry the state of the authorization Pando started.
func TestR091_WebAuthorizationChecksTheStateItStarted(t *testing.T) {
	store := creds{}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, new(int))

	a, err := s.BeginAuthorization(ctx(), "src_fake", api.AuthorizationWeb, "https://pando.example/api/v1/sources/callback?x=1")
	require.NoError(t, err)
	state := a.AuthorizeURL[strings.Index(a.AuthorizeURL, "state=")+len("state="):]
	require.True(t, strings.HasPrefix(state, "src_fake."))

	_, err = s.CompleteWebAuthorization(ctx(), "src_fake.forged", "code")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Empty(t, store["src_fake"])

	id, err := s.CompleteWebAuthorization(ctx(), state, "code")
	require.NoError(t, err)
	require.Equal(t, "src_fake", id)
	require.Equal(t, "good", store["src_fake"]["access_token"].Reveal())
}

// TestR092_ListedRepositoriesLeaveOutWhatPolicyRefuses asserts the picker
// offers only what can be added, and that an expired token is renewed and
// stored before the listing.
func TestR092_ListedRepositoriesLeaveOutWhatPolicyRefuses(t *testing.T) {
	store := creds{"src_fake": {"access_token": secret.New("expired")}}
	s := service(configs{row("src_fake", "fake", map[string]any{"host": "forge.example"})}, store, new(int))
	s.Policy = policy(func(src spec.Source) error {
		if strings.Contains(src.URL, "/blocked/") {
			return errs.New(errs.PolicySourceNotAllowed, "no")
		}
		return nil
	})
	repos, err := s.ListRepositories(ctx(), "src_fake", api.ListRepositoriesRequest{})
	require.NoError(t, err)
	require.Len(t, repos, 1)
	require.Equal(t, "acme/api", repos[0].FullName)
	require.Equal(t, "renewed", store["src_fake"]["access_token"].Reveal())

	branches, err := s.ListBranches(ctx(), "src_fake", "https://forge.example/acme/api")
	require.NoError(t, err)
	require.Equal(t, []string{"main"}, branches)
	_, err = s.ListBranches(ctx(), "src_fake", "https://elsewhere.example/acme/api")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}
