//go:build unicode_rustref

package bamlunicode

import (
	"encoding/json"
	"os"
	"testing"
)

// TestProfileSentinels is CI drift-guard check C, in the S1-safe form: the five
// behaviors that distinguish the exact BAML 0.223.0 dual-version profile from the
// host Go/x-text tables, each verified against the pinned Rust 1.93.0 /
// unicode-normalization 0.1.24 engine (the byte-for-byte reproduction of the
// stock libbaml_cffi Unicode engine, proven exhaustively in
// TestRustReferenceExhaustive).
//
// The through-the-CFFI-artifact end-to-end CLAIM assertions (feeding these
// sentinels through the stock libbaml_cffi coercion path) are Slice 2, because
// they require the match_string coercion wiring in parse.go/coerce.go that S1
// must not touch. This test pins the primitive behavior the artifact must
// exhibit; Slice 2 pins that the artifact actually does.
func TestProfileSentinels(t *testing.T) {
	bin := buildReference(t)

	// 1-3: lowercase sentinels, verified against the reference AND pinned to the
	// profile-distinguishing output.
	lowerCases := []struct {
		name string
		in   []rune
		want []rune
	}{
		{"A7DC lowercases to 019B (Unicode 17 case delta)", []rune{0xA7DC}, []rune{0x019B}},
		{"0130 one-to-many lowercase", []rune{0x0130}, []rune{0x0069, 0x0307}},
		{"Final_Sigma final form", []rune{'a', 0x03A3}, []rune{'a', 0x03C2}},
		{"Final_Sigma medial form", []rune{'a', 0x03A3, 'a'}, []rune{'a', 0x03C3, 'a'}},
	}
	var reqs []strReq
	for _, c := range lowerCases {
		reqs = append(reqs, strReq{"L", string(c.in)})
	}
	// 4: a genuine Unicode 15->16 NFKD delta scalar.
	nfkdCP := firstDeltaCP(t, "nfkd.json")
	reqs = append(reqs, strReq{"K", string([]rune{nfkdCP})})

	outs := runStrmap(t, bin, reqs)

	for i, c := range lowerCases {
		if outs[i] != hexOf(string(c.want)) {
			t.Errorf("%s: reference=%s want=%s", c.name, outs[i], hexOf(string(c.want)))
		}
		if got := hexOf(LowerString(string(c.in))); got != outs[i] {
			t.Errorf("%s: go=%s reference=%s", c.name, got, outs[i])
		}
	}

	// 4: NFKD delta — Go matches reference and the scalar actually decomposes
	//    under the 16.0.0 data (its Go-15 form differed, per the delta manifest).
	nfkdRef := outs[len(lowerCases)]
	if got := hexOf(NFKD(string([]rune{nfkdCP}))); got != nfkdRef {
		t.Errorf("nfkd delta U+%04X: go=%s reference=%s", nfkdCP, got, nfkdRef)
	}
	if nfkdRef == hexOf(string([]rune{nfkdCP})) {
		t.Errorf("nfkd delta U+%04X did not decompose under 16.0.0 data", nfkdCP)
	}

	// 5: a genuine Unicode 15->17 classification delta scalar is alphanumeric
	//    under the pinned 17.0.0 tables (Go-15 disagreed).
	alnumCP := firstDeltaCP(t, "alphanumeric.json")
	if !IsAlphanumeric(alnumCP) {
		t.Errorf("classification delta U+%04X not alphanumeric under 17.0.0 tables", alnumCP)
	}
}

// firstDeltaCP returns the first code point recorded in a committed delta
// manifest, anchoring a sentinel to a genuine 15->target difference.
func firstDeltaCP(t *testing.T, manifest string) rune {
	t.Helper()
	data, err := os.ReadFile("testdata/deltas/" + manifest)
	if err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}
	var raw []struct {
		CP rune `json:"cp"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse %s: %v", manifest, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s: no deltas to anchor sentinel", manifest)
	}
	return raw[0].CP
}
