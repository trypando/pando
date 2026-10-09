package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/trypando/pando/internal/telemetry"
)

// refusingProvider makes no histograms, as a provider rejecting an
// instrument's name or options would.
type refusingProvider struct{ noop.MeterProvider }

func (refusingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return refusingMeter{} }

type refusingMeter struct{ noop.Meter }

func (refusingMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("this provider makes no histograms")
}

// A provider that cannot make Pando's instruments is refused, and recording
// carries on into whatever was installed before rather than into nothing.
func TestInstallRefusesAProviderThatCannotMakeInstruments(t *testing.T) {
	read := collect(t, telemetry.Sources{})

	err := telemetry.Install(refusingProvider{}, telemetry.Sources{
		Pool:       func() (int64, int64, int64) { return 0, 0, 0 },
		Detections: func() int64 { return 0 },
	})
	require.ErrorContains(t, err, "makes no histograms")

	telemetry.Deploy(context.Background(), telemetry.Succeeded, time.Second)
	require.NotEmpty(t, read().ScopeMetrics, "the provider installed before still records")
}
