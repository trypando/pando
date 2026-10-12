package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

func app() state.App { return state.App{ID: "app_01HQ8", Slug: "notes"} }

// addressingRuntime is a runtime that answers Upstream in a shape no other
// runtime would, and records what it was asked.
type addressingRuntime struct {
	api.RuntimeAdapter
	asked []api.WorkloadRef
	ports []int
}

func (r *addressingRuntime) Kind() string           { return "fake" }
func (r *addressingRuntime) Category() api.Category { return api.CategoryRuntime }

func (r *addressingRuntime) Upstream(_ context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	r.asked = append(r.asked, ref)
	r.ports = append(r.ports, port)
	return api.Upstream{URL: fmt.Sprintf("http://%s.%s.example:%d", ref.Workload, ref.BundleID, port)}, nil
}

func upstreams(t *testing.T) (*RuntimeUpstreams, *addressingRuntime) {
	t.Helper()
	rt := &addressingRuntime{}
	registry := api.NewRegistry()
	require.NoError(t, registry.Register("rt_fake", rt))
	return NewRuntimeUpstreams(registry), rt
}

func specWith(workloads ...spec.Workload) *spec.AppSpec {
	return &spec.AppSpec{Runtime: spec.RuntimeRef{AdapterRef: "rt_fake"}, Workloads: workloads}
}

// Where a workload is reachable is the runtime's answer, not the proxy's: how a
// workload is addressed is provider vocabulary core must not learn (R-251). The
// proxy assembled a Docker container name here, which no other runtime could
// have answered to.
func TestR251_TheProxyAsksTheAppsRuntimeWhereItsWorkloadIs(t *testing.T) {
	u, rt := upstreams(t)

	got, err := u.PrimaryAddress(context.Background(), app(), specWith(spec.Workload{
		Name: "web", Primary: true,
		Ports: []spec.Port{{Number: 3000, Protocol: "http"}},
	}))

	require.NoError(t, err)
	require.Equal(t, "http://web.app_01HQ8.example:3000", got)
	require.Equal(t, []api.WorkloadRef{{BundleID: "app_01HQ8", Workload: "web"}}, rt.asked,
		"the bundle is the app, and the workload is the primary one")
}

func TestTheHTTPPortIsPreferredOverWhateverComesFirst(t *testing.T) {
	u, rt := upstreams(t)
	ctx := context.Background()

	_, err := u.PrimaryAddress(ctx, app(), specWith(spec.Workload{
		Name: "web", Primary: true,
		Ports: []spec.Port{
			{Number: 9000, Protocol: "tcp"},
			{Number: 3000, Protocol: "http"},
		},
	}))
	require.NoError(t, err)

	// A port with no protocol stated is treated as HTTP, which is what a spec
	// that only says "3000" means.
	_, err = u.PrimaryAddress(ctx, app(), specWith(spec.Workload{
		Name: "web", Primary: true, Ports: []spec.Port{{Number: 3000}},
	}))
	require.NoError(t, err)

	// Nothing declares HTTP, so the first port is the best guess available.
	_, err = u.PrimaryAddress(ctx, app(), specWith(spec.Workload{
		Name: "web", Primary: true, Ports: []spec.Port{{Number: 9000, Protocol: "tcp"}},
	}))
	require.NoError(t, err)

	require.Equal(t, []int{3000, 3000, 9000}, rt.ports)
}

// TestR097_SeveralPortsNoneOfThemHTTPAreNotGuessedBetween asserts that the
// proxy routes to the spec's web port (R-097) and nowhere else. The first of
// several ports, none of them HTTP, was epmd or MQTT as often as not (issue
// #90); refusing says what is missing, where a guess answered every request
// with a protocol error.
func TestR097_SeveralPortsNoneOfThemHTTPAreNotGuessedBetween(t *testing.T) {
	u, rt := upstreams(t)

	_, err := u.PrimaryAddress(context.Background(), app(), specWith(spec.Workload{
		Name: "web", Primary: true,
		Ports: []spec.Port{{Number: 4369, Protocol: "tcp"}, {Number: 1883, Protocol: "tcp"}},
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "which of this app's ports")
	require.Empty(t, rt.ports, "nothing was asked of the runtime")
}

func TestAnAppWithNothingToProxyToSaysWhichPartIsMissing(t *testing.T) {
	u, rt := upstreams(t)
	ctx := context.Background()

	_, err := u.PrimaryAddress(ctx, app(), nil)
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "no pinned spec")

	_, err = u.PrimaryAddress(ctx, app(), specWith(spec.Workload{Name: "worker"}))
	require.Equal(t, errs.ValidPrimaryWorkload, errs.CodeOf(err))

	_, err = u.PrimaryAddress(ctx, app(), specWith(spec.Workload{Name: "web", Primary: true}))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.NotEmpty(t, errs.As(err).Remedy, "R-105: it says what to do about it")

	require.Empty(t, rt.asked, "the runtime is asked only once there is a workload and a port")
}

// A spec naming a runtime this install does not have is refused, never
// answered with a guess at an address.
func TestAnAppOnARuntimeThatIsNotConfiguredHasNoAddress(t *testing.T) {
	u, _ := upstreams(t)

	s := specWith(spec.Workload{Name: "web", Primary: true, Ports: []spec.Port{{Number: 3000}}})
	s.Runtime.AdapterRef = "rt_gone"

	got, err := u.PrimaryAddress(context.Background(), app(), s)
	require.Equal(t, errs.PlanAdapterNotConfigured, errs.CodeOf(err))
	require.Empty(t, got)
}

// R-023 says there is no bypass, and a counter every request increments —
// allowed or denied, authenticated or anonymous — is how that claim is checked
// rather than asserted.
func TestR023_EveryRequestIsCountedWhateverItsOutcome(t *testing.T) {
	c := NewCounters()

	c.Request("app_01HQ8", authz.KindUser, true)
	c.Request("app_01HQ8", authz.KindUser, false)
	c.Request("app_01HQ8", authz.KindAnonymous, true)
	c.Request("app_01HQ8", authz.KindAnonymous, false)
	c.Request("app_01HQ8", authz.KindToken, true)

	total, allowed, denied, anonymous := c.Snapshot("app_01HQ8")
	require.EqualValues(t, 5, total)
	require.EqualValues(t, 3, allowed)
	require.EqualValues(t, 2, denied)
	require.EqualValues(t, 2, anonymous)
	require.EqualValues(t, total, allowed+denied, "every request lands in exactly one of the two")
}

func TestCountsAreKeptPerApp(t *testing.T) {
	c := NewCounters()
	c.Request("app_01HQ8", authz.KindUser, true)
	c.Request("app_01HQ9", authz.KindUser, false)

	total, allowed, _, _ := c.Snapshot("app_01HQ8")
	require.EqualValues(t, 1, total)
	require.EqualValues(t, 1, allowed)

	total, _, denied, _ := c.Snapshot("app_01HQ9")
	require.EqualValues(t, 1, total)
	require.EqualValues(t, 1, denied)

	total, allowed, denied, anonymous := c.Snapshot("app_never_seen")
	require.Zero(t, total+allowed+denied+anonymous, "an app nobody has asked for counts zero, not nothing")
}

func TestCountingIsSafeUnderConcurrentRequests(t *testing.T) {
	c := NewCounters()

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Request("app_01HQ8", authz.KindUser, i%2 == 0)
			c.Request("app_"+string(rune('a'+i%5)), authz.KindAnonymous, true)
		}()
	}
	wg.Wait()

	total, allowed, denied, _ := c.Snapshot("app_01HQ8")
	require.EqualValues(t, 200, total)
	require.EqualValues(t, 100, allowed)
	require.EqualValues(t, 100, denied)
}

// A policy-violation close frame rather than an abrupt reset, so a client can
// tell revocation from a network fault and does not reconnect in a loop.
func TestTheCloseFrameSaysPolicyViolation(t *testing.T) {
	frame := closeFrame(closePolicyViolation, "access revoked")

	require.Equal(t, byte(0x88), frame[0], "FIN plus opcode 8")
	require.Equal(t, byte(len(frame)-2), frame[1], "the payload length, with no extended length")

	require.EqualValues(t, 1008, binary.BigEndian.Uint16(frame[2:4]))
	require.Equal(t, "access revoked", string(frame[4:]))
}

// Close payloads are capped at 125 bytes, which is also the boundary below
// which the length fits in the header's 7 bits.
func TestALongCloseReasonIsTruncatedToFitTheHeader(t *testing.T) {
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}

	frame := closeFrame(closePolicyViolation, string(long))
	require.Len(t, frame, 2+125)
	require.Equal(t, byte(125), frame[1])
	require.EqualValues(t, 1008, binary.BigEndian.Uint16(frame[2:4]))
}
