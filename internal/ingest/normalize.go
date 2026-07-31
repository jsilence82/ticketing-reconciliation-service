package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// Normalizers turn a raw provider resource into a storable EventRecord.
//
// Both ingestion paths funnel through here, so the resource identity, the
// version stamp and the PII filter are decided in exactly one place. The
// backfill importer uses it today; webhook handlers will in P4.

// ttObjectTypes maps Ticket Tailor's own `object` discriminator onto our
// resource types.
//
// Ticket Tailor supplies this directly, which is worth using rather than
// inferring: it means resource_type is the provider's classification, not our
// guess from an id prefix.
var ttObjectTypes = map[string]model.ResourceType{
	"order":         model.ResourceOrder,
	"issued_ticket": model.ResourceIssuedTicket,
	"event":         model.ResourceEvent,
	"event_series":  model.ResourceEventSeries,
}

// ttEnvelope is the minimum needed to identify and version a Ticket Tailor
// resource, decoded before sanitization strips anything.
type ttEnvelope struct {
	Object    string `json:"object"`
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// FromTicketTailor builds a record from one raw Ticket Tailor resource.
func FromTicketTailor(raw []byte, origin model.Origin) (model.EventRecord, error) {
	var env ttEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return model.EventRecord{}, fmt.Errorf("ticket tailor envelope: %w", err)
	}
	if env.ID == "" {
		return model.EventRecord{}, fmt.Errorf("ticket tailor resource has no id")
	}

	rt, known := ttObjectTypes[env.Object]
	if !known {
		// Waitlist signups and anything else Ticket Tailor adds later are stored
		// for the audit trail but never reconciled. Without the `ignored` status
		// they would sit at `received` forever and pollute dead-letter metrics.
		rt = model.ResourceOther
	}

	payload, err := Sanitize(rt, raw)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("%s %s: %w", env.Object, env.ID, err)
	}

	status := "received"
	if rt == model.ResourceOther {
		status = "ignored"
	}

	return model.EventRecord{
		Source:       model.SourceTicketTailor,
		ResourceType: rt,
		ResourceID:   env.ID,
		Origin:       origin,
		Status:       status,
		Payload:      payload,
		OccurredAt:   ttOccurredAt(env),
	}, nil
}

// ttOccurredAt picks the provider timestamp that versions a resource.
//
// Only issued tickets carry updated_at — verified against the real snapshot:
// 2,562 of 2,562 tickets have one, and 0 of 1,532 orders, 0 of 54 events and
// 0 of 9 series do. A ticket's updated_at moves when it is voided, so it is the
// meaningful version; everything else is frozen at creation.
//
// That is exactly why the upsert cannot rely on occurred_at alone: for three of
// the four resource types a later resync carries an identical timestamp, and
// only the payload hash can tell that the content changed. See store.upsertSQL.
func ttOccurredAt(env ttEnvelope) time.Time {
	if env.UpdatedAt > 0 {
		return time.Unix(env.UpdatedAt, 0).UTC()
	}
	if env.CreatedAt > 0 {
		return time.Unix(env.CreatedAt, 0).UTC()
	}
	return time.Time{}
}

// paypalCacheRecord is the dashboard's already-normalized transaction shape,
// which is what the historical snapshot contains.
type paypalCacheRecord struct {
	TxnID string `json:"txn_id"`
	Date  string `json:"date"`
}

// FromPayPalCache builds a record from one transaction in the dashboard's
// PayPal cache.
//
// Note this is the dashboard's NORMALIZED shape, not raw Transaction Search
// output. Consequently the snapshot path exercises storage and the matching
// rule, but not the PayPal normalizer — the gross/fee sign conversion CLAUDE.md
// calls the highest-risk conversion in the project. Closing that needs a
// sandbox capture of raw transaction_info, which belongs to a later phase.
func FromPayPalCache(raw []byte, origin model.Origin) (model.EventRecord, error) {
	var rec paypalCacheRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal cache record: %w", err)
	}
	if rec.TxnID == "" {
		return model.EventRecord{}, fmt.Errorf("paypal transaction has no txn_id")
	}

	payload, err := Sanitize(model.ResourcePayPalTransaction, raw)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal %s: %w", rec.TxnID, err)
	}

	return model.EventRecord{
		Source:       model.SourcePayPal,
		ResourceType: model.ResourcePayPalTransaction,
		ResourceID:   rec.TxnID,
		Origin:       origin,
		Status:       "received",
		Payload:      payload,
		OccurredAt:   payPalDate(rec.Date),
	}, nil
}

// payPalDate parses the cache's date field.
//
// It is only YYYY-MM-DD, because the reference takes the first ten characters
// of the provider timestamp without timezone conversion (api/paypal.py:85). So
// occurred_at for a PayPal row is day-granular, and 387 of 462 real rows share
// a value with at least one other — another reason the version guard needs the
// payload-hash arm rather than timestamps alone.
func payPalDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// searchTransaction is the RAW Transaction Search shape, as opposed to the
// dashboard's already-normalized cache form.
type searchTransaction struct {
	TransactionInfo struct {
		TransactionID     string `json:"transaction_id"`
		InitiationDate    string `json:"transaction_initiation_date"`
		TransactionStatus string `json:"transaction_status"`
		PayPalReferenceID string `json:"paypal_reference_id"`
		TransactionAmount *money `json:"transaction_amount"`
		FeeAmount         *money `json:"fee_amount"`
	} `json:"transaction_info"`
}

// money is PayPal's amount object. `value` is a DECIMAL STRING, not a number —
// which is convenient, because it means no float ever round-trips through JSON
// on the way in.
type money struct {
	Value        string `json:"value"`
	CurrencyCode string `json:"currency_code"`
}

// FromPayPalSearch builds a record from one raw Transaction Search result.
//
// # The sign convention
//
// CLAUDE.md calls this the highest-risk conversion in the project, and it is:
// every downstream figure flows through net = gross + fee, so getting it wrong
// silently doubles or zeroes the fees in every report rather than failing.
//
// Transaction Search returns fee_amount ALREADY SIGNED — negative on a charge,
// positive on a refund — so net is gross PLUS fee, never minus
// (api/paypal.py:88). Webhooks differ: they report paypal_fee as a positive
// magnitude in both directions and need explicit negation. That divergence is
// why both sources normalize here rather than at their call sites.
func FromPayPalSearch(raw []byte, origin model.Origin) (model.EventRecord, error) {
	var s searchTransaction
	if err := json.Unmarshal(raw, &s); err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal search record: %w", err)
	}

	ti := s.TransactionInfo
	if ti.TransactionID == "" {
		return model.EventRecord{}, fmt.Errorf("paypal search record has no transaction_id")
	}

	gross := parseAmount(ti.TransactionAmount)
	fee := parseAmount(ti.FeeAmount)

	currency := ""
	if ti.TransactionAmount != nil {
		currency = ti.TransactionAmount.CurrencyCode
	}

	// Build the normalized shape the rest of the service uses, so a row from
	// Transaction Search and a row from the dashboard cache are indistinguishable
	// downstream.
	normalized := map[string]any{
		"txn_id": ti.TransactionID,
		// The reference takes the first ten characters with NO timezone
		// conversion (api/paypal.py:85). Parsing and reformatting would shift
		// the date for non-UTC offsets.
		"date":                dateKey(ti.InitiationDate),
		"gross":               gross,
		"fee":                 fee,
		"net":                 gross + fee,
		"status":              ti.TransactionStatus,
		"paypal_reference_id": ti.PayPalReferenceID,
		"currency":            currency,
	}

	blob, err := json.Marshal(normalized)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal %s: %w", ti.TransactionID, err)
	}

	payload, err := Sanitize(model.ResourcePayPalTransaction, blob)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal %s: %w", ti.TransactionID, err)
	}

	return model.EventRecord{
		Source:       model.SourcePayPal,
		ResourceType: model.ResourcePayPalTransaction,
		ResourceID:   ti.TransactionID,
		Origin:       origin,
		Status:       "received",
		Payload:      payload,
		OccurredAt:   payPalDate(dateKey(ti.InitiationDate)),
	}, nil
}

// parseAmount reads PayPal's decimal string. A missing object is zero, matching
// the reference's `(ti.get("transaction_amount") or {}).get("value", 0)`.
func parseAmount(m *money) float64 {
	if m == nil || m.Value == "" {
		return 0
	}
	f, err := strconv.ParseFloat(m.Value, 64)
	if err != nil {
		return 0
	}
	return f
}

// dateKey takes the first ten characters, as the reference does.
func dateKey(s string) string {
	if len(s) < 10 {
		return ""
	}
	return s[:10]
}
