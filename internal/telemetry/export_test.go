package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/telemetry"
)

// collector is an OTLP/HTTP receiver: what a Grafana Alloy, an OpenTelemetry
// collector or Datadog's agent listens with.
type collector struct {
	mu       sync.Mutex
	paths    []string
	apiKeys  []string
	requests []*collectorpb.ExportMetricsServiceRequest
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req collectorpb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.paths = append(c.paths, r.URL.Path)
	c.apiKeys = append(c.apiKeys, r.Header.Get("Api-Key"))
	c.requests = append(c.requests, &req)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&collectorpb.ExportMetricsServiceResponse{})
	_, _ = w.Write(out)
}

// TestR399_PandoPushesItsMetricsOverOTLP asserts R-399 end to end in one
// process: with an endpoint configured, what Pando records reaches a collector
// over OTLP/HTTP, at /v1/metrics, carrying the configured headers and naming
// the process — and stopping flushes what was recorded since the last push.
func TestR399_PandoPushesItsMetricsOverOTLP(t *testing.T) {
	received := &collector{}
	server := httptest.NewServer(received)
	t.Cleanup(server.Close)

	ctx := context.Background()
	stop, err := telemetry.Start(ctx, config.Metrics{
		OTLPEndpoint: server.URL,
		OTLPProtocol: config.OTLPHTTP,
		OTLPHeaders:  "api-key=collector-secret-1",
		Interval:     time.Hour, // only the flush at stop pushes
	}, telemetry.Process{Version: "1.2.3", Replica: "rep_test", Hostname: "pando-0"}, telemetry.Sources{
		Pool: func() (int64, int64, int64) { return 1, 2, 32 },
	})
	require.NoError(t, err)

	telemetry.Deploy(ctx, telemetry.Succeeded, 42*time.Second)
	require.NoError(t, stop(ctx))

	received.mu.Lock()
	defer received.mu.Unlock()
	require.NotEmpty(t, received.requests, "stopping pushes what was recorded")
	require.Equal(t, "/v1/metrics", received.paths[0], "a bare collector address gets the standard path")
	require.Equal(t, "collector-secret-1", received.apiKeys[0])

	names := map[string]bool{}
	resource := map[string]string{}
	for _, req := range received.requests {
		for _, rm := range req.GetResourceMetrics() {
			for _, kv := range rm.GetResource().GetAttributes() {
				resource[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					names[m.GetName()] = true
				}
			}
		}
	}
	require.Equal(t, "pando", resource["service.name"])
	require.Equal(t, "1.2.3", resource["service.version"])
	require.Equal(t, "rep_test", resource["service.instance.id"])
	require.Equal(t, "pando-0", resource["host.name"])
	require.True(t, names["pando.deploy.duration"], "the deploy recorded before stop arrived")
	require.True(t, names["db.client.connection.count"], "readings are taken at collection")
	require.True(t, names["go.memory.used"], "the Go runtime is measured too")

	// Stopped means stopped: recording afterwards goes nowhere and does not fail.
	telemetry.Deploy(ctx, telemetry.Failed, time.Second)
}

// TestR399_NoEndpointExportsNothing asserts R-399's default: with no endpoint
// configured, Start does nothing and recording is a no-op.
func TestR399_NoEndpointExportsNothing(t *testing.T) {
	stop, err := telemetry.Start(context.Background(), config.Metrics{}, telemetry.Process{}, telemetry.Sources{})
	require.NoError(t, err)
	telemetry.Deploy(context.Background(), telemetry.Succeeded, time.Second)
	require.NoError(t, stop(context.Background()))
}
