package bamlunicode

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// TestNFKDConformance runs the full Unicode 16.0.0 NormalizationTest.txt
// conformance suite for NFKD. For every test row (columns c1..c5) the standard
// requires NFKD(c1)==NFKD(c2)==NFKD(c3)==NFKD(c4)==NFKD(c5)==c5. This is a
// data-only proof (no rustc) and always runs.
func TestNFKDConformance(t *testing.T) {
	capStress(t) // 19,965-row conformance + multi-MB unzip; deterministic
	// Extracted from the SHA-256-verified UCD 16.0.0 archive.
	data := extractUCD(t, "16.0.0", "NormalizationTest.txt")
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var rows, checks int
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "@") {
			continue
		}
		fields := strings.Split(text, ";")
		if len(fields) < 5 {
			continue
		}
		cols := make([]string, 5)
		for i := 0; i < 5; i++ {
			cols[i] = decodeScalars(t, line, fields[i])
		}
		want := cols[4] // c5 is the NFKD form
		for i, in := range cols {
			if got := NFKD(in); got != want {
				t.Fatalf("line %d col c%d: NFKD(%s) = %s, want %s (c5)",
					line, i+1, hexdump(in), hexdump(got), hexdump(want))
			}
			checks++
		}
		rows++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rows < 1000 {
		t.Fatalf("only %d conformance rows parsed; corpus looks truncated", rows)
	}
	t.Logf("NFKD conformance: %d rows, %d NFKD checks", rows, checks)
}

func decodeScalars(t *testing.T, line int, field string) string {
	t.Helper()
	var b strings.Builder
	for _, tok := range strings.Fields(field) {
		v, err := strconv.ParseUint(tok, 16, 32)
		if err != nil {
			t.Fatalf("line %d: bad scalar %q: %v", line, tok, err)
		}
		b.WriteRune(rune(v))
	}
	return b.String()
}

func hexdump(s string) string {
	var parts []string
	for _, r := range s {
		parts = append(parts, "U+"+strings.ToUpper(strconv.FormatInt(int64(r), 16)))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
