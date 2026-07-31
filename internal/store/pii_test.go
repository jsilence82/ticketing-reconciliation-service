package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// Three layers guard against buyer PII reaching the database: the allowlist in
// internal/ingest, the rescan at this write boundary, and a CHECK constraint in
// V2. Each is tested here for whether it actually rejects, because a guard that
// has never been seen to fire is indistinguishable from no guard at all.

func TestUpsertRejectsPIIAtTheWriteBoundary(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	tests := []struct {
		name    string
		rt      model.ResourceType
		payload string
	}{
		{"top-level email", model.ResourceIssuedTicket,
			`{"id":"it_1","email":"ada@example.invalid"}`},
		{"nested buyer details", model.ResourceOrder,
			`{"id":"or_1","buyer_details":{"email":"ada@example.invalid"}}`},
		{"email inside an array", model.ResourceOrder,
			`{"id":"or_1","issued_tickets":[{"email":"ada@example.invalid"}]}`},
		{"bearer credential", model.ResourceIssuedTicket,
			`{"id":"it_1","barcode":"BAR-1"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := rec("R-"+tc.name, withType(tc.rt), withSource(model.SourceTicketTailor),
				withPayload(tc.payload))

			_, err := s.Upsert(c, r)
			if err == nil {
				t.Fatal("the store accepted a payload carrying PII; the write-boundary " +
					"rescan is not firing")
			}
			if !strings.Contains(err.Error(), "refusing to store") {
				t.Errorf("error = %v; want the PII refusal", err)
			}
		})
	}
}

// A resource that legitimately carries a name must still be storable, or the
// guard would block ordinary operation.
func TestUpsertAllowsExemptedName(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	r := rec("ev_1",
		withSource(model.SourceTicketTailor),
		withType(model.ResourceEvent),
		withPayload(`{"id":"ev_1","name":"God of Carnage"}`))

	if _, err := s.Upsert(c, r); err != nil {
		t.Fatalf("an event's show name was rejected: %v", err)
	}
}

// The V2 CHECK constraint is the layer that holds regardless of what code does
// the writing. Exercise it directly, bypassing the Go guard entirely — that is
// the scenario it exists for.
func TestDatabaseConstraintRejectsPII(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	_, err := s.Pool().Exec(c, `
		INSERT INTO events (source, resource_type, resource_id, origin, status,
		                    payload, payload_hash, occurred_at)
		VALUES ('tickettailor','issued_ticket','it_raw','backfill','received',
		        $1::jsonb, '\x00'::bytea, now())`,
		`{"id":"it_raw","email":"ada@example.invalid"}`)

	if err == nil {
		t.Fatal("a direct INSERT carrying a top-level email was accepted; " +
			"the V2 CHECK constraint is missing or ineffective")
	}
	if !strings.Contains(err.Error(), "events_payload_no_pii") {
		t.Errorf("rejected, but not by the PII constraint: %v", err)
	}
}

// Sanitized payloads must still pass every layer, or the guards would block the
// normal path.
func TestSanitizedPayloadIsStorable(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	clean := map[string]any{
		"object": "issued_ticket", "id": "it_1", "order_id": "or_1",
		"event_id": "ev_1", "description": "Admission - Student",
		"status": "valid", "listed_price": 1000,
	}
	blob, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Upsert(c, rec("it_1",
		withSource(model.SourceTicketTailor),
		withType(model.ResourceIssuedTicket),
		withPayload(string(blob))))
	if err != nil {
		t.Fatalf("a sanitized ticket was rejected: %v", err)
	}
	if got != Inserted {
		t.Errorf("outcome = %q, want inserted", got)
	}
}
