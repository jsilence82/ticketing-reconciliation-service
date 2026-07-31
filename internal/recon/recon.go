// Package recon is the port of the dashboard's reconciliation matching rule,
// plus the per-resource classification this service exists to produce.
//
// It is a PURE library: plain slices in, plain structs out. It must not import
// any storage, HTTP, or provider-client package. That constraint is what makes
// the parity harness possible and what makes CLAUDE.md guardrail 2
// mechanically enforceable, so it is enforced by depguard in .golangci.yml
// rather than by convention.
//
// # What ships and what does not
//
//   - Classify (classify.go) is the product. It assigns matched / unmatched /
//     transferred / pending per resource, which is what the service stores and
//     what GET /events exposes.
//   - The Totals and Statistics builders live in recon/oracle and are a
//     VERIFICATION ORACLE, not a product surface. They exist to diff against the
//     dashboard's sheets (guardrail 1). No endpoint may serve them.
//
// # Fidelity
//
// The matching rule reproduces core/reconciliation.py bug-for-bug. Behaviors
// that look like mistakes are catalogued in docs/PARITY.md and replicated
// deliberately; each has an opt-in fix flag that defaults to off.
//
// # A note on the reference's column guards
//
// The Python checks whether columns such as "status" and "_order_payment_type"
// exist before using them, with inconsistent fallbacks. Those branches are
// unreachable in practice: mapping.build_canonical always emits every canonical
// column, defaulting status to "unknown" and the order fields to ""/0. Verified
// against the production canonical cache — all 2,562 records carry all 15 keys.
// The Go types therefore make those fields always present, and ledger entry 12
// is documented as unreachable rather than modelled.
package recon

import (
	"math"
	"strings"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/money"
)

// statusExcluded is the set dropped by Active. Note it contains six values
// while voidReadmitted contains two — that asymmetry is ledger entry 5.
var statusExcluded = map[string]bool{
	"void": true, "voided": true,
	"refund": true, "refunded": true,
	"cancelled": true, "canceled": true,
}

// voidReadmitted is the set whose PayPal charges are pulled back in despite
// having been excluded by Active.
var voidReadmitted = map[string]bool{"void": true, "voided": true}

// Flags selects deviations from the reference implementation's behavior.
//
// EVERY flag defaults to false, meaning "reproduce the Python exactly, bug and
// all". Each corresponds to a numbered entry in the behavior ledger in
// docs/PARITY.md. Turning one on is a deliberate, documented divergence — never
// an incidental cleanup (CLAUDE.md guardrail 2).
type Flags struct {
	// FixUnmatchedDetection (ledger 1) computes unmatched PayPal transactions
	// against the full transaction list instead of against the already-filtered
	// subset. Applies to the oracle only: Classify always detects correctly,
	// because it is new behavior with no reference to preserve.
	FixUnmatchedDetection bool

	// FixCrossNightDoubleCount (ledger 2) counts a transaction once across the
	// whole report rather than once per performance date.
	//
	// Attribution then falls to the EARLIEST night the transaction appears in.
	// That makes the TOTAL correct but concentrates a cross-night order on one
	// night; distributing it proportionally instead is a different accounting
	// decision that needs treasury sign-off before it is adopted.
	FixCrossNightDoubleCount bool

	// FixMultiRefund (ledger 3) keeps every refund against an original charge
	// rather than only the last one encountered.
	FixMultiRefund bool

	// FixRetainedFeeRatio (ledger 4) inverts the partial-refund ratio back to
	// rfnd_gross/orig_gross, so a partial refund's returned fee is scaled DOWN
	// to the refunded portion as the reference's own comment intends.
	FixRetainedFeeRatio bool

	// FixStatusReadmit (ledger 5) re-admits refunded and cancelled tickets to
	// the PayPal id set, not only voided ones.
	FixStatusReadmit bool

	// FixStatusTrim (ledger 6) trims whitespace before comparing status, so
	// " void" is not treated as active.
	FixStatusTrim bool

	// FixNaTPerformanceDate (ledger 9) makes the Statistics TOTAL agree with
	// the sum of its rows by counting only rows that reached the body.
	FixNaTPerformanceDate bool

	// FixTransactionUnits (ledger 10) reports a transaction count in the
	// Transactions column even on the fallback path, instead of a ticket count.
	FixTransactionUnits bool
}

// normStatus lowercases a status for comparison.
//
// The reference lowercases but does NOT trim, so " void" compares unequal to
// "void" and survives as active. Trimming is opt-in via FixStatusTrim.
func normStatus(s string, f Flags) string {
	if f.FixStatusTrim {
		s = strings.TrimSpace(s)
	}
	return strings.ToLower(s)
}

// NormTxnID trims a ticket-side PayPal id and reports whether it is usable.
//
// The sentinels "" and "nan" are rejected because build_canonical stringifies
// before dropping nulls, turning a missing id into the literal "nan". Note the
// reference does NOT reject "None" or "NaT"; that is replicated.
func NormTxnID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "nan" {
		return "", false
	}
	return s, true
}

// Active drops voided, refunded and cancelled rows.
func Active(rows []model.CanonicalTicket, f Flags) []model.CanonicalTicket {
	out := make([]model.CanonicalTicket, 0, len(rows))
	for _, r := range rows {
		if !statusExcluded[normStatus(r.Status, f)] {
			out = append(out, r)
		}
	}
	return out
}

// PayPalOnly keeps only tickets whose order was paid via PayPal.
//
// Note the reference's asymmetric fail-safe: when the payment-type column is
// absent this returns EMPTY, whereas Active and ExcludeOperator return their
// input unchanged (ledger 12). That branch is unreachable here — see the package
// comment.
func PayPalOnly(rows []model.CanonicalTicket, f Flags) []model.CanonicalTicket {
	out := make([]model.CanonicalTicket, 0, len(rows))
	for _, r := range rows {
		if normStatus(r.OrderPaymentType, f) == "paypal" {
			out = append(out, r)
		}
	}
	return out
}

// ExcludeOperator drops tickets recorded manually by box-office staff, which
// never pass through PayPal.
func ExcludeOperator(rows []model.CanonicalTicket, f Flags) []model.CanonicalTicket {
	out := make([]model.CanonicalTicket, 0, len(rows))
	for _, r := range rows {
		if normStatus(r.OrderPaymentType, f) != "operator" {
			out = append(out, r)
		}
	}
	return out
}

// isVoidReadmitted reports whether a status is re-admitted to the id set after
// Active excluded it.
func isVoidReadmitted(status string, f Flags) bool {
	s := normStatus(status, f)
	if f.FixStatusReadmit {
		return statusExcluded[s]
	}
	return voidReadmitted[s]
}

// PayPalIDsForShow builds the set of PayPal transaction ids belonging to a
// show: active PayPal-paid tickets, plus voided PayPal tickets both with and
// without refunds.
func PayPalIDsForShow(rows []model.CanonicalTicket, f Flags) map[string]struct{} {
	ids := make(map[string]struct{})

	for _, r := range Active(PayPalOnly(rows, f), f) {
		if id, ok := NormTxnID(r.PayPalTxnID); ok {
			ids[id] = struct{}{}
		}
	}

	// Voided PayPal tickets are added back regardless of refund state: without
	// a refund the original charge is still valid (a Ticket Tailor transfer),
	// and with one, both the charge and its refund need to be in scope.
	for _, r := range rows {
		if !isVoidReadmitted(r.Status, f) || normStatus(r.OrderPaymentType, f) != "paypal" {
			continue
		}
		if id, ok := NormTxnID(r.PayPalTxnID); ok {
			ids[id] = struct{}{}
		}
	}

	return ids
}

// FilterPayPalForShow applies the matching rule: keep a transaction whose TxnID
// OR PayPalReferenceID is in the show's id set.
//
// Comparison is exact and case-sensitive. The ticket side is trimmed; the
// PayPal side is NOT normalized at all, and that asymmetry is deliberate.
//
// When the id set is empty the reference returns ALL transactions as a fallback
// (ledger 7). Classify deliberately does not inherit that.
func FilterPayPalForShow(rows []model.CanonicalTicket, txns []model.PayPalTxn, f Flags) []model.PayPalTxn {
	if len(txns) == 0 {
		return nil
	}

	ids := PayPalIDsForShow(rows, f)
	if len(ids) == 0 {
		return txns
	}

	out := make([]model.PayPalTxn, 0, len(txns))
	for _, tx := range txns {
		_, byID := ids[tx.TxnID]
		_, byRef := ids[tx.PayPalReferenceID]
		if byID || byRef {
			out = append(out, tx)
		}
	}
	return out
}

// SplitVoided partitions voided PayPal tickets into transfers (no refund
// issued, so the original charge stands) and refunded ones.
func SplitVoided(rows []model.CanonicalTicket, f Flags) (transferred, refunded []model.CanonicalTicket) {
	for _, r := range rows {
		if !isVoidReadmitted(r.Status, f) || normStatus(r.OrderPaymentType, f) != "paypal" {
			continue
		}
		if r.OrderRefundAmount == 0 {
			transferred = append(transferred, r)
		} else {
			refunded = append(refunded, r)
		}
	}
	return transferred, refunded
}

// RetainedFee computes the fee PayPal kept on a refund.
//
// The reference scales the returned fee by orig_gross/rfnd_gross, which its own
// guard forces to be greater than one — so on a PARTIAL refund the fee is
// scaled UP rather than down, and the result can go negative. That feeds the
// pass/fail verdict, so it is replicated unless FixRetainedFeeRatio is set
// (ledger 4).
//
// On a full refund the guard does not fire and both variants agree.
func RetainedFee(refund model.PayPalTxn, byID map[string]model.PayPalTxn, f Flags) float64 {
	orig := byID[refund.PayPalReferenceID] // zero value when absent, as in Python

	origFee := math.Abs(orig.Fee)
	origGross := orig.Gross
	rfndGross := math.Abs(refund.Gross)
	returned := refund.Fee

	if origGross != 0 && rfndGross != 0 && origGross > rfndGross {
		if f.FixRetainedFeeRatio {
			returned = returned * rfndGross / origGross
		} else {
			returned = returned * origGross / rfndGross
		}
	}

	return money.RoundCPython(origFee - returned)
}
