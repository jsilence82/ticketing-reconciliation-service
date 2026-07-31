package ingest

import (
	"fmt"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// ValidateStored checks that a stored payload can actually be used.
//
// This is stage 1 of the worker. It runs per row under SKIP LOCKED so that a
// payload which cannot be parsed is dead-lettered on its own, rather than
// aborting the dataset-wide reconcile pass that follows — one malformed row
// should cost one row, not the whole report.
//
// It deliberately checks only what the engine reads. A field the reconciliation
// never touches is not worth failing a row over.
func ValidateStored(rt model.ResourceType, payload []byte) error {
	switch rt {
	case model.ResourceOrder:
		o, err := decodeOrder(payload)
		if err != nil {
			return err
		}
		if o.ID == "" {
			return fmt.Errorf("order payload has no id")
		}

	case model.ResourceIssuedTicket:
		t, err := decodeTicket(payload)
		if err != nil {
			return err
		}
		if t.ID == "" {
			return fmt.Errorf("ticket payload has no id")
		}
		// Without an order the match key is unreachable, and without an event
		// there is no performance date — but neither is fatal. A missing parent
		// must read as empty, exactly as the reference's pandas map+fillna does,
		// so these are not validated here.

	case model.ResourceEvent:
		e, err := decodeEvent(payload)
		if err != nil {
			return err
		}
		if e.ID == "" {
			return fmt.Errorf("event payload has no id")
		}

	case model.ResourcePayPalTransaction:
		tx, err := decodePayPal(payload)
		if err != nil {
			return err
		}
		if tx.TxnID == "" {
			return fmt.Errorf("paypal payload has no txn_id")
		}
		// net must equal gross + fee. A violation means the normalizer got the
		// sign convention wrong, which CLAUDE.md calls the highest-risk
		// conversion in the project — it would silently double or zero the fees
		// in every downstream figure, so it fails loudly here instead.
		if d := tx.Net - (tx.Gross + tx.Fee); d > 0.005 || d < -0.005 {
			return fmt.Errorf("paypal %s: net %.2f != gross %.2f + fee %.2f",
				tx.TxnID, tx.Net, tx.Gross, tx.Fee)
		}

	case model.ResourceEventSeries, model.ResourceOther:
		// Stored, never reconciled.
	}

	return nil
}
