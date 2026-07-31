package model

import "time"

// Assemble joins stored resources into the canonical frame the matching rule
// operates on.
//
// With no projection layer, this runs at READ time on every request rather than
// being materialized. That is the deliberate trade in CLAUDE.md: one source of
// truth and no cached view that can drift, at a data volume (thousands of rows)
// where recomputing is free.
//
// Grain is one row per issued ticket. Order-level fields (the PayPal match key,
// payment type, refund amount) are broadcast down to every ticket on the order,
// which is what makes multi-ticket orders work.
//
// # Missing parents are not an error
//
// A ticket can arrive before its order, or reference an event that has been
// tombstoned. Both must read as empty/zero rather than dropping the ticket or
// failing — this is a LEFT JOIN with COALESCE, reproducing exactly what
// `tickets_df[order_id].map(order_txn_map).fillna("")` does in
// api/tickettailor.py:111. Silently dropping an orphan would change a night's
// numbers; leaving it with a blank match key is the reference's behavior.
func Assemble(orders []TTOrder, tickets []TTIssuedTicket, events []TTEvent) []CanonicalTicket {
	orderByID := make(map[string]TTOrder, len(orders))
	for _, o := range orders {
		orderByID[o.ID] = o
	}

	eventByID := make(map[string]TTEvent, len(events))
	for _, e := range events {
		eventByID[e.ID] = e
	}

	// The reference divides an order's total across its tickets using a count
	// taken over the WHOLE ticket frame, including tickets later filtered out
	// (ledger entry 14). Count the same way.
	ticketsPerOrder := make(map[string]int, len(orders))
	for _, t := range tickets {
		ticketsPerOrder[t.OrderID]++
	}

	out := make([]CanonicalTicket, 0, len(tickets))
	for _, t := range tickets {
		row := CanonicalTicket{
			TicketID: t.ID,
			OrderID:  t.OrderID,
			Category: t.Description,
			// `quantity` is unmapped in the production column mapping, so
			// build_canonical defaults it to 1.0 for every row.
			Quantity:   1,
			Revenue:    float64(t.PriceMinor) / 100,
			Date:       t.UpdatedAt,
			Status:     t.Status,
			Occurrence: t.EventID,
		}

		// LEFT JOIN to the order. A missing order leaves the match key blank,
		// which is exactly how the reference treats it.
		if o, ok := orderByID[t.OrderID]; ok {
			row.PayPalTxnID = o.TxnID
			row.OrderPaymentType = o.PaymentMethodType
			row.OrderRefundAmount = o.RefundAmount
			if n := ticketsPerOrder[t.OrderID]; n > 0 {
				row.OrderTotalPaid = float64(o.TotalPaidMinor) / 100 / float64(n)
			}
		}

		// LEFT JOIN to the event. A tombstoned event must behave identically to
		// a missing one: no show name, and a zero performance date, which the
		// groupby then drops from the report body.
		if e, ok := eventByID[t.EventID]; ok && !e.IsDeleted() {
			row.Show = e.Name
			if e.StartUnix != 0 {
				row.PerformanceDate = e.Start()
			}
		}

		out = append(out, row)
	}

	return out
}

// UnresolvedParents counts tickets whose order or event is absent from the
// supplied set.
//
// CLAUDE.md requires surfacing this on the health endpoint: an orphan silently
// changes a night's numbers rather than producing a visible error, so a stale
// count is an alerting condition, not a log line.
func UnresolvedParents(orders []TTOrder, tickets []TTIssuedTicket, events []TTEvent) (missingOrders, missingEvents int) {
	haveOrder := make(map[string]bool, len(orders))
	for _, o := range orders {
		haveOrder[o.ID] = true
	}
	haveEvent := make(map[string]bool, len(events))
	for _, e := range events {
		if !e.IsDeleted() {
			haveEvent[e.ID] = true
		}
	}

	for _, t := range tickets {
		if t.OrderID != "" && !haveOrder[t.OrderID] {
			missingOrders++
		}
		if t.EventID != "" && !haveEvent[t.EventID] {
			missingEvents++
		}
	}
	return missingOrders, missingEvents
}

// zeroTime is returned where the reference would hold pandas NaT.
var zeroTime time.Time

// HasPerformanceDate reports whether the row reached the report body. Rows
// without one are dropped by the performance-date groupby but still counted in
// the Statistics TOTAL (ledger entry 9).
func (c CanonicalTicket) HasPerformanceDate() bool { return c.PerformanceDate != zeroTime }
