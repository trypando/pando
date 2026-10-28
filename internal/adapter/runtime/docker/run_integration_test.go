//go:build integration

package docker_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR423_RunKeepsStdoutApartFromStderr asserts what a dump relies on: the
// command's stdout is the caller's and nothing else, its stdin reaches it to
// the end, a failure says what the command said, and a workload that is not
// running is NotFound.
func TestR423_RunKeepsStdoutApartFromStderr(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-run-" + time.Now().Format("150405.000")
	id = strings.ReplaceAll(id, ".", "")
	cleanup(t, a, id)
	_, err := a.Apply(ctx, bundle(id, nil))
	require.NoError(t, err)
	ref := api.WorkloadRef{BundleID: id, Workload: "web"}

	var out bytes.Buffer
	require.NoError(t, a.Run(ctx, ref, api.RunRequest{
		Command: []string{"sh", "-c", "cat; echo a warning >&2"},
		Stdin:   strings.NewReader("every byte of the dump"),
		Stdout:  &out,
	}))
	require.Equal(t, "every byte of the dump", out.String())

	err = a.Run(ctx, ref, api.RunRequest{Command: []string{"sh", "-c", "echo first >&2; echo it broke >&2; exit 3"}})
	require.ErrorContains(t, err, "status 3")
	require.ErrorContains(t, err, "it broke")

	err = a.Run(ctx, api.WorkloadRef{BundleID: id, Workload: "absent"}, api.RunRequest{Command: []string{"true"}})
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: id}))
	err = a.Run(ctx, ref, api.RunRequest{Command: []string{"true"}})
	require.Equal(t, errs.NotFound, errs.CodeOf(err), "a stopped workload has nothing to run in")
}

// TestR423_AProvisionedDatabaseDumpsAndLoadsWhileItServes asserts R-423
// against the real engines: each dump the services adapter plans runs in the
// running database through Run, and loading it puts back what it held.
func TestR423_AProvisionedDatabaseDumpsAndLoadsWhileItServes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		slot  spec.SlotType
		query func(sql string) []string
	}{
		{"postgres", spec.SlotPostgres, func(sql string) []string {
			return []string{"psql", "-U", "app", "-d", "app", "-tAc", sql}
		}},
		{"mysql", spec.SlotMySQL, func(sql string) []string {
			return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -N app -e "` + sql + `"`}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			a := adapter(t)
			id := "test-dump-" + tc.name + "-" + time.Now().Format("150405")
			cleanup(t, a, id)

			svcs := servicesdocker.New()
			require.NoError(t, svcs.Configure(ctx, nil))
			serviceID := "svc_" + strings.ReplaceAll(id, "-", "")
			res, err := svcs.Provision(ctx, api.ProvisionRequest{
				AppID: id, BundleID: id, SlotKey: "DATABASE_URL", Type: tc.slot, ServiceID: serviceID,
			})
			require.NoError(t, err)
			plan := bundle(id, nil)
			plan.Volumes = res.Volumes
			// web waits for the database to be healthy before it starts.
			plan.Workloads = append(res.Workloads, api.WorkloadPlan{
				Name: "web", Image: "alpine:3.20", Command: []string{"sleep", "3600"},
				Env:       map[string]secret.Value{},
				DependsOn: []string{res.Workloads[0].Name},
			})
			_, err = a.Apply(ctx, plan)
			require.NoError(t, err)

			dump, ok := svcs.Dump(res.Handle, tc.slot)
			require.True(t, ok)
			require.Equal(t, []string{res.Volumes[0].VolumeID}, dump.Volumes)
			ref := api.WorkloadRef{BundleID: id, Workload: dump.Workload}
			sql := func(q string) string {
				var out bytes.Buffer
				require.NoError(t, a.Run(ctx, ref, api.RunRequest{Command: tc.query(q), Stdout: &out}))
				return strings.TrimSpace(out.String())
			}

			// Healthy is a ping over TCP; the tools use the socket, which a
			// slow host can be a moment behind on.
			require.Eventually(t, func() bool {
				return a.Run(ctx, ref, api.RunRequest{Command: tc.query("SELECT 1")}) == nil
			}, 2*time.Minute, time.Second)
			sql("CREATE TABLE notes (body text); INSERT INTO notes VALUES ('kept')")
			var saved bytes.Buffer
			require.NoError(t, a.Run(ctx, ref, api.RunRequest{Command: dump.Dump, Stdout: &saved}))
			require.Positive(t, saved.Len())

			sql("INSERT INTO notes VALUES ('written after the backup')")
			require.NoError(t, a.Run(ctx, ref, api.RunRequest{Command: dump.Load, Stdin: &saved}))
			require.Equal(t, "kept", sql("SELECT body FROM notes"))
		})
	}
}
