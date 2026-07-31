package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// reconcileLockKey single-flights the dataset-wide pass.
//
// Stage 1 of the worker parallelises freely via SKIP LOCKED, but stage 2 must
// not: two concurrent passes would race on the bulk verdict write and could
// interleave a stale view with a fresh one.
const reconcileLockKey int64 = 0x747273726563 // "trsrec"

// ReconState is the bookkeeping that keeps the reconcile pass from running on
// every tick.
type ReconState struct {
	LastRunAt          time.Time
	LastInputUpdatedAt time.Time
	LastRunID          string
	VerdictsChanged    int64
}

// LoadReconState reads the single bookkeeping row.
func (s *Store) LoadReconState(ctx context.Context) (ReconState, error) {
	var (
		st      ReconState
		runAt   *time.Time
		inputAt *time.Time
		runID   *string
		changed int64
	)

	err := s.pool.QueryRow(ctx, `
		SELECT last_run_at, last_input_updated_at, last_run_id, verdicts_changed
		  FROM recon_state WHERE id`).Scan(&runAt, &inputAt, &runID, &changed)
	if err != nil {
		return st, fmt.Errorf("load recon state: %w", err)
	}

	if runAt != nil {
		st.LastRunAt = *runAt
	}
	if inputAt != nil {
		st.LastInputUpdatedAt = *inputAt
	}
	if runID != nil {
		st.LastRunID = *runID
	}
	st.VerdictsChanged = changed
	return st, nil
}

// InputWatermark is max(updated_at) across reconcilable rows.
//
// One indexed aggregate. Comparing it against the recorded watermark is how the
// worker decides whether anything has changed since the last pass, and in the
// steady state it is the only query a tick runs.
func (s *Store) InputWatermark(ctx context.Context) (time.Time, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(updated_at) FROM events WHERE resource_type <> 'other'`).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("input watermark: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}

// ReconcileResult reports what a pass did.
type ReconcileResult struct {
	// Ran is false when another worker held the lock, which is a skip rather
	// than a failure.
	Ran bool
	// Changed counts rows whose verdict actually moved. Zero is the normal
	// steady-state outcome.
	Changed int64
	// Resources is how many rows the pass classified.
	Resources int
}

// Reconcile runs one dataset-wide pass inside a single transaction.
//
// The transaction spans the read AND the write, at REPEATABLE READ, so a
// concurrent stage-1 write cannot make the classifier's input disagree with the
// rows the update then targets.
//
// The advisory lock is transaction-scoped, so it releases on commit or rollback
// with no cleanup path to forget. It is a TRY: if another worker is already
// reconciling, this returns Ran=false rather than queueing, because a second
// pass over identical data has nothing to add.
//
// classify receives the loaded rows and returns the verdicts to persist. The
// caller supplies it so this package holds SQL and nothing else — the matching
// rule stays in internal/recon.
func (s *Store) Reconcile(
	ctx context.Context,
	runID string,
	classify func([]RawResource) ([]Verdict, error),
) (ReconcileResult, error) {
	var res ReconcileResult

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return res, fmt.Errorf("begin reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var acquired bool
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock($1)`, reconcileLockKey).Scan(&acquired); err != nil {
		return res, fmt.Errorf("reconcile lock: %w", err)
	}
	if !acquired {
		return res, nil // another worker is mid-pass
	}
	res.Ran = true

	rows, err := loadResourcesTx(ctx, tx)
	if err != nil {
		return res, err
	}
	res.Resources = len(rows)

	verdicts, err := classify(rows)
	if err != nil {
		return res, fmt.Errorf("classify: %w", err)
	}

	changed, err := writeVerdictsTx(ctx, tx, runID, verdicts)
	if err != nil {
		return res, err
	}
	res.Changed = changed

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit reconcile: %w", err)
	}
	return res, nil
}

// RecordReconRun stores the bookkeeping for a completed pass.
//
// watermark is the value observed BEFORE the pass, not after. Recording the
// later value would swallow any input that arrived while the pass was running.
// The cost is one extra settling pass after a run that changed verdicts — and
// that pass writes nothing, thanks to the IS DISTINCT FROM guard in the verdict
// update, so the loop converges instead of oscillating.
func (s *Store) RecordReconRun(
	ctx context.Context, runID string, watermark time.Time, changed int64,
) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE recon_state
		   SET last_run_at           = now(),
		       last_input_updated_at = $1,
		       last_run_id           = $2::uuid,
		       verdicts_changed      = $3
		 WHERE id`, nilIfZero(watermark), runID, changed)
	if err != nil {
		return fmt.Errorf("record recon run: %w", err)
	}
	return nil
}

// loadResourcesTx is LoadResources bound to a transaction, so the reconcile
// pass reads and writes the same snapshot.
func loadResourcesTx(ctx context.Context, tx pgx.Tx) ([]RawResource, error) {
	rows, err := tx.Query(ctx, `
		SELECT source, resource_type, resource_id, payload
		  FROM events
		 WHERE status NOT IN ('ignored','dead_lettered')
		   AND resource_type = ANY($1::text[])
		 ORDER BY source, resource_type, seq`, reconcilableTypes)
	if err != nil {
		return nil, fmt.Errorf("load resources: %w", err)
	}
	defer rows.Close()

	var out []RawResource
	for rows.Next() {
		var r RawResource
		if err := rows.Scan(&r.Source, &r.ResourceType, &r.ResourceID, &r.Payload); err != nil {
			return nil, fmt.Errorf("scan resource: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func writeVerdictsTx(ctx context.Context, tx pgx.Tx, runID string, vs []Verdict) (int64, error) {
	if len(vs) == 0 {
		return 0, nil
	}

	sources := make([]string, len(vs))
	ids := make([]string, len(vs))
	statuses := make([]string, len(vs))
	counterparts := make([]string, len(vs))
	for i, v := range vs {
		sources[i] = string(v.Source)
		ids[i] = v.ResourceID
		statuses[i] = string(v.Status)
		counterparts[i] = v.CounterpartID
	}

	tag, err := tx.Exec(ctx, verdictSQL, runID, sources, ids, statuses, counterparts)
	if err != nil {
		return 0, fmt.Errorf("write verdicts: %w", err)
	}
	return tag.RowsAffected(), nil
}
