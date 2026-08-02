package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/snapshot"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// Writer is the storage side of an import, narrowed to what importing needs so
// tests can substitute a recorder.
type Writer interface {
	UpsertBatch(ctx context.Context, recs []model.EventRecord) (map[store.Outcome]int, error)
}

// Report summarises an import.
type Report struct {
	// Read is how many provider objects were seen.
	Read int
	// Outcomes counts inserted / updated / rejected. Rejected is not a failure:
	// it means the row already held this content, which is the dedup-hit signal.
	Outcomes map[store.Outcome]int
}

// Total returns the number of rows the store accepted.
func (r Report) Total() int {
	return r.Outcomes[store.Inserted] + r.Outcomes[store.Updated]
}

func (r Report) String() string {
	return fmt.Sprintf("read %d, inserted %d, updated %d, unchanged %d",
		r.Read, r.Outcomes[store.Inserted], r.Outcomes[store.Updated],
		r.Outcomes[store.Rejected])
}

// batchSize bounds a single transaction. Large enough that a full import is a
// handful of round trips, small enough that one failure does not roll back
// thousands of rows.
const batchSize = 500

// ImportSnapshot loads a captured snapshot into the store.
//
// This is the offline ingestion path. It reaches no provider API and needs
// no credentials, which is what lets the parity-through-Postgres run prove
// the engine end to end without a single live call.
//
// Resources are imported in dependency order — events and series first, then
// orders, then tickets — so that at any point mid-import a ticket's parents are
// more likely to be present. Correctness does not depend on it: the read-time
// join is a LEFT JOIN and a missing parent reads as empty, exactly as the
// reference's pandas map+fillna does. It simply keeps the intermediate state
// less alarming to look at.
//
// Order WITHIN each resource type is the provider's array order and must be
// preserved: it becomes the ingest ordinal the reconcile read sorts by.
func ImportSnapshot(ctx context.Context, w Writer, raw snapshot.RawJSON) (Report, error) {
	report := Report{Read: raw.Total(), Outcomes: map[store.Outcome]int{}}

	groups := []struct {
		name string
		rows []json.RawMessage
		conv func([]byte, model.Origin) (model.EventRecord, error)
	}{
		{"event_series", raw.EventSeries, FromTicketTailor},
		{"events", raw.Events, FromTicketTailor},
		{"orders", raw.Orders, FromTicketTailor},
		{"issued_tickets", raw.Tickets, FromTicketTailor},
		{"paypal_transactions", raw.PayPalTxns, FromPayPalCache},
	}

	for _, g := range groups {
		for start := 0; start < len(g.rows); start += batchSize {
			end := min(start+batchSize, len(g.rows))

			batch := make([]model.EventRecord, 0, end-start)
			for i := start; i < end; i++ {
				rec, err := g.conv(g.rows[i], model.OriginBackfill)
				if err != nil {
					return report, fmt.Errorf("%s[%d]: %w", g.name, i, err)
				}
				batch = append(batch, rec)
			}

			counts, err := w.UpsertBatch(ctx, batch)
			if err != nil {
				return report, fmt.Errorf("%s[%d:%d]: %w", g.name, start, end, err)
			}
			for k, v := range counts {
				report.Outcomes[k] += v
			}
		}
	}

	return report, nil
}
