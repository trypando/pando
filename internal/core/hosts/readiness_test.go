package hosts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type fakeDestination struct {
	api.BackupAdapter
	offHost bool
}

func (f fakeDestination) Capabilities() api.BackupCapabilities {
	return api.BackupCapabilities{OffHost: f.offHost}
}

type fakeDestinations struct {
	def string
	by  map[string]bool
}

func (f fakeDestinations) Default(api.Category) (string, bool) { return f.def, f.def != "" }

func (f fakeDestinations) Backup(ref string) (api.BackupAdapter, bool) {
	off, ok := f.by[ref]
	if !ok {
		return nil, false
	}
	return fakeDestination{offHost: off}, true
}

type fakePolicy struct {
	doc policy.Document
	err error
}

func (f fakePolicy) Load(context.Context) (policy.Document, error) { return f.doc, f.err }

type fakeRegistry struct{ configured bool }

func (f fakeRegistry) Configured() bool { return f.configured }
func (fakeRegistry) Always() bool       { return false }
func (fakeRegistry) Host() string       { return "registry.internal" }

type fakeRegistries struct {
	reg planner.InstallRegistry
	err error
}

func (f fakeRegistries) CurrentRegistry(context.Context) (planner.InstallRegistry, error) {
	return f.reg, f.err
}

// ready is an install with everything a second host needs.
func ready() (fakeDestinations, fakePolicy, fakeRegistries) {
	return fakeDestinations{def: "bk_s3", by: map[string]bool{"bk_s3": true, "bk_local": false}},
		fakePolicy{doc: policy.Document{AuditArchive: audit.ArchiveExport}},
		fakeRegistries{reg: fakeRegistry{configured: true}}
}

func refusal(t *testing.T, err error) *errs.Error {
	t.Helper()
	require.Error(t, err)
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.StateInvalid, e.Code)
	require.NotEmpty(t, e.Remedy, "R-105: a refusal names the fix")
	return e
}

// TestR417_ASecondHostJoinsOnlyOnceNothingStaysOnThisHost asserts R-417's
// join prerequisites: each one refuses alone, naming its fix, and an install
// with all of them passes.
func TestR417_ASecondHostJoinsOnlyOnceNothingStaysOnThisHost(t *testing.T) {
	ctx := context.Background()
	roles := []string{"control", "app"}

	b, p, r := ready()
	require.NoError(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))

	t.Run("local backup destination", func(t *testing.T) {
		b, p, r := ready()
		b.def = "bk_local"
		e := refusal(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
		require.Contains(t, e.Message, `"bk_local"`)
		require.Contains(t, e.Remedy, "off this host")
	})
	t.Run("no backup destination", func(t *testing.T) {
		b, p, r := ready()
		b.def = ""
		e := refusal(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
		require.Contains(t, e.Message, "no backup destination")
	})
	t.Run("a default that is not running", func(t *testing.T) {
		b, p, r := ready()
		b.def = "bk_gone"
		refusal(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
	})
	t.Run("kept audit archives", func(t *testing.T) {
		for _, mode := range []audit.ArchiveMode{audit.ArchiveKeep, ""} {
			b, p, r := ready()
			p.doc.AuditArchive = mode
			e := refusal(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
			require.Contains(t, e.Message, "audit_archive is keep")
			require.Contains(t, e.Remedy, "export")
		}
	})
	t.Run("audit archives exported to this host", func(t *testing.T) {
		b, p, r := ready()
		p.doc.AuditArchiveDestination = "bk_local"
		e := refusal(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
		require.Contains(t, e.Remedy, "audit_archive_destination")

		p.doc.AuditArchiveDestination = "bk_s3"
		require.NoError(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
	})
	t.Run("audit archiving off", func(t *testing.T) {
		b, p, r := ready()
		p.doc.AuditArchive = audit.ArchiveOff
		require.NoError(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, roles))
	})
	t.Run("no registry", func(t *testing.T) {
		for _, reg := range []planner.InstallRegistry{nil, fakeRegistry{}} {
			b, p, r := ready()
			r.reg = reg
			e := refusal(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, []string{"app"}))
			require.Contains(t, e.Remedy, "PANDO_REGISTRY_URL")
		}
	})
	t.Run("every reason at once", func(t *testing.T) {
		e := refusal(t, hosts.NewReadiness(fakeDestinations{}, fakePolicy{}, fakeRegistries{}).ReadyToJoin(ctx, roles))
		require.Contains(t, e.Message, "for 3 reasons")
		require.Contains(t, e.Remedy, "(3)")
	})
}

func TestReadinessSaysWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	b, p, r := ready()
	p.err = errors.New("policy unreadable")
	require.ErrorContains(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, nil), "policy unreadable")

	b, p, r = ready()
	r.err = errors.New("registry unreadable")
	require.ErrorContains(t, hosts.NewReadiness(b, p, r).ReadyToJoin(ctx, nil), "registry unreadable")
}

// A cluster of one is this install: one row once it has recorded itself, and
// one before it has.
func TestR417_TheClusterCountIncludesThisHost(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	n, err := newService(t, store).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	store.hosts = []state.Host{{ID: "host_01A"}}
	n, err = newService(t, store).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	store.hosts = append(store.hosts, state.Host{ID: "host_01B", JoinedWith: "jt_1"})
	n, err = newService(t, store).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// TestR417_ControlHostsAreCountedByRole asserts that the control hosts are
// those holding the control role, at least this install's own: a cluster of
// one control host and app hosts has one edge.
func TestR417_ControlHostsAreCountedByRole(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	n, err := newService(t, store).ControlCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "this install, before it is recorded")

	store.hosts = []state.Host{
		{ID: "host_01A", Roles: []string{"control", "app"}},
		{ID: "host_01B", Roles: []string{"app"}},
		{ID: "host_01C", Roles: []string{"app"}},
	}
	n, err = newService(t, store).ControlCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	store.hosts = append(store.hosts, state.Host{ID: "host_01D", Roles: []string{"control"}})
	n, err = newService(t, store).ControlCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

type fakeRouting struct {
	api.RoutingAdapter
	perEdge bool
	err     error
}

func (f fakeRouting) Capabilities(context.Context) (api.RoutingCapabilities, error) {
	return api.RoutingCapabilities{CertificatesPerEdge: f.perEdge}, f.err
}

type fakeRoutes map[string]fakeRouting

func (f fakeRoutes) ByCategory(api.Category) []string {
	refs := make([]string, 0, len(f)+1)
	for ref := range f {
		refs = append(refs, ref)
	}
	return append(refs, "rte_gone")
}

func (f fakeRoutes) Routing(ref string) (api.RoutingAdapter, bool) {
	r, ok := f[ref]
	return r, ok
}

// TestR417_AControlHostJoinsOnlyWhenItsEdgeCanShareCertificates asserts
// O-68's refusal: a routing adapter whose edge orders its own certificates,
// where Pando cannot order them for every host, refuses a control host's join
// naming the fix, and leaves an app host's join alone, since an app host runs
// no edge.
func TestR417_AControlHostJoinsOnlyWhenItsEdgeCanShareCertificates(t *testing.T) {
	ctx := context.Background()
	b, p, r := ready()
	check := hosts.NewReadiness(b, p, r)
	check.Routing = fakeRoutes{"rte_traefik": {perEdge: false}}
	require.NoError(t, check.ReadyToJoin(ctx, []string{"control"}))

	check.Routing = fakeRoutes{"rte_traefik": {perEdge: false}, "rte_theirs": {perEdge: true}}
	for _, roles := range [][]string{{"control"}, {"app", "control"}, nil} {
		e := refusal(t, check.ReadyToJoin(ctx, roles))
		require.Contains(t, e.Message, `"rte_theirs"`)
		require.Contains(t, e.Message, "orders its own certificates")
		require.Contains(t, e.Remedy, "Let Pando run Traefik")
	}
	require.NoError(t, check.ReadyToJoin(ctx, []string{"app"}))

	check.Routing = fakeRoutes{"rte_traefik": {err: errors.New("capabilities unreadable")}}
	require.ErrorContains(t, check.ReadyToJoin(ctx, []string{"control"}), "capabilities unreadable")
}
