package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Metrics is where Pando pushes metrics about itself, over OTLP (R-399,
// issue #126). Empty OTLPEndpoint: nothing is exported.
type Metrics struct {
	// OTLPEndpoint is the collector: http://otel-collector:4318 for OTLP over
	// HTTP, or http://otel-collector:4317 for gRPC. http:// sends in the
	// clear, https:// over TLS. Over HTTP, a URL with no path has
	// /v1/metrics added, as the OpenTelemetry SDKs do.
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`

	// OTLPProtocol is http/protobuf (the default) or grpc.
	OTLPProtocol string `mapstructure:"otlp_protocol"`

	// OTLPHeaders are sent with every export, written as
	// OTEL_EXPORTER_OTLP_HEADERS writes them: key=value pairs separated by
	// commas, values URL-encoded. Usually an API key, so never reported
	// (sources.go) or logged (R-194).
	OTLPHeaders string `mapstructure:"otlp_headers"`

	// Interval is how often metrics are pushed. Default: a minute.
	Interval time.Duration `mapstructure:"interval"`
}

// Metrics protocols.
const (
	OTLPHTTP = "http/protobuf"
	OTLPGRPC = "grpc"
)

// DefaultMetricsInterval is Metrics.Interval's default, and the OpenTelemetry
// SDKs' own.
const DefaultMetricsInterval = time.Minute

// Enabled reports whether metrics are exported at all.
func (m Metrics) Enabled() bool { return strings.TrimSpace(m.OTLPEndpoint) != "" }

// Headers parses OTLPHeaders.
func (m Metrics) Headers() (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(m.OTLPHeaders, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			// The entry is not repeated: it is most likely a credential.
			return nil, fmt.Errorf("PANDO_METRICS_OTLP_HEADERS has an entry that is not key=value. " +
				"Valid answer: pairs separated by commas, such as api-key=abc123,x-team=platform")
		}
		decoded, err := url.QueryUnescape(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("PANDO_METRICS_OTLP_HEADERS: the value of %q is not URL-encoded correctly; "+
				"encode a comma as %%2C and a percent sign as %%25", key)
		}
		out[key] = decoded
	}
	return out, nil
}

func (m Metrics) validate() error {
	if !m.Enabled() {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(m.OTLPEndpoint))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Not echoed, in case it is a URL with a password that failed to parse.
		return fmt.Errorf("PANDO_METRICS_OTLP_ENDPOINT is not an http:// or https:// URL with a host. " +
			"Valid answer: the OpenTelemetry collector's address, such as http://otel-collector:4318")
	}
	if u.User != nil {
		return fmt.Errorf("PANDO_METRICS_OTLP_ENDPOINT carries a username or password. " +
			"Put the credential in PANDO_METRICS_OTLP_HEADERS instead, such as authorization=Basic%%20<base64>, " +
			"so it is never shown in GET /config or a log line")
	}
	if m.OTLPProtocol != OTLPHTTP && m.OTLPProtocol != OTLPGRPC {
		return fmt.Errorf("PANDO_METRICS_OTLP_PROTOCOL is %q. Valid answer: %s or %s",
			m.OTLPProtocol, OTLPHTTP, OTLPGRPC)
	}
	if m.Interval < time.Second {
		return fmt.Errorf("PANDO_METRICS_INTERVAL is %s. Valid answer: a duration of at least a second, such as 30s or 1m",
			m.Interval)
	}
	_, err = m.Headers()
	return err
}
