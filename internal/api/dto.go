package api

import (
	"encoding/json"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/money"
)

// eventDTO is one row as served.
//
// The payload is passed through VERBATIM as raw JSON. Re-encoding it would
// reformat numbers, and consumers do their own arithmetic over these values —
// the whole point of this service not producing reports.
type eventDTO struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Origin       string `json:"origin"`
	Topic        string `json:"topic,omitempty"`
	Status       string `json:"status"`

	ReconStatus        string `json:"recon_status"`
	ReconCounterpartID string `json:"recon_counterpart_id,omitempty"`

	OccurredAt time.Time `json:"occurred_at"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// Amounts is present only for PayPal transactions. See the comment on
	// amountsDTO for why it exists alongside the raw payload.
	Amounts *amountsDTO `json:"amounts,omitempty"`

	Payload json.RawMessage `json:"payload"`
}

// amountsDTO carries money as decimal strings plus minor-unit integers plus
// a currency code — never a JSON float, which every consumer's parser would
// round-trip unpredictably.
//
// The raw payload is still served unchanged, and for PayPal rows it contains
// those same amounts as JSON numbers. That is a genuine tension: the payload is
// provider data and rewriting it would break the "raw event data" contract, but
// a consumer that parses `23.0` into a float and sums a few hundred of them is
// exactly the arithmetic this service spent P1 getting right.
//
// So both are served. `amounts` is the safe path for anything doing sums;
// `payload` stays authoritative for everything else. A consumer that uses
// `minor` never touches a float at all.
type amountsDTO struct {
	Currency string     `json:"currency,omitempty"`
	Gross    amountJSON `json:"gross"`
	Fee      amountJSON `json:"fee"`
	Net      amountJSON `json:"net"`
}

// amountJSON is one money value in both representations.
type amountJSON struct {
	// Decimal is the human-readable form, as a STRING. A JSON number here would
	// be re-parsed as a float by every consumer, unpredictably.
	Decimal string `json:"decimal"`
	// Minor is the exact integer value in cents — the representation to compute
	// with.
	Minor int64 `json:"minor"`
}

func amount(v float64) amountJSON {
	m := money.FromFloat(v)
	return amountJSON{Decimal: m.String(), Minor: int64(m)}
}

// payPalAmounts extracts the money fields from a stored PayPal payload.
//
// Returns nil for any other resource type, and for a PayPal row whose payload
// somehow lacks them — an absent block is honest, an invented zero is not.
func payPalAmounts(rt model.ResourceType, payload []byte) *amountsDTO {
	if rt != model.ResourcePayPalTransaction {
		return nil
	}

	var p struct {
		Gross    *float64 `json:"gross"`
		Fee      *float64 `json:"fee"`
		Net      *float64 `json:"net"`
		Currency string   `json:"currency"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.Gross == nil || p.Fee == nil || p.Net == nil {
		return nil
	}

	return &amountsDTO{
		Currency: p.Currency,
		Gross:    amount(*p.Gross),
		Fee:      amount(*p.Fee),
		Net:      amount(*p.Net),
	}
}

func toDTO(r model.EventRecord) eventDTO {
	return eventDTO{
		ID:                 r.ID,
		Source:             string(r.Source),
		ResourceType:       string(r.ResourceType),
		ResourceID:         r.ResourceID,
		Origin:             string(r.Origin),
		Topic:              r.Topic,
		Status:             r.Status,
		ReconStatus:        string(r.ReconStatus),
		ReconCounterpartID: r.ReconCounterpartID,
		OccurredAt:         r.OccurredAt,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
		Amounts:            payPalAmounts(r.ResourceType, r.Payload),
		Payload:            json.RawMessage(r.Payload),
	}
}

// listResponse is the paginated envelope.
type listResponse struct {
	Events []eventDTO `json:"events"`
	// NextCursor is absent on the last page. Pass it back as ?cursor= to
	// continue. It is opaque: the ordering columns are an implementation detail,
	// not a published interface.
	NextCursor string `json:"next_cursor,omitempty"`
}

// healthResponse backs uptime checks and the operational counts.
type healthResponse struct {
	Status string `json:"status"`
	// Events counts rows by processing status.
	Events map[string]int `json:"events"`
	// Recon counts rows by verdict. `unmatched` and `pending` are the numbers
	// that matter: both mean a consumer's figures are drifting from reality, and
	// neither produces an error anywhere else.
	Recon map[string]int `json:"recon"`
	// Unresolved counts rows referencing a parent not yet seen. A missing order
	// or event reads as empty/zero at assemble time rather than erroring —
	// correct for the matching rule, but it means an orphan silently changes a
	// night's numbers unless something surfaces it here.
	Unresolved unresolvedDTO `json:"unresolved"`
}

// unresolvedDTO is the orphan-reference counts described above.
type unresolvedDTO struct {
	// MissingOrders is issued tickets whose order_id does not match any stored
	// order — most often a ticket webhook that arrived before its order's.
	MissingOrders int `json:"missing_orders"`
	// MissingEvents is issued tickets whose event_id does not match any stored,
	// non-tombstoned event.
	MissingEvents int `json:"missing_events"`
}

// errorResponse is the single error shape, so a consumer can parse failures
// without special-casing each endpoint.
type errorResponse struct {
	Error string `json:"error"`
}
