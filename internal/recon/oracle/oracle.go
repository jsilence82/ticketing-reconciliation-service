// Package oracle rebuilds the dashboard's Totals and Statistics sheets.
//
// # This is a verification oracle, not a product surface
//
// The service deliberately does not produce aggregated views — Totals and
// Statistics are computed on demand by consumers from raw GET /events data (see
// CLAUDE.md, Persistence & read model). Nothing here may be wired to an HTTP
// handler or an MCP tool.
//
// It exists because guardrail 1 forbids pointing the service at live webhook
// subscriptions until its output has been diffed against the dashboard's
// existing results — and those results ARE these two sheets. They are the only
// artefact there is to compare against. So the aggregation must exist in Go to
// prove the matching rule is right, even though the matching rule is the only
// part that ships.
//
// Do not delete this package for being unreferenced by any endpoint. Deleting it
// forfeits the parity evidence and leaves guardrail 1 unsatisfiable.
package oracle

import (
	"slices"
	"sort"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/money"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/recon"
)

// dateLabelFormat renders a performance night, e.g. "Fri 12 Jul 2024".
//
// Always applied in UTC: performance_date comes from the event's unix start
// parsed with utc=True, so the reference labels a 23:00 local performance with
// the following day. That is replicated, not corrected.
const dateLabelFormat = "Mon 02 Jan 2006"

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

// Result is the full output of an oracle run.
type Result struct {
	Totals     Totals
	Statistics Statistics
	// MatchedTxns are the PayPal transactions attributed to this show.
	MatchedTxns []model.PayPalTxn
	// Unmatched is always empty under the reference's behavior unless
	// Flags.FixUnmatchedDetection is set — see ledger entry 1. For the truthful
	// answer, use recon.Classify instead; this field exists to reproduce the
	// reference, not to be useful.
	Unmatched []model.PayPalTxn
}

// Build reconstructs both sheets for one show.
//
// Passing an empty showFilter reconciles across all shows, matching the
// reference's behavior when no show is selected.
func Build(rows []model.CanonicalTicket, txns []model.PayPalTxn, showFilter string, f recon.Flags) (Result, error) {
	showRows := rows
	if showFilter != "" {
		showRows = make([]model.CanonicalTicket, 0, len(rows))
		for _, r := range rows {
			if r.Show == showFilter {
				showRows = append(showRows, r)
			}
		}
	}

	work := recon.Active(recon.ExcludeOperator(showRows, f), f)
	showTxns := recon.FilterPayPalForShow(showRows, txns, f)

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

	transferredVoided, refundedTickets := recon.SplitVoided(showRows, f)

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
//
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
	f recon.Flags,
) (result []model.PayPalTxn, hadMatch bool) {
	seen := make(map[string]bool)
	if f.FixCrossNightDoubleCount {
		seen = seenAcross
	}

	for _, g := range groups {
		for _, r := range g {
			pid, ok := recon.NormTxnID(r.PayPalTxnID)
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
	f recon.Flags,
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

// unmatchedTxns reproduces the reference's unmatched detection.
//
// The reference filters showTxns — which was itself produced by exactly this
// predicate — against the same id set, so the result is provably empty whenever
// the id set is non-empty, and is everything when it is empty (ledger 1).
// FixUnmatchedDetection evaluates against the full transaction list instead.
//
// For the truthful per-transaction answer, use recon.Classify. This function
// exists to reproduce a known-broken behavior for parity.
func unmatchedTxns(
	rows []model.CanonicalTicket,
	all []model.PayPalTxn,
	showTxns []model.PayPalTxn,
	f recon.Flags,
) []model.PayPalTxn {
	candidates := showTxns
	if f.FixUnmatchedDetection {
		candidates = all
	}
	if len(candidates) == 0 {
		return nil
	}

	ids := recon.PayPalIDsForShow(rows, f)

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
