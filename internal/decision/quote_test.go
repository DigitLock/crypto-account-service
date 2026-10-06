package decision

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// S2-T404 — Req: UC-1 step 7, FR-7. The example of SRS — Card Spend §2.1.2.
func TestT404_QuoteExample(t *testing.T) {
	got, err := TokenAmount("25.40", "1.1642000000", 100, 6)
	if err != nil || got.String() != "29866387" {
		t.Errorf("25.40 EUR at 1.1642000000 with 100 bps = %v, %v; want 29866387", got, err)
	}
}

// S2-T405 — Req: SRS — Card Spend §2.1.2 Rate source. Exact decimals, rounding up, no float.
func TestT405_ExactDecimal(t *testing.T) {
	cases := []struct {
		amount, rate string
		bps          int
		decimals     uint8
		want         string
	}{
		{"25.4", "1.1642", 100, 6, "29866387"},
		// 0.1 × 3 is 0.30000000000000004 in float64: the float product would round up to 300001.
		{"0.1", "3.0000000000", 0, 6, "300000"},
		{"0.1", "3", 0, 6, "300000"},
		// 1234.5678 × 0.0085123457 × 1.01 × 10^6 = 10614158.5827… exactly.
		{"1234.5678", "0.0085123457", 100, 6, "10614159"},
		// Any remainder rounds up to a whole base unit.
		{"0.0001", "0.0000000001", 0, 6, "1"},
		{"25.40", "", 0, 6, "25400000"},
		{"25.4", "", 100, 6, "25400000"}, // USD has no buffer
		{"0.0001", "", 0, 2, "1"},        // a token with fewer decimals than the amount
		// The largest amount of NUMERIC(18,4) at the largest rate of NUMERIC(20,10), buffer 100 %, 18 decimals.
		{"99999999999999.9999", "9999999999.9999999999", 10000, 18, "1999999999999999997980000000000000000020000"},
	}
	for _, c := range cases {
		got, err := TokenAmount(c.amount, c.rate, c.bps, c.decimals)
		if err != nil || got.String() != c.want {
			t.Errorf("TokenAmount(%s, %q, %d, %d) = %v, %v; want %s", c.amount, c.rate, c.bps, c.decimals, got, err, c.want)
		}
	}

	for in, want := range map[string]string{
		"1.1642000000": "1.1642", "1.0000000000": "1", "0.0085123457": "0.0085123457", "10": "10", "0000000001.5": "0000000001.5",
	} {
		if got, err := ParseRate(in); err != nil || got != want {
			t.Errorf("ParseRate(%s) = %q, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "0.0000000000", "1.16420000001", "12345678901", "-1", "+1", "1e3", "1.", ".5", "1,5", " 1", "NaN", "Inf"} {
		if got, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) = %q, want an error", bad, got)
		}
	}
	for in, want := range map[string]string{"25.40": "25.4", "25.4000": "25.4", "10.00": "10", "10": "10", "0.5": "0.5", "100": "100"} {
		if got := TrimDecimal(in); got != want {
			t.Errorf("TrimDecimal(%s) = %s, want %s", in, got, want)
		}
	}
}

// S2-T405 — "no float in any amount, rate or fee path": the hand-written code of the decision path declares no
// float type and parses no float; the latency metric is the one exception. The generated CRS code carries the double field rate; it is never read
// (crstest sends a wrong double with every answer, internal/processorapi T404 and T405).
func TestT405_NoFloatInTheDecisionPath(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..")
	var files []string
	for _, dir := range []string{"decision", "processorapi", "crs", "chain", "debit"} {
		matches, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		// Prometheus takes latencies as float64: metrics.go holds no amount and no rate.
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, filepath.Join("processorapi", "metrics.go")) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if strings.Contains(imp.Path.Value, "math\"") && !strings.Contains(imp.Path.Value, "big") {
				t.Errorf("%s imports math", f)
			}
		}
		code := string(src)
		for _, word := range []string{"float32", "float64", "ParseFloat", "big.Float", "big.Rat"} {
			if strings.Contains(code, word) {
				t.Errorf("%s uses %s", filepath.Base(f), word)
			}
		}
	}
	if len(files) < 8 {
		t.Fatalf("only %d files scanned", len(files))
	}
}
