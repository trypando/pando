package deploy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// listeningRuntime is a running app whose primary port accepts connections
// from opensAt on, reached through Upstream as the proxy reaches it.
type listeningRuntime struct {
	scriptedRuntime
	opensAt time.Time
	never   bool
	dialed  int
	asked   api.WorkloadRef
	port    int
}

func (r *listeningRuntime) Upstream(_ context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	r.asked, r.port = ref, port
	return api.Upstream{URL: "http://web:8080", Dial: func(context.Context) (net.Conn, error) {
		r.dialed++
		if r.never || time.Now().Before(r.opensAt) {
			return nil, errors.New("connection refused")
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}}, nil
}

var portBundle = api.BundlePlan{Workloads: []api.WorkloadPlan{
	{Name: "web", Exposed: true, Ports: []api.PortPlan{{Number: 8080, Protocol: "http"}}},
}}

func upFor(names ...string) func(int) (api.ObservedBundle, error) {
	return func(int) (api.ObservedBundle, error) { return running(names...), nil }
}

// TestR221_ADeployWithNoHealthCheckWaitsForThePortToOpen asserts R-221's TCP
// connect signal: with no compose healthcheck and no HTTP endpoint, a deploy is
// done when the primary port accepts a connection, not when the process is
// merely running. A Spring Boot app took 40 to 91 seconds to listen and was
// declared deployed after ten (issue #90).
func TestR221_ADeployWithNoHealthCheckWaitsForThePortToOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		rt := &listeningRuntime{opensAt: start.Add(95 * time.Second)}
		rt.observe = upFor("web")

		var log strings.Builder
		healthy, err := (&Runner{}).waitForHealth(t.Context(), rt, "app_01HQ8", portBundle, &log)
		require.NoError(t, err)
		require.True(t, healthy)
		require.GreaterOrEqual(t, time.Since(start), 95*time.Second, "not before the port opened")
		require.Equal(t, api.WorkloadRef{BundleID: "app_01HQ8", Workload: "web"}, rt.asked)
		require.Equal(t, 8080, rt.port)
		require.Greater(t, rt.dialed, 1)
		require.Contains(t, log.String(), "waiting for it to accept connections on port 8080")
		require.Contains(t, log.String(), "accepting connections on port 8080")
	})
}

// TestR221_APortThatNeverOpensIsDegraded asserts that an app running but never
// listening is degraded, not failed, once the start wait ends — and the log
// names the port it waited on.
func TestR221_APortThatNeverOpensIsDegraded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		rt := &listeningRuntime{never: true}
		rt.observe = upFor("web")

		var log strings.Builder
		healthy, err := (&Runner{}).waitForHealth(t.Context(), rt, "app_01HQ8", portBundle, &log)
		require.NoError(t, err)
		require.False(t, healthy)
		require.GreaterOrEqual(t, time.Since(start), startWait)
		require.Contains(t, log.String(), "had not accepted a connection on port 8080")
	})
}

// TestR221_AHealthCheckOutranksTheTCPConnect asserts the precedence: a
// workload with its own health check is judged by it, and its port is not
// dialed.
func TestR221_AHealthCheckOutranksTheTCPConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &listeningRuntime{never: true}
		rt.observe = upFor("web")
		checked := api.BundlePlan{Workloads: []api.WorkloadPlan{{
			Name: "web", Exposed: true, Ports: []api.PortPlan{{Number: 8080}},
			Health: &api.HealthPlan{Path: "/healthz", Port: 8080},
		}}}

		healthy, err := (&Runner{}).waitForHealth(t.Context(), rt, "app_01HQ8", checked, io.Discard)
		require.NoError(t, err)
		require.True(t, healthy)
		require.Zero(t, rt.dialed)
	})
}

type dnsRuntime struct{ api.RuntimeAdapter }

func (dnsRuntime) Upstream(context.Context, api.WorkloadRef, int) (api.Upstream, error) {
	return api.Upstream{URL: "http://web:8080", Dial: func(context.Context) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "web", IsNotFound: true}
	}}, nil
}

// A Pando that is not on the app's network has no TCP signal to wait on, and
// does not hold a deploy for the whole start wait over it.
func TestR221_NoRouteToTheAppIsNoTCPSignal(t *testing.T) {
	require.True(t, portOpen(t.Context(), dnsRuntime{}, "app_01HQ8", "web", 8080))
}

// A JVM start under load fits in the start wait (issue #90).
func TestR221_TheStartWaitFitsASlowJVMStart(t *testing.T) {
	require.GreaterOrEqual(t, startWait, 2*time.Minute+30*time.Second)
}
