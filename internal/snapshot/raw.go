package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// Raw is the Ticket Tailor side loaded as individual resources, the way the
// `events` table stores them — before any join.
//
// This is what the real ingestion path produces. Feeding it through
// model.Assemble reproduces the canonical frame at read time, which is how the
// service works now that there is no projection layer.
type Raw struct {
	Orders  []model.TTOrder
	Tickets []model.TTIssuedTicket
	Events  []model.TTEvent
}

// ttDate mirrors Ticket Tailor's nested date object. `unix` is preferred because
// the reference's column discovery prefers a start.unix-shaped column, which
// sends mapping._parse_date_column down its numeric branch.
type ttDate struct {
	Unix int64  `json:"unix"`
	ISO  string `json:"iso"`
}

type rawTTPayload struct {
	Orders []struct {
		ID     string `json:"id"`
		TxnID  string `json:"txn_id"`
		Status string `json:"status"`
		// Currency is a nested object on an order, unlike the bare string it is
		// on an event. Ticket Tailor is not consistent about this.
		Currency struct {
			Code string `json:"code"`
		} `json:"currency"`
		RefundAmount  any   `json:"refund_amount"`
		TotalPaid     any   `json:"total_paid"`
		CreatedAt     int64 `json:"created_at"`
		PaymentMethod struct {
			Type string `json:"type"`
		} `json:"payment_method"`
	} `json:"orders"`

	Tickets []struct {
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
	} `json:"tickets"`

	Events []struct {
		ID            string `json:"id"`
		EventSeriesID string `json:"event_series_id"`
		Name          string `json:"name"`
		Start         ttDate `json:"start"`
		End           ttDate `json:"end"`
	} `json:"events"`
}

// LoadRaw reads tt_raw_cache.json into per-resource shapes.
func LoadRaw(dir string) (Raw, error) {
	path := filepath.Join(dir, "tt_raw_cache.json")

	blob, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, read-only
	if err != nil {
		return Raw{}, fmt.Errorf("read raw TT cache: %w", err)
	}

	var payload rawTTPayload
	if err := json.Unmarshal(blob, &payload); err != nil {
		return Raw{}, fmt.Errorf("parse raw TT cache: %w", err)
	}

	out := Raw{
		Orders:  make([]model.TTOrder, 0, len(payload.Orders)),
		Tickets: make([]model.TTIssuedTicket, 0, len(payload.Tickets)),
		Events:  make([]model.TTEvent, 0, len(payload.Events)),
	}

	for _, o := range payload.Orders {
		out.Orders = append(out.Orders, model.TTOrder{
			ID:                o.ID,
			TxnID:             o.TxnID,
			PaymentMethodType: o.PaymentMethod.Type,
			// Deliberately NOT converted from cents. The reference leaves
			// refund_amount in raw units because it only ever compares it
			// against zero — ledger entry 13.
			RefundAmount:   numeric(o.RefundAmount),
			TotalPaidMinor: int64(numeric(o.TotalPaid)),
			Currency:       o.Currency.Code,
			Status:         o.Status,
			CreatedAt:      unixOrZero(o.CreatedAt),
		})
	}

	for _, t := range payload.Tickets {
		out.Tickets = append(out.Tickets, model.TTIssuedTicket{
			ID:            t.ID,
			OrderID:       t.OrderID,
			EventID:       t.EventID,
			EventSeriesID: t.EventSeriesID,
			TicketTypeID:  t.TicketTypeID,
			Description:   t.Description,
			Status:        t.Status,
			PriceMinor:    int64(numeric(t.ListedPrice)),
			Currency:      t.ListedCurrency.Code,
			CreatedAt:     unixOrZero(t.CreatedAt),
			UpdatedAt:     unixOrZero(t.UpdatedAt),
		})
	}

	for _, e := range payload.Events {
		out.Events = append(out.Events, model.TTEvent{
			ID:            e.ID,
			EventSeriesID: e.EventSeriesID,
			Name:          e.Name,
			StartUnix:     e.Start.Unix,
			EndUnix:       e.End.Unix,
		})
	}

	return out, nil
}

// numeric coerces Ticket Tailor's loosely-typed money fields. They arrive as
// JSON numbers in the cache but as strings from some API responses, and the
// reference runs everything through pd.to_numeric, so a non-numeric value must
// degrade to zero rather than fail the load.
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
