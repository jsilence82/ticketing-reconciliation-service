package ingest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// Fixtures are invented, never real account data — a PII test whose fixture
// contains real PII would be self-defeating anyway.

const rawOrder = `{
  "object": "order",
  "id": "or_1",
  "txn_id": "TX-ABC",
  "status": "completed",
  "created_at": 1710000000,
  "refund_amount": 1300,
  "total_paid": 2000,
  "currency": {"code": "eur", "base_multiplier": 100},
  "payment_method": {"type": "paypal", "id": "pm_1", "external_id": "EXT1"},
  "buyer_details": {
    "email": "ada@example.invalid",
    "first_name": "Ada",
    "last_name": "Lovelace",
    "name": "Ada Lovelace",
    "phone": "+44 7700 900000",
    "address": {"line_1": "14 Foo Street", "postcode": "AB1 2CD"},
    "custom_questions": [{"question": "Dietary needs?", "answer": "None"}]
  },
  "issued_tickets": [
    {"id": "it_1", "email": "ada@example.invalid", "first_name": "Ada",
     "barcode": "BAR1", "listed_price": 1000}
  ],
  "notes": "called about wheelchair access",
  "meta_data": {"utm": "newsletter"},
  "marketing_opt_in": true
}`

const rawTicket = `{
  "object": "issued_ticket",
  "id": "it_1",
  "order_id": "or_1",
  "event_id": "ev_1",
  "event_series_id": "es_1",
  "ticket_type_id": "tt_1",
  "description": "Admission - Student",
  "status": "valid",
  "listed_price": 1000,
  "listed_currency": {"code": "eur", "base_multiplier": 100},
  "created_at": 1710000000,
  "updated_at": 1710000001,
  "email": "grace@example.invalid",
  "first_name": "Grace",
  "last_name": "Hopper",
  "full_name": "Grace Hopper",
  "custom_questions": [{"question": "Address?", "answer": "1 Navy Yard"}],
  "barcode": "BAR-XYZ",
  "barcode_url": "https://example.invalid/BAR-XYZ",
  "qr_code_url": "https://example.invalid/qr/BAR-XYZ",
  "group_ticket_barcode": "GRP-1",
  "reference": "REF-1",
  "reservation": {"id": "res_1"}
}`

// decode is used so assertions are about structure, not byte order.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return m
}

func TestSanitizeOrder(t *testing.T) {
	got, err := Sanitize(model.ResourceOrder, []byte(rawOrder))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	m := decode(t, got)

	// Kept: everything the matching rule and Assemble read.
	for _, k := range []string{
		"object", "id", "txn_id", "status", "created_at",
		"refund_amount", "total_paid", "currency", "payment_method",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("dropped %q, which the reconciliation reads", k)
		}
	}

	// Dropped: the entire buyer subtree, and the embedded ticket copies.
	for _, k := range []string{
		"buyer_details", "issued_tickets", "notes", "meta_data", "marketing_opt_in",
	} {
		if _, ok := m[k]; ok {
			t.Errorf("kept %q, which must be stripped", k)
		}
	}

	// payment_method keeps only what is needed; its other keys go.
	pm, _ := m["payment_method"].(map[string]any)
	if pm["type"] != "paypal" {
		t.Errorf("payment_method.type = %v, want paypal", pm["type"])
	}
	if _, ok := pm["instructions"]; ok {
		t.Error("payment_method kept an unlisted field")
	}
}

func TestSanitizeTicket(t *testing.T) {
	got, err := Sanitize(model.ResourceIssuedTicket, []byte(rawTicket))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	m := decode(t, got)

	for _, k := range []string{
		"id", "order_id", "event_id", "description", "status",
		"listed_price", "listed_currency", "created_at", "updated_at",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("dropped %q, which the reconciliation reads", k)
		}
	}

	// PII, and bearer credentials, both go.
	for _, k := range []string{
		"email", "first_name", "last_name", "full_name", "custom_questions",
		"barcode", "barcode_url", "qr_code_url", "group_ticket_barcode",
		"reference", "reservation",
	} {
		if _, ok := m[k]; ok {
			t.Errorf("kept %q, which must be stripped", k)
		}
	}
}

// The allowlist's whole purpose: a field nobody has heard of is dropped, not
// passed through pending someone noticing.
func TestSanitizeDropsUnknownFields(t *testing.T) {
	raw := `{"object":"issued_ticket","id":"it_1",
	         "brand_new_field":"whatever the provider added last night",
	         "nested_surprise":{"email":"eve@example.invalid"}}`

	got, err := Sanitize(model.ResourceIssuedTicket, []byte(raw))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	m := decode(t, got)

	if _, ok := m["brand_new_field"]; ok {
		t.Error("an unknown field survived; the allowlist is behaving like a denylist")
	}
	if _, ok := m["nested_surprise"]; ok {
		t.Error("an unknown nested object survived, taking an email with it")
	}
}

// An unknown resource type has no allowlist, so nothing about it can be shown
// to be safe.
func TestSanitizeUnknownResourceTypeFailsClosed(t *testing.T) {
	got, err := Sanitize(model.ResourceType("something_new"), []byte(`{"email":"x@y.invalid"}`))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("got %s, want {} — an unknown type must fail closed", got)
	}
}

// Money must survive byte-for-byte. Re-encoding a JSON number can change its
// text, and the parity harness depends on these values round-tripping exactly.
func TestSanitizePreservesNumericText(t *testing.T) {
	raw := `{"txn_id":"TX1","gross":23.0,"fee":-1.08,"net":21.92,
	         "transaction_amount":{"value":"23.00","currency_code":"EUR"}}`

	got, err := Sanitize(model.ResourcePayPalTransaction, []byte(raw))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}

	for _, want := range []string{`"gross":23.0`, `"fee":-1.08`, `"net":21.92`, `"value":"23.00"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("output lost the exact text %s\n  got: %s", want, got)
		}
	}
}

func TestSanitizeIsDeterministic(t *testing.T) {
	first, err := Sanitize(model.ResourceOrder, []byte(rawOrder))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Sanitize(model.ResourceOrder, []byte(rawOrder))
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(again) {
			t.Fatalf("run %d differs; payload_hash would be unstable\n  %s\n  %s", i, first, again)
		}
	}
}

func TestSanitizeHandlesNullObjects(t *testing.T) {
	raw := `{"object":"issued_ticket","id":"it_1","listed_currency":null}`
	if _, err := Sanitize(model.ResourceIssuedTicket, []byte(raw)); err != nil {
		t.Errorf("a null where an object was expected should be fine, got: %v", err)
	}
}

func TestSanitizeRejectsNonObject(t *testing.T) {
	if _, err := Sanitize(model.ResourceOrder, []byte(`["not","an","object"]`)); err == nil {
		t.Error("a non-object payload was accepted")
	}
}

// --- the independent scanner -------------------------------------------------

func TestScanForPII(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		exempt  map[string]bool
		wantErr bool
	}{
		{"clean", `{"id":"it_1","listed_price":1000}`, nil, false},
		{"top-level email", `{"id":"it_1","email":"x@y.invalid"}`, nil, true},
		{"nested email", `{"id":"or_1","buyer":{"email":"x@y.invalid"}}`, nil, true},
		{"email inside an array", `{"tickets":[{"id":"it_1"},{"email":"x@y.invalid"}]}`, nil, true},
		{"deeply nested", `{"a":{"b":{"c":{"phone":"123"}}}}`, nil, true},
		{"barcode is a bearer credential", `{"id":"it_1","barcode":"B1"}`, nil, true},
		{"name is exempt for an event", `{"id":"ev_1","name":"Carmilla"}`,
			map[string]bool{"name": true}, false},
		{"name is NOT exempt by default", `{"id":"x","name":"Ada Lovelace"}`, nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ScanForPII([]byte(tc.payload), tc.exempt)
			if tc.wantErr && err == nil {
				t.Error("expected the scan to reject this payload")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected rejection: %v", err)
			}
		})
	}
}

// The two layers must agree: anything Sanitize emits must pass the independent
// scanner. If they ever disagree, one of them is wrong and it is better to find
// out here than at the storage boundary.
func TestSanitizedOutputPassesTheScanner(t *testing.T) {
	cases := []struct {
		rt  model.ResourceType
		raw string
	}{
		{model.ResourceOrder, rawOrder},
		{model.ResourceIssuedTicket, rawTicket},
	}

	for _, tc := range cases {
		t.Run(string(tc.rt), func(t *testing.T) {
			out, err := Sanitize(tc.rt, []byte(tc.raw))
			if err != nil {
				t.Fatalf("Sanitize: %v", err)
			}
			if err := ScanForPII(out, PIIExemptions(tc.rt)); err != nil {
				t.Errorf("sanitized output still fails the scanner: %v\n  %s", err, out)
			}
		})
	}
}

// And the converse: the scanner must actually reject the raw input, or the
// previous test proves nothing.
func TestScannerRejectsRawInput(t *testing.T) {
	if err := ScanForPII([]byte(rawOrder), PIIExemptions(model.ResourceOrder)); err == nil {
		t.Error("the scanner accepted a raw order full of buyer details; " +
			"TestSanitizedOutputPassesTheScanner would then be vacuous")
	}
	if err := ScanForPII([]byte(rawTicket), PIIExemptions(model.ResourceIssuedTicket)); err == nil {
		t.Error("the scanner accepted a raw ticket carrying an email")
	}
}
