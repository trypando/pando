package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR399_MetricsExportIsConfiguration asserts R-399's configuration: off by
// default, set from the environment, reported in GET /config with where it
// came from — and the headers, which usually carry a collector's API key,
// read but never reported (R-194).
func TestR399_MetricsExportIsConfiguration(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")

	cfg, err := Load("")
	require.NoError(t, err)
	require.False(t, cfg.Metrics.Enabled(), "nothing is exported until an endpoint is set")
	require.Equal(t, OTLPHTTP, cfg.Metrics.OTLPProtocol)
	require.Equal(t, time.Minute, cfg.Metrics.Interval)

	t.Setenv("PANDO_METRICS_OTLP_ENDPOINT", "https://otel.example.com:4318")
	t.Setenv("PANDO_METRICS_OTLP_PROTOCOL", "grpc")
	t.Setenv("PANDO_METRICS_INTERVAL", "15s")
	t.Setenv("PANDO_METRICS_OTLP_HEADERS", "api-key=collector-secret-1,x-team=platform%2Cops")

	cfg, err = Load("")
	require.NoError(t, err)
	require.True(t, cfg.Metrics.Enabled())
	require.Equal(t, OTLPGRPC, cfg.Metrics.OTLPProtocol)
	require.Equal(t, 15*time.Second, cfg.Metrics.Interval)
	headers, err := cfg.Metrics.Headers()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"api-key": "collector-secret-1", "x-team": "platform,ops"}, headers)

	require.Equal(t, Source{Kind: "env", Name: "PANDO_METRICS_OTLP_ENDPOINT"},
		settingNamed(t, cfg, "metrics.otlp_endpoint").Source)
	require.Equal(t, "15s", settingNamed(t, cfg, "metrics.interval").Value)
	for _, s := range cfg.Settings {
		require.NotEqual(t, "metrics.otlp_headers", s.Key)
		require.NotContains(t, fmt.Sprint(s.Value), "collector-secret-1")
	}
}

// TestR399_ABadMetricsSettingStopsStartupSayingWhy asserts that a metrics
// setting Pando cannot use is refused at startup with an answer (R-105), and
// that a credential in it is never repeated back (R-194).
func TestR399_ABadMetricsSettingStopsStartupSayingWhy(t *testing.T) {
	good := Metrics{OTLPEndpoint: "http://otel-collector:4318", OTLPProtocol: OTLPHTTP, Interval: time.Minute}
	require.NoError(t, good.validate())
	require.NoError(t, Metrics{}.validate(), "unset is off, not an error")

	for name, tc := range map[string]struct {
		m    func(Metrics) Metrics
		want string
	}{
		"not a URL":        {func(m Metrics) Metrics { m.OTLPEndpoint = "otel-collector:4318"; return m }, "such as http://otel-collector:4318"},
		"a password in it": {func(m Metrics) Metrics { m.OTLPEndpoint = "https://u:hunter2@otel:4318"; return m }, "PANDO_METRICS_OTLP_HEADERS instead"},
		"an unknown protocol": {func(m Metrics) Metrics { m.OTLPProtocol = "http/json"; return m },
			"http/protobuf or grpc"},
		"too often":          {func(m Metrics) Metrics { m.Interval = time.Millisecond; return m }, "at least a second"},
		"a header with no =": {func(m Metrics) Metrics { m.OTLPHeaders = "hunter2"; return m }, "key=value"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.m(good).validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			require.False(t, strings.Contains(err.Error(), "hunter2"), "a credential is never repeated back")
		})
	}
}
