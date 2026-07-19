//go:build unicode_rustref

// Exact byte-for-byte differential proof against the pinned BAML build toolchain.
//
// This is the primitive oracle for #555. It is gated behind the unicode_rustref
// build tag (and needs `cargo +1.93.0`), so it does not run in a plain `go test`;
// the CI drift-guard job runs it with `-tags unicode_rustref` after installing
// rustc 1.93.0. It compiles testdata/reference with rustc 1.93.0 (Unicode 17.0.0)
// + unicode-normalization 0.1.24 (Unicode 16.0.0) and compares UTF-8 BYTES for:
//
//   - every scalar's str::to_lowercase, nfkd, is_alphanumeric, is_whitespace,
//     and is_combining_mark (exhaustive, all 0..=0x10FFFF minus surrogates);
//   - Final_Sigma contexts generated from property equivalence classes
//     (start/end, cased/uncased/case-ignorable neighbours, multiple sigmas,
//     combining marks, punctuation, non-BMP);
//   - adversarial multi-scalar sequences (canonical-combining-class reordering
//     across original rune boundaries, recursive compatibility decompositions).
//
// Running with BAMLUNICODE_UPDATE_FIXTURES=1 also (re)writes the committed
// testdata/oracle_strings.json consumed by the always-on replay test.

package bamlunicode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const referenceDir = "testdata/reference"

func buildReference(t *testing.T) string {
	t.Helper()
	// rust-toolchain.toml pins 1.93.0; pass +1.93.0 explicitly so a missing
	// toolchain fails loudly rather than silently using the ambient one.
	cmd := exec.Command("cargo", "+1.93.0", "build", "--release")
	cmd.Dir = referenceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building rust reference (cargo +1.93.0 build --release):\n%s\n%v", out, err)
	}
	bin, err := filepath.Abs(filepath.Join(referenceDir, "target", "release", "bamlunicode-reference"))
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestRustReferenceVersions(t *testing.T) {
	bin := buildReference(t)
	out, err := exec.Command(bin, "versions").Output()
	if err != nil {
		t.Fatalf("reference versions: %v", err)
	}
	got := string(out)
	want := fmt.Sprintf("char=%s\nnorm=%s\n", RustStdUnicodeVersion, NormalizationUnicodeVersion)
	if got != want {
		t.Fatalf("reference reports %q, manifest expects %q", got, want)
	}
}

// TestRustReferenceExhaustive compares every scalar's primitives to the Rust
// reference, byte-for-byte.
func TestRustReferenceExhaustive(t *testing.T) {
	bin := buildReference(t)
	// The reference independently parses UCD 17.0.0 DerivedCoreProperties.txt to
	// emit raw Cased/Case_Ignorable bits; hand it the file extracted from the
	// verified archive.
	dcpPath := filepath.Join(t.TempDir(), "DerivedCoreProperties.txt")
	if err := os.WriteFile(dcpPath, extractUCD(t, "17.0.0", "DerivedCoreProperties.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "dump-scalars", dcpPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var scalars, mismatches int
	report := func(format string, args ...any) {
		mismatches++
		if mismatches <= 40 {
			t.Errorf(format, args...)
		}
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 {
			t.Fatalf("malformed dump line: %q", sc.Text())
		}
		cp, err := strconv.ParseInt(fields[0], 16, 32)
		if err != nil {
			t.Fatalf("bad cp %q: %v", fields[0], err)
		}
		r := rune(cp)
		flags, _ := strconv.ParseUint(fields[3], 16, 32)

		if got := hexOf(LowerString(string(r))); got != fields[1] {
			report("U+%04X lowercase: go=%s rust=%s", cp, got, fields[1])
		}
		if got := hexOf(NFKD(string(r))); got != fields[2] {
			report("U+%04X nfkd: go=%s rust=%s", cp, got, fields[2])
		}
		if got := IsAlphanumeric(r); got != (flags&1 != 0) {
			report("U+%04X is_alphanumeric: go=%v rust=%v", cp, got, flags&1 != 0)
		}
		if got := IsWhitespace(r); got != (flags&2 != 0) {
			report("U+%04X is_whitespace: go=%v rust=%v", cp, got, flags&2 != 0)
		}
		if got := IsCombiningMark(r); got != (flags&4 != 0) {
			report("U+%04X is_combining_mark: go=%v rust=%v", cp, got, flags&4 != 0)
		}
		// Independently prove Cased and Case_Ignorable membership for EVERY
		// scalar. bit5/bit6 are the reference's own parse of the authoritative
		// UCD DerivedCoreProperties.txt (what rustc std's tables derive from),
		// so a wrong table entry — including in the Cased ∩ Case_Ignorable
		// overlap — fails here directly.
		if got := isCased(r); got != (flags&32 != 0) {
			report("U+%04X Cased: go=%v ref=%v", cp, got, flags&32 != 0)
		}
		if got := isCaseIgnorable(r); got != (flags&64 != 0) {
			report("U+%04X Case_Ignorable: go=%v ref=%v", cp, got, flags&64 != 0)
		}
		// Additional behavioral coverage: both properties through their sole
		// observable channel (Final_Sigma). bit3: X+Σ word-final; bit4: a+X+Σ.
		if got := sigmaFinal(string(r) + string(capitalSigma)); got != (flags&8 != 0) {
			report("U+%04X cased-via-sigma (X+Σ): go=%v rust=%v", cp, got, flags&8 != 0)
		}
		if got := sigmaFinal("a" + string(r) + string(capitalSigma)); got != (flags&16 != 0) {
			report("U+%04X caseignorable-via-sigma (a+X+Σ): go=%v rust=%v", cp, got, flags&16 != 0)
		}
		scalars++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reference dump-scalars: %v", err)
	}
	// 0x110000 total minus 0x800 surrogates.
	if want := 0x110000 - 0x800; scalars != want {
		t.Fatalf("compared %d scalars, expected %d", scalars, want)
	}
	if mismatches > 0 {
		t.Fatalf("%d scalar mismatches vs Rust reference (byte-for-byte proof failed)", mismatches)
	}
	t.Logf("exhaustive: %d scalars match Rust byte-for-byte", scalars)
}

// TestRustReferenceStrings proves multi-scalar contexts against the reference and
// (re)writes the committed replay fixture.
func TestRustReferenceStrings(t *testing.T) {
	bin := buildReference(t)

	reqs := oracleRequests()
	outs := runStrmap(t, bin, reqs)
	if len(outs) != len(reqs) {
		t.Fatalf("reference returned %d outputs for %d requests", len(outs), len(reqs))
	}

	var fixture []oracleCase
	for i, rq := range reqs {
		// Prove Go matches Rust live.
		var goOut string
		switch rq.mode {
		case "L":
			goOut = LowerString(rq.in)
		case "K":
			goOut = NFKD(rq.in)
		case "T":
			goOut = TrimSpace(rq.in)
		}
		if hexOf(goOut) != outs[i] {
			t.Errorf("mode %s in=%s: go=%s rust=%s", rq.mode, hexOf(rq.in), hexOf(goOut), outs[i])
		}
		fixture = append(fixture, oracleCase{Mode: rq.mode, In: hexOf(rq.in), Out: outs[i]})
	}

	if os.Getenv("BAMLUNICODE_UPDATE_FIXTURES") == "1" {
		writeOracleFixture(t, fixture)
	}
}

type strReq struct {
	mode string
	in   string
}

func runStrmap(t *testing.T, bin string, reqs []strReq) []string {
	t.Helper()
	cmd := exec.Command(bin, "strmap")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		w := bufio.NewWriter(stdin)
		for _, rq := range reqs {
			fmt.Fprintf(w, "%s %s\n", rq.mode, hexOf(rq.in))
		}
		w.Flush()
		stdin.Close()
	}()
	var outs []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		outs = append(outs, strings.TrimSpace(sc.Text()))
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reference strmap: %v", err)
	}
	return outs
}

// oracleRequests builds the multi-scalar context corpus.
func oracleRequests() []strReq {
	var reqs []strReq

	// Final_Sigma contexts derived from the ACTUAL property equivalence classes
	// (sampled from the committed cased17/caseIgnorable17 tables), not fixed
	// literals: representatives are spread across each class so a bad range in
	// any region is exercised in-context, not just by the per-scalar bit3/bit4.
	casedOnly, ciOnly, both, neither := sampleSigmaClasses(6)
	cased := append(append([]rune{}, casedOnly...), both...)      // Cased scalars
	uncased := neither                                            // neither Cased nor Case_Ignorable
	caseIgnorable := append(append([]rune{}, ciOnly...), both...) // Case_Ignorable scalars
	sig := rune(0x03A3)

	type nb struct {
		name string
		rs   []rune
	}
	neighbors := []nb{{"empty", nil}}
	for _, r := range cased {
		neighbors = append(neighbors, nb{"cased", []rune{r}})
	}
	for _, r := range uncased {
		neighbors = append(neighbors, nb{"uncased", []rune{r}})
	}
	for _, ci := range caseIgnorable {
		neighbors = append(neighbors, nb{"ci", []rune{ci}})
		for _, c := range cased[:2] {
			neighbors = append(neighbors, nb{"ci+cased", []rune{ci, c}})
		}
	}
	for _, before := range neighbors {
		for _, after := range neighbors {
			var seq []rune
			seq = append(seq, before.rs...)
			seq = append(seq, sig)
			seq = append(seq, after.rs...)
			reqs = append(reqs, strReq{"L", string(seq)})
			// Two sigmas back-to-back with the same neighbours.
			seq2 := append([]rune{}, before.rs...)
			seq2 = append(seq2, sig, sig)
			seq2 = append(seq2, after.rs...)
			reqs = append(reqs, strReq{"L", string(seq2)})
		}
	}

	// Adversarial NFKD: canonical reordering across boundaries + recursive compat.
	base := []rune{'a', 0x0044, 0x1E0A, 0xFB01, 0xFDFA, 0x1D160, 0xAC01}
	marks := []rune{0x0300, 0x0323, 0x0316, 0x0301, 0x0307, 0x05B0, 0x093C, 0x3099}
	for _, b := range base {
		reqs = append(reqs, strReq{"K", string([]rune{b})})
		for i := range marks {
			// marks in given order and reversed order must both normalize equal.
			seq := []rune{b, marks[i], marks[(i+1)%len(marks)], marks[(i+2)%len(marks)]}
			reqs = append(reqs, strReq{"K", string(seq)})
			rev := []rune{b, marks[(i+2)%len(marks)], marks[(i+1)%len(marks)], marks[i]}
			reqs = append(reqs, strReq{"K", string(rev)})
		}
	}
	// A long chain of same-ccc and cross-ccc marks around multiple starters.
	reqs = append(reqs, strReq{"K", string([]rune{
		0x0044, 0x0307, 0x0323, 0x0316, 0x0301, 0x0061, 0x0323, 0x0300, 0xFB03, 0x0301,
	})})

	// Lowercase of full strings mixing many delta scalars.
	reqs = append(reqs, strReq{"L", string([]rune{0xA7DC, 0x0130, 0x1E9E, 0x0041, 0x03A3})})

	// Trim whitespace (Rust str::trim / char::is_whitespace).
	reqs = append(reqs, strReq{"T", string([]rune{0x0020, 0x00A0, 0x2009, 'h', 'i', 0x3000, 0x000A})})
	reqs = append(reqs, strReq{"T", "  plain  "})

	return reqs
}

func writeOracleFixture(t *testing.T, cases []oracleCase) {
	t.Helper()
	data, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "oracle_strings.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d oracle cases to %s", len(cases), path)
}

// sigmaFinal reports whether lowercasing s yields a trailing final sigma
// (U+03C2). Used to probe Cased/Case_Ignorable through their sole observable
// channel (str::to_lowercase's Final_Sigma context).
func sigmaFinal(s string) bool {
	lowered := LowerString(s)
	r, _ := utf8.DecodeLastRuneInString(lowered)
	return r == finalSigma
}

// sampleSigmaClasses partitions every scalar by (Cased, Case_Ignorable) and
// returns up to n representatives spread across each of the four equivalence
// classes, sourced from the committed property tables.
func sampleSigmaClasses(n int) (casedOnly, ciOnly, both, neither []rune) {
	var c, ci, b, ne []rune
	for cp := rune(0); cp <= 0x10FFFF; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		isC, isCI := isCased(cp), isCaseIgnorable(cp)
		switch {
		case isC && isCI:
			b = append(b, cp)
		case isC:
			c = append(c, cp)
		case isCI:
			ci = append(ci, cp)
		default:
			ne = append(ne, cp)
		}
	}
	return spread(c, n), spread(ci, n), spread(b, n), spread(ne, n)
}

// spread returns up to n elements of xs evenly sampled across its length.
func spread(xs []rune, n int) []rune {
	if len(xs) <= n {
		return xs
	}
	out := make([]rune, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, xs[i*len(xs)/n])
	}
	return out
}
