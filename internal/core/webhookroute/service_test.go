package webhookroute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type revisions struct {
	pinned, newest state.Revision
	created        []*spec.AppSpec
	err            error
}

func (r *revisions) PinnedAndNewest(context.Context, state.App) (state.Revision, state.Revision, error) {
	return r.pinned, r.newest, r.err
}

func (r *revisions) CreateRevision(_ context.Context, _ string, s *spec.AppSpec, _ spec.Origin, _ string) (state.Revision, error) {
	r.created = append(r.created, s)
	r.newest = state.Revision{Revision: r.newest.Revision + 1, Body: s}
	return r.newest, nil
}

type secrets map[string]string

func (k secrets) Put(_ context.Context, _, route string, v secret.Value) error {
	k[route] = v.Reveal()
	return nil
}

func (k secrets) Names(context.Context, string) (map[string]bool, error) {
	out := map[string]bool{}
	for name := range k {
		out[name] = true
	}
	return out, nil
}

func (k secrets) Delete(_ context.Context, _, route string) error {
	delete(k, route)
	return nil
}

type allow struct{ err error }

func (a allow) CheckControl(context.Context, authz.Principal, string, authz.Verb) error { return a.err }

type routesPolicy struct{ err error }

func (p routesPolicy) AllowsWebhookRoutes(context.Context, *spec.AppSpec) error { return p.err }

// validSpec is the smallest spec spec.Validate accepts.
func validSpec() *spec.AppSpec {
	return &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		Build:         spec.Build{Strategy: spec.BuildDockerfile, AdapterRef: "bld_buildkit"},
		Workloads:     []spec.Workload{{Name: "web", Primary: true, Exposed: true}},
		Routing:       spec.Routing{AdapterRef: "rt", Mode: spec.RoutingSubdomain, Hostname: "notes.test"},
		Runtime:       spec.RuntimeRef{AdapterRef: "rt_local"},
	}
}

func route() spec.WebhookRoute {
	return spec.WebhookRoute{Name: "noodle", Method: "POST", Path: "/api/v1/webhook/noodle",
		Verification: spec.WebhookVerification{Kind: spec.WebhookHMACSHA256, Header: "X-Hub-Signature-256"}}
}

func service() (*Service, *revisions, secrets, *[]audit.Event) {
	revs := &revisions{newest: state.Revision{Revision: 1, Body: validSpec()}}
	keys := secrets{}
	events := &[]audit.Event{}
	return &Service{
		Apps: revs, Secrets: keys, Policy: routesPolicy{}, Authz: allow{},
		Audit: func(_ context.Context, e audit.Event) { *events = append(*events, e) },
	}, revs, keys, events
}

var (
	app = state.App{ID: "app_1"}
	who = authz.Principal{Kind: authz.KindUser, ID: "usr_1", UserID: "usr_1"}
)

// TestR427_ASecretIsGeneratedAndShownOnce asserts Add makes one, returns it in
// the clear in that response, and that nothing else carries it.
func TestR427_ASecretIsGeneratedAndShownOnce(t *testing.T) {
	s, revs, keys, events := service()
	saved, err := s.Add(context.Background(), who, app, route(), "")
	require.NoError(t, err)
	require.Equal(t, 2, saved.Revision)
	require.Len(t, revs.created, 1)
	require.True(t, strings.HasPrefix(keys["noodle"], secretPrefix))

	out, err := json.Marshal(saved)
	require.NoError(t, err)
	require.Contains(t, string(out), keys["noodle"])

	view, err := s.List(context.Background(), app)
	require.NoError(t, err)
	listed, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(listed), keys["noodle"], "a secret is never listed (R-432)")
	require.True(t, view.Routes[0].SecretSet)
	require.False(t, view.Routes[0].Deployed, "a change waits for the next deploy")

	require.Len(t, *events, 1)
	require.NotContains(t, (*events)[0].Detail, "secret")
	require.Equal(t, "app.webhook_route.add", (*events)[0].Action)
}

// TestR427_ASuppliedSecretIsKeptAndNotEchoed asserts a provider's secret, which
// the caller already has, does not come back.
func TestR427_ASuppliedSecretIsKeptAndNotEchoed(t *testing.T) {
	s, _, keys, _ := service()
	supplied := "whsec_the-providers-own-secret"
	saved, err := s.Add(context.Background(), who, app, route(), supplied)
	require.NoError(t, err)
	require.Equal(t, supplied, keys["noodle"])
	require.Nil(t, saved.Secret)

	_, err = s.SetSecret(context.Background(), who, app, "noodle", "short")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "at least 16 characters")
	require.Equal(t, supplied, keys["noodle"], "a refused secret changes nothing")
}

// TestR427_AddingAlsoNeedsSecretsWrite asserts the second verb, with nothing
// written when it is refused.
func TestR427_AddingAlsoNeedsSecretsWrite(t *testing.T) {
	s, revs, keys, _ := service()
	s.Authz = allow{err: errs.New(errs.PermDenied, "no")}
	_, err := s.Add(context.Background(), who, app, route(), "")
	require.Equal(t, errs.PermDenied, errs.CodeOf(err))
	require.Empty(t, revs.created)
	require.Empty(t, keys)
}

// TestR427_ReplacingASecretTakesEffectAtOnce asserts rotation.
func TestR427_ReplacingASecretTakesEffectAtOnce(t *testing.T) {
	s, _, keys, events := service()
	_, err := s.Add(context.Background(), who, app, route(), "")
	require.NoError(t, err)
	before := keys["noodle"]

	key, err := s.SetSecret(context.Background(), who, app, "noodle", "")
	require.NoError(t, err)
	require.NotNil(t, key)
	require.NotEqual(t, before, keys["noodle"])

	key, err = s.SetSecret(context.Background(), who, app, "noodle", "whsec_the-providers-own-secret")
	require.NoError(t, err)
	require.Nil(t, key)

	_, err = s.SetSecret(context.Background(), who, app, "nothing", "")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.Equal(t, "app.webhook_route.secret", (*events)[len(*events)-1].Action)
}

// TestR430_RemovingARouteRemovesItsSecretNow asserts a route being taken out
// stops at once rather than at the next deploy.
func TestR430_RemovingARouteRemovesItsSecretNow(t *testing.T) {
	s, revs, keys, _ := service()
	_, err := s.Add(context.Background(), who, app, route(), "")
	require.NoError(t, err)

	revision, err := s.Remove(context.Background(), who, app, "noodle")
	require.NoError(t, err)
	require.Equal(t, 3, revision)
	require.Empty(t, revs.newest.Body.WebhookRoutes)
	require.Empty(t, keys)

	_, err = s.Remove(context.Background(), who, app, "noodle")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}

// TestR431_PolicyRefusesARoute asserts nothing is written.
func TestR431_PolicyRefusesARoute(t *testing.T) {
	s, revs, keys, _ := service()
	s.Policy = routesPolicy{err: errs.New(errs.PlanWebhookRoutesDisabled, "off")}
	_, err := s.Add(context.Background(), who, app, route(), "")
	require.Equal(t, errs.PlanWebhookRoutesDisabled, errs.CodeOf(err))
	require.Empty(t, revs.created)
	require.Empty(t, keys)
}

// TestR432_AddRefusesWhatTheSpecWouldNot asserts the same validation as a spec
// save, and that a duplicate is one.
func TestR432_AddRefusesWhatTheSpecWouldNot(t *testing.T) {
	s, _, keys, _ := service()
	bad := route()
	bad.Path = "/a/../b"
	_, err := s.Add(context.Background(), who, app, bad, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Empty(t, keys)

	_, err = s.Add(context.Background(), who, app, route(), "")
	require.NoError(t, err)
	_, err = s.Add(context.Background(), who, app, route(), "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

// TestR432_ADeployedRouteSaysSo asserts the list against the pinned revision.
func TestR432_ADeployedRouteSaysSo(t *testing.T) {
	s, revs, keys, _ := service()
	body := validSpec()
	body.WebhookRoutes = []spec.WebhookRoute{route()}
	revs.pinned, revs.newest = state.Revision{Body: body}, state.Revision{Body: body}

	view, err := s.List(context.Background(), app)
	require.NoError(t, err)
	require.True(t, view.Routes[0].Deployed)
	require.False(t, view.Routes[0].SecretSet, "a route can arrive by a spec save, with no secret yet")
	require.Empty(t, keys)
}

func TestAnAppWithNoConfigurationHasNoRoutes(t *testing.T) {
	s, revs, _, _ := service()
	revs.newest = state.Revision{}
	view, err := s.List(context.Background(), app)
	require.NoError(t, err)
	require.Empty(t, view.Routes)

	_, err = s.Add(context.Background(), who, app, route(), "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

func TestAFailedReadIsReturned(t *testing.T) {
	s, revs, _, _ := service()
	revs.err = errors.New("database gone")
	_, err := s.List(context.Background(), app)
	require.Error(t, err)
	_, err = s.Add(context.Background(), who, app, route(), "")
	require.Error(t, err)
	_, err = s.Remove(context.Background(), who, app, "noodle")
	require.Error(t, err)
	_, err = s.SetSecret(context.Background(), who, app, "noodle", "")
	require.Error(t, err)
}

// failing fails each write after the route is validated.
type failing struct {
	secrets
	put, names, del error
}

func (f failing) Put(ctx context.Context, appID, route string, v secret.Value) error {
	if f.put != nil {
		return f.put
	}
	return f.secrets.Put(ctx, appID, route, v)
}

func (f failing) Names(ctx context.Context, appID string) (map[string]bool, error) {
	if f.names != nil {
		return nil, f.names
	}
	return f.secrets.Names(ctx, appID)
}

func (f failing) Delete(ctx context.Context, appID, route string) error {
	if f.del != nil {
		return f.del
	}
	return f.secrets.Delete(ctx, appID, route)
}

type noRevision struct{ *revisions }

func (noRevision) CreateRevision(context.Context, string, *spec.AppSpec, spec.Origin, string) (state.Revision, error) {
	return state.Revision{}, errors.New("database gone")
}

// TestR427_AFailedWriteIsReturned asserts each store's failure reaches the
// caller rather than a route saved without its secret.
func TestR427_AFailedWriteIsReturned(t *testing.T) {
	gone := errors.New("database gone")
	ctx := context.Background()

	s, revs, keys, _ := service()
	s.Secrets = failing{secrets: keys, put: gone}
	_, err := s.Add(ctx, who, app, route(), "")
	require.ErrorIs(t, err, gone)
	require.Empty(t, revs.created, "no route without its secret")

	s, revs, _, _ = service()
	_, err = s.Add(ctx, who, app, route(), "")
	require.NoError(t, err)
	s.Secrets = failing{secrets: secrets{}, put: gone}
	_, err = s.SetSecret(ctx, who, app, "noodle", "")
	require.ErrorIs(t, err, gone)
	s.Secrets = failing{secrets: secrets{}, names: gone}
	_, err = s.List(ctx, app)
	require.ErrorIs(t, err, gone)
	s.Secrets = failing{secrets: secrets{}, del: gone}
	_, err = s.Remove(ctx, who, app, "noodle")
	require.ErrorIs(t, err, gone)

	s.Apps = noRevision{revs}
	_, err = s.Remove(ctx, who, app, "noodle")
	require.Error(t, err)
	other := route()
	other.Name, other.Path = "other", "/other"
	_, err = s.Add(ctx, who, app, other, "whsec_the-providers-own-secret")
	require.Error(t, err)

	_, err = s.Add(ctx, who, app, other, "short")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

func TestWithNoAuditWriterAChangeStillSaves(t *testing.T) {
	s, _, _, _ := service()
	s.Audit = nil
	_, err := s.Add(context.Background(), who, app, route(), "")
	require.NoError(t, err)
}
