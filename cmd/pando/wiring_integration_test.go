//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
)

// Adapter registration happens in main, from compiled-in packages (R-253).
// These are its tests: the parts of startup that decide what an install can do.

// connected is a fresh, migrated database of the test's own, copied from one
// prepared for the package (statetest).
func connected(t *testing.T) *state.DB {
	t.Helper()
	db, _ := statetest.Connect(t)
	return db
}

// connectedURL is a database and the URL it connected to, for a command that
// opens its own connection from configuration.
//
// In a Postgres of its own rather than a statetest copy: the command connects
// the way a server starts, through state.Connect, and that sets a new password
// on a role belonging to the whole cluster — under every other test's pool
// if the cluster were shared.
func connectedURL(t *testing.T) (*state.DB, string) {
	t.Helper()
	ctx := context.Background()

	container, err := statetest.Postgres(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: dsn})
	require.NoError(t, err)
	t.Cleanup(db.Close)
	return db, dsn
}

// recorded returns a logger whose lines a test can read back.
func recorded() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// quoted renders a path as a JSON string, for building an adapter's config.
func quoted(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

// R-002: setup cost is paid once, and seeding a working set is part of not
// charging it twice.
func TestR002_AFreshInstallIsSeededWithAWorkingSetOfAdapters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)

	require.NoError(t, seedDefaultAdapters(ctx, store))

	configured, err := store.List(ctx)
	require.NoError(t, err)

	byCategory := map[string]state.AdapterConfig{}
	for _, c := range configured {
		byCategory[c.Category] = c
	}

	// Something to run apps with, something to reach them by, somewhere to keep
	// secrets, something to build with, somewhere to back up to, a way to fill
	// a slot, and somewhere for a notification to land.
	for _, category := range []adapterapi.Category{
		adapterapi.CategoryRuntime, adapterapi.CategoryRouting, adapterapi.CategorySecrets,
		adapterapi.CategoryBuilder, adapterapi.CategoryBackup, adapterapi.CategoryServices,
		adapterapi.CategoryNotify,
	} {
		c, ok := byCategory[string(category)]
		require.True(t, ok, "nothing seeded for %s", category)
		require.True(t, c.Enabled, "%s was seeded disabled", category)
		require.True(t, c.IsDefault, "%s was seeded without being the default", category)
		require.NotEmpty(t, c.Name, "%s has no name for the console to show", category)
	}
}

// Seeded per category, not once per install.
//
// "If any adapter exists, do nothing" is right on the first run and wrong on
// every upgrade: a category added in a later version would never be seeded on
// an install that already had others, so the feature would ship and silently
// not exist. That is what happened to notifications.
func TestACategoryAddedLaterIsSeededOnAnInstallThatAlreadyHasOthers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)

	require.NoError(t, seedDefaultAdapters(ctx, store))

	// An install that predates the notify category.
	_, err := db.Exec(ctx, `DELETE FROM adapter_configs WHERE category = $1`,
		string(adapterapi.CategoryNotify))
	require.NoError(t, err)

	require.NoError(t, seedDefaultAdapters(ctx, store))

	configured, err := store.List(ctx)
	require.NoError(t, err)

	var seeded bool
	for _, c := range configured {
		if c.Category == string(adapterapi.CategoryNotify) {
			seeded = true
		}
	}
	require.True(t, seeded, "the category that arrived later was never filled")
}

// Idempotent on every start after the first: seeding must not multiply the
// adapters or reset one an operator reconfigured.
func TestSeedingTwiceChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)

	require.NoError(t, seedDefaultAdapters(ctx, store))
	before, err := store.List(ctx)
	require.NoError(t, err)

	require.NoError(t, seedDefaultAdapters(ctx, store))
	after, err := store.List(ctx)
	require.NoError(t, err)

	require.Len(t, after, len(before))
}

// An operator's own choice in a category stands: the default is not re-seeded
// over it.
func TestAConfiguredCategoryIsNotOverwrittenBySeeding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)

	require.NoError(t, store.Upsert(ctx, state.AdapterConfig{
		ID: "rte_traefik", Category: string(adapterapi.CategoryRouting), Kind: "traefik",
		Name: "Traefik", IsDefault: true, Enabled: true,
	}))
	require.NoError(t, seedDefaultAdapters(ctx, store))

	configured, err := store.List(ctx)
	require.NoError(t, err)

	var routing []state.AdapterConfig
	for _, c := range configured {
		if c.Category == string(adapterapi.CategoryRouting) {
			routing = append(routing, c)
		}
	}
	require.Len(t, routing, 1, "the operator's routing choice was not joined by a seeded one")
	require.Equal(t, "rte_traefik", routing[0].ID)
}

// R-253: registration is from compiled-in packages. Everything the seed puts in
// the table has an implementation here, or an install comes up missing one.
func TestR253_EverySeededAdapterHasAnImplementation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)
	logger, logs := recorded()

	// The seeded secrets adapter keeps its key at /var/lib/pando, which a test
	// cannot write to — and an adapter that cannot be configured is skipped, so
	// without this the category comes up empty for a reason that is about the
	// test machine rather than about the seed.
	require.NoError(t, store.Upsert(ctx, state.AdapterConfig{
		ID: "sek_local", Category: string(adapterapi.CategorySecrets), Kind: "local",
		Name: "Local storage", IsDefault: true, Enabled: true,
		Config: json.RawMessage(`{"key_path":` + quoted(t, t.TempDir()+"/secrets.key") + `}`),
	}))

	registry, _, err := registerAdapters(ctx, db, store, state.NewNotifications(db), nil, logger)
	require.NoError(t, err)

	require.Empty(t, logs.FilterMessage("skipping adapter of unknown kind").All(),
		"the seed named a kind this binary does not have")

	for _, category := range []adapterapi.Category{
		adapterapi.CategoryRuntime, adapterapi.CategoryRouting, adapterapi.CategorySecrets,
		adapterapi.CategoryBuilder, adapterapi.CategoryBackup, adapterapi.CategoryServices,
		adapterapi.CategoryNotify,
	} {
		ref, ok := registry.Default(category)
		require.True(t, ok, "no default registered for %s", category)
		_, found := registry.Get(ref)
		require.True(t, found, "%s defaults to %s, which is not registered", category, ref)
	}
}

// A disabled adapter is configuration an operator chose, and it must not be
// registered.
func TestADisabledAdapterIsNotRegistered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)
	logger, _ := recorded()

	require.NoError(t, seedDefaultAdapters(ctx, store))
	_, err := db.Exec(ctx, `UPDATE adapter_configs SET enabled = false, is_default = false
	                        WHERE category = $1`, string(adapterapi.CategoryBackup))
	require.NoError(t, err)

	registry, _, err := registerAdapters(ctx, db, store, state.NewNotifications(db), nil, logger)
	require.NoError(t, err)

	_, ok := registry.Default(adapterapi.CategoryBackup)
	require.False(t, ok)
}

// An adapter of a kind this binary does not have is skipped loudly, not fatal:
// one unknown row must not stop an install from starting.
func TestAnUnknownKindIsSkippedRatherThanFatal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)
	logger, logs := recorded()

	require.NoError(t, store.Upsert(ctx, state.AdapterConfig{
		ID: "rt_kubernetes", Category: string(adapterapi.CategoryRuntime), Kind: "kubernetes",
		Name: "Kubernetes", Enabled: true,
	}))

	registry, _, err := registerAdapters(ctx, db, store, state.NewNotifications(db), nil, logger)
	require.NoError(t, err)

	_, ok := registry.Get("rt_kubernetes")
	require.False(t, ok)
	require.NotEmpty(t, logs.FilterMessage("skipping adapter of unknown kind").All(),
		"it was skipped without saying so")
}

// An adapter that cannot be configured is skipped with its reason, rather than
// registered half-built.
func TestAnAdapterThatCannotBeConfiguredIsSkippedWithItsReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)
	logger, logs := recorded()

	require.NoError(t, store.Upsert(ctx, state.AdapterConfig{
		ID: "bkp_broken", Category: string(adapterapi.CategoryBackup), Kind: "local",
		Name: "Broken", Enabled: true, Config: json.RawMessage(`{"path":""}`),
	}))

	registry, _, err := registerAdapters(ctx, db, store, state.NewNotifications(db), nil, logger)
	require.NoError(t, err)

	_, ok := registry.Get("bkp_broken")
	require.False(t, ok)
	require.NotEmpty(t, logs.FilterMessage("adapter could not be configured and was skipped").All())
}

// The adapter owns where its key lives; a second copy in server config is a
// second copy to get wrong.
func TestTheSecretsKeyPathIsReadFromTheAdaptersOwnConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAdapters(db)
	logger, _ := recorded()

	require.NoError(t, store.Upsert(ctx, state.AdapterConfig{
		ID: "sek_local", Category: string(adapterapi.CategorySecrets), Kind: "local",
		Name: "Local storage", IsDefault: true, Enabled: true,
		Config: json.RawMessage(`{"key_path":"/srv/pando/secrets.key"}`),
	}))
	require.Equal(t, "/srv/pando/secrets.key", secretsKeyPath(ctx, store, logger))
}

// An install using an external secrets adapter has no local key, and the empty
// string is the correct answer — the bundle then carries no key because there
// is none to carry.
func TestAnInstallWithNoLocalSecretsAdapterHasNoKeyToBackUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	logger, _ := recorded()

	require.Empty(t, secretsKeyPath(ctx, state.NewAdapters(db), logger))
}

// R-097: nil when no runtime is configured, which detection treats as "no trial
// happened" — refusing to detect would make the failure arrive earlier and less
// clearly.
func TestR097_TheTrialRunnerIsNilWhenNoRuntimeIsConfigured(t *testing.T) {
	t.Parallel()
	require.Nil(t, runtimeForTrial(adapterapi.NewRegistry()))

	ctx := context.Background()
	db := connected(t)
	logger, _ := recorded()

	registry, _, err := registerAdapters(ctx, db, state.NewAdapters(db), state.NewNotifications(db), nil, logger)
	require.NoError(t, err)
	require.NotNil(t, runtimeForTrial(registry), "the seeded runtime supplies the trial")
}

// R-149: an install retrying a broken app every couple of seconds forever is a
// real way to melt a host, and nobody should be able to do it without being
// told. Not refused — a floor would make the schedule untestable end to end,
// which is the whole reason it is configurable.
func TestR149_AFastRetryScheduleIsWarnedAboutRatherThanRefused(t *testing.T) {
	t.Parallel()
	logger, logs := recorded()

	warnIfRetriesAreFast(logger, []time.Duration{0, time.Second})
	require.NotEmpty(t, logs.FilterMessage("retry backoff is configured faster than the shipped default").All())

	logger, logs = recorded()
	warnIfRetriesAreFast(logger, reconciler.DefaultBackoff)
	require.Empty(t, logs.All(), "the shipped default is not worth warning about")

	logger, logs = recorded()
	warnIfRetriesAreFast(logger, nil)
	require.Empty(t, logs.All(), "no schedule is the default, not a fast one")

	logger, logs = recorded()
	warnIfRetriesAreFast(logger, []time.Duration{0, reconciler.MinProductionCap})
	require.Empty(t, logs.All(), "exactly at the threshold is not below it")
}

// Every denial, not only successes: a denial pattern is the signal that matters
// for detecting misuse, and it is the thing most commonly left out.
func TestEveryAuthorizationDenialIsAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	auditDenials{audit.New(db.Pool)}.Denied(ctx,
		authz.Principal{Kind: authz.KindUser, ID: "usr_01HQ8", UserID: "usr_01HQ8"},
		"app_01HQ8", authz.AppDeploy, errs.PermDenied)

	records, err := audit.NewReader(db.Pool).List(ctx, audit.Query{Action: "authz.denied"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "app_01HQ8", records[0].AppID)
	require.Equal(t, string(authz.AppDeploy), records[0].Detail["verb"])
	require.Equal(t, string(errs.PermDenied), records[0].Detail["code"])
}

// An app verb allowed through an install-wide grant is recorded as such, with
// the grant that carried it (issue #81): the log says the access came from
// the installation, not from a grant on this app.
func TestR080_AccessThroughAnInstallGrantIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	auditDenials{audit.New(db.Pool)}.ThroughInstall(ctx,
		authz.Principal{Kind: authz.KindToken, ID: "tok_01HQ8", UserID: "usr_01HQ8"},
		"app_01HQ8", authz.AppDeploy, authz.InstallAppsDeploy,
		authz.Grant{ID: "gr_01HQ8", PrincipalKind: "group", PrincipalID: "grp_01HQ8", RoleID: authz.RoleAppManager})

	records, err := audit.NewReader(db.Pool).List(ctx, audit.Query{Action: "authz.install_wide"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	got := records[0]
	require.Equal(t, "app_01HQ8", got.AppID)
	require.Equal(t, "tok_01HQ8", got.PrincipalID)
	require.Equal(t, "usr_01HQ8", got.OnBehalfOf)
	require.Equal(t, "grant", got.TargetKind)
	require.Equal(t, "gr_01HQ8", got.TargetID)
	require.Equal(t, map[string]any{
		"verb":    string(authz.AppDeploy),
		"through": string(authz.InstallAppsDeploy),
		"role":    authz.RoleAppManager,
		"holder":  "group:grp_01HQ8",
	}, got.Detail)
}

// The reconciler acts with no principal — nobody asked for a drift correction,
// which is the point of it. Recorded as a system action so "who restarted this"
// has an answer, and the answer is Pando.
func TestTheReconcilersEventsAreRecordedAsTheSystem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	require.NoError(t, reconcilerAuditor{audit.New(db.Pool)}.Write(ctx, reconciler.AuditEvent{
		Action: "app.corrected", AppID: "app_01HQ8",
		Detail: map[string]any{"drift": "workload missing"},
	}))

	records, err := audit.NewReader(db.Pool).List(ctx, audit.Query{Action: "app.corrected"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, string(audit.KindSystem), records[0].PrincipalKind)
	require.Equal(t, "reconciler", records[0].PrincipalID)
	require.Equal(t, "app_01HQ8", records[0].AppID)
}

// Two methods rather than the whole registry, so the reconciler cannot reach
// for anything else.
func TestTheReconcilerSeesOnlyTheAdaptersItResolves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	logger, _ := recorded()

	registry, _, err := registerAdapters(ctx, db, state.NewAdapters(db), state.NewNotifications(db), nil, logger)
	require.NoError(t, err)

	adapters := registryAdapters{registry}

	runtimeRef, ok := registry.Default(adapterapi.CategoryRuntime)
	require.True(t, ok)
	_, found := adapters.Runtime(runtimeRef)
	require.True(t, found)

	routingRef, ok := registry.Default(adapterapi.CategoryRouting)
	require.True(t, ok)
	_, found = adapters.Routing(routingRef)
	require.True(t, found)

	_, found = adapters.Runtime("rt_not_configured")
	require.False(t, found)
	_, found = adapters.Routing(runtimeRef)
	require.False(t, found, "a runtime is not a routing adapter")
}

// A developer running `go run ./cmd/pando` without `make console` gets an API
// and no UI, which is the honest outcome — better than a panic at startup or a
// blank page that looks like a broken console rather than an absent one.
func TestTheConsoleIsOptionalAndSaysSoWhenAbsent(t *testing.T) {
	t.Parallel()
	logger, logs := recorded()

	handler := consoleHandler(logger)
	if handler == nil {
		require.NotEmpty(t, logs.FilterMessage(
			"console assets are not built into this binary; the UI will 404").All(),
			"it 404s silently, which reads as a broken console rather than an absent one")
		return
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

// The planner comes from the builder: core asks "how would you build this" and
// stores the answer without interpreting it (R-251). Nil with no builder, where
// detection still recognizes the language and asks its questions.
func TestR251_TheBuildPlannerComesFromTheBuilderOrIsAbsent(t *testing.T) {
	t.Parallel()
	require.Nil(t, buildPlanner(adapterapi.NewRegistry()))

	ctx := context.Background()
	db := connected(t)
	logger, _ := recorded()

	registry, _, err := registerAdapters(ctx, db, state.NewAdapters(db), state.NewNotifications(db), nil, logger)
	require.NoError(t, err)
	require.NotNil(t, buildPlanner(registry), "the seeded builder supplies one")
}

// A ref is resolved without cloning: auto-deploy asks this every few minutes
// for every app tracking a branch, and the answer is usually "the same as last
// time".
func TestTheRefResolverAnswersForSourcesThatCanAndCannotHaveOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Nothing to resolve is not a failure to resolve.
	got, err := refResolver{}.Resolve(ctx, spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.NoError(t, err)
	require.Empty(t, got)

	// A repository that is not there carries an envelope the caller can render.
	_, err = refResolver{}.Resolve(ctx, spec.Source{
		Type: spec.SourceGit, URL: t.TempDir() + "/not-a-repo", Ref: "main",
	})
	require.Error(t, err)
	require.NotNil(t, errs.As(err), "the failure carries an envelope")
	require.False(t, errors.Is(err, context.Canceled))
}
