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

// A cluster of one is this install, with no rows.
func TestR417_TheClusterCountIncludesThisHost(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	n, err := newService(t, store).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	store.hosts = []state.Host{{ID: "host_01A"}}
	n, err = newService(t, store).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}
