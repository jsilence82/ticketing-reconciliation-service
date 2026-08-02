// Package money provides the rounding primitives required for bit-exact parity
// with the Python reconciliation implementation, plus the minor-unit type used
// for durable storage.
//
// The reference implementation applies TWO different rounding algorithms
// depending on the code path, and neither is math.Round. See docs/PARITY.md,
// "Float and rounding parity".
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

// SumSequential adds strictly left to right with no compensation.
//
// This matches NEITHER of the reference's two summation sites. It is kept as
// the naive baseline the other two are contrasted against in tests, and as the
// thing you must not reach for by reflex when porting a Python sum().
func SumSequential(xs []float64) float64 {
	var total float64
	for _, x := range xs {
		total += x
	}
	return total
}

// SumPythonBuiltin adds the way CPython's builtin sum() does over floats.
//
// Since CPython 3.12, sum() does NOT accumulate naively: it applies
// Kahan-Babuska-Neumaier compensated summation to floats. The dashboard runs
// 3.13.6, so `sum(tx["gross"] for tx in matched)` at
// core/reconciliation.py:172-174 is compensated, and a naive loop produces
// different bits.
//
// This is version-sensitive in a way worth flagging: the same dashboard code
// on CPython 3.11 would produce naive-summation results. The goldens are
// therefore only valid against the pinned interpreter, which is why
// docs/PARITY.md records it.
func SumPythonBuiltin(xs []float64) float64 {
	var (
		total float64 // running sum, CPython's f_result
		comp  float64 // compensation term, CPython's c
	)

	for _, x := range xs {
		t := total + x
		// Accumulate the low-order bits lost by the addition above, taking the
		// branch on relative magnitude exactly as CPython does.
		if math.Abs(total) >= math.Abs(x) {
			comp += (total - t) + x
		} else {
			comp += (x - t) + total
		}
		total = t
	}

	return total + comp
}

// pairwiseBlockSize mirrors numpy's PW_BLOCKSIZE. Above it, numpy splits the
// input recursively; at or below it, numpy uses eight running accumulators.
const pairwiseBlockSize = 128

// SumPandas adds the way pandas' Series.sum() does, which is numpy's pairwise
// summation rather than a left-to-right loop.
//
// This is NOT interchangeable with SumSequential. Over the fixture in
// testdata/sums.json the two disagree on roughly half the arrays, and a
// one-ULP disagreement survives rounding often enough to move a cent.
//
// Use it at the sites where the reference reduces a pandas object:
// grp["revenue"].sum() on the no-PayPal fallback path
// (core/reconciliation.py:177) and the TOTAL row (:196-198). Use
// SumSequential where the reference uses Python's builtin sum() over a list of
// floats (:172-174).
//
// SSG runs 8-9 performances, which lands exactly on the threshold where numpy
// switches from a simple loop to eight accumulators, so the distinction is
// live rather than theoretical.
func SumPandas(xs []float64) float64 {
	return pairwiseSum(xs)
}

// pairwiseSum reproduces numpy's npy_pairwise_sum for contiguous float64.
func pairwiseSum(a []float64) float64 {
	n := len(a)

	if n < 8 {
		var res float64
		for _, x := range a {
			res += x
		}
		return res
	}

	if n <= pairwiseBlockSize {
		// Eight independent accumulators, seeded with the first eight
		// elements. Reassociating this into a single running total changes the
		// result, which is the entire point.
		r := [8]float64{a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7]}

		i := 8
		for ; i < n-(n%8); i += 8 {
			r[0] += a[i+0]
			r[1] += a[i+1]
			r[2] += a[i+2]
			r[3] += a[i+3]
			r[4] += a[i+4]
			r[5] += a[i+5]
			r[6] += a[i+6]
			r[7] += a[i+7]
		}

		// The specific bracketing here is load-bearing.
		res := ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]))

		for ; i < n; i++ {
			res += a[i]
		}
		return res
	}

	// Split on a multiple of eight so each half hits the accumulator path in
	// the same alignment numpy would use.
	n2 := n / 2
	n2 -= n2 % 8
	return pairwiseSum(a[:n2]) + pairwiseSum(a[n2:])
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
