//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	aianthropic "github.com/trypando/pando/internal/adapter/ai/anthropic"
	adapterapi "github.com/trypando/pando/internal/adapter/api"
	backuplocal "github.com/trypando/pando/internal/adapter/backup/local"
	identitylocal "github.com/trypando/pando/internal/adapter/identity/local"
	identityoidc "github.com/trypando/pando/internal/adapter/identity/oidc"
	identitysaml "github.com/trypando/pando/internal/adapter/identity/saml"
	notifyconsole "github.com/trypando/pando/internal/adapter/notify/console"
	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/assist"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/deploy"
	"github.com/trypando/pando/internal/core/detection"
	"github.com/trypando/pando/internal/core/idp"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/planner"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/core/subscription"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/reference"
	"github.com/trypando/pando/internal/secret"
)

// The API's own tests, against a real Postgres.
//
// The handlers hold concrete *state.X rather than interfaces, so there is no
// seam to fake: a handler test either runs against the real store or tests
// nothing. Postgres starts once for the package and each test gets a database
// of its own, copied from one migrated once (statetest). Nothing an install
// holds is shared with another, so every test here runs in parallel (issue #31).

// install is one Pando installation: a database, a wired API, and an admin.
type install struct {
	t       *testing.T
	handler http.Handler
	db      *state.DB

	// ownerURL is the database's owner connection, for a test that must do
	// what no Pando role may — plant an audit event months in the past.
	ownerURL string

	// Server is the wired API. Handlers read its fields per request, so a test
	// can set a collaborator the harness leaves nil (a Detector, TeardownNow).
	Server *httpapi.Server

	Apps        *state.Apps
	Users       *state.Users
	Grants      *state.Grants
	Sessions    *state.Sessions
	Tokens      *state.Tokens
	Secrets     *state.Secrets
	Volumes     *state.Volumes
	Adapters    *state.Adapters
	PolicyStore *state.Policy

	// Dispatcher sends what subscriptions route; a test calls Pass. People
	// and Channel are the notify adapters it and the router send through.
	Dispatcher *subscription.Dispatcher
	People     *fakeNotify
	Channel    *fakeNotify

	// Restarts counts POST /restart's calls to Server.Restart.
	Restarts *atomic.Int32

	// AdminID and adminPassword are the first-run account (R-046).
	AdminID       string
	adminPassword string
}

// newInstall brings up a fresh installation wired the way main wires it.
//
// Everything real except the runtime and builder adapters: those need Docker,
// and the handlers under test never reach them — a plan or a deploy fails at
// the planner with an adapter error, which is itself worth asserting.
func newInstall(t *testing.T) *install {
	t.Helper()
	return newInstallWith(t, nil, nil)
}

// newInstallWith is newInstall started with a startup configuration: host
// policy fields fixed in it (R-271), and the settings GET /config reports.
func newInstallWith(t *testing.T, overlay *corepolicy.Overlay, startup *config.Config) *install {
	t.Helper()
	ctx := context.Background()

	db, dbURL := statetest.Connect(t)

	logger := zap.NewNop()
	auditor := audit.New(db.Pool)

	users := state.NewUsers(db)
	sessions := state.NewSessions(db)
	tokens := state.NewTokens(db, testTokenKey)
	apps := state.NewApps(db)
	volumes := state.NewVolumes(db)
	adapters := state.NewAdapters(db)
	allocations := state.NewAllocations(db)
	authzStore := state.NewAuthzStore(db)
	grants := state.NewGrants(db)
	policyStore := state.NewPolicy(db)
	effectivePolicy := overlay.Wrap(policyStore)
	hostPolicy := corepolicy.New(effectivePolicy.Load)

	const adminPassword = "correct-horse-battery-staple"
	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.New(adminPassword))
	require.NoError(t, err)
	require.True(t, first.Created, "a fresh install creates its administrator")

	identity := identitylocal.New(users)

	registry := adapterapi.NewRegistry()
	secretsAdapter := secretslocal.New()
	require.NoError(t, secretsAdapter.Configure(ctx,
		json.RawMessage(`{"key_path":`+quote(t, t.TempDir()+"/secrets.key")+`}`)))
	require.NoError(t, registry.Register("sec_local", secretsAdapter))
	require.NoError(t, registry.SetDefault(adapterapi.CategorySecrets, "sec_local"))

	// Uploaded source goes to a directory this test owns. The default is
	// /var/lib/pando/uploads, which a test cannot write to — and the failure
	// arrives as a 500 from the upload endpoint rather than as anything that
	// names the directory.
	sources := source.Sources{UploadDir: t.TempDir()}

	backupAdapter := backuplocal.New()
	require.NoError(t, backupAdapter.Configure(ctx,
		json.RawMessage(`{"path":`+quote(t, t.TempDir())+`}`)))
	require.NoError(t, registry.Register("bk_local", backupAdapter))
	require.NoError(t, registry.SetDefault(adapterapi.CategoryBackup, "bk_local"))

	secrets := state.NewSecrets(db, secretsAdapter, "sec_local")
	deployments := state.NewDeployments(db)
	logStore := deploy.NewLogStore()
	appPlanner := planner.New(registry, hostPolicy, allocations).WithInventory(apps)
	reconciles := state.NewReconciles(db)

	deployer := deploy.NewRunner(registry, appPlanner, apps, deployments, secrets, reconciles,
		logStore, volumes, "http://pando:8080").
		WithServices(state.NewServices(db), secrets).
		WithSources(sources)

	minter, err := assertion.NewMinter("https://pando.test", clock.System{})
	require.NoError(t, err)

	authenticator := &httpapi.Authenticator{
		Sessions: sessions, Tokens: tokens, Users: users, Groups: authzStore,
	}
	authorizer := authz.New(authzStore, hostPolicy, nil)

	restarts := &atomic.Int32{}
	srv := &httpapi.Server{
		StartedAt: time.Now().UTC(),
		Restart:   func() { restarts.Add(1) },
		Logger:    logger,
		DB:        db,
		Identity:  identity,
		Users:     users,
		Sessions:  sessions,
		Tokens:    tokens,
		Apps:      apps,
		Volumes:   volumes,
		Auditor:   auditor,
		Policy:    hostPolicy,

		Registry:    registry,
		Adapters:    adapters,
		AIFunctions: &assist.Assignments{Store: state.NewAIAssignments(db), Registry: registry, Declared: declaredAI(startup)},
		Assist: &assist.Service{
			Registry: registry, Users: users, Apps: apps,
			Roles: state.NewRoles(db), Groups: state.NewGroups(db), Verbs: authzStore,
			Policy: overlay.Wrap(policyStore), Overlay: overlay, Audit: audit.NewReader(db.Pool),
			Reference: func() string { return reference.Markdown(httpapi.Reference()) },
		},
		AdapterKinds: []adapterapi.KindInfo{aianthropic.Info(), secretslocal.Info()},
		Allocations:  allocations,

		AdapterCredentials: state.NewAdapterCredentials(db, secretsAdapter, "sec_local"),
		Planner:            appPlanner,
		Deployments:        deployments,
		Reconciles:         state.NewReconciles(db),
		Deployer:           deployer,
		Logs:               logStore,
		Secrets:            secrets,
		Detections:         state.NewDetections(db),
		Sources:            sources,
		Images:             &oci.Images{Credentials: state.NewRegistryCredentials(db, secretsAdapter, "sec_local")},

		Authz:   authorizer,
		Authent: authenticator,
		Minter:  minter,

		Grants:     grants,
		HostPolicy: hostPolicy,
		Verbs:      authzStore,
		Defaults:   detection.NewInstallation(registry, "apps.test"),

		PolicyStore:   effectivePolicy,
		PolicyOverlay: overlay,
		Startup:       startup,
		AuditLog:      audit.NewReader(db.Pool),

		Groups:  state.NewGroups(db),
		Roles:   state.NewRoles(db),
		Backups: state.NewBackups(db),
		Backup: &backup.Service{
			Registry:      registry,
			DatabaseURL:   secret.New(dbURL),
			State:         state.NewBundleSource(db),
			Version:       "test",
			SchemaVersion: db.SchemaVersion(),
			WorkDir:       t.TempDir(),
		},
		BundleSource: state.NewBundleSource(db),
		Idempotency:  state.NewIdempotency(db),

		// The real service against a registry with no scanner in it, which is
		// the shipped state of an installation that has not configured one
		// (R-317). Reading a standing still works there and says so; scanning
		// is what has nothing to scan with.
		Security: &security.Service{
			Scans:       state.NewScans(db),
			Deployments: deployments,
			Registry:    registry,
			Policy:      hostPolicy,
			Auditor:     auditor,
			Logger:      zap.NewNop(),
		},
	}

	// Every deploy starts through the approval service (R-154), wired as main
	// wires it. Its Planner, Deployer and Notifier are interfaces a test may
	// replace, since the harness has no runtime to plan or deploy onto.
	srv.Approvals = &approval.Service{
		Deployments: deployments,
		Apps:        apps,
		Authz:       authorizer,
		Policy:      effectivePolicy,
		Planner:     appPlanner,
		Deployer:    deployer,
		Audit:       httpapi.AuditFunc(auditor),
		Approvers:   authzStore,
		Clock:       clock.System{},
		Logger:      logger,
	}

	// External identity, with the kinds main compiles in.
	srv.IDP = &idp.Service{
		Providers:   state.NewIdentityProviders(db),
		Credentials: state.NewIdentityCredentials(db, secretsAdapter, "sec_local"),
		Identities:  state.NewIdentities(db),
		Users:       users,
		Sessions:    sessions,
		Groups:      state.NewGroups(db),
		Flows:       state.NewSSOFlows(db),
		SCIMUsers:   state.NewSCIMUsers(db),
		SCIMGroups:  state.NewSCIMGroups(db),
		Policy:      effectivePolicy,
		Local:       identity,
		Kinds: map[string]idp.Kind{
			identityoidc.Kind: {New: func() adapterapi.IdentityAdapter { return identityoidc.New() }, Info: identityoidc.Info()},
			identitysaml.Kind: {New: func() adapterapi.IdentityAdapter { return identitysaml.New() }, Info: identitysaml.Info()},
		},
		Audit: httpapi.AuditFunc(auditor),
		Clock: clock.System{},
	}

	// Event subscriptions (issue #50), with two notify adapters that record
	// what they are given: one reaching people, one posting to a channel.
	people := &fakeNotify{kind: "console", audience: adapterapi.AudiencePeople}
	channel := &fakeNotify{kind: "slack", audience: adapterapi.AudienceChannel}
	require.NoError(t, registry.Register("ntf_people", people))
	require.NoError(t, registry.Register("ntf_channel", channel))
	// And the real console adapter, recording into the inbox (R-377).
	require.NoError(t, registry.Register("ntf_console", notifyconsole.New(state.NewNotifications(db))))
	prefs := state.NewNotificationPreferences(db)
	router := subscription.Router{Registry: registry, Preferences: prefs, Users: users}
	srv.Notifier = router
	srv.Inbox = &subscription.Inbox{Store: state.NewNotifications(db)}
	srv.Subscriptions = &subscription.Service{
		Subscriptions: state.NewSubscriptions(db),
		Deliveries:    state.NewDeliveries(db),
		Events:        state.NewEvents(db),
		Keys:          state.NewSubscriptionSecrets(db, secretsAdapter, "sec_local"),
		ExternalURL:   "https://pando.test",
		Prefs:         prefs,
		Authz:         authorizer,
		Policy:        effectivePolicy,
		Apps:          apps,
		Registry:      registry,
		Audit:         httpapi.AuditFunc(auditor),
		Clock:         clock.System{},
	}
	dispatcher := &subscription.Dispatcher{
		Events:        srv.Subscriptions.Events,
		Subscriptions: srv.Subscriptions.Subscriptions,
		Deliveries:    srv.Subscriptions.Deliveries,
		Keys:          srv.Subscriptions.Keys,
		Tokens:        tokens,
		TokenOwners:   tokens,
		Deployments:   deployments,
		Holders:       authzStore,
		ExternalURL:   "https://pando.test",
		Authz:         authorizer,
		Policy:        effectivePolicy,
		Apps:          apps,
		Users:         users,
		Groups:        authzStore,
		Registry:      registry,
		Notifier:      router,
		Audit:         httpapi.AuditFunc(auditor),
	}

	return &install{
		Dispatcher: dispatcher, People: people, Channel: channel,
		t: t, handler: srv.Routes(), db: db, ownerURL: dbURL, Server: srv, Restarts: restarts,
		Apps: apps, Users: users, Grants: grants, Sessions: sessions, Tokens: tokens,
		Secrets: secrets, Volumes: volumes, Adapters: adapters, PolicyStore: policyStore,
		AdminID: first.User.ID, adminPassword: adminPassword,
	}
}

func quote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

// --- requests --------------------------------------------------------------

// session is an authenticated caller: a cookie, or a bearer token.
type session struct {
	cookie string
	token  string
}

type reply struct {
	Code int
	Body []byte
	Hdr  http.Header
}

// JSON decodes the body into v.
func (r reply) JSON(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Body, v), string(r.Body))
}

// Code returned by the error envelope, or "" when the body is not one.
func (r reply) ErrorCode() string {
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Body, &env)
	return env.Code
}

func (r reply) String() string { return string(r.Body) }

func (i *install) do(s *session, method, path string, body any) reply {
	i.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(i.t, err)
		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, "/api/v1"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s != nil && s.cookie != "" {
		req.Header.Set("Cookie", httpapi.SessionCookie+"="+s.cookie)
	}
	if s != nil && s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, req)
	return reply{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

// anon makes a request with no credential at all. Anonymous is a principal,
// not an absence (R-075).
func (i *install) anon(method, path string, body any) reply {
	return i.do(nil, method, path, body)
}

// signIn returns a session for a username and password.
func (i *install) signIn(username, password string) (*session, reply) {
	i.t.Helper()
	got := i.do(nil, http.MethodPost, "/sessions",
		map[string]string{"username": username, "password": password})

	s := &session{}
	for _, c := range (&http.Response{Header: got.Hdr}).Cookies() {
		if c.Name == httpapi.SessionCookie {
			s.cookie = c.Value
		}
	}
	return s, got
}

// admin is the first-run administrator, signed in.
func (i *install) admin() *session {
	i.t.Helper()
	s, got := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	require.NotEmpty(i.t, s.cookie)
	return s
}

// user creates an ordinary account and signs in as it. It holds no
// install-scoped verb and owns no app (R-087).
func (i *install) user(username string) *session {
	i.t.Helper()
	const password = "another-correct-horse-staple"

	created := i.do(i.admin(), http.MethodPost, "/users", map[string]any{
		"username": username, "display_name": username, "password": password,
	})
	require.Equal(i.t, http.StatusCreated, created.Code, created.String())

	s, got := i.signIn(username, password)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	return s
}

// tokenFor mints a delegated token for the signed-in caller (R-058).
func (i *install) tokenFor(s *session) *session {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/tokens", map[string]any{"name": "test token"})
	require.Equal(i.t, http.StatusCreated, got.Code, got.String())

	var issued struct {
		Secret string `json:"secret"`
	}
	got.JSON(i.t, &issued)
	require.NotEmpty(i.t, issued.Secret)
	return &session{token: issued.Secret}
}

// minimalSpec is the smallest spec that validates: one primary workload
// serving HTTP, from a prebuilt image so no builder is needed.
func minimalSpec() map[string]any {
	return map[string]any{
		"schema_version": spec.SchemaVersion,
		"source":         map[string]any{"type": "image", "image": "nginx:alpine"},
		"build":          map[string]any{"strategy": "prebuilt"},
		"workloads": []map[string]any{{
			"name":    "web",
			"image":   "nginx:alpine",
			"primary": true,
			"exposed": true,
			"ports":   []map[string]any{{"number": 8080, "protocol": "http", "source": "user"}},
		}},
	}
}

// writeSpec creates a revision from body and returns its number.
func (i *install) writeSpec(s *session, appID string, body map[string]any) int {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/apps/"+appID+"/specs", body)
	require.Equal(i.t, http.StatusCreated, got.Code, got.String())

	var rev struct {
		Revision int `json:"revision"`
	}
	got.JSON(i.t, &rev)
	require.Positive(i.t, rev.Revision)
	return rev.Revision
}

// pinSpec points the app at a revision. Pinning does not deploy.
func (i *install) pinSpec(s *session, appID string, revision int) {
	i.t.Helper()
	got := i.do(s, http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", appID, revision), map[string]any{})
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
}

// appWithSpec is an app carrying one pinned revision, which is the state most
// of the interesting endpoints need before they do anything.
func (i *install) appWithSpec(s *session, name string) string {
	i.t.Helper()
	id := i.createApp(s, name)
	i.pinSpec(s, id, i.writeSpec(s, id, minimalSpec()))
	return id
}

// createApp makes an app owned by the caller and returns its ID.
func (i *install) createApp(s *session, name string) string {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/apps", map[string]any{
		"name":   name,
		"source": map[string]string{"type": "git", "url": "https://github.com/acme/" + name},
	})
	require.Contains(i.t, []int{http.StatusCreated, http.StatusAccepted}, got.Code, got.String())

	var app struct {
		ID string `json:"id"`
	}
	got.JSON(i.t, &app)
	require.NotEmpty(i.t, app.ID)
	return app.ID
}

// declaredAI is what main hands the assignments service from the config
// file's adapters.
func declaredAI(startup *config.Config) []assist.Declared {
	if startup == nil {
		return nil
	}
	var out []assist.Declared
	for _, d := range startup.Adapters {
		for _, f := range d.Functions {
			out = append(out, assist.Declared{
				Function: adapterapi.AIFunction(f.Function), Adapter: d.ID, Model: f.Model,
				Source: assist.Source{Kind: f.Source.Kind, Name: f.Source.Name, Key: f.Source.Key},
			})
		}
	}
	return out
}

// testTokenKey is the API token key every test install uses. In production it
// is a random file under /var/lib/pando (core/tokenkey).
var testTokenKey = []byte("0123456789abcdef0123456789abcdef")
