package money

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

// fixture mirrors internal/money/testdata/rounding.json, produced by
// tools/money/gen_rounding_fixture.py. See CLAUDE.md guardrail 5.
type fixture struct {
	Seed  int `json:"seed"`
	Count int `json:"count"`
	Env   struct {
		Python string `json:"python"`
		NumPy  string `json:"numpy"`
	} `json:"env"`
	Cases []struct {
		X       string `json:"x"`
		CPython string `json:"cpython"`
		NumPy   string `json:"numpy"`
	} `json:"cases"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()

	raw, err := os.ReadFile("testdata/rounding.json")
	if err != nil {
		t.Fatalf("read fixture: %v (regenerate with `make money-fixture`)", err)
	}

	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture contains no cases")
	}
	if len(f.Cases) != f.Count {
		t.Fatalf("fixture count mismatch: header says %d, found %d", f.Count, len(f.Cases))
	}
	return f
}

// parseHex decodes a Python float.hex() literal. Go's ParseFloat accepts the
// same syntax, which is why the fixture uses hex: it round-trips the exact
// IEEE-754 bits rather than a decimal approximation of them.
func parseHex(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse hex float %q: %v", s, err)
	}
	return v
}

// sameBits compares by bit pattern rather than by ==, so that NaN compares
// equal to itself and +0.0 does NOT compare equal to -0.0. Sign of zero matters
// here: it is observable through the fixture and through division.
func sameBits(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

// TestRoundCPython_MatchesCPython is the gate described in CLAUDE.md guardrail
// 5. It must pass with exact bit equality — if it fails, the correct response
// is to fix RoundCPython, never to loosen the comparison.
func TestRoundCPython_MatchesCPython(t *testing.T) {
	f := loadFixture(t)
	t.Logf("fixture: %d cases, python %s, numpy %s", f.Count, f.Env.Python, f.Env.NumPy)

	var failures int
	for _, c := range f.Cases {
		x := parseHex(t, c.X)
		want := parseHex(t, c.CPython)

		if got := RoundCPython(x); !sameBits(got, want) {
			failures++
			if failures <= 10 {
				t.Errorf("RoundCPython(%s / %v):\n  got  %s (%v)\n  want %s (%v)",
					c.X, x, strconv.FormatFloat(got, 'x', -1, 64), got, c.CPython, want)
			}
		}
	}
	if failures > 10 {
		t.Errorf("... and %d further mismatches (%d total of %d cases)",
			failures-10, failures, len(f.Cases))
	}
}

// TestRoundNumpy_MatchesNumPy is the same gate for the numpy code path.
func TestRoundNumpy_MatchesNumPy(t *testing.T) {
	f := loadFixture(t)

	var failures int
	for _, c := range f.Cases {
		x := parseHex(t, c.X)
		want := parseHex(t, c.NumPy)

		if got := RoundNumpy(x); !sameBits(got, want) {
			failures++
			if failures <= 10 {
				t.Errorf("RoundNumpy(%s / %v):\n  got  %s (%v)\n  want %s (%v)",
					c.X, x, strconv.FormatFloat(got, 'x', -1, 64), got, c.NumPy, want)
			}
		}
	}
	if failures > 10 {
		t.Errorf("... and %d further mismatches (%d total of %d cases)",
			failures-10, failures, len(f.Cases))
	}
}

// TestRoundingHelpers_ActuallyDiverge guards the reason both helpers exist.
//
// If this test ever fails, it means the fixture no longer contains a case where
// CPython and NumPy disagree — at which point someone will reasonably conclude
// the two functions are redundant and collapse them. They are not redundant:
// the reference implementation dispatches round() to different algorithms
// depending on whether the value is a Python float or a numpy.float64
// (docs/PARITY.md, "Float and rounding parity").
func TestRoundingHelpers_ActuallyDiverge(t *testing.T) {
	f := loadFixture(t)

	var diverged int
	for _, c := range f.Cases {
		if c.CPython != c.NumPy {
			diverged++
			if diverged == 1 {
				x := parseHex(t, c.X)
				t.Logf("example divergence: x=%v  cpython=%v  numpy=%v",
					x, parseHex(t, c.CPython), parseHex(t, c.NumPy))
			}
		}
	}

	if diverged == 0 {
		t.Fatal("no divergence between CPython and NumPy rounding in the fixture; " +
			"the two helpers would appear redundant. Regenerate the fixture and " +
			"confirm the tie-heavy cases survived.")
	}
	t.Logf("%d of %d cases diverge between the two algorithms", diverged, len(f.Cases))
}

// TestRoundCPython_NotNaiveMultiply documents why the obvious implementation is
// wrong, so nobody "simplifies" RoundCPython into it later.
func TestRoundCPython_NotNaiveMultiply(t *testing.T) {
	naive := func(x float64) float64 { return math.Round(x*100) / 100 }

	f := loadFixture(t)
	var differs int
	for _, c := range f.Cases {
		x := parseHex(t, c.X)
		if !sameBits(naive(x), RoundCPython(x)) {
			differs++
		}
	}

	if differs == 0 {
		t.Fatal("math.Round(x*100)/100 agreed with RoundCPython on every case; " +
			"expected divergence, so this guard is no longer meaningful")
	}
	t.Logf("math.Round(x*100)/100 differs from CPython on %d of %d cases", differs, len(f.Cases))
}

// fold adds left to right using runtime float64 arithmetic.
//
// Expectations here must NOT be written as a constant expression such as
// `0.1 + 0.2 + 0.3`: Go evaluates untyped constant arithmetic at arbitrary
// precision, so that yields exactly 0.6, whereas accumulating the same values
// in float64 yields 0.6000000000000001. Using a constant expression would
// assert against a number float64 cannot produce, and the test would fail
// against correct code. This is the same class of error the rounding helpers
// exist to prevent.
func fold(xs ...float64) float64 {
	var total float64
	for _, x := range xs {
		total += x
	}
	return total
}

func TestSumSequential(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{1.25}, 1.25},
		{"accumulates in float64, not exact decimal", []float64{0.1, 0.2, 0.3}, fold(0.1, 0.2, 0.3)},
		{"negative fees", []float64{10.00, -0.34, -0.35}, fold(10.00, -0.34, -0.35)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SumSequential(tc.in); !sameBits(got, tc.want) {
				t.Errorf("SumSequential(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	// Guard the property the docstring claims: left-to-right accumulation is
	// order-dependent, which is why SumSequential is not interchangeable with
	// pandas' pairwise Series.sum().
	t.Run("is order dependent", func(t *testing.T) {
		a := SumSequential([]float64{1e16, 1, -1e16})
		b := SumSequential([]float64{1e16, -1e16, 1})
		if sameBits(a, b) {
			t.Errorf("expected order dependence, got %v for both orderings", a)
		}
	})
}

func TestMoney(t *testing.T) {
	tests := []struct {
		name    string
		m       Money
		wantStr string
		wantF   float64
	}{
		{"zero", 0, "0.00", 0},
		{"one cent", 1, "0.01", 0.01},
		{"euro", 100, "1.00", 1},
		{"negative fee", -34, "-0.34", -0.34},
		{"large", 123456789, "1234567.89", 1234567.89},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
			if got := tc.m.Float(); got != tc.wantF {
				t.Errorf("Float() = %v, want %v", got, tc.wantF)
			}
		})
	}
}

func TestFromFloat(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want Money
	}{
		{"zero", 0, 0},
		{"euro", 1.00, 100},
		{"cent", 0.01, 1},
		{"negative", -0.34, -34},
		{"half cent ties to even down", 0.005, 0},
		{"half cent ties to even up", 0.015, 2},
		{"NaN is zero", math.NaN(), 0},
		{"Inf is zero", math.Inf(1), 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromFloat(tc.in); got != tc.want {
				t.Errorf("FromFloat(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
