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
	"errors"
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

func init() { Reset() }

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
	// A no-op provider makes every instrument it is asked for.
	in, _ := build(noop.NewMeterProvider(), Sources{})
	current.Store(in)
}

// Durations are in seconds, as OpenTelemetry's semantic conventions have them.
// The buckets run from 5ms, for an API call, to an hour, for a slow build.
var durationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600,
}

func build(provider metric.MeterProvider, sources Sources) (*instruments, error) {
	b := &builder{m: provider.Meter(ScopeName)}
	in := &instruments{
		httpDuration: b.duration("http.server.request.duration",
			"Duration of requests to Pando's API and console, by route pattern."),
		reconcileDuration: b.duration("pando.reconcile.duration",
			"Duration of one reconcile of one app, by outcome."),
		deployDuration: b.duration("pando.deploy.duration",
			"Duration of a deploy, from start to its end state, by outcome."),
		buildDuration: b.duration("pando.build.duration",
			"Duration of an image build, by outcome."),
		detectDuration: b.duration("pando.detection.duration",
			"Duration of a detection, by outcome."),
		retentionDuration: b.duration("pando.retention.duration",
			"Duration of one pass of a retention job, by job and outcome."),
		proxyRequests: b.counter("pando.proxy.requests", "{request}",
			"Requests to apps through Pando's proxy, install-wide, by decision and kind of principal."),
		retentionRemoved: b.counter("pando.retention.removed", "{row}",
			"Rows the retention job removed, by kind."),
	}
	if sources.Pool != nil {
		b.pool(sources.Pool)
	}
	if sources.Detections != nil {
		b.gauge("pando.detection.active", "{detection}", "Detections this replica is running now.",
			func(o metric.Int64Observer) { o.Observe(sources.Detections()) })
	}
	if sources.Adapters != nil {
		b.gauge("pando.adapter.healthy", "1", "1 when an adapter's last health check passed, 0 when it failed.",
			func(o metric.Int64Observer) { observeAdapters(o, sources.Adapters()) })
	}
	if b.err != nil {
		return nil, b.err
	}
	return in, nil
}

// builder makes instruments and keeps every error, so build reads as the
// list of metrics it is.
type builder struct {
	m   metric.Meter
	err error
}

func (b *builder) keep(err error) { b.err = errors.Join(b.err, err) }

func (b *builder) duration(name, desc string) metric.Float64Histogram {
	h, err := b.m.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	b.keep(err)
	return h
}

func (b *builder) counter(name, unit, desc string) metric.Int64Counter {
	c, err := b.m.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
	b.keep(err)
	return c
}

// gauge is a reading taken at each collection.
func (b *builder) gauge(name, unit, desc string, observe func(metric.Int64Observer)) {
	_, err := b.m.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			observe(o)
			return nil
		}))
	b.keep(err)
}

// pool is the database pool's two gauges, read together so they agree.
func (b *builder) pool(read func() (used, idle, limit int64)) {
	count, err := b.m.Int64ObservableGauge("db.client.connection.count", metric.WithUnit("{connection}"),
		metric.WithDescription("Database connections in this replica's pool, by state."))
	b.keep(err)
	most, err := b.m.Int64ObservableGauge("db.client.connection.max", metric.WithUnit("{connection}"),
		metric.WithDescription("The most database connections this replica's pool may open."))
	b.keep(err)
	if b.err != nil {
		return
	}
	_, err = b.m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		used, idle, limit := read()
		o.ObserveInt64(count, used, metric.WithAttributes(attribute.String(keyConnState, "used")))
		o.ObserveInt64(count, idle, metric.WithAttributes(attribute.String(keyConnState, "idle")))
		o.ObserveInt64(most, limit)
		return nil
	}, count, most)
	b.keep(err)
}

func observeAdapters(o metric.Int64Observer, adapters []AdapterHealth) {
	for _, a := range adapters {
		v := int64(0)
		if a.Healthy {
			v = 1
		}
		o.Observe(v, metric.WithAttributes(attribute.String(keyCategory, a.Category), attribute.String(keyAdapter, a.ID)))
	}
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
