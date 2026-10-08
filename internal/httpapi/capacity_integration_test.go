//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
)

// usageRuntime reports everything GET /capacity can show, live use included.
type usageRuntime struct{ egressRuntime }

func (usageRuntime) Capabilities(ctx context.Context) (adapterapi.RuntimeCapabilities, error) {
	caps, err := egressRuntime{}.Capabilities(ctx)
	caps.ReportsUsage = true
	return caps, err
}

func (usageRuntime) Capacity(context.Context) (adapterapi.Capacity, error) {
	return adapterapi.Capacity{
		TotalCPUMillis: 8000, TotalMemoryBytes: 16 << 30,
		RunningWorkloads: 3,
		Details:          map[string]any{"server_version": "27.0"},
		Reported:         time.Now(),
	}, nil
}

func (usageRuntime) InUse(context.Context) (adapterapi.InUse, error) {
	return adapterapi.InUse{CPUMillis: 1250, MemoryBytes: 2 << 30, Reported: time.Now()}, nil
}

// downRuntime cannot say how much room it has.
type downRuntime struct{ egressRuntime }

func (downRuntime) Capacity(context.Context) (adapterapi.Capacity, error) {
	return adapterapi.Capacity{}, errors.New("daemon not answering")
}

// R-243: every runtime reports the same readings in the same shape, so the
// console can answer "how much room is left" without knowing which runtime
// answered — one entry per runtime, its own details beside them, live use only
// from a runtime that reports usage (R-254), and a runtime that does not
// answer said so rather than failing the request.
func TestR243_CapacityReportsTheSameReadingsForEveryRuntime(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	reg := i.Server.Registry
	require.NoError(t, reg.Register("rt_live", usageRuntime{}))
	require.NoError(t, reg.Register("rt_quiet", egressRuntime{}))
	require.NoError(t, reg.Register("rt_down", downRuntime{}))

	got := i.do(i.admin(), http.MethodGet, "/capacity", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var body struct {
		Runtimes []map[string]any `json:"runtimes"`
		AsOf     time.Time        `json:"as_of"`
		Refresh  int              `json:"refresh_seconds"`
	}
	got.JSON(t, &body)
	// The readings are a background snapshot, and say when it was taken
	// (issue #72).
	require.WithinDuration(t, time.Now(), body.AsOf, time.Minute)
	require.Positive(t, body.Refresh)

	byRef := map[string]map[string]any{}
	for _, r := range body.Runtimes {
		byRef[r["adapter_ref"].(string)] = r
	}
	require.Len(t, byRef, 3)

	live := byRef["rt_live"]
	require.Equal(t, "ok", live["status"])
	require.EqualValues(t, 8000, live["total_cpu_millis"])
	require.EqualValues(t, 0, live["allocated_cpu_millis"], "nothing is committed on an empty install")
	require.EqualValues(t, 3, live["running_workloads"])
	require.EqualValues(t, 1250, live["in_use_cpu_millis"])
	require.EqualValues(t, 2<<30, live["in_use_memory_bytes"])
	require.Equal(t, map[string]any{"server_version": "27.0"}, live["details"])

	quiet := byRef["rt_quiet"]
	require.Equal(t, "ok", quiet["status"])
	require.NotContains(t, quiet, "in_use_cpu_millis", "a runtime that does not report usage is not shown as using nothing")

	require.Equal(t, "unreachable", byRef["rt_down"]["status"])
}
