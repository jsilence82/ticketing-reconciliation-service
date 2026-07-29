package recon

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// The behavior ledger in docs/PARITY.md enumerates the reference
// implementation's accidental-but-deterministic behaviors. Every one must be
// reproduced bug-for-bug in v1 (CLAUDE.md guardrail 2).
//
// Each case below asserts the PYTHON's behavior, not the desirable one. A test
// here failing means the port drifted toward "correct" and broke parity.
//
// TestLedgerCoverage fails if docs/PARITY.md grows an entry with no case here,
// so the doc and the suite cannot drift apart silently.

// --- fixture helpers ---------------------------------------------------------

func date(day int) time.Time {
	return time.Date(2024, time.July, day, 19, 30, 0, 0, time.UTC)
}

// ticket builds a valid, PayPal-paid, one-unit ticket on the given night.
func ticket(txnID string, day int, opts ...func(*model.CanonicalTicket)) model.CanonicalTicket {
	t := model.CanonicalTicket{
		Show:             "Show",
		Category:         "General",
		Quantity:         1,
		Revenue:          10,
		Status:           "valid",
		PerformanceDate:  date(day),
		PayPalTxnID:      txnID,
		OrderPaymentType: "paypal",
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func withStatus(s string) func(*model.CanonicalTicket) {
	return func(t *model.CanonicalTicket) { t.Status = s }
}
func withRefund(v float64) func(*model.CanonicalTicket) {
	return func(t *model.CanonicalTicket) { t.OrderRefundAmount = v }
}
func withNoPerformanceDate() func(*model.CanonicalTicket) {
	return func(t *model.CanonicalTicket) { t.PerformanceDate = time.Time{} }
}

// charge builds a completed PayPal capture. Fee is negative, net = gross + fee.
func charge(id string, gross, fee float64) model.PayPalTxn {
	return model.PayPalTxn{
		TxnID: id, Gross: gross, Fee: fee, Net: gross + fee,
		Status: "S", Currency: "EUR",
	}
}

// refund builds a PayPal refund pointing back at an original charge.
func refund(id, original string, gross, fee float64) model.PayPalTxn {
	return model.PayPalTxn{
		TxnID: id, Gross: gross, Fee: fee, Net: gross + fee,
		Status: "S", Currency: "EUR", PayPalReferenceID: original,
	}
}

func mustBuild(t *testing.T, rows []model.CanonicalTicket, txns []model.PayPalTxn, f Flags) Result {
	t.Helper()
	res, err := Build(rows, txns, "", f)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return res
}

// --- the ledger --------------------------------------------------------------

type ledgerCase struct {
	entry     int
	name      string
	reference string
	// run asserts the reference behavior. Nil means the entry is documented as
	// unreachable and carries an explanation in `unreachable`.
	run func(t *testing.T)
	// unreachable explains why no behavioral test exists.
	unreachable string
}

var ledgerCases = []ledgerCase{{
	entry:     1,
	name:      "unmatched detection is a tautology and yields nothing",
	reference: "core/reconciliation.py:226-232",
	run: func(t *testing.T) {
		rows := []model.CanonicalTicket{ticket("TX1", 1)}
		// TX9 has no Ticket Tailor counterpart: a genuinely orphaned charge.
		txns := []model.PayPalTxn{charge("TX1", 10, -0.5), charge("TX9", 99, -3)}

		res := mustBuild(t, rows, txns, Flags{})
		if len(res.Unmatched) != 0 {
			t.Errorf("Unmatched = %d rows, want 0: the reference's predicate is the "+
				"complement of the filter that built its own input", len(res.Unmatched))
		}

		// The orphan is real, and the fix flag surfaces it.
		res = mustBuild(t, rows, txns, Flags{FixUnmatchedDetection: true})
		if len(res.Unmatched) != 1 || res.Unmatched[0].TxnID != "TX9" {
			t.Errorf("with fix: Unmatched = %+v, want exactly TX9", res.Unmatched)
		}
	},
}, {
	entry:     2,
	name:      "order spanning two nights is counted in full into both",
	reference: "core/reconciliation.py:142,169",
	run: func(t *testing.T) {
		// One PayPal charge covering a ticket on each of two nights.
		rows := []model.CanonicalTicket{ticket("TX1", 1), ticket("TX1", 2)}
		txns := []model.PayPalTxn{charge("TX1", 20, -1)}

		res := mustBuild(t, rows, txns, Flags{})
		if len(res.Totals.Rows) != 2 {
			t.Fatalf("got %d nights, want 2", len(res.Totals.Rows))
		}
		for i, r := range res.Totals.Rows {
			if r.Gross != 20 {
				t.Errorf("night %d gross = %v, want the FULL 20 counted twice", i, r.Gross)
			}
		}
		if res.Totals.Total.Gross != 40 {
			t.Errorf("TOTAL gross = %v, want 40 (20 double-counted)", res.Totals.Total.Gross)
		}
		// The header metric uses the deduped transaction list, so it disagrees.
		if len(res.MatchedTxns) != 1 {
			t.Errorf("MatchedTxns = %d, want 1 — the metric and the table disagree",
				len(res.MatchedTxns))
		}

		res = mustBuild(t, rows, txns, Flags{FixCrossNightDoubleCount: true})
		if res.Totals.Total.Gross != 20 {
			t.Errorf("with fix: TOTAL gross = %v, want 20", res.Totals.Total.Gross)
		}
	},
}, {
	entry:     3,
	name:      "only the last refund against a charge survives",
	reference: "core/reconciliation.py:121-125",
	run: func(t *testing.T) {
		rows := []model.CanonicalTicket{ticket("TX1", 1)}
		txns := []model.PayPalTxn{
			charge("TX1", 30, -1.5),
			refund("R1", "TX1", -10, 0.5),
			refund("R2", "TX1", -20, 1.0),
		}

		res := mustBuild(t, rows, txns, Flags{})
		// Both refunds reach MatchedTxns, but only one is attributed to a night.
		if got := len(res.MatchedTxns); got != 3 {
			t.Fatalf("MatchedTxns = %d, want 3", got)
		}
		if got := res.Totals.Rows[0].Transactions; got != 2 {
			t.Errorf("night transactions = %d, want 2 (charge + ONE refund); "+
				"the dict keyed on reference id collapses the other", got)
		}

		res = mustBuild(t, rows, txns, Flags{FixMultiRefund: true})
		if got := res.Totals.Rows[0].Transactions; got != 3 {
			t.Errorf("with fix: night transactions = %d, want 3", got)
		}
	},
}, {
	entry:     4,
	name:      "partial-refund retained fee uses the inverted ratio",
	reference: "sections/reconciliation.py:202-203",
	run: func(t *testing.T) {
		orig := charge("TX1", 100, -3.0)
		// Refund 10 of a 100 order, with 0.30 of fee returned.
		part := refund("R1", "TX1", -10, 0.30)
		byID := map[string]model.PayPalTxn{"TX1": orig}

		// Reference: returned is scaled UP by 100/10 = 10x -> 3.0.
		// retained = 3.0 - 3.0 = 0.
		if got := RetainedFee(part, byID, Flags{}); got != 0 {
			t.Errorf("RetainedFee = %v, want 0 (fee scaled UP tenfold by the "+
				"inverted ratio)", got)
		}

		// Corrected: scaled DOWN by 10/100 -> 0.03; retained = 3.0 - 0.03.
		if got := RetainedFee(part, byID, Flags{FixRetainedFeeRatio: true}); got != 2.97 {
			t.Errorf("with fix: RetainedFee = %v, want 2.97", got)
		}

		// A FULL refund does not trip the guard, so both agree.
		full := refund("R2", "TX1", -100, 3.0)
		a := RetainedFee(full, byID, Flags{})
		b := RetainedFee(full, byID, Flags{FixRetainedFeeRatio: true})
		if a != b || a != 0 {
			t.Errorf("full refund: got %v and %v, want both 0", a, b)
		}
	},
}, {
	entry:     5,
	name:      "refunded and cancelled statuses are never re-admitted",
	reference: "core/reconciliation.py:12 vs :61",
	run: func(t *testing.T) {
		for _, status := range []string{"refunded", "cancelled", "canceled", "refund"} {
			rows := []model.CanonicalTicket{ticket("TX1", 1, withStatus(status))}

			ids := PayPalIDsForShow(rows, Flags{})
			if len(ids) != 0 {
				t.Errorf("status %q: ids = %v, want empty — Active excludes it and "+
					"the void re-admission set does not cover it", status, ids)
			}

			ids = PayPalIDsForShow(rows, Flags{FixStatusReadmit: true})
			if _, ok := ids["TX1"]; !ok {
				t.Errorf("status %q with fix: TX1 should be re-admitted", status)
			}
		}

		// "voided" IS re-admitted, which is the asymmetry.
		rows := []model.CanonicalTicket{ticket("TX1", 1, withStatus("voided"))}
		if _, ok := PayPalIDsForShow(rows, Flags{})["TX1"]; !ok {
			t.Error(`status "voided" should be re-admitted`)
		}
	},
}, {
	entry:     6,
	name:      "status is lowercased but not trimmed",
	reference: "core/reconciliation.py:13",
	run: func(t *testing.T) {
		rows := []model.CanonicalTicket{ticket("TX1", 1, withStatus(" void"))}

		if got := len(Active(rows, Flags{})); got != 1 {
			t.Errorf("Active kept %d rows, want 1: %q is lowercased but not trimmed, "+
				"so it never matches the exclusion set", got, " void")
		}
		// Case folding alone does work.
		upper := []model.CanonicalTicket{ticket("TX1", 1, withStatus("VOID"))}
		if got := len(Active(upper, Flags{})); got != 0 {
			t.Errorf(`Active kept %d rows for "VOID", want 0`, got)
		}

		if got := len(Active(rows, Flags{FixStatusTrim: true})); got != 0 {
			t.Errorf("with fix: Active kept %d rows, want 0", got)
		}
	},
}, {
	entry:     7,
	name:      "empty id set returns all PayPal transactions",
	reference: "core/reconciliation.py:92",
	run: func(t *testing.T) {
		// No ticket carries a usable PayPal id.
		rows := []model.CanonicalTicket{ticket("", 1), ticket("nan", 1)}
		txns := []model.PayPalTxn{charge("TX1", 10, -1), charge("TX2", 20, -2)}

		got := FilterPayPalForShow(rows, txns, Flags{})
		if len(got) != 2 {
			t.Errorf("got %d transactions, want all 2 — an empty id set falls back "+
				"to returning everything", len(got))
		}

		// And with an id set present, filtering is exact.
		rows = []model.CanonicalTicket{ticket("TX1", 1)}
		if got := FilterPayPalForShow(rows, txns, Flags{}); len(got) != 1 {
			t.Errorf("got %d transactions, want 1", len(got))
		}
	},
}, {
	entry:     8,
	name:      "TOTAL sums already-rounded rows",
	reference: "core/reconciliation.py:186-198",
	run: func(t *testing.T) {
		// Three nights whose gross each round up by a fraction of a cent.
		rows := []model.CanonicalTicket{ticket("TX1", 1), ticket("TX2", 2), ticket("TX3", 3)}
		txns := []model.PayPalTxn{
			charge("TX1", 10.005, 0),
			charge("TX2", 10.005, 0),
			charge("TX3", 10.005, 0),
		}

		res := mustBuild(t, rows, txns, Flags{})

		var sumOfRows float64
		for _, r := range res.Totals.Rows {
			sumOfRows += r.Gross
		}
		if res.Totals.Total.Gross != sumOfRows {
			t.Errorf("TOTAL gross = %v but the rounded rows sum to %v; the TOTAL must "+
				"be built from the ROUNDED rows, not the raw values",
				res.Totals.Total.Gross, sumOfRows)
		}
	},
}, {
	entry:     9,
	name:      "statistics TOTAL excludes no rows while the body drops null dates",
	reference: "core/reconciliation.py:158 vs :217-219",
	run: func(t *testing.T) {
		rows := []model.CanonicalTicket{
			ticket("TX1", 1),
			ticket("TX2", 1),
			ticket("TX3", 1, withNoPerformanceDate()),
		}

		res := mustBuild(t, rows, nil, Flags{})

		var bodyTickets int
		for _, r := range res.Statistics.Rows {
			bodyTickets += r.TotalTickets
		}
		if bodyTickets != 2 {
			t.Errorf("body tickets = %d, want 2 (the null-dated row is dropped)", bodyTickets)
		}
		if res.Statistics.Total.TotalTickets != 3 {
			t.Errorf("TOTAL tickets = %d, want 3 — the TOTAL counts ALL active rows, "+
				"so it exceeds the sum of the rows above it",
				res.Statistics.Total.TotalTickets)
		}

		res = mustBuild(t, rows, nil, Flags{FixNaTPerformanceDate: true})
		if res.Statistics.Total.TotalTickets != 2 {
			t.Errorf("with fix: TOTAL = %d, want 2 (agrees with the body)",
				res.Statistics.Total.TotalTickets)
		}
	},
}, {
	entry:     10,
	name:      "transactions column mixes transaction and ticket counts",
	reference: "core/reconciliation.py:175 vs :180",
	run: func(t *testing.T) {
		// Night 1 matches a PayPal charge; night 2 has three tickets and none.
		rows := []model.CanonicalTicket{
			ticket("TX1", 1),
			ticket("NOPE", 2), ticket("NOPE", 2), ticket("NOPE", 2),
		}
		txns := []model.PayPalTxn{charge("TX1", 10, -1)}

		res := mustBuild(t, rows, txns, Flags{})
		if got := res.Totals.Rows[0].Transactions; got != 1 {
			t.Errorf("matched night: Transactions = %d, want 1 (a transaction count)", got)
		}
		if got := res.Totals.Rows[1].Transactions; got != 3 {
			t.Errorf("unmatched night: Transactions = %d, want 3 — this is a TICKET "+
				"count in the same column", got)
		}
		if got := res.Totals.Total.Transactions; got != 4 {
			t.Errorf("TOTAL = %d, want 4 — two different units summed together", got)
		}

		res = mustBuild(t, rows, txns, Flags{FixTransactionUnits: true})
		if got := res.Totals.Rows[1].Transactions; got != 0 {
			t.Errorf("with fix: unmatched night Transactions = %d, want 0", got)
		}
	},
}, {
	entry:     11,
	name:      "fully voided night appears in metrics but has no totals row",
	reference: "core/reconciliation.py:158-168",
	run: func(t *testing.T) {
		rows := []model.CanonicalTicket{
			ticket("TX1", 1),
			// Every ticket for night 2 was voided as a transfer.
			ticket("TX2", 2, withStatus("voided"), withRefund(0)),
		}
		txns := []model.PayPalTxn{charge("TX1", 10, -1), charge("TX2", 20, -2)}

		res := mustBuild(t, rows, txns, Flags{})

		if len(res.Totals.Rows) != 1 {
			t.Errorf("got %d totals rows, want 1 — the fully-voided night produces no "+
				"row because the loop only visits nights with an ACTIVE ticket",
				len(res.Totals.Rows))
		}
		// But its charge is in the matched set feeding the header metrics.
		var found bool
		for _, tx := range res.MatchedTxns {
			if tx.TxnID == "TX2" {
				found = true
			}
		}
		if !found {
			t.Error("TX2 should still be in MatchedTxns, which is what makes the " +
				"header metrics disagree with the table")
		}
	},
}, {
	entry:     12,
	name:      "payPalOnly fails closed while the other filters fail open",
	reference: "core/reconciliation.py:16-20",
	unreachable: "The asymmetry only manifests when the _order_payment_type column is " +
		"ABSENT. mapping.build_canonical always emits it (defaulting to \"\"), " +
		"verified against the production canonical cache: all 2,562 records carry " +
		"all 15 keys. The Go model makes the field always present, so the branch " +
		"cannot be reached and there is no behavior to pin.",
}, {
	entry:     13,
	name:      "order refund amount is not converted from cents",
	reference: "mapping.py:104-105",
	run: func(t *testing.T) {
		// 1300 is 13.00 in cents, left unconverted by build_canonical. It is only
		// ever compared against zero, so the unit mismatch stays latent — but any
		// non-zero value must classify as refunded, not transferred.
		rows := []model.CanonicalTicket{
			ticket("TX1", 1, withStatus("voided"), withRefund(1300)),
			ticket("TX2", 1, withStatus("voided"), withRefund(0)),
		}

		transferred, refunded := splitVoided(rows, Flags{})
		if len(transferred) != 1 || transferred[0].PayPalTxnID != "TX2" {
			t.Errorf("transferred = %+v, want exactly TX2 (refund amount 0)", transferred)
		}
		if len(refunded) != 1 || refunded[0].PayPalTxnID != "TX1" {
			t.Errorf("refunded = %+v, want exactly TX1 (refund amount 1300 cents)", refunded)
		}

		// A sub-euro refund in cents must not be mistaken for zero.
		rows = []model.CanonicalTicket{ticket("TX3", 1, withStatus("voided"), withRefund(50))}
		if _, r := splitVoided(rows, Flags{}); len(r) != 1 {
			t.Error("a 50-cent refund must classify as refunded, not transferred")
		}
	},
}, {
	entry:     14,
	name:      "tickets-per-order divisor counts voided tickets",
	reference: "api/tickettailor.py:118-121",
	unreachable: "This happens in the Ticket Tailor ingestion layer, which computes " +
		"_order_total_paid by dividing the order total by a ticket count that " +
		"includes rows later filtered out. The field is carried on the canonical " +
		"record but is never read by the reconciliation maths, so there is no " +
		"reconciliation behavior to pin here. It belongs to the P2 importer port.",
}}

func TestLedgerBehaviors(t *testing.T) {
	for _, tc := range ledgerCases {
		t.Run(strconv.Itoa(tc.entry)+"_"+tc.name, func(t *testing.T) {
			if tc.run == nil {
				if tc.unreachable == "" {
					t.Fatalf("ledger entry %d has neither a test nor an explanation", tc.entry)
				}
				t.Skipf("documented as unreachable (%s): %s", tc.reference, tc.unreachable)
			}
			tc.run(t)
		})
	}
}

// TestLedgerCoverage keeps this file and docs/PARITY.md in lockstep.
func TestLedgerCoverage(t *testing.T) {
	const doc = "../../docs/PARITY.md"

	raw, err := os.ReadFile(doc)
	if err != nil {
		// docs/ is gitignored in this repo, so a checkout without it is
		// legitimate. Skip rather than fail so CI stays green.
		t.Skipf("cannot read %s: %v", doc, err)
	}

	rowRe := regexp.MustCompile(`(?m)^\|\s*(\d+)\s*\|`)
	matches := rowRe.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatalf("found no numbered ledger rows in %s; has the table format changed?", doc)
	}

	covered := make(map[int]bool, len(ledgerCases))
	for _, tc := range ledgerCases {
		if covered[tc.entry] {
			t.Errorf("duplicate ledger case for entry %d", tc.entry)
		}
		covered[tc.entry] = true
	}

	var documented int
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		documented++
		if !covered[n] {
			t.Errorf("docs/PARITY.md ledger entry %d has no case in ledgerCases", n)
		}
	}

	for _, tc := range ledgerCases {
		if tc.entry > documented {
			t.Errorf("ledgerCases entry %d (%s) is not in the docs/PARITY.md ledger",
				tc.entry, tc.name)
		}
	}

	t.Logf("%d ledger entries documented, %d with cases", documented, len(ledgerCases))
}

// TestFlagsDefaultToReferenceBehavior pins the most important invariant in the
// package: a zero-valued Flags means "behave exactly like the Python".
func TestFlagsDefaultToReferenceBehavior(t *testing.T) {
	var f Flags

	checks := map[string]bool{
		"FixUnmatchedDetection":    f.FixUnmatchedDetection,
		"FixCrossNightDoubleCount": f.FixCrossNightDoubleCount,
		"FixMultiRefund":           f.FixMultiRefund,
		"FixRetainedFeeRatio":      f.FixRetainedFeeRatio,
		"FixStatusReadmit":         f.FixStatusReadmit,
		"FixStatusTrim":            f.FixStatusTrim,
		"FixNaTPerformanceDate":    f.FixNaTPerformanceDate,
		"FixTransactionUnits":      f.FixTransactionUnits,
	}

	for name, on := range checks {
		if on {
			t.Errorf("%s defaults to true; every fix flag must default to false "+
				"so the zero value reproduces the reference (CLAUDE.md guardrail 2)", name)
		}
	}
}
