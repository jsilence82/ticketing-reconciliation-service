// Package recon is the port of the dashboard's reconciliation logic.
//
// It is a PURE library: plain slices in, plain structs out. It must not import
// any storage, HTTP, or provider-client package. That constraint is what makes
// the parity harness possible and what makes CLAUDE.md guardrail 2
// mechanically enforceable, so it is enforced by depguard in .golangci.yml
// rather than by convention.
//
// # Fidelity
//
// This reproduces core/reconciliation.py bug-for-bug. Behaviors that look like
// mistakes are catalogued in docs/PARITY.md and are replicated deliberately;
// each has an opt-in fix flag that defaults to off.
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
	"errors"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/money"
)

// ErrNotImplemented is retained for callers that still reference it.
var ErrNotImplemented = errors.New("recon: not implemented (see docs/PARITY.md)")

// dateLabelFormat renders a performance night, e.g. "Fri 12 Jul 2024".
//
// Always applied in UTC: performance_date comes from the event's unix start
// parsed with utc=True, so the reference labels a 23:00 local performance with
// the following day. That is replicated, not corrected.
const dateLabelFormat = "Mon 02 Jan 2006"

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
	// subset. The reference's version is a tautology over its own input and is
	// provably always empty whenever any ticket carries a PayPal id.
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

// TotalsRow mirrors one row of the dashboard's Totals sheet.
//
// Values are float64 rather than a minor-unit type because parity requires
// reproducing the reference's float arithmetic exactly; see internal/money.
type TotalsRow struct {
	// PerformanceDate is the formatted label, e.g. "Fri 12 Jul 2024".
	PerformanceDate string
	// Transactions counts PayPal transactions for matched groups but TICKETS
	// for unmatched groups — see ledger entry 10.
	Transactions int
	Gross        float64
	Fees         float64
	Net          float64
}

// Totals is the Totals sheet: body rows plus the appended TOTAL row.
type Totals struct {
	Rows []TotalsRow
	// Total sums the ALREADY-ROUNDED body rows (ledger entry 8), so it can
	// drift from a sum-then-round result.
	Total TotalsRow
}

// StatisticsRow mirrors one row of the dashboard's Statistics sheet.
type StatisticsRow struct {
	PerformanceDate string
	TotalTickets    int
	// ByCategory is keyed by category name; iterate it in Categories order.
	ByCategory map[string]int
}

// Statistics is the Statistics sheet: body rows plus the appended TOTAL row.
type Statistics struct {
	Rows []StatisticsRow
	// Categories is the sorted column order. Go's sort.Strings is bytewise over
	// UTF-8, which preserves the code-point ordering Python's sorted() produces.
	Categories []string
	// Total is computed over ALL active rows, including those dropped from the
	// body for having no performance date, so it need not equal the sum of
	// Rows — see ledger entry 9.
	Total StatisticsRow
}

// Result is the full output of a reconciliation run.
type Result struct {
	Totals     Totals
	Statistics Statistics
	// MatchedTxns are the PayPal transactions attributed to this show.
	MatchedTxns []model.PayPalTxn
	// Unmatched is always empty under the reference's behavior unless
	// Flags.FixUnmatchedDetection is set — see ledger entry 1.
	Unmatched []model.PayPalTxn
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

// normTxnID trims a ticket-side PayPal id and reports whether it is usable.
//
// The sentinels "" and "nan" are rejected because build_canonical stringifies
// before dropping nulls, turning a missing id into the literal "nan". Note the
// reference does NOT reject "None" or "NaT"; that is replicated.
func normTxnID(s string) (string, bool) {
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
		if id, ok := normTxnID(r.PayPalTxnID); ok {
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
		if id, ok := normTxnID(r.PayPalTxnID); ok {
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
// (ledger 7).
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

// Build runs a full reconciliation for one show.
//
// Passing an empty showFilter reconciles across all shows, matching the
// reference's behavior when no show is selected.
func Build(rows []model.CanonicalTicket, txns []model.PayPalTxn, showFilter string, f Flags) (Result, error) {
	showRows := rows
	if showFilter != "" {
		showRows = make([]model.CanonicalTicket, 0, len(rows))
		for _, r := range rows {
			if r.Show == showFilter {
				showRows = append(showRows, r)
			}
		}
	}

	work := Active(ExcludeOperator(showRows, f), f)
	showTxns := FilterPayPalForShow(showRows, txns, f)

	// Last-writer-wins on both maps, matching Python dict comprehension
	// semantics. For refundByOrig that collapse is ledger entry 3.
	txnByID := make(map[string]model.PayPalTxn, len(showTxns))
	refundByOrig := make(map[string][]model.PayPalTxn)
	for _, tx := range showTxns {
		txnByID[tx.TxnID] = tx
		if tx.PayPalReferenceID != "" {
			if f.FixMultiRefund {
				refundByOrig[tx.PayPalReferenceID] = append(refundByOrig[tx.PayPalReferenceID], tx)
			} else {
				refundByOrig[tx.PayPalReferenceID] = []model.PayPalTxn{tx}
			}
		}
	}

	transferredVoided, refundedTickets := splitVoided(showRows, f)

	// seenAcross backs FixCrossNightDoubleCount. The reference scopes its seen
	// set per group, which is why a cross-night order is counted twice.
	seenAcross := make(map[string]bool)

	totals := Totals{}
	var statsRows []StatisticsRow

	categories := distinctCategories(work)

	for _, g := range groupByPerformanceDate(work) {
		vgrp := rowsOnDate(transferredVoided, g.date)
		rgrp := rowsOnDate(refundedTickets, g.date)

		var (
			matched  []model.PayPalTxn
			hadMatch bool
		)
		if len(showTxns) > 0 {
			matched, hadMatch = matchedTxnsForGroup(
				[][]model.CanonicalTicket{g.rows, vgrp, rgrp},
				txnByID, refundByOrig, seenAcross, f,
			)
		}

		row := TotalsRow{PerformanceDate: g.date.UTC().Format(dateLabelFormat)}

		if hadMatch {
			// Python's builtin sum() over floats, which since CPython 3.12
			// applies Neumaier compensation; then float.__round__.
			gross := make([]float64, len(matched))
			fees := make([]float64, len(matched))
			nets := make([]float64, len(matched))
			for i, tx := range matched {
				gross[i], fees[i], nets[i] = tx.Gross, tx.Fee, tx.Net
			}
			row.Transactions = len(matched)
			row.Gross = money.RoundCPython(money.SumPythonBuiltin(gross))
			row.Fees = money.RoundCPython(money.SumPythonBuiltin(fees))
			row.Net = money.RoundCPython(money.SumPythonBuiltin(nets))
		} else {
			// pandas Series.sum() -> numpy pairwise; then np.round.
			revenue := make([]float64, len(g.rows))
			quantity := make([]float64, len(g.rows))
			for i, r := range g.rows {
				revenue[i], quantity[i] = r.Revenue, r.Quantity
			}
			total := money.SumPandas(revenue)
			row.Gross = money.RoundNumpy(total)
			row.Fees = money.RoundNumpy(0)
			row.Net = money.RoundNumpy(total)

			if f.FixTransactionUnits {
				row.Transactions = len(matched) // consistently a transaction count
			} else {
				row.Transactions = int(money.SumPandas(quantity)) // a TICKET count
			}
		}

		totals.Rows = append(totals.Rows, row)
		statsRows = append(statsRows, statisticsRow(g, categories))
	}

	totals.Total = totalsRow(totals.Rows)

	stats := Statistics{
		Rows:       statsRows,
		Categories: categories,
		Total:      statisticsTotal(work, statsRows, categories, f),
	}

	return Result{
		Totals:      totals,
		Statistics:  stats,
		MatchedTxns: showTxns,
		Unmatched:   unmatchedTxns(showRows, txns, showTxns, f),
	}, nil
}

// splitVoided partitions voided PayPal tickets into transfers (no refund
// issued, so the original charge stands) and refunded ones.
func splitVoided(rows []model.CanonicalTicket, f Flags) (transferred, refunded []model.CanonicalTicket) {
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

// dateGroup is one performance night's rows, in original order.
type dateGroup struct {
	date time.Time
	rows []model.CanonicalTicket
}

// groupByPerformanceDate reproduces pandas groupby(..., sort=True): groups are
// ordered by date ascending, and rows with no performance date are DROPPED
// (pandas' dropna=True default). That drop is why the reference's "Unknown"
// label is dead code, and it is half of ledger entry 9.
func groupByPerformanceDate(rows []model.CanonicalTicket) []dateGroup {
	byDate := make(map[int64][]model.CanonicalTicket)
	for _, r := range rows {
		if r.PerformanceDate.IsZero() {
			continue
		}
		k := r.PerformanceDate.UTC().UnixNano()
		byDate[k] = append(byDate[k], r)
	}

	keys := make([]int64, 0, len(byDate))
	for k := range byDate {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	groups := make([]dateGroup, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, dateGroup{
			date: time.Unix(0, k).UTC(),
			rows: byDate[k],
		})
	}
	return groups
}

// rowsOnDate selects rows whose performance date equals the group's, matching
// the reference's equality filter on the groupby key.
func rowsOnDate(rows []model.CanonicalTicket, date time.Time) []model.CanonicalTicket {
	var out []model.CanonicalTicket
	for _, r := range rows {
		if !r.PerformanceDate.IsZero() && r.PerformanceDate.UTC().Equal(date) {
			out = append(out, r)
		}
	}
	return out
}

// matchedTxnsForGroup attributes PayPal transactions to one performance night.
//
// The reference scopes `seen` to a single call, so a transaction covering
// tickets on two nights is appended to both and counted twice (ledger 2).
// FixCrossNightDoubleCount promotes that set to report scope.
// It also reports whether the group resolved to any transaction at all, which
// is NOT the same as len(result) > 0 once FixCrossNightDoubleCount is on: a
// night whose only transaction was already counted elsewhere has resolved a
// match but contributes nothing. Without that distinction the caller would fall
// through to the revenue fallback and invent a figure for the second night.
//
// Under the default flags the two are equivalent, because `seen` starts empty
// for every group and the first matching id therefore always appends.
func matchedTxnsForGroup(
	groups [][]model.CanonicalTicket,
	txnByID map[string]model.PayPalTxn,
	refundByOrig map[string][]model.PayPalTxn,
	seenAcross map[string]bool,
	f Flags,
) (result []model.PayPalTxn, hadMatch bool) {
	seen := make(map[string]bool)
	if f.FixCrossNightDoubleCount {
		seen = seenAcross
	}

	for _, g := range groups {
		for _, r := range g {
			pid, ok := normTxnID(r.PayPalTxnID)
			if !ok {
				continue
			}

			if tx, found := txnByID[pid]; found {
				hadMatch = true
				if !seen[pid] {
					result = append(result, tx)
					seen[pid] = true
				}
			}

			for _, rf := range refundByOrig[pid] {
				hadMatch = true
				if !seen[rf.TxnID] {
					result = append(result, rf)
					seen[rf.TxnID] = true
				}
			}
		}
	}
	return result, hadMatch
}

// totalsRow builds the appended TOTAL row by summing the already-rounded body
// rows with pandas semantics, then rounding again (ledger 8).
func totalsRow(rows []TotalsRow) TotalsRow {
	gross := make([]float64, len(rows))
	fees := make([]float64, len(rows))
	nets := make([]float64, len(rows))
	txns := 0
	for i, r := range rows {
		gross[i], fees[i], nets[i] = r.Gross, r.Fees, r.Net
		txns += r.Transactions
	}

	return TotalsRow{
		PerformanceDate: "Transactions Total",
		Transactions:    txns,
		Gross:           money.RoundNumpy(money.SumPandas(gross)),
		Fees:            money.RoundNumpy(money.SumPandas(fees)),
		Net:             money.RoundNumpy(money.SumPandas(nets)),
	}
}

// distinctCategories returns the sorted category set, matching Python's
// sorted() over unique values.
func distinctCategories(rows []model.CanonicalTicket) []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range rows {
		if !seen[r.Category] {
			seen[r.Category] = true
			out = append(out, r.Category)
		}
	}
	sort.Strings(out)
	return out
}

// sumQuantity totals a quantity column with pandas semantics and truncates,
// matching Python's int() on a numpy float.
func sumQuantity(rows []model.CanonicalTicket) int {
	q := make([]float64, len(rows))
	for i, r := range rows {
		q[i] = r.Quantity
	}
	return int(money.SumPandas(q))
}

func statisticsRow(g dateGroup, categories []string) StatisticsRow {
	row := StatisticsRow{
		PerformanceDate: g.date.UTC().Format(dateLabelFormat),
		TotalTickets:    sumQuantity(g.rows),
		ByCategory:      make(map[string]int, len(categories)),
	}
	for _, cat := range categories {
		var inCat []model.CanonicalTicket
		for _, r := range g.rows {
			if r.Category == cat {
				inCat = append(inCat, r)
			}
		}
		row.ByCategory[cat] = sumQuantity(inCat)
	}
	return row
}

// statisticsTotal builds the Statistics TOTAL row.
//
// The reference computes it over ALL active rows rather than over the rows that
// reached the body, so when a ticket has no performance date the TOTAL exceeds
// the sum of the rows above it (ledger 9).
func statisticsTotal(
	work []model.CanonicalTicket,
	rows []StatisticsRow,
	categories []string,
	f Flags,
) StatisticsRow {
	total := StatisticsRow{
		PerformanceDate: "Total",
		ByCategory:      make(map[string]int, len(categories)),
	}

	if f.FixNaTPerformanceDate {
		for _, r := range rows {
			total.TotalTickets += r.TotalTickets
			for _, cat := range categories {
				total.ByCategory[cat] += r.ByCategory[cat]
			}
		}
		return total
	}

	total.TotalTickets = sumQuantity(work)
	for _, cat := range categories {
		var inCat []model.CanonicalTicket
		for _, r := range work {
			if r.Category == cat {
				inCat = append(inCat, r)
			}
		}
		total.ByCategory[cat] = sumQuantity(inCat)
	}
	return total
}

// unmatchedTxns surfaces PayPal transactions with no Ticket Tailor counterpart.
//
// The reference filters showTxns — which was itself produced by exactly this
// predicate — against the same id set, so the result is provably empty whenever
// the id set is non-empty, and is everything when it is empty (ledger 1).
// FixUnmatchedDetection evaluates against the full transaction list instead,
// which is what the business contract actually asks for.
func unmatchedTxns(
	rows []model.CanonicalTicket,
	all []model.PayPalTxn,
	showTxns []model.PayPalTxn,
	f Flags,
) []model.PayPalTxn {
	candidates := showTxns
	if f.FixUnmatchedDetection {
		candidates = all
	}
	if len(candidates) == 0 {
		return nil
	}

	ids := PayPalIDsForShow(rows, f)

	var out []model.PayPalTxn
	for _, tx := range candidates {
		_, byID := ids[tx.TxnID]
		_, byRef := ids[tx.PayPalReferenceID]
		if !byID && !byRef {
			out = append(out, tx)
		}
	}
	return out
}
