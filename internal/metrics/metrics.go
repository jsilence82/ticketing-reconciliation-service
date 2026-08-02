// Package metrics defines this service's OpenTelemetry metric instruments and
// wires them to a Prometheus-compatible exporter.
//
// Metrics only, deliberately. CLAUDE.md's Tech stack line calls for
// Prometheus/Grafana, which is a metrics backend — this is one binary talking
// to Postgres, not a multi-service topology, so there is nothing here for
// distributed tracing to usefully explain.
package metrics

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

const meterName = "github.com/jsilence82/ticketing-reconciliation-service"

// Instruments, created once against the no-op global Meter. Init (below)
// swaps in the real global MeterProvider; per otel.Meter's documented
// behavior, every instrument already created is automatically rewired to it.
// Call sites never need an "is metrics ready" check — .Add()/.Record() is
// always safe, before or after Init runs.
var (
	meter = otel.Meter(meterName)

	// EventsIngested covers CLAUDE.md's "events received" (sum of all
	// outcomes) and "dedup hits" (outcome="rejected"). Labels: source,
	// resource_type, outcome.
	EventsIngested metric.Int64Counter

	// WorkerRows covers stage-1 rows that reached a terminal, non-failure
	// outcome. Label: outcome (processed|ignored).
	WorkerRows metric.Int64Counter

	// WorkerFailures covers CLAUDE.md's "retries" and "dead-letters".
	// Labels: resource_type, outcome (retrying|dead_lettered).
	WorkerFailures metric.Int64Counter

	// ReconcileRuns counts stage-2 attempts by a small, fixed result
	// category — never the free-text ReconcileSkipped string, which is
	// unbounded cardinality.
	ReconcileRuns metric.Int64Counter

	// ReconcileChanged sums verdicts changed by passes that actually ran.
	ReconcileChanged metric.Int64Counter

	// ReconcileSeconds times passes that actually ran.
	ReconcileSeconds metric.Float64Histogram
)

func init() {
	var err error
	if EventsIngested, err = meter.Int64Counter("events.ingested",
		metric.WithDescription("Webhook/backfill events accepted by Upsert, by outcome")); err != nil {
		panic(err)
	}
	if WorkerRows, err = meter.Int64Counter("worker.rows",
		metric.WithDescription("Stage-1 rows drained to a non-failure terminal outcome")); err != nil {
		panic(err)
	}
	if WorkerFailures, err = meter.Int64Counter("worker.failures",
		metric.WithDescription("Stage-1 validation failures, by retry outcome")); err != nil {
		panic(err)
	}
	if ReconcileRuns, err = meter.Int64Counter("reconcile.runs",
		metric.WithDescription("Stage-2 reconcile pass attempts, by result category")); err != nil {
		panic(err)
	}
	if ReconcileChanged, err = meter.Int64Counter("reconcile.changed",
		metric.WithDescription("Verdicts changed by reconcile passes that ran")); err != nil {
		panic(err)
	}
	if ReconcileSeconds, err = meter.Float64Histogram("reconcile.duration",
		metric.WithDescription("Wall time of reconcile passes that ran"),
		metric.WithUnit("s")); err != nil {
		panic(err)
	}
}

// RegisterHealthGauges wires store.Health() as an OTel Observable Gauge
// callback, refreshed on each Prometheus scrape rather than polled on a
// timer — Health is already a cheap aggregate query (one UNION ALL over two
// GROUP BYs), the same one /healthz uses, so there is no reason to cache it
// between scrapes.
func RegisterHealthGauges(st *store.Store) error {
	byStatus, err := meter.Int64ObservableGauge("events.by_status",
		metric.WithDescription("Row count by events.status"))
	if err != nil {
		return err
	}
	byRecon, err := meter.Int64ObservableGauge("events.by_recon_status",
		metric.WithDescription("Row count by events.recon_status"))
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		counts, err := st.Health(ctx)
		if err != nil {
			return fmt.Errorf("metrics: health gauges: %w", err)
		}
		for status, n := range counts.ByStatus {
			o.ObserveInt64(byStatus, int64(n), metric.WithAttributes(
				attribute.String("status", status)))
		}
		for status, n := range counts.ByReconStatus {
			o.ObserveInt64(byRecon, int64(n), metric.WithAttributes(
				attribute.String("recon_status", status)))
		}
		return nil
	}, byStatus, byRecon)
	return err
}

// Init builds the Prometheus-backed MeterProvider and installs it globally.
// Call once, early in cmd/server's run(). The returned shutdown flushes and
// releases the provider; call it during graceful shutdown.
func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", "ticketing-reconciliation-service")))
	if err != nil {
		return nil, fmt.Errorf("metrics: resource: %w", err)
	}

	exporter, err := otelprometheus.New(
		otelprometheus.WithNamespace("recon"),
		otelprometheus.WithRegisterer(prometheus.DefaultRegisterer),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: prometheus exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(provider)

	return provider.Shutdown, nil
}
