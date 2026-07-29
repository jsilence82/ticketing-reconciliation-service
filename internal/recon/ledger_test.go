package recon

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The behavior ledger in docs/PARITY.md enumerates the reference
// implementation's accidental-but-deterministic behaviors. Every one of them
// must be reproduced bug-for-bug in v1 (CLAUDE.md guardrail 2).
//
// This file gives each ledger entry a test case up front, skipped until the
// corresponding logic lands. The point is that a ledger entry with no test is a
// visible gap rather than an oversight discovered during a treasury audit.
//
// TestLedgerCoverage below fails if docs/PARITY.md grows an entry that has no
// case here, so the two cannot drift apart silently.

type ledgerCase struct {
	// entry is the row number in the docs/PARITY.md behavior ledger.
	entry int
	// name describes the behavior being pinned.
	name string
	// reference points at the line(s) in the Python that produce it.
	reference string
	// run asserts the reference behavior. Nil means "not written yet".
	run func(t *testing.T)
}

var ledgerCases = []ledgerCase{
	{
		entry:     1,
		name:      "unmatched detection is a tautology and yields nothing",
		reference: "core/reconciliation.py:226-232",
	},
	{
		entry:     2,
		name:      "order spanning two nights is counted in full into both",
		reference: "core/reconciliation.py:142,169",
	},
	{
		entry:     3,
		name:      "only the last refund against a charge survives",
		reference: "core/reconciliation.py:121-125",
	},
	{
		entry:     4,
		name:      "partial-refund retained fee uses the inverted ratio",
		reference: "sections/reconciliation.py:202-203",
	},
	{
		entry:     5,
		name:      "refunded and cancelled statuses are never re-admitted",
		reference: "core/reconciliation.py:12 vs :61",
	},
	{
		entry:     6,
		name:      "status is lowercased but not trimmed",
		reference: "core/reconciliation.py:13",
	},
	{
		entry:     7,
		name:      "empty id set returns all PayPal transactions",
		reference: "core/reconciliation.py:92",
	},
	{
		entry:     8,
		name:      "TOTAL sums already-rounded rows",
		reference: "core/reconciliation.py:186-198",
	},
	{
		entry:     9,
		name:      "statistics TOTAL excludes no rows while the body drops null dates",
		reference: "core/reconciliation.py:158 vs :217-219",
	},
	{
		entry:     10,
		name:      "transactions column mixes transaction and ticket counts",
		reference: "core/reconciliation.py:175 vs :180",
	},
	{
		entry:     11,
		name:      "fully voided night appears in metrics but has no totals row",
		reference: "core/reconciliation.py:158-168",
	},
	{
		entry:     12,
		name:      "payPalOnly fails closed while the other filters fail open",
		reference: "core/reconciliation.py:16-20",
	},
	{
		entry:     13,
		name:      "order refund amount is not converted from cents",
		reference: "mapping.py:104-105",
	},
	{
		entry:     14,
		name:      "tickets-per-order divisor counts voided tickets",
		reference: "api/tickettailor.py:118-121",
	},
}

func TestLedgerBehaviors(t *testing.T) {
	for _, tc := range ledgerCases {
		t.Run(strconv.Itoa(tc.entry)+"_"+tc.name, func(t *testing.T) {
			if tc.run == nil {
				t.Skipf("ledger entry %d not yet implemented (%s)", tc.entry, tc.reference)
			}
			tc.run(t)
		})
	}
}

// TestLedgerCoverage keeps this file and docs/PARITY.md in lockstep.
//
// It parses the ledger table out of the doc and asserts that every numbered
// entry has a case above. Without this, adding a behavior to the doc and
// forgetting the test would be invisible — which is exactly the failure mode
// guardrail 2 is meant to prevent.
func TestLedgerCoverage(t *testing.T) {
	const doc = "../../docs/PARITY.md"

	raw, err := os.ReadFile(doc)
	if err != nil {
		// docs/ is gitignored in this repo, so a checkout without it is
		// legitimate. Skip rather than fail so CI stays green.
		t.Skipf("cannot read %s: %v", doc, err)
	}

	// Ledger rows look like:  | 7 | Empty `pp_ids` triggers ... | `:92` | ... |
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

	t.Logf("%d ledger entries documented, %d with test cases", documented, len(ledgerCases))
}

// TestFlagsDefaultToReferenceBehavior pins the most important invariant in the
// package: a zero-valued Flags means "behave exactly like the Python".
//
// If someone later flips a default to true because the fixed behavior seems
// obviously better, parity breaks silently across every historical report.
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

func TestBuildReportsNotImplemented(t *testing.T) {
	if _, err := Build(nil, nil, "", Flags{}); err == nil {
		t.Fatal("Build returned nil error; it should report ErrNotImplemented until P1 lands")
	}
}
