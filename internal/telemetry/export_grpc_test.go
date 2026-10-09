package telemetry_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/telemetry"
)

// grpcCollector is an OTLP/gRPC receiver, on the collector's port 4317.
type grpcCollector struct {
	collectorpb.UnimplementedMetricsServiceServer
	mu      sync.Mutex
	pushes  int
	apiKeys []string
}

func (g *grpcCollector) Export(ctx context.Context, _ *collectorpb.ExportMetricsServiceRequest) (*collectorpb.ExportMetricsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	g.mu.Lock()
	g.pushes++
	g.apiKeys = append(g.apiKeys, md.Get("api-key")...)
	g.mu.Unlock()
	return &collectorpb.ExportMetricsServiceResponse{}, nil
}

// TestR399_PandoPushesOverGRPCToo asserts the other protocol R-399's
// configuration offers.
func TestR399_PandoPushesOverGRPCToo(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	received := &grpcCollector{}
	server := grpc.NewServer()
	collectorpb.RegisterMetricsServiceServer(server, received)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	ctx := context.Background()
	stop, err := telemetry.Start(ctx, config.Metrics{
		OTLPEndpoint: "http://" + listener.Addr().String(), // http:// is without TLS
		OTLPProtocol: config.OTLPGRPC,
		OTLPHeaders:  "api-key=collector-secret-1",
		Interval:     time.Hour,
	}, telemetry.Process{Version: "1.2.3"}, telemetry.Sources{})
	require.NoError(t, err)
	telemetry.Build(ctx, telemetry.Succeeded, time.Minute)
	require.NoError(t, stop(ctx))

	received.mu.Lock()
	defer received.mu.Unlock()
	require.Positive(t, received.pushes)
	require.Equal(t, "collector-secret-1", received.apiKeys[0])
}

// TestR399_HeadersPandoCannotReadStopStartup asserts that Start refuses
// headers it cannot parse rather than exporting without them, and does not
// repeat the value back (R-194).
func TestR399_HeadersPandoCannotReadStopStartup(t *testing.T) {
	_, err := telemetry.Start(context.Background(), config.Metrics{
		OTLPEndpoint: "http://otel-collector:4318", OTLPProtocol: config.OTLPHTTP,
		OTLPHeaders: "collector-secret-1", Interval: time.Minute,
	}, telemetry.Process{}, telemetry.Sources{})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "collector-secret-1")
}
