// Package model holds the domain types shared by the reconciliation engine and
// the storage layer.
//
// These types deliberately contain no buyer PII. The dashboard's canonical frame
// carries email/buyer_name/buyer_id for its repeat-buyer analysis, but
// reconciliation never reads them, and omitting them shrinks the blast radius of
// the access-control requirement in CLAUDE.md.
//
// # Storage shape
//
// There is exactly one persisted table, `events`, holding one row per real-world
// resource keyed on (source, resource_id). There is NO projection/derived-table
// layer — an earlier design had one and it was deliberately removed. The joined
// CanonicalTicket below is assembled at READ time (see assemble.go), never
// stored.
package model

import "time"

// Source identifies which provider a record came from.
type Source string

// The provider identifiers used in the events table's source column.
const (
	SourcePayPal       Source = "paypal"
	SourceTicketTailor Source = "tickettailor"
)

// Origin records how a row was obtained. It doubles as the tie-breaker in the
// version-ordered upsert: a webhook outranks a backfill page carrying an older
// snapshot of the same resource.
type Origin string

// The ingestion paths. Both write to the same table and converge on the same
// worker; only acquisition and verification differ.
const (
	OriginWebhook  Origin = "webhook"
	OriginBackfill Origin = "backfill"
)

// Rank returns the provenance precedence used to break ties when two writes
// carry the same occurred_at. Higher wins, so a slow backfill can never clobber
// fresher webhook state.
func (o Origin) Rank() int {
	if o == OriginWebhook {
		return 1
	}
	return 0
}

// ResourceType names what kind of thing a row is about.
//
// The events table holds four Ticket Tailor resource types plus PayPal
// transactions, not just orders. Ticket Tailor supplies this directly as the
// payload's `object` field; for PayPal it is derived from the webhook topic.
//
// Without it you cannot express "give me all issued tickets", and the unique
// constraint would be relying on Ticket Tailor's id prefixes (or_, it_, ev_,
// es_) never colliding — true today, but not a property to depend on.
type ResourceType string

// The resource types this service ingests.
const (
	ResourceOrder             ResourceType = "order"
	ResourceIssuedTicket      ResourceType = "issued_ticket"
	ResourceEvent             ResourceType = "event"
	ResourceEventSeries       ResourceType = "event_series"
	ResourcePayPalTransaction ResourceType = "paypal_transaction"
	// ResourceOther covers ingested-but-not-reconciled payloads such as
	// waitlist_signup, which are stored with status `ignored`.
	ResourceOther ResourceType = "other"
)

// ReconStatus is the verdict this service exists to produce and store.
//
// It means different things per ResourceType, which is a deliberate choice
// rather than an accident: a PayPal transaction is matched or unmatched, whereas
// `transferred` is a property of a Ticket Tailor ticket (voided there, but the
// PayPal charge stands and no refund was issued).
type ReconStatus string

// The per-resource reconciliation verdicts.
const (
	// ReconMatched: a PayPal transaction with a corresponding Ticket Tailor
	// order, or a refund linked back to its original charge.
	ReconMatched ReconStatus = "matched"

	// ReconUnmatched: a PayPal transaction with no Ticket Tailor counterpart.
	// The business contract requires these be surfaced, never discarded.
	ReconUnmatched ReconStatus = "unmatched"

	// ReconTransferred: a Ticket Tailor ticket voided with no refund, where the
	// PayPal charge remains valid. Must be flagged, not silently dropped.
	ReconTransferred ReconStatus = "transferred"

	// ReconPending: a refund whose link to its original charge is not yet
	// resolved. PayPal webhooks omit paypal_reference_id, so it must be fetched.
	// Never guess it — an unresolved refund silently understates a night's net.
	ReconPending ReconStatus = "pending"

	// ReconNotApplicable: the resource carries no reconciliation verdict, e.g.
	// an event or an ordinary non-voided ticket.
	ReconNotApplicable ReconStatus = "not_applicable"
)

// EventRecord is one row of the `events` table — the only persisted state.
//
// The unique constraint is (Source, ResourceID). Multiple deliveries about one
// resource collapse into this single row: the table is latest-state-per-resource,
// not an append-only delivery log.
type EventRecord struct {
	ID string

	// Seq is the ingest ordinal assigned by Postgres. It is the ONLY ordering
	// that reproduces the provider's array order: the engine is order-sensitive,
	// and (created_at, id) cannot recover it because 1,026 of 2,562 real tickets
	// share a created_at and 387 of 462 PayPal rows share a date. Zero until the
	// row has been written.
	Seq int64

	Source       Source
	ResourceType ResourceType
	// ResourceID is the underlying resource's own ID — a PayPal transaction or
	// capture ID, or a Ticket Tailor order/ticket/event/series ID. NOT the
	// webhook envelope's notification ID: backfilled rows have no notification,
	// so keying on it would stop the two ingestion paths from recognising the
	// same real-world resource.
	ResourceID string

	// WebhookNotificationID is set only for webhook-ingested rows, purely to
	// debug delivery-level duplication.
	WebhookNotificationID string

	Origin Origin
	Topic  string
	Status string

	Payload []byte

	// OccurredAt is the PROVIDER's timestamp, and the primary ordering key for
	// the upsert. Never our receive time — ordering on receive time is what lets
	// a late delivery overwrite fresher state.
	OccurredAt time.Time

	// ReconStatus and ReconCounterpartID hold the verdict produced by
	// recon.Classify. This is the service's actual product.
	ReconStatus        ReconStatus
	ReconCounterpartID string

	RetryCount int

	// NextAttemptAt is the backoff floor. Without it a failing row is re-claimed
	// on every poll tick and burns its whole retry budget in seconds, which is
	// why retry_count alone is not enough to implement backoff.
	NextAttemptAt time.Time

	// LastError is the most recent failure, so a dead-lettered row can be
	// diagnosed without correlating logs.
	LastError string

	// SignatureVerifiedAt is nil for backfilled rows: a REST response has no
	// signature to verify.
	SignatureVerifiedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsNewerThan reports whether e should overwrite prev in the version-ordered
// upsert: strictly newer by the provider's clock, or equal-but-better-sourced.
//
// This mirrors the SQL guard in CLAUDE.md and exists so the same rule can be
// unit-tested without a database.
func (e EventRecord) IsNewerThan(prev EventRecord) bool {
	if e.OccurredAt.After(prev.OccurredAt) {
		return true
	}
	if e.OccurredAt.Equal(prev.OccurredAt) {
		return e.Origin.Rank() > prev.Origin.Rank()
	}
	return false
}

// CanonicalTicket is the joined, one-row-per-issued-ticket shape the matching
// rule operates on. It mirrors the output of the dashboard's
// mapping.build_canonical field for field.
//
// It is NOT a stored table. With no projection layer, it is assembled at read
// time from `events` rows — ticket joined to its order (for the PayPal
// transaction ID, payment type and refund amount) and to its event (for the
// performance date). See Assemble.
type CanonicalTicket struct {
	// TicketID and OrderID identify the underlying resources. The reconciliation
	// maths never reads them, but the classification output needs to name which
	// resource a verdict belongs to.
	TicketID string
	OrderID  string

	// Show is mapped from the joined event name.
	Show string
	// Category is mapped from the ticket type description. It becomes a column
	// in the Statistics table.
	Category string

	// Quantity is always 1.0 under the production mapping, because `quantity`
	// is unmapped and mapping.py defaults it. It is kept as a field rather than
	// assumed, so a future mapping change stays expressible.
	Quantity float64

	// Revenue is listed_price converted from cents. Under the production
	// mapping revenue_is_per_unit is false, so it is NOT multiplied by Quantity.
	Revenue float64

	// Date is the sale date, mapped from updated_at (not created_at). Zero
	// value means unset, matching pandas NaT.
	Date time.Time

	// Status drives both the active filter and the voided/transferred masks.
	// Compared lowercased but NOT trimmed, matching the reference.
	Status string

	// Occurrence is the night grouping key, mapped from event_id.
	Occurrence string

	// PerformanceDate is derived from the joined event start (unix seconds ->
	// UTC). Zero value means unset, matching pandas NaT. Rows with an unset
	// PerformanceDate are dropped from the Totals and Statistics bodies but
	// still counted in the Statistics TOTAL — see ledger entry 9.
	PerformanceDate time.Time

	// PayPalTxnID is the match key, joined from the ticket's order. Stripped of
	// surrounding whitespace, with the sentinels "" and "nan" treated as absent.
	PayPalTxnID string

	// OrderPaymentType is the TT order's payment_method.type. Compared
	// lowercased against "paypal" and "operator".
	OrderPaymentType string

	// OrderRefundAmount is the TT order's refund_amount. Note it is NOT
	// converted from cents in the reference, because it is only ever compared
	// against zero — see ledger entry 13.
	OrderRefundAmount float64

	// OrderTotalPaid is the order total split evenly across the order's
	// tickets, converted from cents.
	OrderTotalPaid float64
}

// PayPalTxn is one normalized PayPal transaction. Both the Transaction Search
// backfill and the webhook stream normalize into this single shape.
type PayPalTxn struct {
	TxnID string

	// DateKey is the raw first 10 characters of the provider's timestamp, NOT a
	// parsed date. The reference slices the string without timezone conversion
	// (api/paypal.py:85); parsing and reformatting would diverge for non-UTC
	// offsets, so the string is preserved verbatim.
	DateKey string

	// Gross is signed: negative on refunds and reversals.
	Gross float64

	// Fee is negative for charges and positive for refunds. Net is Gross+Fee,
	// never Gross-Fee.
	Fee float64

	// Net must equal Gross + Fee. Stored rather than derived so a normalization
	// bug is visible in the data instead of silently recomputed away.
	Net float64

	Status string

	// PayPalReferenceID points at the original charge on a refund or reversal.
	// It is the second half of the matching rule. Transaction Search supplies it
	// directly; webhooks do not, and it must be resolved from the refund's
	// links. An unresolved value silently drops the refund from a night's
	// figures, so it must never be guessed — classify it ReconPending instead.
	PayPalReferenceID string

	// Currency is captured even though the reference ignores it and labels all
	// output in euro.
	Currency string
}

// IsRefund reports whether the transaction moves money back to the buyer.
//
// The reference detects refunds by sign rather than by status
// (sections/reconciliation.py:191), and that is reproduced here.
func (t PayPalTxn) IsRefund() bool { return t.Gross < 0 }

// TTOrder is a Ticket Tailor order as stored in `events`. It carries the match
// key.
type TTOrder struct {
	ID string
	// TxnID is the order's PayPal transaction id — the reconciliation match key.
	TxnID             string
	PaymentMethodType string
	RefundAmount      float64
	TotalPaidMinor    int64
	Currency          string
	Status            string
	CreatedAt         time.Time
}

// TTIssuedTicket is a Ticket Tailor issued ticket as stored in `events`. This is
// the grain of the canonical frame.
type TTIssuedTicket struct {
	ID string
	// OrderID may reference an order this service has not yet received. That is
	// not an error state: the read-time join is a LEFT JOIN, reproducing what
	// the reference's pandas map+fillna does for a missing key.
	OrderID       string
	EventID       string
	EventSeriesID string
	TicketTypeID  string
	// Description is the ticket type description, which the production column
	// mapping binds to the canonical `category`.
	Description string
	Status      string
	PriceMinor  int64
	Currency    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TTEvent is a Ticket Tailor event — one performance.
type TTEvent struct {
	ID            string
	EventSeriesID string
	Name          string
	// StartUnix is kept as raw unix seconds because that is what the reference
	// parses, and the UTC interpretation of it determines the night label.
	StartUnix int64
	EndUnix   int64
	Capacity  int
	// DeletedAt tombstones the event. A tombstoned event must behave exactly
	// like a missing one, not be hard-deleted.
	DeletedAt *time.Time
}

// Start returns the event start as a UTC time. The reference formats night
// labels from the UTC interpretation of the unix timestamp, so a performance
// starting late in the local evening can be labelled the following day. That is
// replicated deliberately.
func (e TTEvent) Start() time.Time { return time.Unix(e.StartUnix, 0).UTC() }

// IsDeleted reports whether the event has been tombstoned.
func (e TTEvent) IsDeleted() bool { return e.DeletedAt != nil }
