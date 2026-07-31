// Package ingest turns a raw provider payload into a storable EventRecord.
//
// It is the single choke point both ingestion paths pass through — the backfill
// importer today, webhook handlers in P4 — so anything that must be true of
// every stored row is enforced here exactly once.
//
// The most important of those is that buyer PII never reaches the database.
package ingest

import (
	"encoding/json"
	"fmt"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// fieldSet declares what survives sanitization for one resource type.
//
// This is an ALLOWLIST, and the direction matters more than the contents. A
// denylist fails open: the day Ticket Tailor adds a field, or a buyer types
// their address into a custom question, it lands in jsonb and stays there. An
// allowlist fails closed — an unknown field is dropped, and the cost of a
// genuinely-needed new field is a one-line addition that announces itself
// immediately as a loud reconciliation break rather than a silent leak.
type fieldSet struct {
	// scalars are copied verbatim as raw bytes.
	scalars map[string]bool
	// objects are recursed into with a nested set.
	objects map[string]*fieldSet
	// arrays are recursed into element-wise with a nested set.
	arrays map[string]*fieldSet
}

func fields(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// currencyObject is Ticket Tailor's money-unit descriptor. It appears under
// different keys on different resources, which is why it is shared rather than
// repeated.
var currencyObject = &fieldSet{scalars: fields("code", "base_multiplier")}

// ttDateObject is Ticket Tailor's nested date. `unix` is what the port reads;
// the rest is kept because it is small and makes a stored row diagnosable.
var ttDateObject = &fieldSet{
	scalars: fields("unix", "iso", "date", "time", "timezone", "formatted"),
}

// allowed maps a resource type to the fields that may be stored.
//
// Every omission below is deliberate. The ones that are PII rather than merely
// unnecessary are called out, because the two reasons should not get conflated
// by someone later deciding a field looks harmless.
var allowed = map[model.ResourceType]*fieldSet{

	// Dropped as PII: buyer_details (email, name, phone, address, free-text
	// custom questions), and issued_tickets — an order embeds a full copy of
	// each of its tickets, and all 2,562 of those copies carry an email. That
	// nesting is precisely why this is an allowlist: a top-level denylist would
	// have missed every one of them.
	//
	// Dropped as unnecessary: meta_data, notes, marketing_opt_in, referral_tag,
	// sold_products, refunded_voucher_id, line_items, event_summary.
	// Also dropped as PII, and only found by sweeping the real data:
	// status_message. It is operator-authored FREE TEXT, and in production it
	// contains entries like "Exchange other show (by <staff name> on <date>)".
	// It looked like a harmless status field, which is the whole argument for
	// checking sanitizer output by value and not only by key name. The engine
	// never reads it.
	//
	// payment_method.external_id is dropped too: it identifies a PayPal account
	// and nothing in the port reads it.
	model.ResourceOrder: {
		scalars: fields(
			"object", "id", "txn_id", "status", "created_at",
			"refund_amount", "total", "total_paid", "subtotal", "tax",
			"tax_treatment", "credited_out_amount",
		),
		objects: map[string]*fieldSet{
			"currency": currencyObject,
			// type is what the matching rule compares against "paypal" and
			// "operator"; id makes a mismatch diagnosable.
			"payment_method": {scalars: fields("type", "id")},
		},
	},

	// Dropped as PII: email, first_name, last_name, full_name, and
	// custom_questions (free text — a buyer can put anything in it).
	//
	// Dropped as bearer credentials rather than PII: barcode, barcode_url,
	// qr_code_url, group_ticket_barcode, reference, reservation. These admit
	// someone to a performance. Not personal data, but equally not ours to keep.
	model.ResourceIssuedTicket: {
		scalars: fields(
			"object", "id", "order_id", "event_id", "event_series_id",
			"ticket_type_id", "add_on_id", "description", "status",
			"listed_price", "created_at", "updated_at", "voided_at",
			"checked_in", "source",
		),
		objects: map[string]*fieldSet{"listed_currency": currencyObject},
	},

	// No PII on an event. Everything dropped here is bulk: images, description,
	// checkout_url, chk, access_code, waitlist_*, ticket_types, venue.
	model.ResourceEvent: {
		scalars: fields(
			"object", "id", "event_series_id", "name", "status", "timezone",
			"currency", "created_at",
		),
		objects: map[string]*fieldSet{"start": ttDateObject, "end": ttDateObject},
	},

	model.ResourceEventSeries: {
		scalars: fields("object", "id", "name"),
		objects: map[string]*fieldSet{
			"tickets_available_at":   ttDateObject,
			"tickets_unavailable_at": ttDateObject,
		},
	},

	// Two vocabularies in one set: the raw Transaction Search `transaction_info`
	// shape, and the dashboard's already-normalized cache shape that
	// --from-snapshot replays. Supporting both keeps one allowlist rather than
	// branching on which producer supplied the row.
	//
	// Dropped: transaction_subject and invoice_id, neither of which the engine
	// reads and both of which can carry buyer-supplied text; payer_info, which
	// is not requested today but must stay excluded if anyone widens `fields`.
	model.ResourcePayPalTransaction: {
		scalars: fields(
			// Transaction Search
			"transaction_id", "transaction_initiation_date", "transaction_status",
			// dashboard-normalized
			"txn_id", "date", "gross", "fee", "net", "status",
			// shared
			"paypal_reference_id", "currency",
		),
		objects: map[string]*fieldSet{
			"transaction_amount": {scalars: fields("value", "currency_code")},
			"fee_amount":         {scalars: fields("value", "currency_code")},
		},
	},
}

// Sanitize reduces a payload to the fields allowed for its resource type.
//
// Values are copied as raw bytes rather than decoded and re-encoded. That is
// deliberate: re-encoding a JSON number can change its text ("1e2" becomes
// "100", trailing zeros move), and the parity harness depends on money values
// surviving storage bit-for-bit. Keeping them verbatim also means PayPal's
// decimal-string amounts stay strings, satisfying CLAUDE.md's "never a JSON
// float" rule with no conversion at all.
func Sanitize(rt model.ResourceType, raw []byte) ([]byte, error) {
	spec, ok := allowed[rt]
	if !ok {
		// An unknown resource type has no allowlist, so nothing can be shown to
		// be safe. Storing an empty object is the only fail-closed answer.
		return []byte(`{}`), nil
	}
	return sanitizeObject(spec, raw)
}

func sanitizeObject(spec *fieldSet, raw []byte) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("sanitize: payload is not a JSON object: %w", err)
	}

	out := make(map[string]json.RawMessage, len(spec.scalars))

	for k, v := range in {
		switch {
		case spec.scalars[k]:
			out[k] = v

		case spec.objects[k] != nil:
			// A null where an object was expected is normal, not an error.
			if isNull(v) {
				out[k] = v
				continue
			}
			nested, err := sanitizeObject(spec.objects[k], v)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", k, err)
			}
			out[k] = nested

		case spec.arrays[k] != nil:
			if isNull(v) {
				out[k] = v
				continue
			}
			nested, err := sanitizeArray(spec.arrays[k], v)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", k, err)
			}
			out[k] = nested
		}
		// Anything not named above is dropped. Silently, and on purpose.
	}

	// Marshalling a map sorts keys, so output is deterministic — which the
	// golden tests and the stability of payload_hash both rely on.
	return json.Marshal(out)
}

func sanitizeArray(spec *fieldSet, raw []byte) ([]byte, error) {
	var in []json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("sanitize: expected an array: %w", err)
	}

	out := make([]json.RawMessage, 0, len(in))
	for i, el := range in {
		nested, err := sanitizeObject(spec, el)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out = append(out, nested)
	}
	return json.Marshal(out)
}

func isNull(v json.RawMessage) bool {
	return len(v) == 4 && string(v) == "null"
}

// ScanForPII re-exports the storage-boundary check so callers of this package
// do not need to reach into model for it. The rule itself lives in model,
// because it constrains what may be persisted rather than how it is ingested.
func ScanForPII(raw []byte, exempt map[string]bool) error {
	return model.ScanForPII(raw, exempt)
}

// PIIExemptions re-exports model.PIIExemptions.
func PIIExemptions(rt model.ResourceType) map[string]bool {
	return model.PIIExemptions(rt)
}
