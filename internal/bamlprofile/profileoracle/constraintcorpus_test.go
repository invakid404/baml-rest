package profileoracle

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// These are structural (no build tag, CFFI-free): they assert the constraint
// corpus is well-formed and that the profile leg classifies every row. They
// assert NO expected outcome VALUES beyond each row's own declaration — the
// authority is the stock-BAML differential in constraint_integration_test.go.

func TestConstraintCorpusRowIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	fns := map[string]bool{}
	for _, r := range ConstraintCorpus() {
		if r.ID == "" {
			t.Fatal("a constraint row has an empty ID")
		}
		if seen[r.ID] {
			t.Fatalf("duplicate constraint row ID %q", r.ID)
		}
		seen[r.ID] = true
		fn := r.FuncName()
		if fns[fn] {
			t.Fatalf("two constraint rows map to the same BAML function name %q", fn)
		}
		fns[fn] = true
	}
}

// TestConstraintCorpusIsWellFormed pins the invariants the generated .baml
// source depends on. A violation would otherwise surface as an opaque
// CreateRuntime parse error naming neither the row nor the reason.
func TestConstraintCorpusIsWellFormed(t *testing.T) {
	for _, r := range ConstraintCorpus() {
		t.Run(r.ID, func(t *testing.T) {
			if r.ReturnType == "" {
				t.Error("empty ReturnType")
			}
			if len(r.Constraints) == 0 {
				t.Error("no constraints; the row would prove nothing about the evaluator")
			}
			for i, c := range r.Constraints {
				switch c.Level {
				case bamlprofile.ConstraintCheck:
					if c.Label == nil {
						t.Errorf("constraint %d is an unlabelled check; BAML rejects it at CreateRuntime", i)
					}
				case bamlprofile.ConstraintAssert:
				default:
					t.Errorf("constraint %d has level %v, which is not a BAML level", i, c.Level)
				}
				if c.Label != nil && !isBAMLIdentifier(*c.Label) {
					t.Errorf("constraint %d label %q is not a BAML identifier", i, *c.Label)
				}
				if strings.Contains(c.Expression, "{{") || strings.Contains(c.Expression, "}}") {
					t.Errorf("constraint %d expression %q carries jinja brackets; it must be the BARE expression", i, c.Expression)
				}
				if strings.TrimSpace(c.Expression) == "" {
					t.Errorf("constraint %d has an empty expression", i)
				}
			}
			// The row's This must lower through the same path the profile leg uses,
			// with the row's declared return type.
			if _, err := hostValue(r.ReturnType, r.This); err != nil {
				t.Errorf("This does not lower as %s: %v", r.ReturnType, err)
			}
			// The generator must be able to emit the row.
			src := constraintFunctionSource(r)
			if !strings.Contains(src, "-> "+r.ReturnType) {
				t.Errorf("generated source does not declare the return type:\n%s", src)
			}
		})
	}
}

// TestConstraintExpectDeclarationsAreOutcomeClasses guards the one way a row
// could silently escape the differential.
//
// ConstraintRow.Expect routes a row: empty means both legs must PARSE, non-empty
// declares a failure class asserted on both legs. `Expect: ConstraintParsed`
// would compile, look like a declaration, and be trivially satisfied — so only a
// genuine failure class may appear. ConstraintUnsupported is a PROFILE-ONLY
// decline with no stock counterpart, so it may never be declared either.
func TestConstraintExpectDeclarationsAreOutcomeClasses(t *testing.T) {
	parsed, declared := 0, 0
	for _, r := range ConstraintCorpus() {
		switch r.Expect {
		case "":
			parsed++
		case ConstraintAssertFailed, ConstraintEvalError, ConstraintPanic:
			declared++
		default:
			t.Errorf("row %q declares Expect=%q, which is not a stock failure class; "+
				"only %q, %q and %q route a row to the failure comparison",
				r.ID, r.Expect, ConstraintAssertFailed, ConstraintEvalError, ConstraintPanic)
		}
	}
	if declared == 0 {
		t.Fatal("no rows declare a failure class; the failure contract must not silently cover nothing")
	}
	t.Logf("constraint corpus: %d rows — %d expected to parse, %d declared failures",
		len(ConstraintCorpus()), parsed, declared)
}

// TestConstraintCorpusCoversEverySurface pins that the minimum discriminating
// surface list from the settled scope is actually populated. Without it a future
// edit could delete every enum-projection row and still leave a green suite.
func TestConstraintCorpusCoversEverySurface(t *testing.T) {
	want := []string{"core", "levels", "get_env", "enum", "class", "list", "isolation", "fault"}
	have := map[string]int{}
	for _, r := range ConstraintCorpus() {
		have[r.Surface]++
	}
	for _, s := range want {
		if have[s] == 0 {
			t.Errorf("no constraint rows for the required surface %q", s)
		}
	}
	for s := range have {
		if !slicesContains(want, s) {
			t.Errorf("row surface %q is not in the declared surface list; add it there so its coverage is guarded", s)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestProfileLegClassifiesEveryConstraintRow is the CGO-free half of the
// constraint contract: every row must reach a CLASSIFIED profile outcome, and a
// row that declares a failure class must produce that class on the profile leg.
// Parsing where stock fails is the parity-decline rule's out-do, and this catches
// it without CFFI; the stock half (that BAML really produces the declared class)
// needs the live runtime and lives in TestConstraintDifferential.
//
// ConstraintUnsupported is failed explicitly rather than compared: it is a
// profile-side decline with no stock counterpart, so a row producing it is a real
// gap in the projection, not a parity result.
func TestProfileLegClassifiesEveryConstraintRow(t *testing.T) {
	for _, r := range ConstraintCorpus() {
		t.Run(r.ID, func(t *testing.T) {
			got := ConstraintOutcomeProfile(r)
			if got.Kind == ConstraintUnsupported {
				t.Fatalf("the profile DECLINED this row (%s); a corpus row must exercise the evaluator, "+
					"not the projection's fail-closed path", got.Detail)
			}
			want := r.Expect
			if want == "" {
				want = ConstraintParsed
			}
			if got.Kind != want {
				t.Errorf("profile outcome = %s, want class %s", got, want)
			}
		})
	}
}

// TestConstraintOutcomeProfileFailsLoudOnHarnessError proves the constraint
// contract cannot be satisfied by a broken HARNESS. A row whose This does not
// match its declared return type fails during lowering — a *harnessError, not the
// leaf's verdict. Classifying that as an evaluator error would let a row
// declaring ConstraintEvalError pass because the harness failed to set it up.
func TestConstraintOutcomeProfileFailsLoudOnHarnessError(t *testing.T) {
	r := ConstraintRow{
		ID:          "constraint_harness_failure_probe",
		ReturnType:  "C",
		Raw:         `{"prop1": "value"}`,
		This:        "not a class map",
		Constraints: []ConstraintDecl{as_("true")},
		Expect:      ConstraintEvalError,
	}
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("ConstraintOutcomeProfile classified a harness lowering failure as an outcome instead of failing loudly")
		}
	}()
	o := ConstraintOutcomeProfile(r)
	t.Fatalf("ConstraintOutcomeProfile returned %v instead of re-raising the harness failure", o)
}

// fakeCheck mirrors baml_go/shared.Check's field set (Name/Expression/Status),
// and fakeChecked mirrors shared.Checked[T] (Value + Checks). The stock reader is
// reflective by design — so that adding a checked return type needs only a
// binding — which is exactly what lets these stand in for the real decoded types
// without linking the CFFI.
type fakeCheck struct {
	Name       string
	Expression string
	Status     string
}

type fakeChecked struct {
	Value  any
	Checks map[string]fakeCheck
}

// TestStockChecksOfShapeContract pins the stock reader's fail-loud contract in
// BOTH directions.
//
// The rule it enforces: a row's own declaration decides which result SHAPE is
// legal. Returning an empty check set for a shape the harness could not decode
// would either make that row's check comparison vacuously green, or surface later
// as a generic "check mismatch" against the profile leg that hides an undecodable
// stock result behind what looks like a parity failure.
func TestStockChecksOfShapeContract(t *testing.T) {
	checked := fakeChecked{Value: "v", Checks: map[string]fakeCheck{
		"b": {Name: "b", Expression: "this > 1", Status: "failed"},
		"a": {Name: "a", Expression: "this > 0", Status: "succeeded"},
	}}

	t.Run("declared_check_reads_and_sorts", func(t *testing.T) {
		got, err := stockChecksOf(checked, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []CheckOutcome{
			{Label: "a", Expression: "this > 0", Passed: true},
			{Label: "b", Expression: "this > 1", Passed: false},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d checks, want %d: %#v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("check %d = %#v, want %#v", i, got[i], want[i])
			}
		}
	})

	// An EMPTY check list on a wrapped value is legitimate, not a shape failure:
	// stock returns exactly that for a bare `string` return type, whose
	// constraints it never evaluates.
	t.Run("declared_check_empty_map_is_not_an_error", func(t *testing.T) {
		got, err := stockChecksOf(fakeChecked{Value: "v", Checks: map[string]fakeCheck{}}, true)
		if err != nil {
			t.Fatalf("an empty Checks map must not be a shape error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v, want no checks", got)
		}
	})

	// THE FINDING: a declared-check row whose result is not a Checked[T] must be
	// reported as an undecodable result, not silently as "no checks".
	t.Run("declared_check_undecodable_results_fail_loud", func(t *testing.T) {
		cases := []struct {
			name   string
			result any
		}{
			{"nil", nil},
			{"plain_string", "hello"},
			{"plain_slice", []string{"a"}},
			{"struct_without_checks", struct{ Name, Value string }{"C", "v"}},
			{"checks_not_a_map", struct{ Checks string }{"nope"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := stockChecksOf(tc.result, true)
				if err == nil {
					t.Fatalf("a declared-check row decoded %T as %#v checks instead of failing loud", tc.result, got)
				}
				if got != nil {
					t.Errorf("an errored read returned %#v, want no checks", got)
				}
			})
		}
	})

	// An UNCHECKED row legitimately decodes to a plain value, and reads as no
	// checks rather than as a failure.
	t.Run("undeclared_check_plain_results_read_as_none", func(t *testing.T) {
		for _, result := range []any{nil, "hello", int64(5), []string{"a"}, struct{ Name, Value string }{"C", "v"}} {
			got, err := stockChecksOf(result, false)
			if err != nil {
				t.Errorf("%T: unexpected error: %v", result, err)
			}
			if len(got) != 0 {
				t.Errorf("%T: got %#v, want no checks", result, got)
			}
		}
	})

	// The other direction: the CFFI wraps only a checked type, so a wrapped result
	// for a row that declares no check means the row and the generated source
	// disagree — a corpus bug, reported rather than absorbed.
	t.Run("undeclared_check_wrapped_result_fails_loud", func(t *testing.T) {
		if got, err := stockChecksOf(checked, false); err == nil {
			t.Fatalf("a wrapped result for an unchecked row read as %#v instead of failing loud", got)
		}
	})

	// A status outside stock's two spellings must not silently become "failed".
	t.Run("unknown_status_fails_loud", func(t *testing.T) {
		bad := fakeChecked{Checks: map[string]fakeCheck{"a": {Name: "a", Expression: "x", Status: "pending"}}}
		if got, err := stockChecksOf(bad, true); err == nil {
			t.Fatalf("status %q read as %#v instead of failing loud", "pending", got)
		}
	})

	// A check entry missing one of the three strings is a shape failure too.
	t.Run("incomplete_check_entry_fails_loud", func(t *testing.T) {
		bad := struct {
			Checks map[string]struct{ Name string }
		}{
			Checks: map[string]struct{ Name string }{"a": {Name: "a"}},
		}
		if got, err := stockChecksOf(bad, true); err == nil {
			t.Fatalf("a check entry without Expression/Status read as %#v instead of failing loud", got)
		}
	})
}

// TestConstraintRowDeclaresCheck pins the predicate that selects the stock result
// shape, since getting it backwards would silently relax the reader's contract
// for every checked row.
func TestConstraintRowDeclaresCheck(t *testing.T) {
	if (ConstraintRow{Constraints: []ConstraintDecl{as_("true"), asl("l", "true")}}).DeclaresCheck() {
		t.Error("assert-only row reported as declaring a check")
	}
	if !(ConstraintRow{Constraints: []ConstraintDecl{as_("true"), ck("c", "true")}}).DeclaresCheck() {
		t.Error("row with a check reported as declaring none")
	}
	if (ConstraintRow{}).DeclaresCheck() {
		t.Error("constraint-free row reported as declaring a check")
	}

	// Every corpus row's answer must match its generated source, which is the
	// property the stock reader relies on.
	for _, r := range ConstraintCorpus() {
		src := constraintFunctionSource(r)
		if got, want := r.DeclaresCheck(), strings.Contains(src, " @check("); got != want {
			t.Errorf("row %q: DeclaresCheck()=%v but the generated source %s a @check", r.ID, got,
				map[bool]string{true: "carries", false: "does not carry"}[want])
		}
	}
}

// TestConstraintSourceIsDeterministic pins that the generated project does not
// depend on row order or Go map iteration — the source hash in the checked-in
// fixture would otherwise drift between runs and the guard would be worthless.
func TestConstraintSourceIsDeterministic(t *testing.T) {
	rows := ConstraintCorpus()
	reversed := make([]ConstraintRow, len(rows))
	for i, r := range rows {
		reversed[len(rows)-1-i] = r
	}
	a, b := GenerateConstraintBAMLSource(rows), GenerateConstraintBAMLSource(reversed)
	if len(a) != len(b) {
		t.Fatalf("file sets differ: %d vs %d", len(a), len(b))
	}
	for name, content := range a {
		if b[name] != content {
			t.Errorf("%s differs between row orders", name)
		}
	}
}

// TestConstraintFunctionSourcePanicsOnMalformedDeclaration pins the fail-loud
// generator guards: an unlabelled check and a bracket-wrapped expression must
// panic at generation time rather than emit .baml stock BAML rejects with an
// error that names neither.
func TestConstraintFunctionSourcePanicsOnMalformedDeclaration(t *testing.T) {
	cases := []struct {
		name string
		decl ConstraintDecl
	}{
		{"unlabelled_check", ConstraintDecl{Level: bamlprofile.ConstraintCheck, Expression: "true"}},
		{"bracket_wrapped", as_("{{ true }}")},
		{"unknown_level", ConstraintDecl{Level: bamlprofile.ConstraintLevel(0), Expression: "true"}},
		{"label_with_space", ck("not an identifier", "true")},
		{"label_with_paren", ck("bad)label", "true")},
		{"label_leading_digit", ck("1bad", "true")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected constraintAttributeSource to panic")
				}
			}()
			_ = constraintAttributeSource(tc.decl)
		})
	}
}
