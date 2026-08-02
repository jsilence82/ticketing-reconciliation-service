package recon

import (
	"sort"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// Classification is one per-resource verdict, destined for the `recon_status`
// and `recon_counterpart_id` columns of the `events` row it names.
//
// This is the service's actual product. The Totals/Statistics builders in
// recon/oracle exist only to prove the matching rule is right; this is what
// ships and what `GET /events?recon_status=` filters on.
type Classification struct {
	Source       model.Source
	ResourceType model.ResourceType
	ResourceID   string
	Status       model.ReconStatus

	// CounterpartID names the resource this one reconciled against: the Ticket
	// Tailor order for a matched charge, the original capture for a refund.
	// Empty when there is nothing to point at.
	CounterpartID string
}

// Classify assigns a reconciliation verdict to every resource.
//
// Unlike the report builders, this is show-agnostic: it runs over the whole
// dataset, because the `events` table is global and a row's verdict cannot
// depend on which show a caller happened to ask about.
//
// # Relationship to the reference implementation
//
// The Python has no per-resource classification, so there is no behavior
// here to port bug-for-bug. Two consequences, both deliberate:
//
//   - Unmatched detection is computed correctly, against the full transaction
//     list. The reference's `unmatched_df` is a tautology over its own filtered
//     input and is provably always empty (ledger entry 1); reproducing that
//     would ship an always-empty field, which is worse than useless and would
//     violate the business contract's "must be surfaced, not discarded".
//   - The empty-pp_ids "return everything" fallback (ledger entry 7) is NOT
//     inherited. It is an artefact of per-show filtering; here, no Ticket Tailor
//     ticket carrying a PayPal ID means nothing matches, which is the truthful
//     answer rather than a permissive one.
//
// The matching RULE itself — exact, case-sensitive equality on
// `txn_id ∈ pp_ids OR paypal_reference_id ∈ pp_ids`, with the ticket side
// trimmed and the PayPal side untouched — is the ported behavior, and it is
// shared with the oracle via PayPalIDsForShow.
func Classify(rows []model.CanonicalTicket, txns []model.PayPalTxn, f Flags) []Classification {
	ids := PayPalIDsForShow(rows, f)

	// Which Ticket Tailor order does a given PayPal transaction ID belong to?
	// Built from the ticket grain, since that is where the joined key lives.
	orderByTxnID := make(map[string]string, len(rows))
	for _, r := range rows {
		if id, ok := NormTxnID(r.PayPalTxnID); ok && r.OrderID != "" {
			orderByTxnID[id] = r.OrderID
		}
	}

	known := make(map[string]struct{}, len(txns))
	for _, tx := range txns {
		known[tx.TxnID] = struct{}{}
	}

	out := make([]Classification, 0, len(txns)+len(rows))

	for _, tx := range txns {
		out = append(out, classifyTxn(tx, ids, known, orderByTxnID))
	}
	for _, r := range rows {
		out = append(out, classifyTicket(r, f))
	}

	// Stable output so a diff between two runs shows real changes only.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].ResourceID < out[j].ResourceID
	})

	return out
}

func classifyTxn(
	tx model.PayPalTxn,
	ids map[string]struct{},
	known map[string]struct{},
	orderByTxnID map[string]string,
) Classification {
	c := Classification{
		Source:       model.SourcePayPal,
		ResourceType: model.ResourcePayPalTransaction,
		ResourceID:   tx.TxnID,
	}

	// A refund with no resolved link to its original charge cannot be judged.
	// PayPal webhooks omit paypal_reference_id entirely, so this is the normal
	// state on first ingest, not an anomaly — but it must never be guessed, and
	// it must not be silently filed as unmatched: an unresolved refund that
	// drops out of the match filter understates net for every consumer.
	if tx.IsRefund() && tx.PayPalReferenceID == "" {
		c.Status = model.ReconPending
		return c
	}

	// The matching rule. Direct hit: this transaction's own ID is referenced by
	// a Ticket Tailor ticket.
	if _, ok := ids[tx.TxnID]; ok {
		c.Status = model.ReconMatched
		c.CounterpartID = orderByTxnID[tx.TxnID]
		return c
	}

	// Indirect hit: a refund pointing back at a charge that IS referenced.
	if _, ok := ids[tx.PayPalReferenceID]; ok {
		c.Status = model.ReconMatched
		c.CounterpartID = tx.PayPalReferenceID
		return c
	}

	// A refund whose original charge we hold but which no ticket references is
	// still unmatched, though naming the capture makes it diagnosable.
	if tx.PayPalReferenceID != "" {
		if _, ok := known[tx.PayPalReferenceID]; ok {
			c.CounterpartID = tx.PayPalReferenceID
		}
	}

	c.Status = model.ReconUnmatched
	return c
}

// classifyTicket judges a Ticket Tailor ticket.
//
// Only `transferred` is defined by the business contract on this side: voided in
// Ticket Tailor, paid by PayPal, with no refund issued, so the original charge
// still stands. Everything else is not_applicable — the reference has no notion
// of an "unmatched ticket", and inventing one here would be an undocumented
// silent improvement over the ported behavior.
func classifyTicket(r model.CanonicalTicket, f Flags) Classification {
	c := Classification{
		Source:       model.SourceTicketTailor,
		ResourceType: model.ResourceIssuedTicket,
		ResourceID:   r.TicketID,
		Status:       model.ReconNotApplicable,
	}

	if isVoidReadmitted(r.Status, f) &&
		normStatus(r.OrderPaymentType, f) == "paypal" &&
		r.OrderRefundAmount == 0 {
		c.Status = model.ReconTransferred
		if id, ok := NormTxnID(r.PayPalTxnID); ok {
			c.CounterpartID = id
		}
	}

	return c
}

// Counts summarises a classification run. It backs the health endpoint, where a
// rising unmatched or pending count is the signal that consumers' figures are
// drifting from reality.
type Counts struct {
	Matched       int
	Unmatched     int
	Transferred   int
	Pending       int
	NotApplicable int
}

// Summarise tallies verdicts by status.
func Summarise(cs []Classification) Counts {
	var n Counts
	for _, c := range cs {
		switch c.Status {
		case model.ReconMatched:
			n.Matched++
		case model.ReconUnmatched:
			n.Unmatched++
		case model.ReconTransferred:
			n.Transferred++
		case model.ReconPending:
			n.Pending++
		case model.ReconNotApplicable:
			n.NotApplicable++
		}
	}
	return n
}
