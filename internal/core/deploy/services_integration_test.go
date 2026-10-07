//go:build integration

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// fakeServices provisions a Postgres by returning a workload, a volume and a
// connection string. It returns the prior secret when given one, as a real
// adapter must, so the same credentials survive a redeploy.
type fakeServices struct {
	calls   []api.ProvisionRequest
	failing bool
}

func (*fakeServices) Kind() string                                     { return "fake" }
func (*fakeServices) Category() api.Category                           { return api.CategoryServices }
func (*fakeServices) Configure(context.Context, json.RawMessage) error { return nil }
func (*fakeServices) HealthCheck(context.Context) error                { return nil }
func (*fakeServices) Capabilities() api.ServicesCapabilities {
	return api.ServicesCapabilities{DataInAppVolumes: true}
}
func (*fakeServices) Supports() []spec.SlotType                        { return []spec.SlotType{spec.SlotPostgres} }
func (*fakeServices) Destroy(context.Context, api.ServiceHandle) error { return nil }
func (*fakeServices) Snapshot(context.Context, api.ServiceHandle, io.Writer) error {
	return nil
}
func (*fakeServices) Restore(context.Context, api.ServiceHandle, io.Reader) error { return nil }
func (f *fakeServices) Provision(_ context.Context, req api.ProvisionRequest) (api.ProvisionResult, error) {
	f.calls = append(f.calls, req)
	if f.failing {
		return api.ProvisionResult{}, errors.New("the daemon refused")
	}
	dsn := secret.New("postgres://app:generated-" + req.ServiceID + "@db:5432/app")
	if !req.ExistingSecret.IsZero() {
		dsn = req.ExistingSecret
	}
	return api.ProvisionResult{
		Handle:           api.ServiceHandle{Handle: "svc-" + req.ServiceID},
		ConnectionSecret: dsn,
		Workloads: []api.WorkloadPlan{{Name: "svc-db", Image: "postgres:17-alpine",
			Env: map[string]secret.Value{"POSTGRES_PASSWORD": secret.New("pw")}}},
		Volumes: []api.VolumePlan{{VolumeID: "vol_db", Name: "svc-db-data"}},
	}, nil
}

// provisioningRunner is a Runner with a real state store, a fake services
// adapter, and an app to provision for.
func provisioningRunner(t *testing.T) (*Runner, *fakeServices, *spec.AppSpec) {
	t.Helper()
	ctx := context.Background()

	db, _ := statetest.Connect(t)

	users := state.NewUsers(db)
	first, err := bootstrap.Run(ctx, users, state.NewGrants(db), db, audit.New(db.Pool), secret.New("a-first-password-123"))
	require.NoError(t, err)
	app, err := state.NewApps(db).Create(ctx, "notes", "notes-x1", first.User.ID, first.User.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/notes"})
	require.NoError(t, err)

	secretsAdapter := secretslocal.New()
	require.NoError(t, secretsAdapter.Configure(ctx, json.RawMessage(`{"key_path":"`+filepath.Join(t.TempDir(), "k")+`"}`)))
	fake := &fakeServices{}
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("svc_docker", fake))

	r := &Runner{registry: reg}
	r.WithServices(state.NewServices(db), state.NewSecrets(db, secretsAdapter, "sec_local"))

	s := specWithProvisionedSlot()
	s.AppID = app.ID
	return r, fake, s
}

// TestR131_AProvisionedDatabaseKeepsItsCredentialsAcrossDeploys asserts R-131:
// the first deploy provisions and stores the connection string; every later one
// hands the stored one back so the data on disk still opens.
func TestR131_AProvisionedDatabaseKeepsItsCredentialsAcrossDeploys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, fake, s := provisioningRunner(t)

	var log bytes.Buffer
	first, err := r.provision(ctx, s, &log)
	require.NoError(t, err)
	require.Contains(t, log.String(), "Provisioning a PostgreSQL database for DATABASE_URL")
	require.Len(t, first.workloads, 1)
	require.Equal(t, "svc-db", first.names["DATABASE_URL"])
	dsn := first.connections["DATABASE_URL"].Reveal()

	log.Reset()
	second, err := r.provision(ctx, s, &log)
	require.NoError(t, err)
	require.Empty(t, log.String(), "provisioned once, not again")
	require.Equal(t, dsn, second.connections["DATABASE_URL"].Reveal(), "the same credentials the data was made with")
	require.False(t, fake.calls[1].ExistingSecret.IsZero())
	require.Equal(t, fake.calls[0].ServiceID, fake.calls[1].ServiceID)

	// The reconciler's view: the service's shape, and never its secrets (R-193).
	shape, err := r.ServiceShapes(ctx, s)
	require.NoError(t, err)
	require.Len(t, shape.Workloads, 1)
	require.Nil(t, shape.Workloads[0].Env)
	require.Len(t, shape.Volumes, 1)
	require.True(t, fake.calls[2].ExistingSecret.IsZero(), "the shape is never a reason to decrypt a secret")
}

// TestR148_ARestoredWorkloadGetsTheEnvironmentItWasDeployedWith asserts what
// the reconciler starts when it re-creates something that was killed: the
// service with its own credentials, and the app with the connection string
// to it — the same values the deploy gave them.
//
// The reconciler applied the shape it compares, which has no environment by
// design (R-193). A provisioned Redis takes its password from the
// environment, so the one it re-created exited on every start and the
// killed service never came back.
func TestR148_ARestoredWorkloadGetsTheEnvironmentItWasDeployedWith(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, fake, s := provisioningRunner(t)
	r.secrets = r.secretStore

	deployed, err := r.provision(ctx, s, io.Discard)
	require.NoError(t, err)
	dsn := deployed.connections["DATABASE_URL"].Reveal()

	envs, err := r.Environments(ctx, s)
	require.NoError(t, err)
	require.Equal(t, "pw", envs["svc-db"]["POSTGRES_PASSWORD"].Reveal(), "the service keeps its credentials")
	require.Equal(t, dsn, envs["web"]["DATABASE_URL"].Reveal(), "the app is pointed at the same database")
	require.False(t, fake.calls[len(fake.calls)-1].ExistingSecret.IsZero(),
		"the stored credentials, never new ones that would not open the data on disk")
}

// TestR148_RestoringAnEnvironmentNeverProvisionsAService asserts R-148's
// limit: the reconciler restores what exists and creates nothing. A slot with
// no recorded instance, or one whose type no adapter now provisions, is
// skipped rather than stood up; a bound slot is not a service at all; and an
// adapter that refuses stops the correction instead of starting the app
// without its connection string.
func TestR148_RestoringAnEnvironmentNeverProvisionsAService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, fake, s := provisioningRunner(t)
	r.secrets = r.secretStore
	cacheKey := "CACHE_URL"
	s.Slots = append(s.Slots, spec.Slot{Key: cacheKey, Type: spec.SlotRedis,
		Resolution: &spec.Resolution{Mode: spec.ResolutionBound, Target: "redis://cache:6379"}})
	s.Workloads[0].Env = append(s.Workloads[0].Env, spec.EnvEntry{Key: cacheKey, SlotRef: &cacheKey})

	// Never deployed: nothing recorded, so nothing is provisioned.
	s.Slots[0].Required = false
	s.Slots[0].Resolution = nil
	envs, err := r.Environments(ctx, s)
	require.NoError(t, err)
	require.Empty(t, fake.calls, "an optional slot nobody filled is not a reason to provision")
	require.Equal(t, "redis://cache:6379", envs["web"][cacheKey].Reveal())

	s.Slots[0].Resolution = &spec.Resolution{Mode: spec.ResolutionProvisioned}
	_, err = r.Environments(ctx, s)
	require.Error(t, err, "the app reads a slot that has no service yet")
	require.Empty(t, fake.calls, "the reconciler never creates a database; a deploy does")

	_, err = r.provision(ctx, s, io.Discard)
	require.NoError(t, err)
	calls := len(fake.calls)

	fake.failing = true
	_, err = r.Environments(ctx, s)
	require.ErrorContains(t, err, "refused")
	fake.failing = false

	// The recorded instance's type has no adapter any more.
	s.Slots[0].Type = spec.SlotRedis
	_, err = r.Environments(ctx, s)
	require.Error(t, err, "without the service there is no connection string to give the app")
	require.Len(t, fake.calls, calls+1, "only the refused attempt reached the adapter")
}

func TestASlotNothingCanProvisionIsRefusedWithTheWayOut(t *testing.T) {
	t.Parallel()
	r, _, s := provisioningRunner(t)
	s.Slots[0].Type = spec.SlotRedis
	_, err := r.provision(context.Background(), s, io.Discard)
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, errs.PlanAdapterNotConfigured, e.Code)
	require.Contains(t, e.Remedy, "connection string")

	shape, err := r.ServiceShapes(context.Background(), s)
	require.NoError(t, err)
	require.Empty(t, shape.Workloads, "a slot with no recorded instance is not provisioned by the reconciler")
}

func TestAProvisionerThatFailsStopsTheDeploy(t *testing.T) {
	t.Parallel()
	r, fake, s := provisioningRunner(t)
	fake.failing = true
	_, err := r.provision(context.Background(), s, io.Discard)
	require.ErrorContains(t, err, "refused")

	fake.failing = false
	_, err = r.provision(context.Background(), s, io.Discard)
	require.NoError(t, err)
	fake.failing = true
	_, err = r.ServiceShapes(context.Background(), s)
	require.ErrorContains(t, err, "refused")
}

func TestWithoutAServicesStoreNothingIsProvisioned(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	got, err := r.provision(context.Background(), specWithProvisionedSlot(), io.Discard)
	require.NoError(t, err)
	require.Empty(t, got.workloads)
	shape, err := r.ServiceShapes(context.Background(), specWithProvisionedSlot())
	require.NoError(t, err)
	require.Empty(t, shape.Workloads)
}
