package telemetry_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/telemetry"
)

// collect installs a manual reader for the test and returns a function that
// reads everything recorded so far.
func collect(t *testing.T, sources telemetry.Sources) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	require.NoError(t, telemetry.Install(provider, sources))
	t.Cleanup(func() {
		telemetry.Reset()
		_ = provider.Shutdown(context.Background())
	})
	return func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		return rm
	}
}

// idShaped matches a Pando ID anywhere in a value: a prefix, an underscore
// and a ULID.
var idShaped = regexp.MustCompile(`[a-z]+_[0-9A-HJKMNP-TV-Z]{26}`)

// TestR400_NoMetricNamesAPersonTokenOrApp asserts R-400: every metric Pando
// records carries only the attribute keys telemetry.AttributeKeys lists, and
// no value is a person's, a token's or an app's ID — even when the work being
// recorded was about one.
func TestR400_NoMetricNamesAPersonTokenOrApp(t *testing.T) {
	require.Regexp(t, idShaped, id.New(id.User), "the pattern must catch a real ID, or this test checks nothing")

	read := collect(t, telemetry.Sources{
		Pool:       func() (int64, int64, int64) { return 3, 5, 32 },
		Detections: func() int64 { return 2 },
		Adapters: func() []telemetry.AdapterHealth {
			return []telemetry.AdapterHealth{{Category: "runtime", ID: "docker", Healthy: true}}
		},
	})
	ctx := context.Background()

	telemetry.HTTPRequest(ctx, "GET", "/api/v1/apps/{id}", 200, 30*time.Millisecond)
	telemetry.HTTPRequest(ctx, "BREW", "", 404, time.Millisecond)
	telemetry.ProxyRequest(ctx, false, "anonymous")
	telemetry.ProxyRequest(ctx, true, "user")
	telemetry.Reconcile(ctx, telemetry.Succeeded, time.Second)
	telemetry.Deploy(ctx, telemetry.Failed, time.Minute)
	telemetry.Build(ctx, telemetry.Succeeded, 2*time.Minute)
	telemetry.Detection(ctx, telemetry.Canceled, 10*time.Second)
	telemetry.RetentionPass(ctx, telemetry.RetentionRows, telemetry.Succeeded, time.Second)
	telemetry.RetentionRemoved(ctx, "sessions", 4)

	rm := read()
	seen := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, attrs := range attributeSets(m) {
				for _, kv := range attrs {
					seen++
					require.Contains(t, telemetry.AttributeKeys, string(kv.Key), "metric %s carries an attribute R-400 does not allow", m.Name)
					require.NotRegexp(t, idShaped, kv.Value.String(), "metric %s carries an ID", m.Name)
				}
			}
		}
	}
	require.Positive(t, seen, "the test recorded nothing, so it checked nothing")
}

// TestR399_ProcessMetricsAreRecorded asserts R-399's list: each kind of work
// Pando does is recorded under its own metric, and readings are taken from
// their sources at collection.
func TestR399_ProcessMetricsAreRecorded(t *testing.T) {
	read := collect(t, telemetry.Sources{
		Pool:       func() (int64, int64, int64) { return 3, 5, 32 },
		Detections: func() int64 { return 2 },
		Adapters: func() []telemetry.AdapterHealth {
			return []telemetry.AdapterHealth{{Category: "runtime", ID: "docker", Healthy: false}}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	telemetry.HTTPRequest(ctx, "POST", "/api/v1/apps", 201, time.Millisecond)
	telemetry.ProxyRequest(ctx, false, "anonymous")
	telemetry.Reconcile(ctx, telemetry.OutcomeOf(ctx, errors.New("runtime unreachable")), time.Second)
	telemetry.Deploy(ctx, telemetry.OutcomeOf(context.Background(), nil), time.Second)
	telemetry.Build(ctx, telemetry.Succeeded, time.Second)
	telemetry.Detection(ctx, telemetry.Succeeded, time.Second)
	telemetry.RetentionPass(ctx, telemetry.RetentionRows, telemetry.Succeeded, time.Second)
	telemetry.RetentionRemoved(ctx, "sessions", 4)

	rm := read()
	names := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		require.Equal(t, telemetry.ScopeName, sm.Scope.Name)
		for _, m := range sm.Metrics {
			names[m.Name] = m
		}
	}
	for _, want := range []string{
		"http.server.request.duration", "pando.proxy.requests", "pando.reconcile.duration",
		"pando.deploy.duration", "pando.build.duration", "pando.detection.duration",
		"pando.retention.duration", "pando.retention.removed", "db.client.connection.count",
		"db.client.connection.max", "pando.detection.active", "pando.adapter.healthy",
	} {
		require.Contains(t, names, want)
	}

	reconcile := names["pando.reconcile.duration"].Data.(metricdata.Histogram[float64]).DataPoints
	require.Len(t, reconcile, 1)
	outcome, _ := reconcile[0].Attributes.Value("pando.outcome")
	require.Equal(t, "canceled", outcome.AsString(), "an error under a canceled context is a cancel, not a failure")

	healthy := names["pando.adapter.healthy"].Data.(metricdata.Gauge[int64]).DataPoints
	require.Len(t, healthy, 1)
	require.Equal(t, int64(0), healthy[0].Value)

	denied := names["pando.proxy.requests"].Data.(metricdata.Sum[int64]).DataPoints
	require.Len(t, denied, 1)
	decision, _ := denied[0].Attributes.Value("pando.proxy.decision")
	require.Equal(t, "denied", decision.AsString())
}

// An unknown method is folded into _OTHER, so a client cannot mint a series
// per made-up method.
func TestHTTPRequestFoldsUnknownMethods(t *testing.T) {
	read := collect(t, telemetry.Sources{})
	telemetry.HTTPRequest(context.Background(), "BREW", "", 418, time.Millisecond)

	rm := read()
	points := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64]).DataPoints
	method, _ := points[0].Attributes.Value("http.request.method")
	route, _ := points[0].Attributes.Value("http.route")
	require.Equal(t, "_OTHER", method.AsString())
	require.Equal(t, "unmatched", route.AsString())
}

func attributeSets(m metricdata.Metrics) [][]attribute.KeyValue {
	var out [][]attribute.KeyValue
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes.ToSlice())
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes.ToSlice())
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes.ToSlice())
		}
	}
	return out
}
