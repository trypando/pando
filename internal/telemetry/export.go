package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

	"github.com/trypando/pando/internal/config"
)

// Process names this Pando process to the collector. Neither field is a
// person, a token or an app (R-400).
type Process struct {
	Version string // the build's version
	// Replica is this replica's ID, as the console's list of replicas
	// shows it: service.instance.id.
	Replica string
	// Hostname is the container's or pod's: host.name.
	Hostname string
}

// Start exports metrics over OTLP as cfg says, and returns what stops it,
// flushing what was recorded since the last push. With no endpoint
// configured it does nothing and returns a no-op.
func Start(ctx context.Context, cfg config.Metrics, process Process, sources Sources) (func(context.Context) error, error) {
	if !cfg.Enabled() {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Schemaless: the SDK's default resource carries its own semconv schema,
	// and merging one that names a different version is refused — which would
	// stop startup on the next SDK upgrade.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("pando"),
		semconv.ServiceVersion(process.Version),
		semconv.ServiceInstanceID(process.Replica),
		semconv.HostName(process.Hostname),
	))
	if err != nil {
		return nil, fmt.Errorf("describing this process to the metrics collector: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.Interval))),
	)
	shutdown := func(ctx context.Context) error {
		Reset()
		return provider.Shutdown(ctx)
	}

	if err := Install(provider, sources); err != nil {
		return nil, errors.Join(err, shutdown(ctx))
	}
	// The Go runtime: memory, garbage collection, goroutines.
	if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
		return nil, errors.Join(err, shutdown(ctx))
	}
	return shutdown, nil
}

// newExporter builds the exporter. The endpoint and headers are always
// Pando's own, given explicitly so they win over OTEL_EXPORTER_OTLP_ENDPOINT
// and _HEADERS: what is exported where is what GET /config shows (R-271).
// The SDK's other variables still apply, such as
// OTEL_EXPORTER_OTLP_CERTIFICATE for a collector behind a private CA.
func newExporter(ctx context.Context, cfg config.Metrics) (sdkmetric.Exporter, error) {
	headers, err := cfg.Headers()
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(cfg.OTLPEndpoint)

	if cfg.OTLPProtocol == config.OTLPGRPC {
		exporter, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpointURL(endpoint),
			otlpmetricgrpc.WithHeaders(headers))
		if err != nil {
			return nil, fmt.Errorf("setting up OTLP over gRPC to the metrics collector: %w", err)
		}
		return exporter, nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/metrics"
	}
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(u.String()),
		otlpmetrichttp.WithHeaders(headers))
	if err != nil {
		return nil, fmt.Errorf("setting up OTLP over HTTP to the metrics collector: %w", err)
	}
	return exporter, nil
}
