package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// Writer is the storage side of a live import.
type Writer interface {
	UpsertBatch(ctx context.Context, recs []model.EventRecord) (map[store.Outcome]int, error)
}

// liveBatch bounds a single transaction.
const liveBatch = 500

// ImportTicketTailor pulls every reconcilable Ticket Tailor resource.
//
// Endpoint order matters: it becomes the ingest ordinal, and fetching parents
// before children keeps intermediate state coherent. Correctness does not depend
// on it — the read-time join is a LEFT JOIN — but a half-imported database is
// easier to reason about.
//
// FULL RESYNC is the only mode offered — see docs/ARCHITECTURE.md, "Backfill
// is always a full resync" for why a `created_at` watermark isn't safe here.
func ImportTicketTailor(ctx context.Context, c *TicketTailorClient, w Writer) (Report, error) {
	report := Report{Outcomes: map[store.Outcome]int{}}

	for _, endpoint := range []string{"event_series", "events", "orders", "issued_tickets"} {
		pager := c.Page(endpoint, url.Values{})

		for {
			page, more, err := pager.Next(ctx)
			if err != nil {
				return report, fmt.Errorf("%s: %w", endpoint, err)
			}
			if len(page) > 0 {
				if err := writeRecords(ctx, w, page, ingest.FromTicketTailor, &report); err != nil {
					return report, fmt.Errorf("%s: %w", endpoint, err)
				}
			}
			if !more {
				break
			}
		}
	}

	return report, nil
}

// ImportPayPal pulls Transaction Search over a date range.
func ImportPayPal(
	ctx context.Context, c *PayPalClient, w Writer, start, end time.Time,
) (Report, error) {
	report := Report{Outcomes: map[store.Outcome]int{}}
	pager := c.Transactions(start, end)

	for {
		page, more, err := pager.Next(ctx)
		if err != nil {
			return report, fmt.Errorf("paypal: %w", err)
		}
		if len(page) > 0 {
			if err := writeRecords(ctx, w, page, ingest.FromPayPalSearch, &report); err != nil {
				return report, fmt.Errorf("paypal: %w", err)
			}
		}
		if !more {
			return report, nil
		}
	}
}

func writeRecords(
	ctx context.Context,
	w Writer,
	page []json.RawMessage,
	conv func([]byte, model.Origin) (model.EventRecord, error),
	report *Report,
) error {
	report.Read += len(page)

	for start := 0; start < len(page); start += liveBatch {
		end := min(start+liveBatch, len(page))

		batch := make([]model.EventRecord, 0, end-start)
		for i := start; i < end; i++ {
			rec, err := conv(page[i], model.OriginBackfill)
			if err != nil {
				return fmt.Errorf("record %d: %w", i, err)
			}
			batch = append(batch, rec)
		}

		counts, err := w.UpsertBatch(ctx, batch)
		if err != nil {
			return err
		}
		for k, v := range counts {
			report.Outcomes[k] += v
		}
	}
	return nil
}

// Report is the same shape the snapshot importer reports, aliased rather than
// duplicated so both paths summarise identically.
type Report = ingest.Report
