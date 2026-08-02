package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// Outcome reports what an upsert actually did.
//
// Rejected is a real signal, not an error: it means the incoming write was
// older than, or identical to, what is already stored. It is the dedup-hit
// metric internal/metrics reports.
type Outcome string

// Upsert outcomes.
const (
	Inserted Outcome = "inserted"
	Updated  Outcome = "updated"
	Rejected Outcome = "rejected"
)

// upsertSQL is the version-ordered upsert.
//
// The WHERE clause has two arms and BOTH are required:
//
//  1. Strictly newer by the provider's clock, or equal-but-better-sourced
//     (a webhook outranks a backfill page carrying an older snapshot).
//
//  2. Same version, different content. This arm is not an optimisation — it is
//     a correctness fix. Ticket Tailor orders carry no updated_at (verified: 0
//     of 1,532), so a refund posted against an old order changes refund_amount
//     while created_at, and therefore occurred_at, stays put. Both the original
//     and the resync write are origin='backfill', so arm 1 ties and the update
//     would be discarded — defeating the full-resync policy that exists
//     precisely to catch those refunds. The payload hash is the only remaining
//     discriminator, which is why payload_hash is NOT NULL rather than optional.
//
// status comes from EXCLUDED rather than being hardcoded to 'received', so the
// caller decides whether new content for a resource is reconcilable or ignored.
// retry_count and the backoff floor reset because new content deserves a fresh
// processing budget.
const upsertSQL = `
INSERT INTO events (source, resource_type, resource_id, webhook_notification_id,
                    origin, topic, status, payload, payload_hash, occurred_at,
                    signature_verified_at)
VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (source, resource_id) DO UPDATE SET
    resource_type           = excluded.resource_type,
    webhook_notification_id = COALESCE(excluded.webhook_notification_id,
                                       events.webhook_notification_id),
    origin                  = excluded.origin,
    topic                   = excluded.topic,
    payload                 = excluded.payload,
    payload_hash            = excluded.payload_hash,
    occurred_at             = excluded.occurred_at,
    signature_verified_at   = COALESCE(excluded.signature_verified_at,
                                       events.signature_verified_at),
    status                  = excluded.status,
    retry_count             = 0,
    next_attempt_at         = now(),
    last_error              = '',
    updated_at              = now()
WHERE  (excluded.occurred_at, (excluded.origin = 'webhook'))
     > (events.occurred_at,   (events.origin   = 'webhook'))
   OR (excluded.occurred_at = events.occurred_at
       AND excluded.payload_hash <> events.payload_hash)
RETURNING (xmax = 0) AS inserted, seq`

// Upsert writes one record, honouring the version guard.
//
// The payload hash is computed here rather than taken from the caller, so it
// cannot be forgotten or fall out of step with the bytes actually stored.
func (s *Store) Upsert(ctx context.Context, rec model.EventRecord) (Outcome, error) {
	out, err := upsertOn(ctx, s.pool, rec)
	return out, err
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func upsertOn(ctx context.Context, q querier, rec model.EventRecord) (Outcome, error) {
	// Last line of defence. internal/ingest already reduces payloads to an
	// allowlist, so this is duplication — and that is the point: it turns
	// "someone added a write path that skipped Sanitize" into a hard failure
	// rather than a leak that is only discovered by reading the table.
	// One recursive walk over a few KB, microseconds.
	if err := model.ScanForPII(rec.Payload, model.PIIExemptions(rec.ResourceType)); err != nil {
		return "", fmt.Errorf("refusing to store %s/%s: %w",
			rec.Source, rec.ResourceID, err)
	}

	sum := sha256.Sum256(rec.Payload)

	var inserted bool
	var seq int64

	err := q.QueryRow(ctx, upsertSQL,
		rec.Source, rec.ResourceType, rec.ResourceID, rec.WebhookNotificationID,
		rec.Origin, rec.Topic, rec.Status, rec.Payload, sum[:], rec.OccurredAt,
		rec.SignatureVerifiedAt,
	).Scan(&inserted, &seq)

	if err != nil {
		// No row returned means the guard rejected the write. That is the
		// expected path for a duplicate delivery, not a failure.
		if err == pgx.ErrNoRows {
			return Rejected, nil
		}
		return "", fmt.Errorf("upsert %s/%s: %w", rec.Source, rec.ResourceID, err)
	}

	if inserted {
		return Inserted, nil
	}
	return Updated, nil
}

// UpsertBatch writes many records in one transaction.
//
// Used by the backfill importer, where a per-row round trip over thousands of
// records dominates the runtime.
func (s *Store) UpsertBatch(ctx context.Context, recs []model.EventRecord) (map[Outcome]int, error) {
	counts := map[Outcome]int{}
	if len(recs) == 0 {
		return counts, nil
	}

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for _, rec := range recs {
			out, err := upsertOn(ctx, tx, rec)
			if err != nil {
				return err
			}
			counts[out]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// claimSQL takes a batch of due rows and marks them in flight.
//
// SKIP LOCKED means a second worker takes different rows rather than blocking,
// so horizontal scaling needs no coordination. Ordering by (next_attempt_at,
// seq) keeps the queue roughly FIFO and makes the partial index usable.
const claimSQL = `
WITH claimed AS (
    SELECT id FROM events
     WHERE status IN ('received','failed')
       AND next_attempt_at <= now()
     ORDER BY next_attempt_at, seq
     FOR UPDATE SKIP LOCKED
     LIMIT $1
)
UPDATE events e
   SET status = 'processing', updated_at = now()
  FROM claimed c
 WHERE e.id = c.id
RETURNING ` + eventColumns

// Claim marks up to limit due rows as processing and returns them.
//
// The transaction commits before the caller processes anything, so the rows are
// visibly in flight rather than holding locks for the duration of the work. A
// worker that dies mid-batch therefore leaves rows stuck in 'processing' — that
// is what ReleaseStale exists to recover, and under Compose's
// `restart: unless-stopped` it is routine rather than exotic.
func (s *Store) Claim(ctx context.Context, limit int) ([]model.EventRecord, error) {
	rows, err := s.pool.Query(ctx, claimSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// ReleaseStale returns rows abandoned in 'processing' to the queue.
//
// Without this a worker killed between claim and completion strands its rows
// permanently: they are neither claimable nor finished, and nothing reports
// them as stuck.
func (s *Store) ReleaseStale(ctx context.Context, lease time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE events
		   SET status = 'received', next_attempt_at = now(), updated_at = now()
		 WHERE status = 'processing'
		   AND updated_at < now() - $1::interval`,
		lease.String())
	if err != nil {
		return 0, fmt.Errorf("release stale: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkProcessed completes rows successfully.
func (s *Store) MarkProcessed(ctx context.Context, ids []string) error {
	return s.setStatus(ctx, ids, "processed")
}

// MarkIgnored parks rows that are stored but never reconciled — non-whitelisted
// PayPal topics and waitlist signups. Without this status they would sit at
// 'received' forever and pollute dead-letter metrics.
func (s *Store) MarkIgnored(ctx context.Context, ids []string) error {
	return s.setStatus(ctx, ids, "ignored")
}

func (s *Store) setStatus(ctx context.Context, ids []string, status string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE events SET status = $2, last_error = '', updated_at = now()
		 WHERE id = ANY($1::uuid[])`, ids, status)
	if err != nil {
		return fmt.Errorf("set status %s: %w", status, err)
	}
	return nil
}

// FailureOutcome reports which branch MarkFailed's UPDATE took, mirroring
// Outcome's naming convention. It is the "retries" vs. "dead-letters" metrics
// signal, sourced from the SQL's own RETURNING clause rather than re-deriving
// retry_count+1 >= maxRetries a second time in Go, so that comparison lives
// in exactly one place.
type FailureOutcome string

// MarkFailed outcomes.
const (
	Retrying     FailureOutcome = "retrying"
	DeadLettered FailureOutcome = "dead_lettered"
)

// MarkFailed records a failure and schedules the next attempt.
//
// The backoff interval is computed by the caller (with cenkalti/backoff and
// jitter) rather than in SQL, so the retry policy is unit-testable without a
// database. Crossing maxRetries dead-letters the row.
func (s *Store) MarkFailed(
	ctx context.Context, id string, cause error, retryAfter time.Duration, maxRetries int,
) (FailureOutcome, error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	var status string
	err := s.pool.QueryRow(ctx, `
		UPDATE events
		   SET retry_count     = retry_count + 1,
		       last_error      = $2,
		       next_attempt_at = now() + $3::interval,
		       status          = CASE WHEN retry_count + 1 >= $4
		                              THEN 'dead_lettered' ELSE 'failed' END,
		       updated_at      = now()
		 WHERE id = $1
		RETURNING status`,
		id, msg, retryAfter.String(), maxRetries,
	).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("mark failed %s: %w", id, err)
	}

	if status == "dead_lettered" {
		return DeadLettered, nil
	}
	return Retrying, nil
}

// Verdict is one per-resource reconciliation outcome to persist.
//
// Deliberately a store-local type rather than recon.Classification, so this
// package depends only on internal/model and the layering stays one-directional.
type Verdict struct {
	Source        model.Source
	ResourceID    string
	Status        model.ReconStatus
	CounterpartID string
}

// verdictSQL is shared by the pooled and transactional paths so the two cannot
// drift. One statement for the whole pass, not a round trip per row.
//
// The IS DISTINCT FROM guard is load-bearing, not an optimisation. A pass that
// changes nothing must write zero rows, so that updated_at keeps meaning "state
// actually changed". The worker's trigger is derived from max(updated_at);
// without this guard every pass would touch every row, which would re-trigger
// the next pass, and the loop would never settle.
const verdictSQL = `
UPDATE events e
   SET recon_status         = v.recon_status,
       recon_counterpart_id = v.counterpart_id,
       recon_run_id         = $1::uuid,
       updated_at           = now()
  FROM (SELECT * FROM unnest($2::text[], $3::text[], $4::text[], $5::text[]))
        AS v(source, resource_id, recon_status, counterpart_id)
 WHERE e.source = v.source
   AND e.resource_id = v.resource_id
   AND (e.recon_status, e.recon_counterpart_id)
       IS DISTINCT FROM (v.recon_status, v.counterpart_id)`

// WriteVerdicts persists verdicts outside a reconcile pass. The pass itself
// uses store.Reconcile, which does the same write inside its transaction.
func (s *Store) WriteVerdicts(ctx context.Context, runID string, vs []Verdict) (int64, error) {
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

	tag, err := s.pool.Exec(ctx, verdictSQL, runID, sources, ids, statuses, counterparts)
	if err != nil {
		return 0, fmt.Errorf("write verdicts: %w", err)
	}
	return tag.RowsAffected(), nil
}
