// Package money provides the rounding primitives required for bit-exact parity
// with the Python reconciliation implementation, plus the minor-unit type used
// for durable storage.
//
// The reference implementation applies TWO different rounding algorithms
// depending on the code path, and neither is math.Round. See CLAUDE.md
// guardrail 5 and docs/PARITY.md.
package money

import (
	"math"
	"strconv"
)

// RoundCPython rounds to 2 decimal places the way CPython's builtin round(x, 2)
// does: correct decimal rounding of the exact binary value, with ties resolved
// to even.
//
// Use this wherever the reference sums with builtin sum() over Python floats.
// In core/reconciliation.py those are the per-row Totals cells at :186-188,
// because `matched` is a list of dicts holding Python floats produced by
// float(...) in api/paypal.py:81-82.
//
// Implementation note: formatting to a fixed 2-decimal string and parsing back
// reproduces CPython's _Py_double_round, which is itself a correctly-rounded
// string round-trip (David Gay's algorithm). Go's strconv resolves exact
// halfway cases to even, matching CPython. Do NOT replace this with
// math.Round(x*100)/100 — that rounds half away from zero AND introduces a
// multiplication error before the rounding decision is made.
func RoundCPython(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 2, 64), 64)
	if err != nil {
		// Unreachable: FormatFloat always emits a parseable fixed-point string
		// for finite input, which the guard above has already ensured.
		return x
	}
	return r
}

// RoundNumpy rounds to 2 decimal places the way numpy.round(x, 2) does:
// multiply by 100, round-half-to-even, divide by 100.
//
// Use this wherever the reference rounds a pandas/numpy value rather than a
// Python float. In core/reconciliation.py that is the TOTAL row at :196-198
// (Series.sum().round(2)) and the no-PayPal fallback path at :177, where
// grp["revenue"].sum() yields a numpy.float64 and therefore dispatches
// round() to np.round rather than float.__round__.
//
// This is deliberately a different algorithm from RoundCPython. The two
// disagree on values where the x*100 multiplication is itself inexact, which is
// exactly the class of bug an epsilon comparison would hide.
func RoundNumpy(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	return math.RoundToEven(x*100) / 100
}

// SumSequential adds left to right, matching Python's builtin sum().
//
// Note that pandas' Series.sum() is NOT this — it uses pairwise summation and
// can differ by 1 ULP. Per docs/PARITY.md we implement sequential first and
// only port pairwise if a parity cell actually mismatches.
func SumSequential(xs []float64) float64 {
	var total float64
	for _, x := range xs {
		total += x
	}
	return total
}

// Money is an amount in minor units (cents). This is the durable storage
// representation and the API wire representation.
//
// It is deliberately NOT used in the parity path: reproducing the Python
// requires float64 arithmetic with the rounding helpers above, because the
// reference computes in floats and its results depend on that. Converting to
// minor units early would produce different — arguably more correct — numbers
// and break bit-exact comparison.
type Money int64

// Float returns the amount in major units. Lossy above 2^53 minor units.
func (m Money) Float() float64 { return float64(m) / 100 }

// String renders the amount in major units with 2 decimal places, without a
// currency symbol. Currency travels alongside Money, never inside it.
func (m Money) String() string { return strconv.FormatFloat(m.Float(), 'f', 2, 64) }

// FromFloat converts major units to minor units, rounding half to even to stay
// consistent with the rounding helpers above.
func FromFloat(f float64) Money {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return Money(math.RoundToEven(f * 100))
}
