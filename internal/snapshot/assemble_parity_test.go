package snapshot_test

import (
	"os"
	"sort"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/snapshot"
)

// TestAssembleMatchesCanonicalFrame is the evidence that the read-time join is
// correct.
//
// With no projection layer, model.Assemble is what turns stored `events` rows
// back into the canonical frame the matching rule needs. If it diverges from
// what the dashboard's mapping.build_canonical produced, every downstream number
// is wrong — and nothing else in the test suite would notice, because the parity
// harness feeds the canonical frame in pre-built.
//
// So: assemble from the RAW Ticket Tailor resources, and compare field by field
// against the dashboard's own canonical output for the same data.
//
// Skips when SSG_PARITY_DATA is unset, so a clean checkout stays green.
func TestAssembleMatchesCanonicalFrame(t *testing.T) {
	dir := os.Getenv("SSG_PARITY_DATA")
	if dir == "" {
		t.Skip("SSG_PARITY_DATA unset; see docs/ENVIRONMENT.md")
	}

	raw, err := snapshot.LoadRaw(dir)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	snap, err := snapshot.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := model.Assemble(raw.Orders, raw.Tickets, raw.Events)
	want := snap.Tickets

	if len(got) != len(want) {
		t.Fatalf("assembled %d rows, canonical frame has %d", len(got), len(want))
	}

	// The canonical cache carries no ticket ID, so rows cannot be paired up by
	// identity. Sort both sides by a composite of the fields that do exist and
	// compare positionally.
	sortRows(got)
	sortRows(want)

	var mismatches int
	report := func(i int, field string, g, w any) {
		mismatches++
		if mismatches <= 10 {
			t.Errorf("row %d %s: assembled=%v canonical=%v", i, field, g, w)
		}
	}

	for i := range got {
		g, w := got[i], want[i]

		if g.Category != w.Category {
			report(i, "Category", g.Category, w.Category)
		}
		if g.Show != w.Show {
			report(i, "Show", g.Show, w.Show)
		}
		if g.Status != w.Status {
			report(i, "Status", g.Status, w.Status)
		}
		if g.Quantity != w.Quantity {
			report(i, "Quantity", g.Quantity, w.Quantity)
		}
		if g.Revenue != w.Revenue {
			report(i, "Revenue", g.Revenue, w.Revenue)
		}
		if g.Occurrence != w.Occurrence {
			report(i, "Occurrence", g.Occurrence, w.Occurrence)
		}
		if g.PayPalTxnID != w.PayPalTxnID {
			report(i, "PayPalTxnID", g.PayPalTxnID, w.PayPalTxnID)
		}
		if g.OrderPaymentType != w.OrderPaymentType {
			report(i, "OrderPaymentType", g.OrderPaymentType, w.OrderPaymentType)
		}
		if g.OrderRefundAmount != w.OrderRefundAmount {
			report(i, "OrderRefundAmount", g.OrderRefundAmount, w.OrderRefundAmount)
		}
		if !g.PerformanceDate.Equal(w.PerformanceDate) {
			report(i, "PerformanceDate", g.PerformanceDate, w.PerformanceDate)
		}
		if !g.Date.Equal(w.Date) {
			report(i, "Date", g.Date, w.Date)
		}
	}

	if mismatches > 10 {
		t.Errorf("... and %d further field mismatches", mismatches-10)
	}
	if mismatches == 0 {
		t.Logf("read-time Assemble reproduces the canonical frame exactly across %d rows", len(got))
	}
}

func sortRows(rows []model.CanonicalTicket) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.PayPalTxnID != b.PayPalTxnID {
			return a.PayPalTxnID < b.PayPalTxnID
		}
		if a.Occurrence != b.Occurrence {
			return a.Occurrence < b.Occurrence
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Revenue != b.Revenue {
			return a.Revenue < b.Revenue
		}
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		return a.Status < b.Status
	})
}

// TestUnresolvedParents exercises the orphan counter that backs the health
// endpoint. Real data should have no dangling references; if it does, that is a
// finding rather than a test failure, so it is reported not asserted.
func TestUnresolvedParents(t *testing.T) {
	dir := os.Getenv("SSG_PARITY_DATA")
	if dir == "" {
		t.Skip("SSG_PARITY_DATA unset")
	}

	raw, err := snapshot.LoadRaw(dir)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}

	missingOrders, missingEvents := model.UnresolvedParents(raw.Orders, raw.Tickets, raw.Events)
	t.Logf("tickets with a missing order: %d; with a missing event: %d (of %d tickets)",
		missingOrders, missingEvents, len(raw.Tickets))
}
