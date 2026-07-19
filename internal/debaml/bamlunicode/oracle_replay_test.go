package bamlunicode

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// oracleCase is one committed multi-scalar oracle result. The expected `Out`
// bytes were produced by the exact Rust 1.93.0 / unicode-normalization 0.1.24
// reference (see rustref_test.go, run with BAMLUNICODE_UPDATE_FIXTURES=1). This
// gives always-on coverage of Final_Sigma contexts and adversarial NFKD/trim
// sequences without needing rustc on the machine running `go test`.
type oracleCase struct {
	Mode string `json:"mode"` // L=lowercase, K=nfkd, T=trim
	In   string `json:"in"`   // hex-encoded UTF-8 input
	Out  string `json:"out"`  // hex-encoded UTF-8 expected output
}

func hexOf(s string) string { return hex.EncodeToString([]byte(s)) }

// TestOracleStringReplay replays the committed Rust-verified multi-scalar corpus.
func TestOracleStringReplay(t *testing.T) {
	capStress(t) // 6k+ deterministic replays; cap under -race -count=100
	path := filepath.Join("testdata", "oracle_strings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read oracle fixture: %v (regenerate with `go test -tags unicode_rustref -run TestRustReferenceStrings` and BAMLUNICODE_UPDATE_FIXTURES=1)", err)
	}
	var cases []oracleCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse oracle fixture: %v", err)
	}
	if len(cases) < 100 {
		t.Fatalf("oracle fixture has only %d cases; looks truncated", len(cases))
	}
	var sawFinal, sawMedial bool
	for _, c := range cases {
		in := decodeHex(t, c.In)
		var got string
		switch c.Mode {
		case "L":
			got = LowerString(in)
		case "K":
			got = NFKD(in)
		case "T":
			got = TrimSpace(in)
		default:
			t.Fatalf("unknown oracle mode %q", c.Mode)
		}
		if hexOf(got) != c.Out {
			t.Errorf("mode %s in=%s: got=%s want=%s", c.Mode, c.In, hexOf(got), c.Out)
		}
		// Confirm the corpus actually exercises both sigma forms.
		if c.Mode == "L" {
			if containsScalar(c.Out, 0x03C2) {
				sawFinal = true
			}
			if containsScalar(c.Out, 0x03C3) {
				sawMedial = true
			}
		}
	}
	if !sawFinal || !sawMedial {
		t.Errorf("oracle corpus did not exercise both final (%v) and medial (%v) sigma", sawFinal, sawMedial)
	}
	t.Logf("oracle replay: %d Rust-verified cases", len(cases))
}

func decodeHex(t *testing.T, h string) string {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad hex %q: %v", h, err)
	}
	return string(b)
}

func containsScalar(outHex string, target rune) bool {
	b, err := hex.DecodeString(outHex)
	if err != nil {
		return false
	}
	for _, r := range string(b) {
		if r == target {
			return true
		}
	}
	return false
}
