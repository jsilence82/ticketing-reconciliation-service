// Package worker turns stored events into reconciliation verdicts.
//
// # Why two stages
//
// recon.Classify is DATASET-WIDE. Whether a payment counts as matched depends on
// whether any ticket anywhere references it, and a late-arriving order flips a
// previously-unmatched payment to matched. A per-row worker therefore cannot
// classify a row in isolation.
//
// Stage 1 is per-row and parallelises freely under SELECT ... FOR UPDATE SKIP
// LOCKED: it validates a stored payload and marks the row processed, ignored or
// dead-lettered. A payload that cannot be parsed is dead-lettered HERE, so it
// never poisons the pass that follows.
//
// Stage 2 is the dataset-wide reconcile pass, single-flighted by a
// transaction-scoped advisory lock and debounced so a 4,000-row backfill
// triggers a handful of passes rather than 4,000.
//
// The alternative — maintaining the match incrementally — was rejected
// deliberately. It would require a second implementation of the matching
// rule living outside internal/recon, which the depguard rule in
// .golangci.yml exists to prevent. A full pass over a few thousand rows
// is milliseconds; trading a divergence risk in the one piece of logic that must
// not diverge, to save that, is a bad bargain.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/metrics"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/recon"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// Config tunes the loop. The zero value is not useful; use DefaultConfig.
type Config struct {
	// ClaimBatch is how many rows stage 1 takes per claim.
	ClaimBatch int

	// PollInterval is how often the loop wakes.
	PollInterval time.Duration

	// QuietPeriod is how long the input must be still before a reconcile pass
	// runs. This is what collapses a large backfill into a few passes instead of
	// one per row.
	QuietPeriod time.Duration

	// MaxStaleness forces a pass even if the input never goes quiet, so a
	// continuously-trickling stream cannot starve reconciliation forever.
	MaxStaleness time.Duration

	// Lease is how long a row may sit in 'processing' before the reaper assumes
	// the worker died and returns it to the queue.
	Lease time.Duration

	// MaxRetries before a row is dead-lettered.
	MaxRetries int

	// Flags selects deviations from the reference. Default (zero) reproduces it.
	Flags recon.Flags
}

// DefaultConfig is tuned for SSG's volume: thousands of rows, bursty during a
// backfill, near-idle otherwise.
func DefaultConfig() Config {
	return Config{
		ClaimBatch:   200,
		PollInterval: 30 * time.Second,
		QuietPeriod:  5 * time.Second,
		MaxStaleness: 5 * time.Minute,
		Lease:        15 * time.Minute,
		MaxRetries:   5,
	}
}

// Worker runs both stages.
type Worker struct {
	store *store.Store
	cfg   Config
	log   *slog.Logger

	// backoff is stateless here; a fresh policy is derived per attempt from the
	// row's own retry_count, so restarting the worker does not reset a row's
	// backoff.
	base time.Duration
	max  time.Duration
}

// New builds a worker.
func New(st *store.Store, cfg Config, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store: st,
		cfg:   cfg,
		log:   log,
		base:  time.Second,
		max:   10 * time.Minute,
	}
}

// TickResult reports one iteration.
type TickResult struct {
	Released  int64
	Processed int
	Failed    int
	Reconcile store.ReconcileResult
	// ReconcileSkipped explains why stage 2 did not run, if it did not.
	ReconcileSkipped string
}

// Run loops until the context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker starting",
		"poll", w.cfg.PollInterval, "batch", w.cfg.ClaimBatch)

	t := time.NewTicker(w.cfg.PollInterval)
	defer t.Stop()

	for {
		res, err := w.Tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A tick failure is transient by assumption — the next tick retries.
			// Dying here would strand rows the reaper would then have to recover.
			w.log.Error("tick failed", "err", err)
		} else if res.Processed > 0 || res.Failed > 0 || res.Reconcile.Changed > 0 {
			w.log.Info("tick",
				"processed", res.Processed, "failed", res.Failed,
				"released", res.Released, "verdicts_changed", res.Reconcile.Changed)
		}

		select {
		case <-ctx.Done():
			w.log.Info("worker stopping")
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Tick runs one iteration: reap, drain stage 1, then consider stage 2.
//
// Exported so tests drive the loop deterministically rather than sleeping.
func (w *Worker) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult

	released, err := w.store.ReleaseStale(ctx, w.cfg.Lease)
	if err != nil {
		return res, err
	}
	res.Released = released

	processed, failed, err := w.DrainStageOne(ctx)
	res.Processed, res.Failed = processed, failed
	if err != nil {
		return res, err
	}

	rec, skipped, err := w.MaybeReconcile(ctx)
	res.Reconcile, res.ReconcileSkipped = rec, skipped
	return res, err
}

// DrainStageOne claims and validates until nothing is due.
func (w *Worker) DrainStageOne(ctx context.Context) (processed, failed int, err error) {
	for {
		rows, err := w.store.Claim(ctx, w.cfg.ClaimBatch)
		if err != nil {
			return processed, failed, err
		}
		if len(rows) == 0 {
			return processed, failed, nil
		}

		var ok, ignored []string
		for _, r := range rows {
			switch verr := w.validate(r); {
			case verr != nil:
				// The row is stored but unusable. Dead-lettering it here keeps a
				// malformed payload out of the reconcile pass, where it would
				// abort the whole thing.
				outcome, err := w.store.MarkFailed(ctx, r.ID, verr,
					w.backoffFor(r.RetryCount), w.cfg.MaxRetries)
				if err != nil {
					return processed, failed, err
				}
				metrics.WorkerFailures.Add(ctx, 1, metric.WithAttributes(
					attribute.String("resource_type", string(r.ResourceType)),
					attribute.String("outcome", string(outcome))))
				failed++
			case r.ResourceType == model.ResourceOther:
				ignored = append(ignored, r.ID)
			default:
				ok = append(ok, r.ID)
			}
		}

		if err := w.store.MarkProcessed(ctx, ok); err != nil {
			return processed, failed, err
		}
		if err := w.store.MarkIgnored(ctx, ignored); err != nil {
			return processed, failed, err
		}
		if len(ok) > 0 {
			metrics.WorkerRows.Add(ctx, int64(len(ok)), metric.WithAttributes(
				attribute.String("outcome", "processed")))
		}
		if len(ignored) > 0 {
			metrics.WorkerRows.Add(ctx, int64(len(ignored)), metric.WithAttributes(
				attribute.String("outcome", "ignored")))
		}
		processed += len(ok) + len(ignored)

		if ctx.Err() != nil {
			return processed, failed, ctx.Err()
		}
	}
}

// validate is the per-row work: can this payload actually be used?
func (w *Worker) validate(r model.EventRecord) error {
	if r.ResourceType == model.ResourceOther {
		return nil // stored for the audit trail, never reconciled
	}
	return ingest.ValidateStored(r.ResourceType, r.Payload)
}

// backoffFor derives the delay for a row's next attempt from its retry count.
//
// Deriving it from stored state rather than from in-memory attempt tracking
// means restarting the worker does not reset a row's backoff, and two workers
// cannot disagree about how long a row has been failing.
func (w *Worker) backoffFor(retryCount int) time.Duration {
	e := backoff.NewExponentialBackOff()
	e.InitialInterval = w.base
	e.MaxInterval = w.max
	e.RandomizationFactor = 0.3 // jitter, so retries do not synchronise
	e.Reset()

	d := e.NextBackOff()
	for i := 0; i < retryCount; i++ {
		next := e.NextBackOff()
		if next == backoff.Stop {
			break
		}
		d = next
	}
	return d
}

// MaybeReconcile runs stage 2 if the input has changed and settled.
//
// Returns the reason when it declines, so a worker that is not reconciling can
// say why rather than looking hung.
func (w *Worker) MaybeReconcile(ctx context.Context) (store.ReconcileResult, string, error) {
	var zero store.ReconcileResult

	watermark, err := w.store.InputWatermark(ctx)
	if err != nil {
		return zero, "", err
	}
	state, err := w.store.LoadReconState(ctx)
	if err != nil {
		return zero, "", err
	}

	// Steady state. One indexed aggregate and we are done — this is the common
	// case and it must stay cheap.
	if !watermark.IsZero() && watermark.Equal(state.LastInputUpdatedAt) {
		recordReconcileRun(ctx, "unchanged")
		return zero, "input unchanged", nil
	}
	if watermark.IsZero() {
		recordReconcileRun(ctx, "no_rows")
		return zero, "no reconcilable rows", nil
	}

	// Wait for a burst to finish rather than reconciling after every batch.
	// MaxStaleness overrides, so a continuous trickle still gets reconciled.
	quietFor := time.Since(watermark)
	staleFor := time.Since(state.LastRunAt)
	if quietFor < w.cfg.QuietPeriod && staleFor < w.cfg.MaxStaleness {
		recordReconcileRun(ctx, "settling")
		return zero, fmt.Sprintf("input still settling (%s)", quietFor.Round(time.Millisecond)), nil
	}

	res, err := w.Reconcile(ctx, watermark)
	if err != nil {
		return zero, "", err
	}
	if !res.Ran {
		recordReconcileRun(ctx, "lock_held")
		return res, "another worker holds the reconcile lock", nil
	}
	return res, "", nil
}

// recordReconcileRun records ReconcileRuns with a small, fixed result
// category. Deliberately not fed MaybeReconcile's free-text skip reason
// (e.g. "input still settling (340ms)") — that string is unbounded
// cardinality and must never reach a metric label.
func recordReconcileRun(ctx context.Context, result string) {
	metrics.ReconcileRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// Reconcile forces a pass regardless of the debounce.
//
// cmd/backfill calls this at the end of an import so a backfill is
// self-contained and does not have to wait on a poll tick.
func (w *Worker) Reconcile(ctx context.Context, watermark time.Time) (store.ReconcileResult, error) {
	if watermark.IsZero() {
		var err error
		watermark, err = w.store.InputWatermark(ctx)
		if err != nil {
			return store.ReconcileResult{}, err
		}
	}

	runID, err := newRunID()
	if err != nil {
		return store.ReconcileResult{}, err
	}

	start := time.Now()
	res, err := w.store.Reconcile(ctx, runID, func(rows []store.RawResource) ([]store.Verdict, error) {
		inputs, err := ingest.DecodeResources(rows)
		if err != nil {
			return nil, err
		}

		tickets := model.Assemble(inputs.Orders, inputs.Tickets, inputs.Events)

		// The one and only matching rule. Nothing here re-implements it.
		classified := recon.Classify(tickets, inputs.Txns, w.cfg.Flags)

		verdicts := make([]store.Verdict, 0, len(classified))
		for _, c := range classified {
			verdicts = append(verdicts, store.Verdict{
				Source:        c.Source,
				ResourceID:    c.ResourceID,
				Status:        c.Status,
				CounterpartID: c.CounterpartID,
			})
		}
		return verdicts, nil
	})
	if err != nil {
		return res, err
	}
	if !res.Ran {
		return res, nil
	}

	// Record the PRE-pass watermark. Recording a later one would swallow input
	// that arrived while the pass was running.
	if err := w.store.RecordReconRun(ctx, runID, watermark, res.Changed); err != nil {
		return res, err
	}

	metrics.ReconcileSeconds.Record(ctx, time.Since(start).Seconds())
	recordReconcileRun(ctx, "ran")
	metrics.ReconcileChanged.Add(ctx, res.Changed)

	w.log.Info("reconciled",
		"run", runID, "resources", res.Resources, "verdicts_changed", res.Changed)
	return res, nil
}

// newRunID makes a UUIDv4 without pulling in a UUID dependency for one call.
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("run id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// ErrNotRunning is returned when a forced reconcile finds the lock held.
var ErrNotRunning = errors.New("worker: reconcile lock held by another worker")
