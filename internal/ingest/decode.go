package ingest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// ReconcileInputs is everything model.Assemble and the matching rule need.
type ReconcileInputs struct {
	Orders  []model.TTOrder
	Tickets []model.TTIssuedTicket
	Events  []model.TTEvent
	Txns    []model.PayPalTxn
}

// DecodeResources turns stored rows back into the engine's inputs.
//
// This is the read half of the round trip, and it must produce values
// IDENTICAL to snapshot.LoadRaw / snapshot.Load — the file-based path the parity
// harness already validates. Any divergence here shows up as a wrong figure with
// no indication of the cause, which is why `make parity-db` asserts the two
// paths emit byte-identical JSON rather than merely both matching Python.
//
// Order is preserved: store.LoadResources returns rows in ingest order, and
// appending in that order keeps each slice in the provider's original sequence.
// That matters because the engine's summation is order-sensitive.
func DecodeResources(rs []store.RawResource) (ReconcileInputs, error) {
	var out ReconcileInputs

	for i, r := range rs {
		switch r.ResourceType {
		case model.ResourceOrder:
			o, err := decodeOrder(r.Payload)
			if err != nil {
				return out, fmt.Errorf("row %d (%s): %w", i, r.ResourceID, err)
			}
			out.Orders = append(out.Orders, o)

		case model.ResourceIssuedTicket:
			t, err := decodeTicket(r.Payload)
			if err != nil {
				return out, fmt.Errorf("row %d (%s): %w", i, r.ResourceID, err)
			}
			out.Tickets = append(out.Tickets, t)

		case model.ResourceEvent:
			e, err := decodeEvent(r.Payload)
			if err != nil {
				return out, fmt.Errorf("row %d (%s): %w", i, r.ResourceID, err)
			}
			out.Events = append(out.Events, e)

		case model.ResourcePayPalTransaction:
			tx, err := decodePayPal(r.Payload)
			if err != nil {
				return out, fmt.Errorf("row %d (%s): %w", i, r.ResourceID, err)
			}
			out.Txns = append(out.Txns, tx)

		default:
			// event_series and anything ignored are stored but not reconciled.
		}
	}

	return out, nil
}

type storedOrder struct {
	ID        string `json:"id"`
	TxnID     string `json:"txn_id"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	// Loosely typed for the same reason snapshot.LoadRaw is: Ticket Tailor
	// returns these as numbers in the cache and as strings from some API
	// responses, and the reference coerces everything through pd.to_numeric.
	RefundAmount any `json:"refund_amount"`
	TotalPaid    any `json:"total_paid"`
	Currency     struct {
		Code string `json:"code"`
	} `json:"currency"`
	PaymentMethod struct {
		Type string `json:"type"`
	} `json:"payment_method"`
}

func decodeOrder(payload []byte) (model.TTOrder, error) {
	var s storedOrder
	if err := json.Unmarshal(payload, &s); err != nil {
		return model.TTOrder{}, fmt.Errorf("decode order: %w", err)
	}
	return model.TTOrder{
		ID:                s.ID,
		TxnID:             s.TxnID,
		PaymentMethodType: s.PaymentMethod.Type,
		// Deliberately NOT converted from cents, matching the reference: it is
		// only ever compared against zero (ledger entry 13).
		RefundAmount:   numeric(s.RefundAmount),
		TotalPaidMinor: int64(numeric(s.TotalPaid)),
		Currency:       s.Currency.Code,
		Status:         s.Status,
		CreatedAt:      unixOrZero(s.CreatedAt),
	}, nil
}

type storedTicket struct {
	ID             string `json:"id"`
	OrderID        string `json:"order_id"`
	EventID        string `json:"event_id"`
	EventSeriesID  string `json:"event_series_id"`
	TicketTypeID   string `json:"ticket_type_id"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	ListedPrice    any    `json:"listed_price"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	ListedCurrency struct {
		Code string `json:"code"`
	} `json:"listed_currency"`
}

func decodeTicket(payload []byte) (model.TTIssuedTicket, error) {
	var s storedTicket
	if err := json.Unmarshal(payload, &s); err != nil {
		return model.TTIssuedTicket{}, fmt.Errorf("decode ticket: %w", err)
	}
	return model.TTIssuedTicket{
		ID:            s.ID,
		OrderID:       s.OrderID,
		EventID:       s.EventID,
		EventSeriesID: s.EventSeriesID,
		TicketTypeID:  s.TicketTypeID,
		Description:   s.Description,
		Status:        s.Status,
		PriceMinor:    int64(numeric(s.ListedPrice)),
		Currency:      s.ListedCurrency.Code,
		CreatedAt:     unixOrZero(s.CreatedAt),
		UpdatedAt:     unixOrZero(s.UpdatedAt),
	}, nil
}

type storedEvent struct {
	ID            string `json:"id"`
	EventSeriesID string `json:"event_series_id"`
	Name          string `json:"name"`
	Start         struct {
		Unix int64 `json:"unix"`
	} `json:"start"`
	End struct {
		Unix int64 `json:"unix"`
	} `json:"end"`
}

func decodeEvent(payload []byte) (model.TTEvent, error) {
	var s storedEvent
	if err := json.Unmarshal(payload, &s); err != nil {
		return model.TTEvent{}, fmt.Errorf("decode event: %w", err)
	}
	return model.TTEvent{
		ID:            s.ID,
		EventSeriesID: s.EventSeriesID,
		Name:          s.Name,
		StartUnix:     s.Start.Unix,
		EndUnix:       s.End.Unix,
	}, nil
}

type storedPayPal struct {
	TxnID             string  `json:"txn_id"`
	Date              string  `json:"date"`
	Gross             float64 `json:"gross"`
	Fee               float64 `json:"fee"`
	Net               float64 `json:"net"`
	Status            string  `json:"status"`
	PayPalReferenceID string  `json:"paypal_reference_id"`
}

func decodePayPal(payload []byte) (model.PayPalTxn, error) {
	var s storedPayPal
	if err := json.Unmarshal(payload, &s); err != nil {
		return model.PayPalTxn{}, fmt.Errorf("decode paypal transaction: %w", err)
	}
	return model.PayPalTxn{
		TxnID: s.TxnID,
		// Already a bare YYYY-MM-DD, produced by the reference's raw string
		// slice. Kept verbatim rather than parsed and reformatted.
		DateKey:           s.Date,
		Gross:             s.Gross,
		Fee:               s.Fee,
		Net:               s.Net,
		Status:            s.Status,
		PayPalReferenceID: s.PayPalReferenceID,
		// Currency is deliberately left unset: the dashboard's cache does not
		// carry one, and snapshot.Load does not invent one either. Inventing it
		// here would make the two paths disagree.
	}, nil
}

// numeric mirrors snapshot.numeric so both paths coerce loosely-typed money the
// same way.
func numeric(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

func unixOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
