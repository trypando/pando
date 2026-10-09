package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/telemetry"
)

func metricsLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)
	return zap.New(core), logs
}

// TestR399_ServeExportsMetricsOnlyWhenConfigured asserts what serve does with
// R-399's configuration: nothing, and says nothing, with no endpoint; with
// one, says so at startup and flushes the last metrics when it stops.
func TestR399_ServeExportsMetricsOnlyWhenConfigured(t *testing.T) {
	ctx := context.Background()

	logger, logs := metricsLogger()
	stop, err := startMetrics(ctx, config.Metrics{}, telemetry.Process{}, telemetry.Sources{}, logger)
	require.NoError(t, err)
	stop()
	require.Zero(t, logs.Len(), "an install that exports nothing says nothing about it")

	var pushes atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pushes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	logger, logs = metricsLogger()
	stop, err = startMetrics(ctx, config.Metrics{
		OTLPEndpoint: collector.URL, OTLPProtocol: config.OTLPHTTP,
		OTLPHeaders: "api-key=collector-secret-1", Interval: time.Hour,
	}, telemetry.Process{Version: "test"}, telemetry.Sources{}, logger)
	require.NoError(t, err)
	telemetry.Deploy(ctx, telemetry.Succeeded, time.Second)
	stop()

	require.Positive(t, pushes.Load(), "stopping flushes")
	require.Equal(t, 1, logs.FilterMessage("exporting metrics over OTLP").Len())
	for _, entry := range logs.All() {
		for _, f := range entry.Context {
			require.NotContains(t, f.String, "collector-secret-1", "the headers are never logged (R-194)")
		}
	}
}

// A collector that cannot be reached at shutdown is logged, not fatal: the
// metrics are lost and the server still stops.
func TestAnUnreachableCollectorAtShutdownIsLogged(t *testing.T) {
	collector := httptest.NewServer(http.NotFoundHandler())
	endpoint := collector.URL
	collector.Close() // nothing listens there now

	logger, logs := metricsLogger()
	stop, err := startMetrics(context.Background(), config.Metrics{
		OTLPEndpoint: endpoint, OTLPProtocol: config.OTLPHTTP, Interval: time.Hour,
	}, telemetry.Process{}, telemetry.Sources{}, logger)
	require.NoError(t, err)
	telemetry.Deploy(context.Background(), telemetry.Succeeded, time.Second)
	stop()

	require.Equal(t, 1, logs.FilterMessage("could not send the last metrics to the collector").Len())
}

func TestABadMetricsSettingStopsTheServer(t *testing.T) {
	logger, _ := metricsLogger()
	_, err := startMetrics(context.Background(), config.Metrics{
		OTLPEndpoint: "http://otel-collector:4318", OTLPProtocol: config.OTLPHTTP,
		OTLPHeaders: "not-a-pair", Interval: time.Minute,
	}, telemetry.Process{}, telemetry.Sources{}, logger)
	require.Error(t, err)
}
