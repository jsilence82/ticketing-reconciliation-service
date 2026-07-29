// Package model holds the domain types shared by the reconciliation engine and
// the projection layer.
//
// These types deliberately contain no buyer PII. The dashboard's canonical
// frame carries email/buyer_name/buyer_id for its repeat-buyer analysis, but
// reconciliation Totals and Statistics never read them, and omitting them
// shrinks the blast radius of the access-control requirement in CLAUDE.md.
package model

import "time"

// Source identifies which provider a record came from.
type Source string

// The provider identifiers used in the events table's source column.
const (
	SourcePayPal       Source = "paypal"
	SourceTicketTailor Source = "tickettailor"
)

// Provenance ranks how much a write is trusted when two writes race. Higher
// wins. See the projection layer in CLAUDE.md: a slow backfill page must never
// clobber a fresh webhook.
type Provenance int16

// Provenance ranks, lowest to highest trust. A backfill page carrying stale
// state must never overwrite a fresher webhook, so ordering matters more than
// the specific values.
const (
	ProvenanceBackfill   Provenance = 1
	ProvenanceRESTRepair Provenance = 2
	ProvenanceWebhook    Provenance = 3
)

// Version carries the ordering tuple every projection row is upserted with.
// SourceVersion is the PROVIDER's timestamp, never our receive time.
type Version struct {
	SourceVersion time.Time
	SourceRank    Provenance
	IngestSeq     int64
	ContentHash   []byte
}

// CanonicalTicket mirrors the output of the dashboard's mapping.build_canonical,
// field for field, at one-issued-ticket grain.
//
// The production column mapping is frozen (see docs/PARITY.md); this struct is
// the Go shape of its result. Fields prefixed Order* correspond to the
// underscore-prefixed internals the dashboard joins on from TT orders — they
// are not user-mapped.
type CanonicalTicket struct {
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
	// Totals, so it must never be guessed.
	PayPalReferenceID string

	// ReferenceState tracks whether PayPalReferenceID has been resolved.
	ReferenceState ReferenceState

	// Currency is captured even though the reference ignores it and labels all
	// output in euro.
	Currency string
}

// ReferenceState records whether a refund has been linked to its original charge.
type ReferenceState string

const (
	// ReferenceNotApplicable is used for non-refund transactions.
	ReferenceNotApplicable ReferenceState = ""
	// ReferenceResolved means PayPalReferenceID is trustworthy.
	ReferenceResolved ReferenceState = "resolved"
	// ReferencePending means the link is not yet known. Alert if it persists.
	ReferencePending ReferenceState = "pending"
)

// TTOrder is the projection of a Ticket Tailor order. It carries the match key.
type TTOrder struct {
	ID string
	// TxnID is the order's PayPal transaction id — the reconciliation match key.
	TxnID             string
	PaymentMethodType string
	RefundAmountMinor int64
	TotalPaidMinor    int64
	Currency          string
	Status            string
	CreatedAt         time.Time
	Version           Version
}

// TTIssuedTicket is the projection of a Ticket Tailor issued ticket. This is
// the grain of the canonical frame.
type TTIssuedTicket struct {
	ID string
	// OrderID may reference an order this service has not yet received. That is
	// not an error state: the reconciliation read is a LEFT JOIN, which
	// reproduces what the reference's pandas map+fillna does for a missing key.
	OrderID       string
	EventID       string
	EventSeriesID string
	TicketTypeID  string
	Category      string
	Status        string
	PriceMinor    int64
	Currency      string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Version       Version
}

// TTEvent is the projection of a Ticket Tailor event — one performance.
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
	Version   Version
}

// TTEventSeries is the projection of a Ticket Tailor event series — a show.
type TTEventSeries struct {
	ID                   string
	Name                 string
	TicketsAvailableAt   *time.Time
	TicketsUnavailableAt *time.Time
	Version              Version
}

// Start returns the event start as a UTC time. The reference formats night
// labels from the UTC interpretation of the unix timestamp, so a performance
// starting late in the local evening can be labelled the following day. That is
// replicated deliberately.
func (e TTEvent) Start() time.Time { return time.Unix(e.StartUnix, 0).UTC() }

// IsDeleted reports whether the event has been tombstoned.
func (e TTEvent) IsDeleted() bool { return e.DeletedAt != nil }
