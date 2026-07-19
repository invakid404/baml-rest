//go:build integration

package integration

// De-BAML Phase 7C — committed, DETERMINISTIC live-BAML proof for the native
// number→string display formatter (coerce.go displayNumber / displayFloat64, the
// serde_json/ryu shortest-round-trip reproduction). For each token it feeds
// {"u": <token>} to BOTH native Parse (which coerces the number into the string
// field via displayNumber) and live BAML v0.223, and asserts byte-exact. This is the
// executable evidence the re-review asked for (the 2,060-case claim was previously
// prose-only): a structured boundary sweep + subnormals/huge + a fixed-seed randomized
// f64 sweep, reproducible in CI.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/debaml"
)

func TestStream7CNumberDisplayParity(t *testing.T) {
	dynclientCallGate(t)
	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	preserve := true
	strField := &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(bamlutils.OrderedKV("u", &bamlutils.DynamicProperty{Type: "string"}))}
	baml := func(raw string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, e := dyn.DynamicParse(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: strField, PreserveSchemaOrder: &preserve, Stream: false})
		if e != nil {
			return "", false
		}
		return string(res.Data), true
	}
	nat := func(raw string) (string, bool) {
		p, e := debaml.Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: strField, Stream: false})
		if e != nil {
			return "", false
		}
		out, fe := bamlutils.FlattenDynamicOutput(p.JSON)
		if fe != nil {
			out = p.JSON
		}
		return string(out), true
	}

	// (a) structured boundary sweep: the ryu fixed/scientific thresholds
	// (scientific when the leading-digit power of ten is < -5 or >= 16) + negatives,
	// negative zero, redundant spellings, past-u64 integers.
	var toks []string
	for k := -12; k <= 25; k++ {
		toks = append(toks, fmt.Sprintf("1e%d", k))
	}
	toks = append(toks,
		"5", "5.0", "5e0", "5E0", "5.00", "-0", "-0.0", "2.50", "1.5e2", "1E3",
		"3.14159265358979", "123456789.123456789", "0.000001", "0.0000001",
		"9.999999e5", "9e15", "9e16", "1.0e16", "1.5e20", "-1.5e20", "1.5e-10",
		"18446744073709551615", "18446744073709551616", "99999999999999999999",
		"-99999999999999999999", "0.1", "-3", "100", "42.0", "0.5", "1.23e-5",
	)
	// (b) subnormals + extremes.
	toks = append(toks,
		strconv.FormatFloat(math.SmallestNonzeroFloat64, 'e', -1, 64),
		strconv.FormatFloat(math.MaxFloat64, 'e', -1, 64),
		strconv.FormatFloat(-math.SmallestNonzeroFloat64, 'e', -1, 64),
	)
	// (c) fixed-seed randomized f64 (uniform bit patterns across all magnitude
	// regimes incl. subnormals/huge) + few-sig-digit mantissas dense near boundaries.
	rng := rand.New(rand.NewSource(0xC0FFEE))
	for i := 0; i < 1600; i++ {
		f := math.Float64frombits(rng.Uint64())
		if math.IsNaN(f) || math.IsInf(f, 0) || f == 0 {
			continue
		}
		toks = append(toks, strconv.FormatFloat(f, 'e', -1, 64))
	}
	for i := 0; i < 400; i++ {
		digs := 1 + rng.Intn(6)
		mant := string(rune('1' + rng.Intn(9)))
		if digs > 1 {
			mant += "."
			for j := 1; j < digs; j++ {
				mant += string(rune('0' + rng.Intn(10)))
			}
		}
		toks = append(toks, fmt.Sprintf("%se%d", mant, rng.Intn(46)-20))
	}

	total, mism := 0, 0
	for _, n := range toks {
		raw := `{"u": ` + n + `}`
		b, bok := baml(raw)
		na, nok := nat(raw)
		total++
		if bok != nok || (bok && nok && b != na) {
			mism++
			if mism <= 40 {
				t.Errorf("number-display mismatch on %q: BAML(ok=%v)=%s NATIVE(ok=%v)=%s", n, bok, b, nok, na)
			}
		}
	}
	t.Logf("NUMBER-DISPLAY PARITY: %d tokens, %d mismatches", total, mism)
}
