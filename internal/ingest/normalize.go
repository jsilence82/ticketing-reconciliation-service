package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// paypalWebhookEnvelope is the top-level shape of every PayPal webhook
// delivery. Unlike Ticket Tailor's envelope, this one is fixed and
// extensively documented by PayPal itself — nothing here needed confirming
// against a real capture the way Ticket Tailor's did.
type paypalWebhookEnvelope struct {
	ID           string          `json:"id"`
	EventType    string          `json:"event_type"`
	ResourceType string          `json:"resource_type"`
	CreateTime   string          `json:"create_time"`
	Resource     json.RawMessage `json:"resource"`
}

// paypalLink is one entry of a PayPal HATEOAS links array.
type paypalLink struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}

// paypalWhitelistedTopics mirrors CLAUDE.md, Data sources: there is no
// webhook equivalent of Transaction Search's balance_affecting_records_only=Y,
// so every other topic must be stored as ignored rather than reconciled.
var paypalWhitelistedTopics = map[string]bool{
	"PAYMENT.CAPTURE.COMPLETED": true,
	"PAYMENT.CAPTURE.REFUNDED":  true,
	"PAYMENT.CAPTURE.REVERSED":  true,
	"PAYMENT.CAPTURE.DENIED":    true,
}

// paypalCaptureResource is the `resource` object on PAYMENT.CAPTURE.COMPLETED
// and PAYMENT.CAPTURE.DENIED events.
type paypalCaptureResource struct {
	ID                        string `json:"id"`
	Status                    string `json:"status"`
	Amount                    *money `json:"amount"`
	SellerReceivableBreakdown struct {
		PayPalFee *money `json:"paypal_fee"`
	} `json:"seller_receivable_breakdown"`
	CreateTime string       `json:"create_time"`
	Links      []paypalLink `json:"links"`
}

// paypalRefundResource is the `resource` object on PAYMENT.CAPTURE.REFUNDED
// and (best-effort — see FromPayPalWebhook's doc comment) REVERSED events.
// PayPal names the fee breakdown `seller_payable_breakdown` here, not
// `seller_receivable_breakdown` as on a capture — a real API inconsistency
// between the two resource shapes, not a typo in this struct.
type paypalRefundResource struct {
	ID                     string `json:"id"`
	Status                 string `json:"status"`
	Amount                 *money `json:"amount"`
	SellerPayableBreakdown struct {
		PayPalFee *money `json:"paypal_fee"`
	} `json:"seller_payable_breakdown"`
	CreateTime string       `json:"create_time"`
	Links      []paypalLink `json:"links"`
}

// FromPayPalWebhook builds a record from one PayPal webhook delivery.
//
// raw is the FULL delivery body, envelope included — unlike
// FromTicketTailor, which only ever sees the bare resource. That asymmetry is
// deliberate: Ticket Tailor's payload has the same shape whether it arrives by
// webhook or REST fetch, but PayPal's resource shape genuinely depends on
// event_type (a capture's fee breakdown is keyed differently than a refund's),
// so the topic dispatch has to happen here rather than in a caller that treats
// the resource opaquely.
//
// # Confidence
//
// PAYMENT.CAPTURE.COMPLETED and PAYMENT.CAPTURE.REFUNDED are PROVEN
// (2026-08-01) against a real sandbox transaction: an order captured then
// refunded via the live Sandbox REST API, both deliveries genuinely signed by
// PayPal, both verified, both normalized correctly (including the fee-sign
// flip in both directions, and the refund's paypal_reference_id correctly
// derived from its "up" link — the classifier then paired the two rows
// correctly). See CLAUDE.md's Architecture status note for the full trace.
//
// PAYMENT.CAPTURE.REVERSED is implemented AS IF it shares the refund shape,
// which matches PayPal's public webhook reference but — unlike COMPLETED and
// REFUNDED above — has NOT been validated against a real delivery, because
// triggering a genuine reversal (a bank-initiated chargeback) is not
// practical to simulate on demand in the sandbox. Confirm against a real
// delivery before trusting it in production.
func FromPayPalWebhook(raw []byte, origin model.Origin) (model.EventRecord, error) {
	var env paypalWebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal webhook envelope: %w", err)
	}
	if env.ID == "" {
		return model.EventRecord{}, errors.New("paypal webhook: delivery has no id")
	}

	if !paypalWhitelistedTopics[env.EventType] {
		return paypalIgnoredRecord(env, origin)
	}

	switch env.EventType {
	case "PAYMENT.CAPTURE.COMPLETED", "PAYMENT.CAPTURE.DENIED":
		return fromPayPalCaptureResource(env, origin)
	case "PAYMENT.CAPTURE.REFUNDED", "PAYMENT.CAPTURE.REVERSED":
		return fromPayPalRefundResource(env, origin)
	default:
		// Unreachable given paypalWhitelistedTopics above. Kept as its own
		// branch rather than folded into that check so a future topic added to
		// the whitelist without a case here fails loudly at runtime instead of
		// silently falling through to "ignored".
		return model.EventRecord{}, fmt.Errorf(
			"paypal webhook: topic %q is whitelisted but has no handler", env.EventType)
	}
}

func fromPayPalCaptureResource(env paypalWebhookEnvelope, origin model.Origin) (model.EventRecord, error) {
	var r paypalCaptureResource
	if err := json.Unmarshal(env.Resource, &r); err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal capture resource: %w", err)
	}
	if r.ID == "" {
		return model.EventRecord{}, errors.New("paypal capture resource has no id")
	}

	gross := parseAmount(r.Amount)
	// Webhook fee is a positive magnitude on both charge and refund
	// (CLAUDE.md's field mapping table); Transaction Search's convention —
	// which the rest of this service is normalized to — is negative on a
	// charge. Negate here, once, so net = gross + fee holds identically
	// regardless of which path ingested the row.
	fee := -parseAmount(r.SellerReceivableBreakdown.PayPalFee)

	currency := ""
	if r.Amount != nil {
		currency = r.Amount.CurrencyCode
	}

	// A capture is the charge itself, not a refund referencing one — no
	// paypal_reference_id, same as Transaction Search leaves it empty for a
	// plain charge.
	return buildPayPalRecord(env, r.ID, r.Status, gross, fee, currency, "", r.CreateTime, origin)
}

func fromPayPalRefundResource(env paypalWebhookEnvelope, origin model.Origin) (model.EventRecord, error) {
	var r paypalRefundResource
	if err := json.Unmarshal(env.Resource, &r); err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal refund resource: %w", err)
	}
	if r.ID == "" {
		return model.EventRecord{}, errors.New("paypal refund resource has no id")
	}

	// Refunds move money back to the buyer. The webhook gives a positive
	// magnitude; Transaction Search's convention is negative gross on a
	// refund, so negate.
	gross := -parseAmount(r.Amount)
	// Fee here is ALREADY the sign Transaction Search uses (positive on a
	// refund) per the same table — no negation, unlike the capture case above.
	fee := parseAmount(r.SellerPayableBreakdown.PayPalFee)

	currency := ""
	if r.Amount != nil {
		currency = r.Amount.CurrencyCode
	}

	// Never guess an unresolvable reference (CLAUDE.md, Data sources): an
	// empty string here is exactly what tells recon.Classify to mark the row
	// ReconPending instead of silently dropping it — see classify.go's
	// `IsRefund() && PayPalReferenceID == ""` check.
	ref := paypalParentCaptureID(r.Links)

	return buildPayPalRecord(env, r.ID, r.Status, gross, fee, currency, ref, r.CreateTime, origin)
}

// paypalParentCaptureID extracts the parent capture's id from the refund
// resource's "up" link (CLAUDE.md: "derive from links[rel=\"up\"]"), e.g.
// ".../v2/payments/captures/3C679366HD394342E" -> "3C679366HD394342E".
func paypalParentCaptureID(links []paypalLink) string {
	for _, l := range links {
		if l.Rel != "up" {
			continue
		}
		href := strings.TrimRight(l.Href, "/")
		if idx := strings.LastIndex(href, "/"); idx >= 0 && idx+1 < len(href) {
			return href[idx+1:]
		}
	}
	return ""
}

// buildPayPalRecord assembles the SAME normalized shape FromPayPalSearch
// produces (txn_id/date/gross/fee/net/status/paypal_reference_id/currency),
// so a row read back by DecodeResources is indistinguishable regardless of
// which path ingested it.
func buildPayPalRecord(
	env paypalWebhookEnvelope, resourceID, status string,
	gross, fee float64, currency, paypalReferenceID, createTime string,
	origin model.Origin,
) (model.EventRecord, error) {
	normalized := map[string]any{
		"txn_id":              resourceID,
		"date":                dateKey(createTime),
		"gross":               gross,
		"fee":                 fee,
		"net":                 gross + fee,
		"status":              status,
		"paypal_reference_id": paypalReferenceID,
		"currency":            currency,
	}

	blob, err := json.Marshal(normalized)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal %s: %w", resourceID, err)
	}

	payload, err := Sanitize(model.ResourcePayPalTransaction, blob)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal %s: %w", resourceID, err)
	}

	return model.EventRecord{
		Source:                model.SourcePayPal,
		ResourceType:          model.ResourcePayPalTransaction,
		ResourceID:            resourceID,
		WebhookNotificationID: env.ID,
		Origin:                origin,
		Topic:                 env.EventType,
		Status:                "received",
		Payload:               payload,
		OccurredAt:            payPalDate(dateKey(createTime)),
	}, nil
}

// paypalIgnoredRecord stores a non-whitelisted delivery for the audit trail
// without reconciling it — mirrors ResourceOther for Ticket Tailor's
// non-whitelisted topics (e.g. waitlist_signup).
func paypalIgnoredRecord(env paypalWebhookEnvelope, origin model.Origin) (model.EventRecord, error) {
	// Not every PayPal resource type keys itself "id" at the top level (e.g.
	// disputes use dispute_id). This event is never reconciled regardless, so
	// falling back to the delivery id keeps every non-whitelisted topic
	// storable rather than rejected outright.
	var generic struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(env.Resource, &generic)
	resourceID := generic.ID
	if resourceID == "" {
		resourceID = env.ID
	}

	// Sanitize has no allowlist entry for ResourceOther, so this returns "{}"
	// via its fail-closed default — the same behavior Ticket Tailor's ignored
	// resources get.
	payload, err := Sanitize(model.ResourceOther, env.Resource)
	if err != nil {
		return model.EventRecord{}, fmt.Errorf("paypal %s: %w", env.EventType, err)
	}

	return model.EventRecord{
		Source:                model.SourcePayPal,
		ResourceType:          model.ResourceOther,
		ResourceID:            resourceID,
		WebhookNotificationID: env.ID,
		Origin:                origin,
		Topic:                 env.EventType,
		Status:                "ignored",
		Payload:               payload,
		OccurredAt:            payPalDate(dateKey(env.CreateTime)),
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
