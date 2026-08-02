package metrics_test

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/metrics"
)

// testReader is installed exactly once, in TestMain. otel.SetMeterProvider's
// documented delegation — instruments created against the no-op global Meter
// are automatically rewired once a real MeterProvider is registered — only
// fires on the FIRST such registration in a process. A second SetMeterProvider
// call (e.g. one per test) does NOT retroactively re-point instruments already
// bound by the first, so per-test provider swapping silently drops every test
// after the first. TestMain gives this test binary exactly one registration,
// matching how cmd/server calls metrics.Init exactly once in production.
//
// Because instruments are cumulative Sums keyed by attribute set, tests stay
// independent by using disjoint attribute values rather than resetting state
// between them.
var testReader = sdkmetric.NewManualReader()

func TestMain(m *testing.M) {
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader))
	otel.SetMeterProvider(provider)
	os.Exit(m.Run())
}

// findSum locates a Sum[int64] data point matching name/attrs among the
// collected resource metrics, or fails the test.
func findSum(t *testing.T, rm metricdata.ResourceMetrics, name string, attrs attribute.Set) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is not a Sum[int64]: %T", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&attrs) {
					return dp.Value
				}
			}
		}
	}
	t.Fatalf("metric %q with attributes %v not found in %+v", name, attrs, rm)
	return 0
}

func TestEventsIngestedRecordsAttributes(t *testing.T) {
	ctx := context.Background()

	attrs := attribute.NewSet(
		attribute.String("source", "paypal"),
		attribute.String("resource_type", "paypal_transaction"),
		attribute.String("outcome", "inserted"))

	metrics.EventsIngested.Add(ctx, 1, metric.WithAttributeSet(attrs))
	metrics.EventsIngested.Add(ctx, 2, metric.WithAttributeSet(attrs))

	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}

	if got := findSum(t, rm, "events.ingested", attrs); got != 3 {
		t.Errorf("events.ingested = %d, want 3", got)
	}
}

func TestWorkerFailuresRecordsAttributes(t *testing.T) {
	ctx := context.Background()

	retrying := attribute.NewSet(
		attribute.String("resource_type", "order_test_wf"),
		attribute.String("outcome", "retrying"))
	deadLettered := attribute.NewSet(
		attribute.String("resource_type", "order_test_wf"),
		attribute.String("outcome", "dead_lettered"))

	metrics.WorkerFailures.Add(ctx, 1, metric.WithAttributeSet(retrying))
	metrics.WorkerFailures.Add(ctx, 1, metric.WithAttributeSet(deadLettered))

	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}

	if got := findSum(t, rm, "worker.failures", retrying); got != 1 {
		t.Errorf("worker.failures{outcome=retrying} = %d, want 1", got)
	}
	if got := findSum(t, rm, "worker.failures", deadLettered); got != 1 {
		t.Errorf("worker.failures{outcome=dead_lettered} = %d, want 1", got)
	}
}

func TestReconcileSecondsRecords(t *testing.T) {
	ctx := context.Background()

	metrics.ReconcileSeconds.Record(ctx, 0.25)

	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "reconcile.duration" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("reconcile.duration not found in %+v", rm)
	}
}
