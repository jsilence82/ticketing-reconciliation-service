package recon

import (
	"testing"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

func ticket(ticketID, orderID, txnID string, opts ...func(*model.CanonicalTicket)) model.CanonicalTicket {
	t := model.CanonicalTicket{
		TicketID:         ticketID,
		OrderID:          orderID,
		Show:             "Show",
		Category:         "General",
		Quantity:         1,
		Revenue:          10,
		Status:           "valid",
		PerformanceDate:  time.Date(2024, time.July, 1, 19, 30, 0, 0, time.UTC),
		PayPalTxnID:      txnID,
		OrderPaymentType: "paypal",
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func voided(refund float64) func(*model.CanonicalTicket) {
	return func(t *model.CanonicalTicket) {
		t.Status = "voided"
		t.OrderRefundAmount = refund
	}
}

func charge(id string, gross, fee float64) model.PayPalTxn {
	return model.PayPalTxn{TxnID: id, Gross: gross, Fee: fee, Net: gross + fee, Status: "S"}
}

func refundTxn(id, original string, gross, fee float64) model.PayPalTxn {
	return model.PayPalTxn{
		TxnID: id, Gross: gross, Fee: fee, Net: gross + fee,
		Status: "S", PayPalReferenceID: original,
	}
}

// find returns the classification for a resource, or fails.
func find(t *testing.T, cs []Classification, resourceID string) Classification {
	t.Helper()
	for _, c := range cs {
		if c.ResourceID == resourceID {
			return c
		}
	}
	t.Fatalf("no classification for %q", resourceID)
	return Classification{}
}

func TestClassify_MatchedCharge(t *testing.T) {
	rows := []model.CanonicalTicket{ticket("it_1", "or_1", "TX1")}
	txns := []model.PayPalTxn{charge("TX1", 10, -0.5)}

	got := find(t, Classify(rows, txns, Flags{}), "TX1")

	if got.Status != model.ReconMatched {
		t.Errorf("Status = %q, want matched", got.Status)
	}
	if got.CounterpartID != "or_1" {
		t.Errorf("CounterpartID = %q, want the Ticket Tailor order or_1", got.CounterpartID)
	}
	if got.ResourceType != model.ResourcePayPalTransaction {
		t.Errorf("ResourceType = %q", got.ResourceType)
	}
}

// The whole point of Classify: a genuinely orphaned charge must be visible.
// The reference's unmatched_df can never report this (ledger 1).
func TestClassify_UnmatchedChargeIsSurfaced(t *testing.T) {
	rows := []model.CanonicalTicket{ticket("it_1", "or_1", "TX1")}
	txns := []model.PayPalTxn{charge("TX1", 10, -0.5), charge("TX9", 99, -3)}

	cs := Classify(rows, txns, Flags{})

	if got := find(t, cs, "TX9"); got.Status != model.ReconUnmatched {
		t.Errorf("orphan TX9 Status = %q, want unmatched", got.Status)
	}
	if got := find(t, cs, "TX1"); got.Status != model.ReconMatched {
		t.Errorf("TX1 Status = %q, want matched", got.Status)
	}

	if n := Summarise(cs); n.Unmatched != 1 || n.Matched != 1 {
		t.Errorf("Summarise = %+v, want 1 matched and 1 unmatched", n)
	}
}

// A refund reaches `matched` through the second half of the matching rule:
// its reference points at a charge a ticket knows about.
func TestClassify_RefundMatchesViaReference(t *testing.T) {
	rows := []model.CanonicalTicket{ticket("it_1", "or_1", "TX1")}
	txns := []model.PayPalTxn{charge("TX1", 30, -1.5), refundTxn("R1", "TX1", -30, 1.5)}

	got := find(t, Classify(rows, txns, Flags{}), "R1")

	if got.Status != model.ReconMatched {
		t.Errorf("Status = %q, want matched", got.Status)
	}
	if got.CounterpartID != "TX1" {
		t.Errorf("CounterpartID = %q, want the original charge TX1", got.CounterpartID)
	}
}

// PayPal webhooks omit paypal_reference_id. Such a refund must be held as
// pending and resolved, never guessed and never filed as unmatched — an
// unresolved refund that silently drops out understates net for every consumer.
func TestClassify_UnlinkedRefundIsPendingNotUnmatched(t *testing.T) {
	rows := []model.CanonicalTicket{ticket("it_1", "or_1", "TX1")}
	txns := []model.PayPalTxn{charge("TX1", 30, -1.5), refundTxn("R1", "", -30, 1.5)}

	got := find(t, Classify(rows, txns, Flags{}), "R1")

	if got.Status != model.ReconPending {
		t.Errorf("Status = %q, want pending — an unlinked refund is unresolved, "+
			"not proven orphaned", got.Status)
	}
}

func TestClassify_TransferredTicket(t *testing.T) {
	rows := []model.CanonicalTicket{
		ticket("it_1", "or_1", "TX1", voided(0)),    // transfer: no refund issued
		ticket("it_2", "or_2", "TX2", voided(1300)), // refunded, so not a transfer
		ticket("it_3", "or_3", "TX3"),               // ordinary valid ticket
	}

	cs := Classify(rows, nil, Flags{})

	if got := find(t, cs, "it_1"); got.Status != model.ReconTransferred {
		t.Errorf("it_1 Status = %q, want transferred", got.Status)
	} else if got.CounterpartID != "TX1" {
		t.Errorf("it_1 CounterpartID = %q, want TX1", got.CounterpartID)
	}

	if got := find(t, cs, "it_2"); got.Status != model.ReconNotApplicable {
		t.Errorf("it_2 (voided WITH refund) Status = %q, want not_applicable", got.Status)
	}
	if got := find(t, cs, "it_3"); got.Status != model.ReconNotApplicable {
		t.Errorf("it_3 (valid) Status = %q, want not_applicable", got.Status)
	}
}

// A non-PayPal transfer is not a transfer: the definition requires the order to
// have been paid through PayPal, because that is what makes the charge stand.
func TestClassify_TransferRequiresPayPal(t *testing.T) {
	rows := []model.CanonicalTicket{
		ticket("it_1", "or_1", "", voided(0), func(c *model.CanonicalTicket) {
			c.OrderPaymentType = "no_cost"
		}),
	}

	if got := find(t, Classify(rows, nil, Flags{}), "it_1"); got.Status != model.ReconNotApplicable {
		t.Errorf("Status = %q, want not_applicable for a non-PayPal voided ticket", got.Status)
	}
}

// Classify deliberately does NOT inherit ledger entry 7. That fallback is an
// artefact of per-show filtering; globally, no ticket carrying a PayPal ID means
// nothing matches, and saying so is more useful than calling everything matched.
func TestClassify_DoesNotInheritEmptyIDFallback(t *testing.T) {
	rows := []model.CanonicalTicket{ticket("it_1", "or_1", "")} // no usable PayPal id
	txns := []model.PayPalTxn{charge("TX1", 10, -1), charge("TX2", 20, -2)}

	cs := Classify(rows, txns, Flags{})

	if n := Summarise(cs); n.Unmatched != 2 || n.Matched != 0 {
		t.Errorf("Summarise = %+v, want both transactions unmatched; the "+
			"empty-id-set fallback must not leak into classification", n)
	}

	// Contrast: the oracle path still reproduces the fallback for parity.
	if got := FilterPayPalForShow(rows, txns, Flags{}); len(got) != 2 {
		t.Errorf("FilterPayPalForShow returned %d, want 2 — the reference's "+
			"fallback must still be reproduced there", len(got))
	}
}

// "nan" is a real sentinel: build_canonical stringifies before dropping nulls,
// so a missing PayPal id becomes the literal string.
func TestClassify_NanSentinelIsNotAnID(t *testing.T) {
	rows := []model.CanonicalTicket{ticket("it_1", "or_1", "nan")}
	txns := []model.PayPalTxn{charge("nan", 10, -1)}

	if got := find(t, Classify(rows, txns, Flags{}), "nan"); got.Status != model.ReconUnmatched {
		t.Errorf("Status = %q, want unmatched — \"nan\" must not act as a match key", got.Status)
	}
}

func TestClassify_IsDeterministic(t *testing.T) {
	rows := []model.CanonicalTicket{
		ticket("it_2", "or_2", "TX2"),
		ticket("it_1", "or_1", "TX1"),
	}
	txns := []model.PayPalTxn{charge("TX2", 20, -1), charge("TX1", 10, -1)}

	first := Classify(rows, txns, Flags{})
	for i := 0; i < 5; i++ {
		next := Classify(rows, txns, Flags{})
		for j := range first {
			if first[j] != next[j] {
				t.Fatalf("run %d differs at index %d: %+v vs %+v", i, j, first[j], next[j])
			}
		}
	}
}
