package money

import (
	"encoding/json"
	"os"
	"testing"
)

type sumFixture struct {
	Count     int `json:"count"`
	Differing int `json:"differing"`
	Env       struct {
		Python string `json:"python"`
		NumPy  string `json:"numpy"`
		Pandas string `json:"pandas"`
	} `json:"env"`
	Cases []struct {
		Values  []string `json:"values"`
		Pandas  string   `json:"pandas"`
		Builtin string   `json:"builtin"`
	} `json:"cases"`
}

func loadSumFixture(t *testing.T) sumFixture {
	t.Helper()

	raw, err := os.ReadFile("testdata/sums.json")
	if err != nil {
		t.Fatalf("read fixture: %v (regenerate with `make sum-fixture`)", err)
	}

	var f sumFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(f.Cases) != f.Count {
		t.Fatalf("fixture count mismatch: header says %d, found %d", f.Count, len(f.Cases))
	}
	return f
}

// TestSumPandas_MatchesPandas proves the Go pairwise implementation reproduces
// what pandas' Series.sum() actually returns, rather than what numpy's C source
// suggests it should. The distinction matters because numpy may take SIMD paths
// that reassociate differently from the scalar reference algorithm.
func TestSumPandas_MatchesPandas(t *testing.T) {
	f := loadSumFixture(t)
	t.Logf("fixture: %d arrays, python %s numpy %s pandas %s",
		f.Count, f.Env.Python, f.Env.NumPy, f.Env.Pandas)

	var failures int
	for i, c := range f.Cases {
		values := make([]float64, len(c.Values))
		for j, v := range c.Values {
			values[j] = parseHex(t, v)
		}
		want := parseHex(t, c.Pandas)

		if got := SumPandas(values); !sameBits(got, want) {
			failures++
			if failures <= 8 {
				t.Errorf("SumPandas(case %d, n=%d):\n  got  %v\n  want %v",
					i, len(values), got, want)
			}
		}
	}
	if failures > 8 {
		t.Errorf("... and %d further mismatches (%d of %d arrays)",
			failures-8, failures, len(f.Cases))
	}
}

// TestSumPythonBuiltin_MatchesBuiltin proves SumPythonBuiltin reproduces
// CPython's sum(), which is what the reference uses on the matched-transaction
// path — and which has applied Neumaier compensation since CPython 3.12.
func TestSumPythonBuiltin_MatchesBuiltin(t *testing.T) {
	f := loadSumFixture(t)

	var failures int
	for i, c := range f.Cases {
		values := make([]float64, len(c.Values))
		for j, v := range c.Values {
			values[j] = parseHex(t, v)
		}
		want := parseHex(t, c.Builtin)

		if got := SumPythonBuiltin(values); !sameBits(got, want) {
			failures++
			if failures <= 8 {
				t.Errorf("SumPythonBuiltin(case %d, n=%d):\n  got  %v\n  want %v",
					i, len(values), got, want)
			}
		}
	}
	if failures > 8 {
		t.Errorf("... and %d further mismatches (%d of %d arrays)",
			failures-8, failures, len(f.Cases))
	}
}

// TestSumSequential_IsNotPythonSum pins the trap that motivated
// SumPythonBuiltin: the obvious naive loop does NOT reproduce Python's sum().
//
// If this ever passes with zero divergence, either the fixture lost its
// adversarial cases or the pinned interpreter dropped compensated summation —
// both worth knowing before trusting a parity run.
func TestSumSequential_IsNotPythonSum(t *testing.T) {
	f := loadSumFixture(t)

	var differs int
	for _, c := range f.Cases {
		values := make([]float64, len(c.Values))
		for j, v := range c.Values {
			values[j] = parseHex(t, v)
		}
		if !sameBits(SumSequential(values), SumPythonBuiltin(values)) {
			differs++
		}
	}

	if differs == 0 {
		t.Fatal("naive sequential summation agreed with CPython's sum() on every " +
			"array; expected divergence from Neumaier compensation")
	}
	t.Logf("naive summation differs from CPython's sum() on %d of %d arrays",
		differs, len(f.Cases))
}

// TestSumStrategiesDiverge guards the reason both functions exist, the same way
// TestRoundingHelpers_ActuallyDiverge does for rounding.
func TestSumStrategiesDiverge(t *testing.T) {
	f := loadSumFixture(t)

	var diverged int
	for _, c := range f.Cases {
		if c.Pandas != c.Builtin {
			diverged++
		}
	}

	if diverged == 0 {
		t.Fatal("pandas and builtin sums agree on every fixture array; the two " +
			"functions would appear redundant. Regenerate the fixture.")
	}
	if diverged != f.Differing {
		t.Errorf("fixture header claims %d differing arrays, counted %d", f.Differing, diverged)
	}
	t.Logf("%d of %d arrays differ between pairwise and sequential summation",
		diverged, len(f.Cases))
}
