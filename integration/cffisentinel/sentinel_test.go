//go:build integration

// Package cffisentinel is #555 drift-guard check C: an ADDITIVE black-box
// behavioral probe of the pinned stock BAML 0.223.0 libbaml_cffi artifact.
//
// It drives the stock runtime's OFFLINE coercion path (dynclient.DynamicParse ->
// match_string; no LLM, no HTTP, no containers) with inputs whose match REQUIRES
// the exact dual-version Unicode profile, and asserts the artifact claims the
// expected enum value. This confirms the DOWNLOADED artifact actually exhibits
// the profile the vendored internal/debaml/bamlunicode tables reproduce (Rust std
// Unicode 17.0.0 case/properties + unicode-normalization Unicode 16.0.0 NFKD/marks).
//
// Deliberately its own tiny package (not integration/) so it does NOT pull in the
// integration TestMain's Docker/testcontainers setup: the drift-guard job needs
// only cgo + the auto-downloaded release artifact. It is additive and does NOT
// touch the match_string coercion wiring in parse.go/coerce.go (native-claim
// enablement is Slice 2); it only observes the stock artifact black-box.
package cffisentinel

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml/bamlunicode"
)

// Pinned probe anchors. These are committed 15->target delta scalars (present in
// internal/debaml/bamlunicode/testdata/deltas/{nfkd,alphanumeric}.json), NOT
// discovered at runtime — a host Go/x-text Unicode update must not be able to
// leave the probe with no candidate and fail before the artifact is exercised.
// Their intended properties are re-validated below against the PINNED bamlunicode
// tables (host-independent), so a regeneration that changes them fails loudly.
const (
	// U+1CCD6 gains an NFKD compatibility decomposition to "A" in Unicode 16
	// (absent from Go/x-text 15) — a version-discriminating NFKD sentinel.
	nfkdDeltaScalar = rune(0x1CCD6)
	// U+088F is alphanumeric under Unicode 17 (not under Go 15) and is neither a
	// combining mark nor whitespace, so the punctuation-strip difference is
	// observable via the KEPT-vs-STRIPPED enum values.
	alnumDeltaScalar = rune(0x088F)
)

func newClient(t *testing.T) *dynclient.Client {
	t.Helper()
	// Offline parse needs no LLM endpoint; a bare client is sufficient.
	c, err := dynclient.New()
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	return c
}

// probe parses raw against a single-enum-field class and returns the claimed
// enum value for field "v" (empty string => declined / no match).
func probe(t *testing.T, client *dynclient.Client, values []*dynclient.EnumValue, rawFieldValue string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := &dynclient.OutputSchema{
		Enums: dynFrom(map[string]*dynclient.Enum{
			"Sentinel": {Values: values},
		}),
		Properties: dynFrom(map[string]*dynclient.Property{
			"v": {Ref: "Sentinel"},
		}),
	}
	rawObj, err := json.Marshal(map[string]string{"v": rawFieldValue})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.DynamicParse(ctx, dynclient.ParseRequest{
		Raw:          string(rawObj),
		OutputSchema: schema,
	})
	if err != nil {
		return "" // hard coercion failure == declined
	}
	var out struct {
		V *string `json:"v"`
	}
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatalf("decode parse result %s: %v", resp.Data, err)
	}
	if out.V == nil {
		return ""
	}
	return *out.V
}

func dynFrom[V any](m map[string]V) dynclient.OrderedMap[V] {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var om dynclient.OrderedMap[V]
	for _, k := range keys {
		if err := om.Set(k, m[k]); err != nil {
			panic(err)
		}
	}
	return om
}

func TestUnicodeCFFISentinels(t *testing.T) {
	client := newClient(t)

	// A distractor in every schema so a match is a real claim of the target, not
	// a fallback to the only option.
	distractor := &dynclient.EnumValue{Name: "OTHER", Alias: "zzqqxx"}

	t.Run("A7DC_lowercase_17", func(t *testing.T) {
		// U+A7DC lowercases to U+019B only under Unicode 17.
		vals := []*dynclient.EnumValue{
			{Name: "MATCH", Alias: string(rune(0x019B))},
			distractor,
		}
		if got := probe(t, client, vals, string(rune(0xA7DC))); got != "MATCH" {
			t.Errorf("stock artifact did not lowercase U+A7DC->U+019B: claimed %q, want MATCH", got)
		}
	})

	t.Run("U0130_one_to_many_lowercase", func(t *testing.T) {
		// İ (U+0130) lowercases to i + combining dot; after mark strip matches "i".
		vals := []*dynclient.EnumValue{
			{Name: "MATCH", Alias: "i"},
			distractor,
		}
		if got := probe(t, client, vals, string(rune(0x0130))); got != "MATCH" {
			t.Errorf("stock artifact did not handle U+0130 one-to-many lowercase: claimed %q, want MATCH", got)
		}
	})

	t.Run("final_sigma_context", func(t *testing.T) {
		// Word-final capital sigma lowercases to ς (U+03C2), not σ (U+03C3).
		final := string([]rune{0x03BF, 0x03B4, 0x03BF, 0x03C2})  // οδος (final)
		medial := string([]rune{0x03BF, 0x03B4, 0x03BF, 0x03C3}) // οδοσ (non-final)
		vals := []*dynclient.EnumValue{
			{Name: "FINAL", Alias: final},
			{Name: "MEDIAL", Alias: medial},
			distractor,
		}
		raw := string([]rune{0x039F, 0x0394, 0x039F, 0x03A3}) // ΟΔΟΣ (all caps)
		if got := probe(t, client, vals, raw); got != "FINAL" {
			t.Errorf("stock artifact Final_Sigma: all-caps input claimed %q, want FINAL (ς)", got)
		}
	})

	t.Run("nfkd_15_to_16_delta", func(t *testing.T) {
		cp := nfkdDeltaScalar
		alias := bamlunicode.NFKD(string(cp)) // Unicode-16 NFKD form
		// The pinned scalar must actually decompose under the vendored 16 tables
		// (host-independent validation); otherwise the sentinel is meaningless.
		if alias == string(cp) {
			t.Fatalf("pinned NFKD anchor U+%04X no longer decomposes under vendored tables; re-pick", cp)
		}
		vals := []*dynclient.EnumValue{
			{Name: "MATCH", Alias: alias},
			distractor,
		}
		if got := probe(t, client, vals, string(cp)); got != "MATCH" {
			t.Errorf("stock artifact NFKD-16 delta U+%04X: claimed %q, want MATCH (alias %q)", cp, got, alias)
		}
	})

	t.Run("classification_15_to_17_delta", func(t *testing.T) {
		cp := alnumDeltaScalar
		// The pinned scalar must be alphanumeric AND not a mark/whitespace under
		// the vendored 17/16 tables (host-independent validation).
		if !bamlunicode.IsAlphanumeric(cp) || bamlunicode.IsCombiningMark(cp) || bamlunicode.IsWhitespace(cp) {
			t.Fatalf("pinned alphanumeric anchor U+%04X no longer meets the sentinel conditions; re-pick", cp)
		}
		// Under Unicode 17 cp is alphanumeric -> kept during punctuation strip.
		kept := "cafe" + string(cp)
		vals := []*dynclient.EnumValue{
			{Name: "KEPT", Alias: kept},
			{Name: "STRIPPED", Alias: "cafe"},
			distractor,
		}
		if got := probe(t, client, vals, kept); got != "KEPT" {
			t.Errorf("stock artifact classification-17 delta U+%04X: claimed %q, want KEPT", cp, got)
		}
	})
}
