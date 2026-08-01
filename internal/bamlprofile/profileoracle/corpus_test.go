package profileoracle

import (
	"strings"
	"testing"
)

// These are structural (no build tag, CFFI-free): they assert the corpus is
// well-formed and the profile leg renders every row without error. They assert
// NO expected output values — the byte-exact authority is the stock-BAML
// integration differential (oracle_integration_test.go, `integration` tag).

func TestCorpusRowIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	fns := map[string]bool{}
	for _, r := range Corpus() {
		if r.ID == "" {
			t.Fatal("a corpus row has an empty ID")
		}
		if seen[r.ID] {
			t.Fatalf("duplicate corpus row ID %q", r.ID)
		}
		seen[r.ID] = true
		fn := r.FuncName()
		if fns[fn] {
			t.Fatalf("two rows map to the same BAML function name %q", fn)
		}
		fns[fn] = true

		// Params and Args must agree: every declared param is bound by an arg, and
		// every arg binds a declared param. A mismatch would render/encode wrong.
		params := map[string]bool{}
		for _, p := range r.Params {
			if params[p.Name] {
				t.Errorf("row %q (%s): duplicate param %q", r.ID, fn, p.Name)
			}
			params[p.Name] = true
			if _, ok := r.Args[p.Name]; !ok {
				t.Errorf("row %q (%s): param %q has no matching arg", r.ID, fn, p.Name)
			}
		}
		for name := range r.Args {
			if !params[name] {
				t.Errorf("row %q (%s): arg %q has no matching param", r.ID, fn, name)
			}
		}
	}
}

// TestDedentAndTrim exercises dedentAndTrim's indented-dedent branch, which the
// column-0 corpus never reaches (its min indent is always 0). It mirrors BAML's
// render_minijinja preprocessing: dedent by the minimum leading whitespace of
// non-empty lines, then trim.
func TestDedentAndTrim(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"column0_noop", "\nfoo\n", "foo"},
		{"indented", "    a\n      b\n    c", "a\n  b\nc"},
		{"line_shorter_than_min", "    a\n  \n    b", "a\n\nb"},
		{"tabs", "\t\tx\n\t\t\ty", "x\n\ty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dedentAndTrim(tc.in); got != tc.want {
				t.Errorf("dedentAndTrim(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRawStringHashes pins the collision-proof fence sizing: N is one more than
// the longest run of '#' immediately following a '"' in the body (the only thing
// that can prematurely close a BAML raw string), capped at BAML's 5-hash max.
func TestRawStringHashes(t *testing.T) {
	cases := []struct {
		name string
		body string
		n    int
		ok   bool
	}{
		{"plain", "hello", 1, true},                    // no quote -> original #"..."#
		{"lone_quote", `a"b`, 1, true},                 // quote not followed by # -> N=1
		{"trailing_quote", `foo"`, 1, true},            // trailing quote is harmless
		{"leading_hashes", `###foo`, 1, true},          // '#' not after a quote is harmless
		{"quote_hash1", `a"#b`, 2, true},               // "# -> needs N=2
		{"quote_hash2", `a"##b`, 3, true},              // "## -> N=3
		{"quote_hash4_max", `a"####b`, 5, true},        // "#### -> N=5 (BAML max)
		{"quote_hash5_overflow", `a"#####b`, 6, false}, // "##### -> N=6 > 5, unrepresentable
		{"max_of_several", `x"# y"### z"##`, 4, true},  // longest post-quote run is 3 -> N=4
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if n, ok := rawStringHashes(tc.body); n != tc.n || ok != tc.ok {
				t.Errorf("rawStringHashes(%q) = (%d, %v), want (%d, %v)", tc.body, n, ok, tc.n, tc.ok)
			}
		})
	}
}

// TestFunctionSourceFencesArbitraryBody proves functionSource embeds a body that
// carries the raw-string terminator without truncating it: the fence grows to
// N+1 hashes and the full body survives verbatim in the generated source. This is
// the CFFI-free half of the delimiter proof (the CreateRuntime round-trip is
// TestRawStringDelimiterRoundtrip, integration tag). The row mirrors the reported
// arbitrary-pattern vector: a regex_match literal bearing a quote+hash (\"#), so
// the generated .baml body contains the raw-string terminator "#.
func TestFunctionSourceFencesArbitraryBody(t *testing.T) {
	r := Row{ID: "fence_rx", Params: []Param{{Name: "s", BamlType: "string"}},
		Args: map[string]any{"s": `x"#y`}, Template: `{{ s|regex_match("x\"#y") }}`}
	src := functionSource(r)
	body := r.blockContent()
	if !strings.Contains(src, body) {
		t.Fatalf("generated source truncated the body:\nbody: %q\nsrc:  %q", body, src)
	}
	// "# needs a 2-hash fence; the source must open ##" and close "## around it.
	if !strings.Contains(src, `prompt ##"`) || !strings.Contains(src, `"##`+"\n}") {
		t.Errorf("expected a 2-hash (##\"...\"##) fence around a \"#-bearing body; got:\n%s", src)
	}
}

// TestFunctionSourcePanicsOnUnrepresentableBody pins the fail-loud guard: a body
// with a '"' followed by 5+ '#' exceeds BAML's 5-hash maximum, so no fence can
// hold it — the generator must panic, never silently truncate.
func TestFunctionSourcePanicsOnUnrepresentableBody(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected functionSource to panic on an unrepresentable body")
		}
	}()
	_ = functionSource(Row{ID: "overflow", Template: `x"#####y`})
}

// TestProfileLegRendersEveryRow covers every row that does NOT declare a Fault.
// A fault row is expected to fail on both legs, so requiring it to render here
// would contradict its own declaration; TestProfileFaultRowsFaultOnProfileLeg
// asserts the other half.
func TestProfileLegRendersEveryRow(t *testing.T) {
	for _, r := range Corpus() {
		if r.Fault != "" {
			continue
		}
		t.Run(r.ID, func(t *testing.T) {
			out, err := RenderProfile(r)
			if err != nil {
				t.Fatalf("profile render error: %v", err)
			}
			if r.Chat {
				if _, err := SplitChat(out); err != nil {
					t.Fatalf("chat split error: %v (rendered %q)", err, out)
				}
			}
		})
	}
}

// TestProfileFaultRowsFaultOnProfileLeg is the CGO-free half of the fault
// contract: a row that declares stock BAML faults must ALSO fault on the profile
// leg, in the declared class. Rendering a value where BAML faults is the
// parity-decline rule's out-do, and this catches it without CFFI.
//
// The stock half — that BAML really produces the declared class — needs the live
// runtime and lives in TestProfileFaultDifferential (integration tag).
func TestProfileFaultRowsFaultOnProfileLeg(t *testing.T) {
	seen := 0
	for _, r := range Corpus() {
		if r.Fault == "" {
			continue
		}
		seen++
		t.Run(r.ID, func(t *testing.T) {
			switch r.Fault {
			case OutcomeError, OutcomePanic:
			default:
				t.Fatalf("Fault=%q is not a failure class", r.Fault)
			}
			if o := RenderProfileOutcome(r); o.Kind != r.Fault {
				t.Errorf("profile outcome = %s, want class %s", o, r.Fault)
			}
		})
	}
	if seen == 0 {
		t.Fatal("no fault rows in the corpus; the fault contract must not silently cover nothing")
	}
}

// TestProfileFaultDeclarationsAreFailureClasses guards the one way a row could
// silently escape the differential entirely.
//
// Row.Fault routes a row: empty sends it to the byte-exact
// TestProfileDifferential, non-empty sends it to the outcome-class
// TestProfileFaultDifferential. So `Fault: OutcomeRendered` would be accepted by
// the compiler, removed from the byte comparison as a "fault row", and then
// trivially satisfied by a successful render — a row that looks covered twice
// and is actually proved by nothing. Only a FAILURE class may appear here.
func TestProfileFaultDeclarationsAreFailureClasses(t *testing.T) {
	byteCompared, outcomeCompared := 0, 0
	for _, r := range Corpus() {
		switch r.Fault {
		case "":
			byteCompared++
		case OutcomeError, OutcomePanic:
			outcomeCompared++
		default:
			t.Errorf("row %q declares Fault=%q, which is not a failure class; "+
				"only %q and %q route a row to the outcome differential",
				r.ID, r.Fault, OutcomeError, OutcomePanic)
		}
	}
	t.Logf("corpus: %d rows — %d byte-compared, %d outcome-compared",
		len(Corpus()), byteCompared, outcomeCompared)
}

// TestRenderProfileOutcomeFailsLoudOnHarnessError proves the fault-outcome proof
// cannot be satisfied by a broken HARNESS. A row whose declared host parameter is
// missing from Args fails during arg lowering — a *harnessError — not during the
// engine's render. Classifying that as OutcomeError would let a fault row that
// declares OutcomeError pass because the harness failed to set the row up rather
// than because the engine faulted. RenderProfileOutcome re-raises it instead, so
// the row cannot masquerade as an engine outcome.
func TestRenderProfileOutcomeFailsLoudOnHarnessError(t *testing.T) {
	r := Row{
		ID:       "harness_failure_probe",
		Fault:    OutcomeError,
		Params:   []Param{{Name: "c", BamlType: "C"}},
		Args:     map[string]any{}, // the declared host arg is absent -> lowering fails
		Template: `{{ c }}`,
	}
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("RenderProfileOutcome classified a harness setup failure as an outcome instead of failing loudly")
		}
	}()
	o := RenderProfileOutcome(r)
	t.Fatalf("RenderProfileOutcome returned %v instead of re-raising the harness failure", o)
}
