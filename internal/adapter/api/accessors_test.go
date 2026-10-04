package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

// base carries the four methods every adapter has, so each stub below only
// declares what makes it its own category.
type base struct {
	kind string
	cat  api.Category
	// health is returned by HealthCheck, so a test can make one adapter in a
	// registry unhealthy and leave the rest alone.
	health error
}

func (b base) Kind() string                                   { return b.kind }
func (b base) Category() api.Category                         { return b.cat }
func (base) Configure(context.Context, json.RawMessage) error { return nil }
func (b base) HealthCheck(context.Context) error              { return b.health }

type stubRuntime struct{ base }

func (stubRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{}, nil
}
func (stubRuntime) Capacity(context.Context) (api.Capacity, error) { return api.Capacity{}, nil }
func (stubRuntime) InUse(context.Context) (api.InUse, error)       { return api.InUse{}, nil }
func (stubRuntime) Apply(context.Context, api.BundlePlan) (api.BundleHandle, error) {
	return api.BundleHandle{}, nil
}
func (stubRuntime) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	return api.ObservedBundle{}, nil
}
func (stubRuntime) Usage(context.Context, api.BundleRef) (api.BundleUsage, error) {
	return api.BundleUsage{}, nil
}
func (stubRuntime) Stop(context.Context, api.BundleRef) error                        { return nil }
func (stubRuntime) Destroy(context.Context, api.BundleRef, api.DestroyOptions) error { return nil }
func (stubRuntime) CreateVolume(context.Context, api.VolumeRequest) (api.VolumeHandle, error) {
	return api.VolumeHandle{}, nil
}
func (stubRuntime) DestroyVolume(context.Context, api.VolumeHandle) error { return nil }
func (stubRuntime) SnapshotVolume(context.Context, api.VolumeHandle, io.Writer) error {
	return nil
}
func (stubRuntime) RestoreVolume(context.Context, api.VolumeHandle, io.Reader) error { return nil }
func (stubRuntime) ImportImage(context.Context, io.Reader) (string, error)           { return "", nil }
func (stubRuntime) Logs(context.Context, api.WorkloadRef, api.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (stubRuntime) Exec(context.Context, api.WorkloadRef, api.ExecRequest) (api.ExecSession, error) {
	return nil, nil
}
func (stubRuntime) Upstream(context.Context, api.WorkloadRef, int) (api.Upstream, error) {
	return api.Upstream{}, nil
}
func (stubRuntime) Trial(context.Context, api.TrialRequest) (api.TrialResult, error) {
	return api.TrialResult{}, nil
}

type stubBuilder struct{ base }

func (stubBuilder) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return api.BuilderCapabilities{}, nil
}
func (stubBuilder) Bid(context.Context, api.SourceView) (api.Bid, error) { return api.Bid{}, nil }
func (stubBuilder) Build(context.Context, api.BuildRequest) (api.BuildResult, error) {
	return api.BuildResult{}, nil
}
func (stubBuilder) Forget(context.Context, string) error { return nil }

type stubSecrets struct{ base }

func (stubSecrets) Put(context.Context, api.SecretRef, secret.Value) (api.StoredRef, error) {
	return api.StoredRef{}, nil
}
func (stubSecrets) Get(context.Context, api.StoredRef) (secret.Value, error) {
	return secret.Value{}, nil
}
func (stubSecrets) Delete(context.Context, api.StoredRef) error { return nil }

type stubServices struct {
	base
	supports []spec.SlotType
}

func (stubServices) Capabilities() api.ServicesCapabilities { return api.ServicesCapabilities{} }
func (s stubServices) Supports() []spec.SlotType            { return s.supports }
func (stubServices) Provision(context.Context, api.ProvisionRequest) (api.ProvisionResult, error) {
	return api.ProvisionResult{}, nil
}
func (stubServices) Destroy(context.Context, api.ServiceHandle) error             { return nil }
func (stubServices) Snapshot(context.Context, api.ServiceHandle, io.Writer) error { return nil }
func (stubServices) Restore(context.Context, api.ServiceHandle, io.Reader) error  { return nil }

type stubNotify struct{ base }

func (stubNotify) Notify(context.Context, api.Notification) error { return nil }

type stubBackup struct{ base }

func (stubBackup) Capabilities() api.BackupCapabilities { return api.BackupCapabilities{} }
func (stubBackup) Writer(context.Context, string) (io.WriteCloser, error) {
	return nil, nil
}
func (stubBackup) Reader(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (stubBackup) List(context.Context) ([]api.StoredBundle, error)      { return nil, nil }
func (stubBackup) Delete(context.Context, string) error                  { return nil }

type stubIdentity struct{ base }

func (stubIdentity) Begin(context.Context, api.BeginRequest) (*api.Redirect, error) { return nil, nil }
func (stubIdentity) ServiceMetadata(context.Context, api.Endpoints) (*api.Metadata, error) {
	return nil, nil
}
func (stubIdentity) Authenticate(context.Context, api.Credential) (api.Subject, error) {
	return api.Subject{}, nil
}
func (stubIdentity) SessionPolicy() api.SessionPolicy { return api.SessionPolicy{} }
func (stubIdentity) SupportsPush() bool               { return false }

func full(t *testing.T) *api.Registry {
	t.Helper()
	r := api.NewRegistry()
	require.NoError(t, r.Register("rt_docker_local", stubRuntime{base{kind: "docker", cat: api.CategoryRuntime}}))
	require.NoError(t, r.Register("rte_loopback", &stubRouting{}))
	require.NoError(t, r.Register("bld_buildkit", stubBuilder{base{kind: "buildkit", cat: api.CategoryBuilder}}))
	require.NoError(t, r.Register("sec_local", stubSecrets{base{kind: "local", cat: api.CategorySecrets}}))
	require.NoError(t, r.Register("ntf_console", stubNotify{base{kind: "console", cat: api.CategoryNotify}}))
	require.NoError(t, r.Register("bk_local", stubBackup{base{kind: "local", cat: api.CategoryBackup}}))
	require.NoError(t, r.Register("idp_local", stubIdentity{base{kind: "local", cat: api.CategoryIdentity}}))
	return r
}

// The typed accessors save every call site a type assertion. Which adapter you
// have is a different question from what it can do — the latter is always a
// Capabilities() call (R-254).
func TestTypedAccessorsResolveTheirOwnCategory(t *testing.T) {
	r := full(t)

	rt, ok := r.Runtime("rt_docker_local")
	require.True(t, ok)
	require.NotNil(t, rt)

	rte, ok := r.Routing("rte_loopback")
	require.True(t, ok)
	require.NotNil(t, rte)

	b, ok := r.Builder("bld_buildkit")
	require.True(t, ok)
	require.NotNil(t, b)

	s, ok := r.Secrets("sec_local")
	require.True(t, ok)
	require.NotNil(t, s)

	bk, ok := r.Backup("bk_local")
	require.True(t, ok)
	require.NotNil(t, bk)

	n, ok := r.Notify("ntf_console")
	require.True(t, ok)
	require.NotNil(t, n)

	_, ok = r.Notify("bk_local")
	require.False(t, ok, "a backup adapter is not a notifier")
}

func TestATypedAccessorRefusesAnotherCategorysAdapter(t *testing.T) {
	r := full(t)

	_, ok := r.Runtime("rte_loopback")
	require.False(t, ok, "a routing adapter is not a runtime")

	_, ok = r.Builder("sec_local")
	require.False(t, ok)

	_, ok = r.Secrets("bld_buildkit")
	require.False(t, ok)

	_, ok = r.Backup("rt_docker_local")
	require.False(t, ok)

	_, ok = r.Services("ntf_console")
	require.False(t, ok)
}

func TestATypedAccessorForAnUnregisteredRefIsNotFound(t *testing.T) {
	r := api.NewRegistry()

	for _, lookup := range []func(string) bool{
		func(ref string) bool { _, ok := r.Runtime(ref); return ok },
		func(ref string) bool { _, ok := r.Routing(ref); return ok },
		func(ref string) bool { _, ok := r.Builder(ref); return ok },
		func(ref string) bool { _, ok := r.Secrets(ref); return ok },
		func(ref string) bool { _, ok := r.Services(ref); return ok },
		func(ref string) bool { _, ok := r.Backup(ref); return ok },
	} {
		require.False(t, lookup("nothing_registered"))
	}
}

func TestByCategoryAndRefsAreSortedAndCopied(t *testing.T) {
	r := api.NewRegistry()
	require.NoError(t, r.Register("rte_zulu", &stubRouting{}))
	require.NoError(t, r.Register("rte_alpha", &stubRouting{}))

	refs := r.ByCategory(api.CategoryRouting)
	require.Equal(t, []string{"rte_alpha", "rte_zulu"}, refs)

	// The caller gets a copy: mutating it must not reorder the registry.
	refs[0] = "clobbered"
	require.Equal(t, []string{"rte_alpha", "rte_zulu"}, r.ByCategory(api.CategoryRouting))

	require.Equal(t, []string{"rte_alpha", "rte_zulu"}, r.Refs())
	require.Empty(t, r.ByCategory(api.CategoryBackup), "an empty category is empty, not missing")
}

func TestRefsListsEveryCategory(t *testing.T) {
	require.Equal(t, []string{
		"bk_local", "bld_buildkit", "idp_local", "ntf_console",
		"rt_docker_local", "rte_loopback", "sec_local",
	}, full(t).Refs())
}

func TestDefaultOfACategoryWithNoDefault(t *testing.T) {
	_, ok := api.NewRegistry().Default(api.CategoryRuntime)
	require.False(t, ok)
}

// R-131: a slot says what it needs, not who should supply it. An app declaring
// REDIS_URL has no opinion about which adapter stands a Redis up.
func TestR131_ServicesForResolvesBySlotTypeNotByReference(t *testing.T) {
	r := api.NewRegistry()
	require.NoError(t, r.Register("svc_postgres_only",
		stubServices{base{kind: "a", cat: api.CategoryServices}, []spec.SlotType{spec.SlotPostgres}}))
	require.NoError(t, r.Register("svc_redis_only",
		stubServices{base{kind: "b", cat: api.CategoryServices}, []spec.SlotType{spec.SlotRedis}}))

	_, ref, ok := r.ServicesFor(spec.SlotRedis)
	require.True(t, ok)
	require.Equal(t, "svc_redis_only", ref)

	_, ref, ok = r.ServicesFor(spec.SlotPostgres)
	require.True(t, ok)
	require.Equal(t, "svc_postgres_only", ref)

	_, _, ok = r.ServicesFor(spec.SlotMySQL)
	require.False(t, ok, "no adapter supports it, and none is invented")
}

func TestServicesForPrefersTheCategoryDefault(t *testing.T) {
	r := api.NewRegistry()
	both := []spec.SlotType{spec.SlotPostgres}
	require.NoError(t, r.Register("svc_alpha",
		stubServices{base{kind: "a", cat: api.CategoryServices}, both}))
	require.NoError(t, r.Register("svc_zulu",
		stubServices{base{kind: "z", cat: api.CategoryServices}, both}))

	// Alphabetically first wins without a default set.
	_, ref, ok := r.ServicesFor(spec.SlotPostgres)
	require.True(t, ok)
	require.Equal(t, "svc_alpha", ref)

	require.NoError(t, r.SetDefault(api.CategoryServices, "svc_zulu"))
	_, ref, ok = r.ServicesFor(spec.SlotPostgres)
	require.True(t, ok)
	require.Equal(t, "svc_zulu", ref, "the default is asked first")
}

// The planner refuses to plan against an unhealthy adapter rather than failing
// mid-deploy (R-254).
func TestHealthCheckAllReportsOnlyTheUnhealthy(t *testing.T) {
	r := api.NewRegistry()
	require.NoError(t, r.Register("rte_ok", &stubRouting{}))

	down := errors.New("connection refused")
	require.NoError(t, r.Register("bk_down",
		stubBackup{base{kind: "local", cat: api.CategoryBackup, health: down}}))

	unhealthy := r.HealthCheckAll(context.Background())
	require.Len(t, unhealthy, 1)
	require.ErrorIs(t, unhealthy["bk_down"], down)
	require.NotContains(t, unhealthy, "rte_ok")
}

func TestHealthCheckAllOfAnEmptyRegistry(t *testing.T) {
	require.Empty(t, api.NewRegistry().HealthCheckAll(context.Background()))
}

func TestRegisterRejectsAnUnknownCategory(t *testing.T) {
	err := api.NewRegistry().Register("x_odd", base{kind: "odd", cat: api.Category("telepathy")})
	require.ErrorContains(t, err, "unknown category")
}

// Capabilities are data, and the membership tests over them are the planner's
// only way to ask what an adapter can do (R-254).
func TestCapabilitySupportsIsAMembershipTest(t *testing.T) {
	routing := api.RoutingCapabilities{Modes: []api.RoutingMode{spec.RoutingPort}}
	require.True(t, routing.Supports(spec.RoutingPort))
	require.False(t, routing.Supports(spec.RoutingSubdomain))
	require.False(t, api.RoutingCapabilities{}.Supports(spec.RoutingPort))

	builder := api.BuilderCapabilities{Strategies: []api.BuildStrategy{spec.BuildDockerfile}}
	require.True(t, builder.Supports(spec.BuildDockerfile))
	require.False(t, builder.Supports(spec.BuildBuildpack))
	require.False(t, api.BuilderCapabilities{}.Supports(spec.BuildDockerfile))
}

func (stubRuntime) ApplyEdge(context.Context, api.EdgePlan) error { return nil }
func (stubRuntime) ObserveEdge(context.Context, string) (api.EdgeState, error) {
	return api.EdgeState{}, nil
}
func (stubRuntime) RemoveEdge(context.Context, string) error { return nil }
func (stubRuntime) Edges(context.Context) ([]string, error)  { return nil, nil }
func (stubRuntime) EdgeVolumes(context.Context) ([]api.VolumeHandle, error) {
	return nil, nil
}
