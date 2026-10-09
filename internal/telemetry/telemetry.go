// Package telemetry is the metrics Pando exports about itself (R-399, issue
// #126): pushed over OTLP to a collector the operator names, stored, graphed
// and alerted on there and nowhere in Pando (R-016, R-245).
//
// Every recording goes through a function here that takes typed, bounded
// values — an outcome, a route pattern, a kind of principal — and no free-form
// attributes. That is R-400's mechanism: there is no way to hand this package
// a user ID, a token ID or an app ID, so no metric can carry one, and
// TestR400_NoMetricNamesAPersonTokenOrApp checks what comes out.
//
// Until Install is called every function records into a no-op, which is what
// an install with no endpoint configured runs on.
package telemetry

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// ScopeName is the instrumentation scope every Pando metric is recorded under.
const ScopeName = "github.com/trypando/pando"

// Outcome is how a piece of work ended.
type Outcome string

const (
	Succeeded Outcome = "succeeded"
	Failed    Outcome = "failed"
	Canceled  Outcome = "canceled"
)

// OutcomeOf is Succeeded for a nil error, Canceled when the context was
// canceled, and Failed otherwise.
func OutcomeOf(ctx context.Context, err error) Outcome {
	switch {
	case err == nil:
		return Succeeded
	case ctx.Err() != nil:
		return Canceled
	default:
		return Failed
	}
}

// Attribute keys. Every key a Pando metric may carry is here; the R-400 test
// fails on any other.
const (
	keyOutcome       = "pando.outcome"
	keyDecision      = "pando.proxy.decision"
	keyPrincipalKind = "pando.principal.kind"
	keyCategory      = "pando.adapter.category"
	keyAdapter       = "pando.adapter.id"
	keyKind          = "pando.retention.kind"
	keyJob           = "pando.retention.job"
	keyMethod        = "http.request.method"
	keyRoute         = "http.route"
	keyStatus        = "http.response.status_code"
	keyConnState     = "db.client.connection.state"
)

// AttributeKeys is every attribute key a Pando metric may carry (R-400).
var AttributeKeys = []string{
	keyOutcome, keyDecision, keyPrincipalKind, keyCategory, keyAdapter, keyKind, keyJob,
	keyMethod, keyRoute, keyStatus, keyConnState,
}

// Sources are the readings taken when metrics are collected, rather than
// recorded as things happen. Any may be nil.
type Sources struct {
	// Pool is the database pool's connections: in use, idle and the most
	// it may open.
	Pool func() (used, idle, max int64)

	// Detections is how many detections this replica is running now.
	Detections func() int64

	// Adapters is each adapter's health as last checked, true for healthy.
	Adapters func() []AdapterHealth
}

// AdapterHealth is one adapter's last health check.
type AdapterHealth struct {
	Category string // an adapter category, such as runtime
	ID       string // the adapter's configured ID, such as docker
	Healthy  bool
}

type instruments struct {
	httpDuration      metric.Float64Histogram
	proxyRequests     metric.Int64Counter
	reconcileDuration metric.Float64Histogram
	deployDuration    metric.Float64Histogram
	buildDuration     metric.Float64Histogram
	detectDuration    metric.Float64Histogram
	retentionDuration metric.Float64Histogram
	retentionRemoved  metric.Int64Counter
}

var current atomic.Pointer[instruments]

func init() {
	in, _ := build(noop.NewMeterProvider(), Sources{})
	current.Store(in)
}

// Install records every metric from here on into provider, and registers
// sources to be read at each collection. It replaces whatever was installed
// before, which tests rely on.
func Install(provider metric.MeterProvider, sources Sources) error {
	in, err := build(provider, sources)
	if err != nil {
		return err
	}
	current.Store(in)
	return nil
}

// Reset goes back to recording into a no-op.
func Reset() {
	in, _ := build(noop.NewMeterProvider(), Sources{})
	current.Store(in)
}

// Durations are in seconds, as OpenTelemetry's semantic conventions have them.
// The buckets run from 5ms, for an API call, to an hour, for a slow build.
var durationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600,
}

func build(provider metric.MeterProvider, sources Sources) (*instruments, error) {
	m := provider.Meter(ScopeName)
	var in instruments
	var err error
	duration := func(name, desc string) metric.Float64Histogram {
		if err != nil {
			return nil
		}
		var h metric.Float64Histogram
		h, err = m.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(durationBuckets...))
		return h
	}

	in.httpDuration = duration("http.server.request.duration",
		"Duration of requests to Pando's API and console, by route pattern.")
	in.reconcileDuration = duration("pando.reconcile.duration",
		"Duration of one reconcile of one app, by outcome.")
	in.deployDuration = duration("pando.deploy.duration",
		"Duration of a deploy, from start to its end state, by outcome.")
	in.buildDuration = duration("pando.build.duration",
		"Duration of an image build, by outcome.")
	in.detectDuration = duration("pando.detection.duration",
		"Duration of a detection, by outcome.")
	in.retentionDuration = duration("pando.retention.duration",
		"Duration of one pass of a retention job, by job and outcome.")
	if err != nil {
		return nil, err
	}

	if in.proxyRequests, err = m.Int64Counter("pando.proxy.requests", metric.WithUnit("{request}"),
		metric.WithDescription("Requests to apps through Pando's proxy, install-wide, by decision and kind of principal.")); err != nil {
		return nil, err
	}
	if in.retentionRemoved, err = m.Int64Counter("pando.retention.removed", metric.WithUnit("{row}"),
		metric.WithDescription("Rows the retention job removed, by kind.")); err != nil {
		return nil, err
	}

	if sources.Pool != nil {
		used, err := m.Int64ObservableGauge("db.client.connection.count", metric.WithUnit("{connection}"),
			metric.WithDescription("Database connections in this replica's pool, by state."))
		if err != nil {
			return nil, err
		}
		limit, err := m.Int64ObservableGauge("db.client.connection.max", metric.WithUnit("{connection}"),
			metric.WithDescription("The most database connections this replica's pool may open."))
		if err != nil {
			return nil, err
		}
		if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			u, i, max := sources.Pool()
			o.ObserveInt64(used, u, metric.WithAttributes(attribute.String(keyConnState, "used")))
			o.ObserveInt64(used, i, metric.WithAttributes(attribute.String(keyConnState, "idle")))
			o.ObserveInt64(limit, max)
			return nil
		}, used, limit); err != nil {
			return nil, err
		}
	}

	if sources.Detections != nil {
		if _, err := m.Int64ObservableGauge("pando.detection.active", metric.WithUnit("{detection}"),
			metric.WithDescription("Detections this replica is running now."),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				o.Observe(sources.Detections())
				return nil
			})); err != nil {
			return nil, err
		}
	}

	if sources.Adapters != nil {
		if _, err := m.Int64ObservableGauge("pando.adapter.healthy", metric.WithUnit("1"),
			metric.WithDescription("1 when an adapter's last health check passed, 0 when it failed."),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				for _, a := range sources.Adapters() {
					v := int64(0)
					if a.Healthy {
						v = 1
					}
					o.Observe(v, metric.WithAttributes(
						attribute.String(keyCategory, a.Category), attribute.String(keyAdapter, a.ID)))
				}
				return nil
			})); err != nil {
			return nil, err
		}
	}

	return &in, nil
}

func seconds(d time.Duration) float64 { return d.Seconds() }

func outcome(o Outcome) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(keyOutcome, string(o)))
}

// HTTPRequest records one request to Pando's own API or console. route is the
// router's pattern, such as /api/v1/apps/{id}, never the path: a path carries
// IDs (R-400). Empty means no route matched.
func HTTPRequest(ctx context.Context, method, route string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	current.Load().httpDuration.Record(ctx, seconds(d), metric.WithAttributes(
		attribute.String(keyMethod, knownMethod(method)),
		attribute.String(keyRoute, route),
		attribute.String(keyStatus, strconv.Itoa(status)),
	))
}

// knownMethod keeps a client from minting series with made-up methods.
func knownMethod(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return m
	}
	return "_OTHER"
}

// ProxyRequest records one request to an app through the proxy. kind is the
// kind of principal — user, token, anonymous — never which one (R-400), and
// no app is named: the count is install-wide.
func ProxyRequest(ctx context.Context, allowed bool, kind string) {
	decision := "denied"
	if allowed {
		decision = "allowed"
	}
	current.Load().proxyRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String(keyDecision, decision),
		attribute.String(keyPrincipalKind, kind),
	))
}

// Reconcile records one reconcile of one app.
func Reconcile(ctx context.Context, o Outcome, d time.Duration) {
	current.Load().reconcileDuration.Record(ctx, seconds(d), outcome(o))
}

// Deploy records a deploy reaching its end state.
func Deploy(ctx context.Context, o Outcome, d time.Duration) {
	current.Load().deployDuration.Record(ctx, seconds(d), outcome(o))
}

// Build records one image build.
func Build(ctx context.Context, o Outcome, d time.Duration) {
	current.Load().buildDuration.Record(ctx, seconds(d), outcome(o))
}

// Detection records one detection.
func Detection(ctx context.Context, o Outcome, d time.Duration) {
	current.Load().detectDuration.Record(ctx, seconds(d), outcome(o))
}

// RetentionJob is which retention job passed.
type RetentionJob string

const (
	// RetentionAudit archives and drops months of the audit log (R-347).
	RetentionAudit RetentionJob = "audit"
	// RetentionRows removes old rows from every other table (R-224).
	RetentionRows RetentionJob = "rows"
)

// RetentionPass records one pass of a retention job.
func RetentionPass(ctx context.Context, job RetentionJob, o Outcome, d time.Duration) {
	current.Load().retentionDuration.Record(ctx, seconds(d), metric.WithAttributes(
		attribute.String(keyJob, string(job)), attribute.String(keyOutcome, string(o))))
}

// RetentionRemoved records rows of one kind the retention job removed. kind
// is the kind of row, such as deployments or sessions.
func RetentionRemoved(ctx context.Context, kind string, n int64) {
	if n <= 0 {
		return
	}
	current.Load().retentionRemoved.Add(ctx, n, metric.WithAttributes(attribute.String(keyKind, kind)))
}
