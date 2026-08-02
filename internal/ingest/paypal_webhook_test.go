package ingest_test

import (
	"encoding/json"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// normalizedPayPalPayload mirrors the shape buildPayPalRecord writes, so tests
// can assert on the sign conventions directly rather than by string search.
type normalizedPayPalPayload struct {
	TxnID             string  `json:"txn_id"`
	Date              string  `json:"date"`
	Gross             float64 `json:"gross"`
	Fee               float64 `json:"fee"`
	Net               float64 `json:"net"`
	Status            string  `json:"status"`
	PayPalReferenceID string  `json:"paypal_reference_id"`
	Currency          string  `json:"currency"`
}

func decodePayPalPayload(t *testing.T, rec model.EventRecord) normalizedPayPalPayload {
	t.Helper()
	var p normalizedPayPalPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatalf("payload did not decode: %v\npayload: %s", err, rec.Payload)
	}
	return p
}

const paypalCaptureCompleted = `{
	"id": "WH-2WR32451HC0233532-67976317FL4543714",
	"event_type": "PAYMENT.CAPTURE.COMPLETED",
	"resource_type": "capture",
	"create_time": "2026-08-01T12:00:00.000Z",
	"resource": {
		"id": "3C679366HD394342E",
		"status": "COMPLETED",
		"amount": {"value": "10.00", "currency_code": "USD"},
		"seller_receivable_breakdown": {
			"gross_amount": {"value": "10.00", "currency_code": "USD"},
			"paypal_fee": {"value": "0.59", "currency_code": "USD"},
			"net_amount": {"value": "9.41", "currency_code": "USD"}
		},
		"create_time": "2026-08-01T11:59:50.000Z",
		"links": [
			{"href": "https://api.sandbox.paypal.com/v2/payments/captures/3C679366HD394342E", "rel": "self"},
			{"href": "https://api.sandbox.paypal.com/v2/checkout/orders/5O190127TN364715T", "rel": "up"}
		]
	}
}`

func TestFromPayPalWebhook_CaptureCompleted(t *testing.T) {
	rec, err := ingest.FromPayPalWebhook([]byte(paypalCaptureCompleted), model.OriginWebhook)
	if err != nil {
		t.Fatalf("FromPayPalWebhook: %v", err)
	}

	if rec.ResourceType != model.ResourcePayPalTransaction {
		t.Errorf("ResourceType = %q, want paypal_transaction", rec.ResourceType)
	}
	if rec.ResourceID != "3C679366HD394342E" {
		t.Errorf("ResourceID = %q, want 3C679366HD394342E", rec.ResourceID)
	}
	if rec.WebhookNotificationID != "WH-2WR32451HC0233532-67976317FL4543714" {
		t.Errorf("WebhookNotificationID = %q", rec.WebhookNotificationID)
	}
	if rec.Topic != "PAYMENT.CAPTURE.COMPLETED" {
		t.Errorf("Topic = %q", rec.Topic)
	}
	if rec.Status != "received" {
		t.Errorf("Status = %q, want received", rec.Status)
	}

	p := decodePayPalPayload(t, rec)

	// The sign-convention assertions: this is the highest-risk conversion in
	// the project. Webhook gives a positive fee magnitude; the stored
	// convention (matching Transaction Search) is negative on a charge.
	if p.Gross != 10.00 {
		t.Errorf("Gross = %v, want 10.00 (positive on a charge)", p.Gross)
	}
	if p.Fee != -0.59 {
		t.Errorf("Fee = %v, want -0.59 (negated from the webhook's positive magnitude)", p.Fee)
	}
	if p.Net != p.Gross+p.Fee {
		t.Errorf("Net = %v, want Gross+Fee = %v", p.Net, p.Gross+p.Fee)
	}
	if p.PayPalReferenceID != "" {
		t.Errorf("PayPalReferenceID = %q, want empty for a plain charge", p.PayPalReferenceID)
	}
	if p.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", p.Currency)
	}
	if p.Date != "2026-08-01" {
		t.Errorf("Date = %q, want 2026-08-01 (from the resource's own create_time)", p.Date)
	}
}

const paypalCaptureRefunded = `{
	"id": "WH-58D32451HC0233532-1JU5505970079194P",
	"event_type": "PAYMENT.CAPTURE.REFUNDED",
	"resource_type": "refund",
	"create_time": "2026-08-02T09:00:00.000Z",
	"resource": {
		"id": "3TY19547JX188154X",
		"status": "COMPLETED",
		"amount": {"value": "10.00", "currency_code": "USD"},
		"seller_payable_breakdown": {
			"gross_amount": {"value": "10.00", "currency_code": "USD"},
			"paypal_fee": {"value": "0.59", "currency_code": "USD"},
			"net_amount": {"value": "9.41", "currency_code": "USD"},
			"total_refunded_amount": {"value": "10.00", "currency_code": "USD"}
		},
		"create_time": "2026-08-02T08:59:59.000Z",
		"links": [
			{"href": "https://api.sandbox.paypal.com/v2/payments/captures/3C679366HD394342E", "rel": "up"},
			{"href": "https://api.sandbox.paypal.com/v2/payments/refunds/3TY19547JX188154X", "rel": "self"}
		]
	}
}`

func TestFromPayPalWebhook_CaptureRefunded(t *testing.T) {
	rec, err := ingest.FromPayPalWebhook([]byte(paypalCaptureRefunded), model.OriginWebhook)
	if err != nil {
		t.Fatalf("FromPayPalWebhook: %v", err)
	}

	if rec.ResourceID != "3TY19547JX188154X" {
		t.Errorf("ResourceID = %q, want the refund's OWN id, distinct from the capture", rec.ResourceID)
	}

	p := decodePayPalPayload(t, rec)

	if p.Gross != -10.00 {
		t.Errorf("Gross = %v, want -10.00 (negative on a refund)", p.Gross)
	}
	if p.Fee != 0.59 {
		t.Errorf("Fee = %v, want 0.59 (already the target sign, no negation on a refund)", p.Fee)
	}
	if p.Net != p.Gross+p.Fee {
		t.Errorf("Net = %v, want Gross+Fee = %v", p.Net, p.Gross+p.Fee)
	}
	if p.PayPalReferenceID != "3C679366HD394342E" {
		t.Errorf("PayPalReferenceID = %q, want the parent capture id derived from the \"up\" link", p.PayPalReferenceID)
	}
}

func TestFromPayPalWebhook_RefundWithoutUpLinkStaysUnresolved(t *testing.T) {
	// Never guess an unresolvable reference. The classifier
	// (internal/recon/classify.go) is what turns an empty reference on a
	// refund into ReconPending — this test only proves the normalizer does
	// not fabricate one when the "up" link is missing.
	const body = `{
		"id": "WH-no-up-link",
		"event_type": "PAYMENT.CAPTURE.REFUNDED",
		"resource_type": "refund",
		"create_time": "2026-08-02T09:00:00.000Z",
		"resource": {
			"id": "3TY19547JX188154X",
			"status": "COMPLETED",
			"amount": {"value": "10.00", "currency_code": "USD"},
			"seller_payable_breakdown": {"paypal_fee": {"value": "0.59", "currency_code": "USD"}},
			"create_time": "2026-08-02T08:59:59.000Z",
			"links": [{"href": "https://api.sandbox.paypal.com/v2/payments/refunds/3TY19547JX188154X", "rel": "self"}]
		}
	}`

	rec, err := ingest.FromPayPalWebhook([]byte(body), model.OriginWebhook)
	if err != nil {
		t.Fatalf("FromPayPalWebhook: %v", err)
	}

	p := decodePayPalPayload(t, rec)
	if p.PayPalReferenceID != "" {
		t.Errorf("PayPalReferenceID = %q, want empty rather than guessed", p.PayPalReferenceID)
	}
}

func TestFromPayPalWebhook_NonWhitelistedTopicIsIgnored(t *testing.T) {
	const body = `{
		"id": "WH-dispute-1",
		"event_type": "CUSTOMER.DISPUTE.CREATED",
		"resource_type": "dispute",
		"create_time": "2026-08-03T00:00:00.000Z",
		"resource": {"dispute_id": "PP-D-12345", "reason": "MERCHANDISE_OR_SERVICE_NOT_RECEIVED"}
	}`

	rec, err := ingest.FromPayPalWebhook([]byte(body), model.OriginWebhook)
	if err != nil {
		t.Fatalf("FromPayPalWebhook: %v", err)
	}

	if rec.ResourceType != model.ResourceOther {
		t.Errorf("ResourceType = %q, want other", rec.ResourceType)
	}
	if rec.Status != "ignored" {
		t.Errorf("Status = %q, want ignored", rec.Status)
	}
	// No top-level "id" on a dispute resource (it uses dispute_id), so this
	// must fall back to the delivery id rather than failing to store at all.
	if rec.ResourceID != "WH-dispute-1" {
		t.Errorf("ResourceID = %q, want fallback to the delivery id", rec.ResourceID)
	}
	if string(rec.Payload) != "{}" {
		t.Errorf("Payload = %s, want {} (no allowlist for ResourceOther)", rec.Payload)
	}
}

func TestFromPayPalWebhook_MissingEnvelopeID(t *testing.T) {
	const body = `{"event_type": "PAYMENT.CAPTURE.COMPLETED", "resource": {}}`
	if _, err := ingest.FromPayPalWebhook([]byte(body), model.OriginWebhook); err == nil {
		t.Fatal("want error for missing delivery id, got nil")
	}
}

func TestFromPayPalWebhook_MissingResourceID(t *testing.T) {
	const body = `{
		"id": "WH-x",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {"amount": {"value": "1.00", "currency_code": "USD"}}
	}`
	if _, err := ingest.FromPayPalWebhook([]byte(body), model.OriginWebhook); err == nil {
		t.Fatal("want error for missing resource id, got nil")
	}
}
