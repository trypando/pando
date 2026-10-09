// Package telemetrytest reads back what Pando recorded, for tests asserting
// that a piece of work is measured (R-399).
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/trypando/pando/internal/telemetry"
)

// Reader is what was recorded since Install.
type Reader struct {
	t      *testing.T
	reader *sdkmetric.ManualReader
}

// Install records into a reader the test can read, until the test ends.
//
// Recording is process-wide, so a test using this must not run in parallel
// with another that does.
func Install(t *testing.T, sources telemetry.Sources) *Reader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	if err := telemetry.Install(provider, sources); err != nil {
		t.Fatalf("installing a test meter provider: %v", err)
	}
	t.Cleanup(func() {
		telemetry.Reset()
		_ = provider.Shutdown(context.Background())
	})
	return &Reader{t: t, reader: reader}
}

// Metrics collects everything recorded so far, by name.
func (r *Reader) Metrics() map[string]metricdata.Metrics {
	r.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		r.t.Fatalf("collecting metrics: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// Count is how many times the named metric was recorded with every one of
// attrs: a histogram's count, or a counter's sum. Attributes not named are
// ignored.
func (r *Reader) Count(name string, attrs map[string]string) int64 {
	r.t.Helper()
	m, ok := r.Metrics()[name]
	if !ok {
		return 0
	}
	var n int64
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			n += countIf(matches(p.Attributes, attrs), int64(p.Count)) //nolint:gosec // G115: a test records a handful, nowhere near 2^63.
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			n += countIf(matches(p.Attributes, attrs), p.Value)
		}
	default:
		r.t.Fatalf("metric %s is not a histogram or a counter", name)
	}
	return n
}

// matches reports whether set holds every one of attrs.
func matches(set attribute.Set, attrs map[string]string) bool {
	for k, want := range attrs {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.String() != want {
			return false
		}
	}
	return true
}

func countIf(ok bool, n int64) int64 {
	if ok {
		return n
	}
	return 0
}
