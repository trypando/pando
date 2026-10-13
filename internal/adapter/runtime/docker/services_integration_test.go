//go:build integration

package docker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

// TestR131_AProvisionedPostgresTakesEveryClientsDefault asserts R-131: the URL a
// provisioned Postgres fills its slot with connects whatever sslmode the
// client's driver assumes. lib/pq assumes require, asyncpg and libpq prefer, and
// a driver told disable is still let in (issue #90).
func TestR131_AProvisionedPostgresTakesEveryClientsDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-pgtls-" + time.Now().Format("150405")
	cleanup(t, a, id)

	svcs := servicesdocker.New()
	require.NoError(t, svcs.Configure(ctx, nil))
	res, err := svcs.Provision(ctx, api.ProvisionRequest{
		AppID: id, BundleID: id, SlotKey: "DATABASE_URL", Type: spec.SlotPostgres,
		ServiceID: "svc_" + strings.ReplaceAll(id, "-", ""), Scheme: "postgres",
	})
	require.NoError(t, err)
	dsn := res.ConnectionSecret.Reveal()
	require.True(t, strings.HasPrefix(dsn, "postgres://"), "the app's own spelling")

	plan := bundle(id, nil)
	plan.Volumes = res.Volumes
	plan.Workloads = append(res.Workloads, api.WorkloadPlan{
		Name:      "web",
		Image:     "postgres:17-alpine",
		Command:   []string{"sleep", "3600"},
		Env:       map[string]secret.Value{"DATABASE_URL": res.ConnectionSecret},
		DependsOn: []string{res.Workloads[0].Name},
		Exposed:   true,
	})
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)

	query := "select ssl from pg_stat_ssl where pid = pg_backend_pid()"
	for mode, wantTLS := range map[string]string{"require": "t", "prefer": "t", "disable": "f"} {
		out := inContainer(t, id, "web", "sh", "-c", `psql "$DATABASE_URL?sslmode=`+mode+`" -tAc "`+query+`"`)
		require.Equal(t, wantTLS, strings.TrimSpace(out), "sslmode=%s", mode)
	}

	// And with nothing added, which is what an app does.
	out := inContainer(t, id, "web", "sh", "-c", `psql "$DATABASE_URL" -tAc "`+query+`"`)
	require.Equal(t, "t", strings.TrimSpace(out))

	// A redeploy changes nothing, so the database is not restarted: the
	// certificate is the same bytes every time (postgresTLS).
	again, err := svcs.Provision(ctx, api.ProvisionRequest{
		AppID: id, BundleID: id, SlotKey: "DATABASE_URL", Type: spec.SlotPostgres,
		ServiceID: "svc_" + strings.ReplaceAll(id, "-", ""), Scheme: "postgres", ExistingSecret: res.ConnectionSecret,
	})
	require.NoError(t, err)
	before := dockerInspect(t, "pando-"+id+"-"+res.Workloads[0].Name, "{{.Id}}")
	plan.Workloads[0] = again.Workloads[0]
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)
	require.Equal(t, before, dockerInspect(t, "pando-"+id+"-"+res.Workloads[0].Name, "{{.Id}}"))
}
