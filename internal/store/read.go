package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// eventColumns is the canonical projection, shared by every query returning
// rows, so a column added to one path cannot be forgotten in another.
const eventColumns = `
    e.id, e.seq, e.source, e.resource_type, e.resource_id,
    COALESCE(e.webhook_notification_id, ''), e.origin, e.topic, e.status,
    e.payload, e.occurred_at, e.recon_status, e.recon_counterpart_id,
    e.retry_count, e.next_attempt_at, e.last_error, e.signature_verified_at,
    e.created_at, e.updated_at`

func scanEvents(rows pgx.Rows) ([]model.EventRecord, error) {
	var out []model.EventRecord
	for rows.Next() {
		var r model.EventRecord
		if err := rows.Scan(
			&r.ID, &r.Seq, &r.Source, &r.ResourceType, &r.ResourceID,
			&r.WebhookNotificationID, &r.Origin, &r.Topic, &r.Status,
			&r.Payload, &r.OccurredAt, &r.ReconStatus, &r.ReconCounterpartID,
			&r.RetryCount, &r.NextAttemptAt, &r.LastError, &r.SignatureVerifiedAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RawResource is one stored resource, payload undecoded.
//
// Decoding lives outside this package: store holds SQL, not provider payload
// knowledge.
type RawResource struct {
	Source       model.Source
	ResourceType model.ResourceType
	ResourceID   string
	Payload      []byte
}

// reconcilableTypes are the resource types the matching rule reads.
var reconcilableTypes = []string{"order", "issued_ticket", "event", "paypal_transaction"}

// LoadResources returns everything the reconcile pass needs, IN INGEST ORDER.
//
// Two decisions here are easy to undo by accident:
//
//   - ORDER BY seq. Verified by experiment: reversing the ingest order of PayPal
//     rows makes the parity run fail on matched_txn_ids, which compare.py checks
//     as an ordered list. Sorting by created_at or id could not reproduce the
//     provider order anyway — 1,026 of 2,562 real tickets share a created_at.
//
//     The money sums are order-sensitive in principle as well (pairwise and
//     Neumaier summation), though on the present dataset reversing ticket order
//     moves no figure; the values are well-conditioned enough that the
//     compensation absorbs it. Not something to rely on.
//
//   - The visible-status set includes 'received'. CLAUDE.md's architecture
//     diagram reads "Postgres (processed) → query API", which taken literally
//     would make the first pass after a backfill see an empty table and
//     classify every transaction unmatched. Only 'ignored' and 'dead_lettered'
//     are excluded: the former is not reconcilable, the latter could not be
//     parsed.
func (s *Store) LoadResources(ctx context.Context) ([]RawResource, error) {
	rows, err := s.pool.Query(ctx, `
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

// GetEvent fetches one row by id. Returns pgx.ErrNoRows when absent.
func (s *Store) GetEvent(ctx context.Context, id string) (model.EventRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+eventColumns+` FROM events e WHERE e.id = $1`, id)
	if err != nil {
		return model.EventRecord{}, translate(err)
	}
	defer rows.Close()

	// pgx can defer a parameter error to row iteration rather than raising it at
	// Query time, so this path needs translating too — a malformed uuid arrives
	// here, not above.
	recs, err := scanEvents(rows)
	if err != nil {
		return model.EventRecord{}, translate(err)
	}
	if len(recs) == 0 {
		return model.EventRecord{}, ErrNotFound
	}
	return recs[0], nil
}

// ListFilter selects a slice of the event log for GET /events.
type ListFilter struct {
	Source      string
	Status      string
	ReconStatus string
	// Since filters on occurred_at — the PROVIDER's clock, so "sales since X"
	// means what a treasurer would expect, not "rows we happened to write since X".
	Since  time.Time
	Cursor string
	Limit  int
}

// Page is one keyset page plus the cursor for the next.
type Page struct {
	Events []model.EventRecord
	// Next is empty when the page is the last one.
	Next string
}

const defaultPageLimit = 100
const maxPageLimit = 1000

// ListEvents returns a keyset-paginated page.
//
// Keyset, not OFFSET: aggregation happens consumer-side, so a consumer summing
// a date range must be able to rely on seeing every row exactly once. OFFSET
// silently skips or repeats rows when the underlying data shifts between pages.
func (s *Store) ListEvents(ctx context.Context, f ListFilter) (Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}

	var (
		cursorAt time.Time
		cursorID string
	)
	if f.Cursor != "" {
		var err error
		cursorAt, cursorID, err = decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
	}

	// Fetch one extra row to discover whether another page exists, rather than
	// issuing a second COUNT query.
	rows, err := s.pool.Query(ctx, `
		SELECT `+eventColumns+`
		  FROM events e
		 WHERE ($1::text        IS NULL OR e.source       = $1)
		   AND ($2::text        IS NULL OR e.status       = $2)
		   AND ($3::text        IS NULL OR e.recon_status = $3)
		   AND ($4::timestamptz IS NULL OR e.occurred_at >= $4)
		   AND ($5::timestamptz IS NULL OR (e.created_at, e.id) > ($5, $6::uuid))
		 ORDER BY e.created_at, e.id
		 LIMIT $7`,
		nilIfEmpty(f.Source), nilIfEmpty(f.Status), nilIfEmpty(f.ReconStatus),
		nilIfZero(f.Since), nilIfZero(cursorAt), nilIfEmpty(cursorID),
		limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	recs, err := scanEvents(rows)
	if err != nil {
		return Page{}, translate(err)
	}

	page := Page{Events: recs}
	if len(recs) > limit {
		last := recs[limit-1]
		page.Events = recs[:limit]
		page.Next = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// HealthCounts is what /healthz reports.
//
// Unmatched and pending are the numbers that matter operationally: both mean a
// consumer's figures are drifting from reality, and neither produces an error
// anywhere else.
type HealthCounts struct {
	ByStatus      map[string]int
	ByReconStatus map[string]int
}

// Health tallies rows by status and verdict.
func (s *Store) Health(ctx context.Context) (HealthCounts, error) {
	out := HealthCounts{
		ByStatus:      map[string]int{},
		ByReconStatus: map[string]int{},
	}

	rows, err := s.pool.Query(ctx, `
		SELECT 'status', status, count(*) FROM events GROUP BY status
		UNION ALL
		SELECT 'recon', recon_status, count(*) FROM events GROUP BY recon_status`)
	if err != nil {
		return out, fmt.Errorf("health: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind, value string
		var n int
		if err := rows.Scan(&kind, &value, &n); err != nil {
			return out, fmt.Errorf("scan health: %w", err)
		}
		if kind == "status" {
			out.ByStatus[value] = n
		} else {
			out.ByReconStatus[value] = n
		}
	}
	return out, rows.Err()
}

// encodeCursor makes the keyset position opaque, so the ordering columns stay
// an implementation detail rather than a published interface.
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(s string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %w", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("%w: malformed", ErrInvalidCursor)
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad timestamp: %w", ErrInvalidCursor, err)
	}
	return at, parts[1], nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ProbeWritable reports whether this store's role can INSERT.
//
// It exists so the API can PROVE its connection is read-only rather than trust
// configuration (guardrail 3). The write is attempted inside a transaction that
// is always rolled back, so even a misconfigured role leaves nothing behind.
//
// Returns (true, nil) when the write succeeded — meaning the role is too
// privileged for the API to use. Returns (false, nil) when Postgres refused on
// permission grounds. Any other failure is returned as an error rather than
// being mistaken for proof of safety.
func (s *Store) ProbeWritable(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("writability probe: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO events (source, resource_type, resource_id, origin, status,
		                    payload, payload_hash, occurred_at)
		VALUES ('paypal','paypal_transaction','__readonly_probe__','backfill',
		        'received','{}'::jsonb,'\x00'::bytea, now())`)

	switch {
	case err == nil:
		return true, nil
	case isPermissionDenied(err):
		return false, nil
	default:
		return false, fmt.Errorf("writability probe failed unexpectedly: %w", err)
	}
}
