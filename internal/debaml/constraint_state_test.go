package debaml

// Discriminating tests for the TEST-ONLY canonical coercion-state collector
// (de-BAML Slice 7.2a-2). See constraint_state_collect_test.go for the model.
//
// HOW THESE ASSERT, and why it is written out. A constraint witness is only
// worth what its assertions are worth, so every test here states the EXACT
// outcome — the canonical identity, the entry order, the disposition, the event
// order with labels and results — usually through one equality against a
// rendered form ([constraintStateDescribe] / [constraintStateEvent.describe]).
// None of them asserts "no error", non-nil, truthiness or len>0.
//
// Three rules are applied throughout:
//
//   - NO VACUOUS SET ARMS. Every assertion that quantifies over a set requires
//     the set to have an exact size first ([requireConstraintStateCount]), or
//     tests the empty case as its explicit subject with a non-empty CONTROL
//     beside it.
//   - NO FALSE-GREEN SKIPS. A skip/decline arm asserts POSITIVE evidence it
//     reached that disposition: the counterfactual outcome the evaluator
//     produced, the marker node's path and reason, and a CONTROL where the same
//     constrained type does evaluate. Absence of events is never proof.
//   - PRODUCTION STAYS DECLINED. Every fixture asserts, in the SAFE direction
//     only, that checkSupported still refuses its bundle, so no state result can
//     be misread as admission moving.
//
// THE INVARIANT IS NOW UNNARROWED. "Constraint-bearing bundles still decline"
// holds for EVERY shape in this file, with no exception. It did not always: a
// constraint declared on `b.Target` ITSELF (a bare-string return type, or a
// constrained element/value of a target list/map) used to be ADMITTED, because
// checkSupported -> checkSupportedFields walked b.Enums and b.Classes and never
// walked b.Target. That pre-existing over-claim was carried here as a documented,
// temporary exception and an asserted tripwire while the collector slice (which
// was TEST-ONLY and could not touch a gate) landed. The decline-more fix has since
// walked the target in checkSupportedFields, the tripwire tripped as designed, and
// its fixtures now live in the ordinary declining set:
// [TestConstraintStateConstrainedBundlesAreStillRefused] hard-asserts EVERY
// constrained shape — target-level included — through checkSupported,
// SupportsNativeFinalBundle AND ParseStaticBundle, and
// [TestTargetLevelConstraintDeclineClassification] keeps the closed gap's own
// evidence: per shape, the exact value native WOULD have served, and which of
// stock v0.223's three dispositions that value met — an out-claim removed, or
// plain over-decline. The stock half of that is measured live through CFFI by
// TestStockTargetLevelConstraintDispositionAndNativeDecline in
// internal/bamlprofile/profileoracle, never inferred from the native evaluator.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

func constraintStateStringType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
}

func constraintStateIntType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
}

func constraintStateOptional(t schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{t}, Nullable: true}}
}

func ptrConstraintStateType(t schema.Type) *schema.Type { return &t }

func strPtrConstraintState(s string) *string { return &s }

// constraintStateAppendConstraint appends one predicate to a COPY of the type's
// constraint slice.
//
// THE COPY IS LOAD-BEARING. schema.Type is passed by value, but Meta.Constraints
// still shares its backing array with the caller, so two descents from the same
// base type would write the same slot:
//
//	base := constraintStateCheck(constraintStateIntType(), "a", "this > 0")
//	x := constraintStateCheck(base, "b", "this > 1")
//	y := constraintStateCheck(base, "c", "this > 2")  // could clobber x's predicate
//
// A fixture that silently declared the wrong predicate is exactly the false-green
// this witness suite exists to prevent, so the aliasing is removed rather than
// avoided by convention.
func constraintStateAppendConstraint(t schema.Type, c schema.Constraint) schema.Type {
	next := make([]schema.Constraint, 0, len(t.Meta.Constraints)+1)
	next = append(next, t.Meta.Constraints...)
	t.Meta.Constraints = append(next, c)
	return t
}

// constraintStateCheck / constraintStateAssert attach a LABELLED predicate,
// appending so declaration order is exactly the call order.
func constraintStateCheck(t schema.Type, label, expr string) schema.Type {
	return constraintStateAppendConstraint(t, schema.Constraint{
		Level: schema.ConstraintCheck, Expression: expr, Label: &label,
	})
}

func constraintStateAssert(t schema.Type, label, expr string) schema.Type {
	return constraintStateAppendConstraint(t, schema.Constraint{
		Level: schema.ConstraintAssert, Expression: expr, Label: &label,
	})
}

// TestConstraintStateFixtureAppendersDoNotAliasSiblings pins the copy above.
// Without it the second descent from a shared base overwrites the first's
// predicate, so a fixture would declare something other than what it reads.
//
// SPARE CAPACITY IS THE CASE THAT MATTERS, exactly as for
// [constraintStatePath.descend]. With cap == len a bare append allocates a fresh
// array anyway, so a base built by the helpers themselves cannot exhibit the bug
// and a test using one would be vacuous. The base here is therefore constructed
// with room to spare — the shape any future fixture builder could produce — and
// the spare capacity is asserted so the case cannot silently disappear.
func TestConstraintStateFixtureAppendersDoNotAliasSiblings(t *testing.T) {
	base := constraintStateIntType()
	base.Meta.Constraints = make([]schema.Constraint, 1, 4)
	base.Meta.Constraints[0] = schema.Constraint{
		Level: schema.ConstraintCheck, Expression: "this > 0", Label: strPtrConstraintState("a"),
	}
	if got, want := cap(base.Meta.Constraints)-len(base.Meta.Constraints), 3; got != want {
		t.Fatalf("fixture has %d spare capacity, want %d; the aliasing case would not be exercised", got, want)
	}

	x := constraintStateCheck(base, "b", "this > 1")
	y := constraintStateAssert(base, "c", "this > 2")
	// x is read AFTER y is built, which is when an aliasing append would already
	// have clobbered x's second predicate.
	if got, want := constraintStateDeclaredConstraints(x), []string{`check/"a"/this > 0`, `check/"b"/this > 1`}; !constraintStateStringsEqual(got, want) {
		t.Errorf("first descent = %v after a sibling descent, want %v", got, want)
	}
	if got, want := constraintStateDeclaredConstraints(y), []string{`check/"a"/this > 0`, `assert/"c"/this > 2`}; !constraintStateStringsEqual(got, want) {
		t.Errorf("second descent = %v, want %v", got, want)
	}
	if got, want := constraintStateDeclaredConstraints(base), []string{`check/"a"/this > 0`}; !constraintStateStringsEqual(got, want) {
		t.Errorf("base mutated by a descent: %v, want %v", got, want)
	}
}

// constraintStateStringsEqual compares two ordered string slices exactly,
// including length — so a shorter "got" can never satisfy a longer "want".
func constraintStateStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// constraintStateBundle assembles a bundle and builds its indexes, so
// FindClass/FindEnum behave exactly as they do for a lowered bundle.
func constraintStateBundle(t *testing.T, target schema.Type, classes []schema.ClassDef, enums []schema.EnumDef) *schema.Bundle {
	t.Helper()
	b := &schema.Bundle{Target: target, Classes: classes, Enums: enums}
	if err := b.RebuildIndexes(); err != nil {
		t.Fatalf("RebuildIndexes: %v", err)
	}
	return b
}

// constraintStateClass builds a non-streaming class definition.
func constraintStateClass(name string, fields ...schema.ClassField) schema.ClassDef {
	return schema.ClassDef{Name: schema.Name{Name: name}, Mode: schema.NonStreaming, Fields: fields}
}

// constraintStateField builds a field with an optional rendered ALIAS.
func constraintStateField(canonical, alias string, t schema.Type) schema.ClassField {
	n := schema.Name{Name: canonical}
	if alias != "" {
		n.Alias = strPtrConstraintState(alias)
	}
	return schema.ClassField{Name: n, Type: t}
}

// constraintStateClassType references a class by name.
func constraintStateClassType(name string) schema.Type {
	return schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming}
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// requireConstraintStateCount is the non-vacuity gate: an assertion that
// quantifies over a set must first prove the set has the size it claims. It
// FATALs, so a wrong count can never leave a later loop iterating over nothing.
func requireConstraintStateCount(t *testing.T, got, want int, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %d, want %d", what, got, want)
	}
}

// requireConstraintStateNode resolves a path and FATALs when it is absent, so a
// renamed/moved node fails loudly instead of skipping the assertions below it.
func requireConstraintStateNode(t *testing.T, run *constraintCoercionRun, path string) *constraintCoercionState {
	t.Helper()
	n := run.Root.find(path)
	if n == nil {
		var have []string
		run.Root.walk(func(s *constraintCoercionState) { have = append(have, s.Path.String()) })
		t.Fatalf("no state at path %s; collected paths: %v", path, have)
	}
	return n
}

// requireConstraintStateEvents pins a node's ENTIRE event list, in order.
func requireConstraintStateEvents(t *testing.T, st *constraintCoercionState, want []string) {
	t.Helper()
	requireConstraintStateCount(t, len(st.Events), len(want), st.Path.String()+": event count")
	for i := range want {
		if got := st.Events[i].describe(); got != want[i] {
			t.Errorf("%s: event %d = %s, want %s", st.Path, i, got, want[i])
		}
	}
}

// requireConstraintStateSkipped pins a node's ENTIRE skipped list, in order.
func requireConstraintStateSkipped(t *testing.T, st *constraintCoercionState, want []string) {
	t.Helper()
	requireConstraintStateCount(t, len(st.Skipped), len(want), st.Path.String()+": skipped count")
	for i := range want {
		if got := st.Skipped[i].describe(); got != want[i] {
			t.Errorf("%s: skipped %d = %s, want %s", st.Path, i, got, want[i])
		}
	}
}

// requireConstraintStateStillDeclines asserts the boundary invariant in the ONE
// direction that is safe to assert: a constraint-bearing bundle must still be
// refused.
//
// IT IS DELIBERATELY ONE-DIRECTIONAL. No fixture that runs through this helper
// may assert that a bundle is ACCEPTED; the safe direction is the only one it
// checks. EVERY fixture in this file routes through it, including the
// target-level ones: checkSupportedFields walks b.Target for constraints, so
// there is no longer a constrained shape the gate fails to reach.
func requireConstraintStateStillDeclines(t *testing.T, run *constraintCoercionRun) {
	t.Helper()
	got := run.ProductionSupport
	if got == nil {
		t.Fatalf("checkSupported ACCEPTED this constraint-bearing bundle; Slice 7.2a requires it to keep declining")
	}
	if !errors.Is(got, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("checkSupported returned %v; want the ErrDeBAMLParseUnsupported fallback sentinel", got)
	}
}

// collectConstraintStateFixture runs the collector, fails on any error, and
// asserts production still refuses the bundle.
func collectConstraintStateFixture(t *testing.T, b *schema.Bundle, raw string) *constraintCoercionRun {
	t.Helper()
	run, err := collectConstraintCoercionState(b, raw)
	if err != nil {
		t.Fatalf("collect(%s): %v", raw, err)
	}
	requireConstraintStateStillDeclines(t, run)
	return run
}

// ---------------------------------------------------------------------------
// Path rendering
// ---------------------------------------------------------------------------

func TestConstraintStatePathRendersEveryStepDistinctly(t *testing.T) {
	cases := []struct {
		name string
		path constraintStatePath
		want string
	}{
		{"root", constraintStatePath{{Kind: constraintPathRoot}}, "$"},
		{
			"field-then-index",
			constraintStatePath{{Kind: constraintPathRoot}, {Kind: constraintPathField, Name: "hand"}, {Kind: constraintPathIndex, Index: 2}},
			"$.hand[2]",
		},
		{
			"map-entry",
			constraintStatePath{{Kind: constraintPathRoot}, {Kind: constraintPathMapEntry, Name: "b"}},
			`$["b"]`,
		},
		{
			"map-key-type",
			constraintStatePath{{Kind: constraintPathRoot}, {Kind: constraintPathMapKeyType}},
			"$.<key>",
		},
		{
			"union-arm",
			constraintStatePath{{Kind: constraintPathRoot}, {Kind: constraintPathUnionArm, Index: 1}},
			"$|arm1",
		},
	}
	requireConstraintStateCount(t, len(cases), 5, "path cases")
	for _, c := range cases {
		if got := c.path.String(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// A map ENTRY named `b` and a FIELD named `b` must not collide, or a single
	// path string would address two different nodes.
	entry := constraintStatePath{{Kind: constraintPathRoot}, {Kind: constraintPathMapEntry, Name: "b"}}.String()
	field := constraintStatePath{{Kind: constraintPathRoot}, {Kind: constraintPathField, Name: "b"}}.String()
	if entry == field {
		t.Errorf("map entry and class field render identically as %q", entry)
	}
}

// TestConstraintStatePathDescendDoesNotAliasSiblings pins that two descents from
// the same parent produce independent paths.
//
// SPARE CAPACITY IS THE CASE THAT MATTERS. With cap == len a bare
// `append(p, seg)` allocates a fresh array anyway, so an aliasing implementation
// looks correct and the test would be vacuous. The parent here is built with room
// to spare, which is the shape where the second descent would overwrite the
// first's segment in place.
func TestConstraintStatePathDescendDoesNotAliasSiblings(t *testing.T) {
	base := make(constraintStatePath, 1, 4)
	base[0] = constraintStatePathSegment{Kind: constraintPathRoot}
	if got, want := cap(base)-len(base), 3; got != want {
		t.Fatalf("fixture has %d spare capacity, want %d; the aliasing case would not be exercised", got, want)
	}
	a := base.descend(constraintStatePathSegment{Kind: constraintPathField, Name: "a"})
	b := base.descend(constraintStatePathSegment{Kind: constraintPathField, Name: "b"})
	// `a` is read AFTER `b` is built, which is when an in-place append would
	// already have clobbered it.
	if got := a.String(); got != "$.a" {
		t.Errorf("first descent = %s after a sibling descent, want $.a", got)
	}
	if got := b.String(); got != "$.b" {
		t.Errorf("second descent = %s, want $.b", got)
	}
	if base.String() != "$" {
		t.Errorf("parent path mutated by a descent: %s", base)
	}
}

// ---------------------------------------------------------------------------
// Strict leaf readback
// ---------------------------------------------------------------------------

func TestConstraintStateScalarReadbackIsStrict(t *testing.T) {
	if _, err := constraintStateReadString([]byte(`"a" "b"`)); err == nil {
		t.Error("string readback accepted trailing data")
	}
	if _, err := constraintStateReadInt([]byte(`1 2`)); err == nil {
		t.Error("int readback accepted trailing data")
	}
	if _, err := constraintStateReadBool([]byte(`true false`)); err == nil {
		t.Error("bool readback accepted trailing data")
	}
	// A float-spelled token is NOT an exact i64 and must not be truncated into one.
	if _, err := constraintStateReadInt([]byte(`1.5`)); err == nil {
		t.Error("int readback accepted 1.5")
	}
	// 2^63 does not fit an i64 (BAML's Int is an i64).
	if _, err := constraintStateReadInt([]byte(`9223372036854775808`)); err == nil {
		t.Error("int readback accepted an out-of-i64 token")
	}
	if got, err := constraintStateReadInt([]byte(`-7`)); err != nil || got != -7 {
		t.Errorf("int readback(-7) = %d, %v", got, err)
	}
	if got, err := constraintStateReadFloat([]byte(`1.5`)); err != nil || got != 1.5 {
		t.Errorf("float readback(1.5) = %v, %v", got, err)
	}
	if got, err := constraintStateReadString([]byte(`"a"`)); err != nil || got != "a" {
		t.Errorf("string readback = %q, %v", got, err)
	}
	if got, err := constraintStateReadBool([]byte(`true`)); err != nil || got != true {
		t.Errorf("bool readback = %v, %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// The divergence comparator
// ---------------------------------------------------------------------------

// constraintStateRequireDistinctOperands is the non-vacuity gate for a comparator
// table: every row except the ONE whose subject is byte equality must feed the
// comparator two DIFFERENT documents.
//
// A row whose operands are byte-identical cannot exercise the normalization it
// names — it silently degrades into another copy of the byte-equality row and
// keeps passing, which is precisely the vacuous arm this file's header forbids
// and which an outcome assertion can never catch on its own.
func constraintStateRequireDistinctOperands(t *testing.T, n int, row func(int) (name, state, product string, sameBytesIsThePoint bool)) {
	t.Helper()
	if n == 0 {
		t.Fatal("comparator table is empty")
	}
	sameBytesRows := 0
	for i := 0; i < n; i++ {
		name, state, product, sameBytes := row(i)
		if sameBytes {
			sameBytesRows++
			if state != product {
				t.Errorf("%s: marked as the byte-equality row but its operands differ", name)
			}
			continue
		}
		if state == product {
			t.Errorf("%s: operands are byte-identical (%s), so the row exercises no comparison; "+
				"feed it the two spellings it claims to normalize", name, state)
		}
	}
	if sameBytesRows != 1 {
		t.Errorf("the table marks %d rows as the byte-equality subject, want exactly 1", sameBytesRows)
	}
}

// TestConstraintStateJSONEquivalentIsOrderSensitive proves the check that guards
// every node actually bites. It is the reason a traversal that drifted from
// production cannot report a state: reordering one class field, dropping one
// element, or renaming one key all fail here.
func TestConstraintStateJSONEquivalentIsOrderSensitive(t *testing.T) {
	cases := []struct {
		name           string
		state, product string
		wantEquivalent bool
		// sameBytesIsThePoint marks the ONE row whose subject IS byte equality.
		// Every other row must have operands that DIFFER, or it exercises no
		// comparison at all — see the non-vacuity gate below.
		sameBytesIsThePoint bool
	}{
		{name: "identical-object", state: `{"b":1,"a":2}`, product: `{"b":1,"a":2}`, wantEquivalent: true, sameBytesIsThePoint: true},
		{name: "reordered-object", state: `{"a":2,"b":1}`, product: `{"b":1,"a":2}`},
		{name: "renamed-key", state: `{"amount":3}`, product: `{"qty":3}`},
		{name: "dropped-entry", state: `{"b":1}`, product: `{"b":1,"a":2}`},
		{name: "reordered-array", state: `[2,1]`, product: `[1,2]`},
		{name: "dropped-element", state: `[1]`, product: `[1,2]`},
		{name: "different-scalar", state: `{"b":1}`, product: `{"b":2}`},
		{name: "different-kind", state: `"1"`, product: `1`},
		{name: "nested-reorder", state: `{"o":{"a":1,"b":2}}`, product: `{"o":{"b":2,"a":1}}`},
		// The two normalizations, and ONLY these two: HTML escaping (production
		// disables it, encoding/json does not) and float exponent spelling.
		// The state side is what encoding/json emits (it escapes HTML by default);
		// the production side is what marshalJSON emits (SetEscapeHTML(false)). The
		// two spellings must differ, or the row proves nothing about the
		// normalization it names.
		{name: "html-escaping", state: `"\u003ca\u003e"`, product: `"<a>"`, wantEquivalent: true},
		{name: "float-exponent-spelling", state: `1e-7`, product: `1e-07`, wantEquivalent: true},
	}
	requireConstraintStateCount(t, len(cases), 11, "comparator cases")
	constraintStateRequireDistinctOperands(t, len(cases), func(i int) (string, string, string, bool) {
		return cases[i].name, cases[i].state, cases[i].product, cases[i].sameBytesIsThePoint
	})
	for _, c := range cases {
		diff, ok := constraintStateJSONEquivalent([]byte(c.state), []byte(c.product))
		if ok != c.wantEquivalent {
			t.Errorf("%s: equivalent=%v (want %v), diff=%q", c.name, ok, c.wantEquivalent, diff)
			continue
		}
		if !ok && diff == "" {
			t.Errorf("%s: reported a difference with no description", c.name)
		}
	}
}

// TestConstraintStateJSONEquivalentComparesNumbersExactly pins the one property
// a float64 comparison silently breaks.
//
// BAML's Int is an i64. Two ADJACENT exact integers above 2^53 —
// 9007199254740992 and 9007199254740993 — are the SAME float64, so a comparator
// that fell back to float64 would report them equivalent and the divergence
// check that guards every node would go green on precisely the large-number
// surface the guard ledger treats as parity-sensitive. The comparison is
// therefore exact (big.Rat), and normalizes ONLY the intended spelling
// differences.
func TestConstraintStateJSONEquivalentComparesNumbersExactly(t *testing.T) {
	cases := []struct {
		name                string
		state, product      string
		wantEquivalent      bool
		sameBytesIsThePoint bool
	}{
		// The float64-collapse pairs. These are the regression subject.
		{name: "adjacent-above-2^53", state: `9007199254740993`, product: `9007199254740992`},
		{name: "adjacent-above-2^53-nested", state: `{"n":9007199254740993}`, product: `{"n":9007199254740992}`},
		{name: "adjacent-above-2^53-negative", state: `-9007199254740993`, product: `-9007199254740992`},
		{name: "adjacent-at-i64-max", state: `9223372036854775807`, product: `9223372036854775806`},
		{name: "same-large-integer", state: `{"n":9007199254740993}`, product: `{"n":9007199254740993}`, wantEquivalent: true, sameBytesIsThePoint: true},
		// The spelling normalizations that must survive.
		{name: "float-exponent-spelling", state: `{"n":1e-7}`, product: `{"n":1e-07}`, wantEquivalent: true},
		{name: "integral-float-vs-int-token", state: `{"n":1.0}`, product: `{"n":1}`, wantEquivalent: true},
		{name: "ordinary-difference", state: `{"n":1}`, product: `{"n":2}`},
	}
	requireConstraintStateCount(t, len(cases), 8, "exact-number cases")
	constraintStateRequireDistinctOperands(t, len(cases), func(i int) (string, string, string, bool) {
		return cases[i].name, cases[i].state, cases[i].product, cases[i].sameBytesIsThePoint
	})
	for _, c := range cases {
		diff, ok := constraintStateJSONEquivalent([]byte(c.state), []byte(c.product))
		if ok != c.wantEquivalent {
			t.Errorf("%s: equivalent=%v (want %v) for %s vs %s, diff=%q", c.name, ok, c.wantEquivalent, c.state, c.product, diff)
		}
	}
	// Non-vacuity for the pairs above: they really are two DISTINCT integers that
	// collapse to the SAME float64, so a float64 comparison would have fused them.
	//
	// The values go through int64 VARIABLES on purpose. Written as untyped
	// constants the compiler folds both conversions to one float64 constant and
	// decides the comparison at compile time, which makes the guard dead code —
	// the same never-fires class this file's header forbids.
	lo, hi := int64(9007199254740992), int64(9007199254740993)
	if lo == hi {
		t.Fatal("fixture is stale: the two tokens are the same integer, so the regression case proves nothing")
	}
	if float64(lo) != float64(hi) {
		t.Errorf("fixture is stale: %d and %d are no longer the same float64, so an exact "+
			"comparator is no longer what distinguishes them", lo, hi)
	}
}

// ---------------------------------------------------------------------------
// Identity and order come from the traversal, not from the JSON
// ---------------------------------------------------------------------------

// TestConstraintStateLargeIntegerLeafIsExact is the end-to-end half of the test
// above: a class field holding an i64 beyond float64's exact range keeps its
// EXACT value through the traversal, the leaf readback and the divergence check.
//
// The predicate over it is declined by the fail-closed numeric profile, which is
// the documented behaviour and is asserted as such — an `unsupported` outcome,
// not a fabricated boolean.
func TestConstraintStateLargeIntegerLeafIsExact(t *testing.T) {
	cls := constraintStateClass("Big",
		constraintStateField("n", "", constraintStateCheck(constraintStateIntType(), "big", "this > 0")))
	b := constraintStateBundle(t, constraintStateClassType("Big"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"n":9007199254740993}`)

	n := requireConstraintStateNode(t, run, "$.n")
	if got, want := constraintStateDescribe(n.Canonical), "int:9007199254740993"; got != want {
		t.Fatalf("$.n canonical = %s, want %s", got, want)
	}
	if got, want := string(n.CanonicalJSON), "9007199254740993"; got != want {
		t.Errorf("$.n CanonicalJSON = %s, want %s", got, want)
	}
	// The value survived as an exact i64, not as the nearest float64.
	if got, want := n.Canonical.i, int64(9007199254740993); got != want {
		t.Errorf("$.n i64 = %d, want %d", got, want)
	}
	requireConstraintStateEvents(t, n, []string{`type_meta/check/"big"/this > 0=unsupported`})
	if got, want := n.Disposition, constraintDispositionUnsupportedExpression; got != want {
		t.Errorf("$.n disposition = %s, want %s", got, want)
	}
}

// TestConstraintStateIdentityIsNotRecoverableFromCanonicalJSON is the direct
// statement of the model's reason to exist.
//
// The canonical document is `{"suit":"Hearts","bid":2}`. From those bytes alone
// nothing can tell you that the object is the class `Hand`, that `"Hearts"` is
// the enum `Suit` rather than a string, or that the schema declares `suit`
// before `bid` while the model wrote them the other way round. The state carries
// all three, so it cannot have come from decoding the JSON.
func TestConstraintStateIdentityIsNotRecoverableFromCanonicalJSON(t *testing.T) {
	suit := schema.EnumDef{
		Name:   schema.Name{Name: "Suit"},
		Values: []schema.EnumValue{{Name: schema.Name{Name: "Hearts"}}, {Name: schema.Name{Name: "Spades"}}},
	}
	hand := constraintStateClass("Hand",
		constraintStateField("suit", "", schema.Type{Kind: schema.TypeEnum, Name: "Suit"}),
		constraintStateField("bid", "", constraintStateCheck(constraintStateIntType(), "positive", "this > 0")),
	)
	b := constraintStateBundle(t, constraintStateClassType("Hand"), []schema.ClassDef{hand}, []schema.EnumDef{suit})

	// INPUT ORDER IS THE REVERSE OF SCHEMA ORDER, so schema order cannot be an
	// accident of the input.
	run := collectConstraintStateFixture(t, b, `{"bid":2,"suit":"Hearts"}`)

	if got, want := constraintStateDescribe(run.Root.Canonical), `class:Hand{suit=enum:Suit=Hearts,bid=int:2}`; got != want {
		t.Fatalf("root canonical = %s, want %s", got, want)
	}
	if got, want := string(run.Root.CanonicalJSON), `{"suit":"Hearts","bid":2}`; got != want {
		t.Errorf("root CanonicalJSON = %s, want %s", got, want)
	}
	// The JSON above is a plain object of a string and a number; the state is a
	// NAMED class holding a NAMED enum. Pin both names explicitly.
	if got := run.Root.Canonical.TypeName(); got != "Hand" {
		t.Errorf("root TypeName = %q, want %q", got, "Hand")
	}
	suitNode := requireConstraintStateNode(t, run, "$.suit")
	if got := suitNode.Canonical.Kind(); got != ConstraintKindEnum {
		t.Errorf("$.suit kind = %s, want enum", got)
	}
	if got := suitNode.Canonical.TypeName(); got != "Suit" {
		t.Errorf("$.suit TypeName = %q, want %q", got, "Suit")
	}
	// Children are in SCHEMA order too, not input order.
	var paths []string
	for _, c := range run.Root.Children {
		paths = append(paths, c.Path.String())
	}
	requireConstraintStateCount(t, len(paths), 2, "root children")
	if paths[0] != "$.suit" || paths[1] != "$.bid" {
		t.Errorf("children = %v, want [$.suit $.bid]", paths)
	}
	// Only the constrained node carries an event; the unconstrained one is
	// UNCONSTRAINED, not "evaluated with nothing".
	if got := suitNode.Disposition; got != constraintDispositionUnconstrained {
		t.Errorf("$.suit disposition = %s, want %s", got, constraintDispositionUnconstrained)
	}
	requireConstraintStateEvents(t, suitNode, nil)
	bid := requireConstraintStateNode(t, run, "$.bid")
	if got := bid.Disposition; got != constraintDispositionEvaluated {
		t.Errorf("$.bid disposition = %s, want %s", got, constraintDispositionEvaluated)
	}
	requireConstraintStateEvents(t, bid, []string{`type_meta/check/"positive"/this > 0=true`})
}

// TestConstraintStateScalarDomainComesFromTheSchema pins that the BAML value
// domain of a scalar leaf is chosen by the DECLARED type, not guessed from the
// coerced token.
//
// `2` and `2` are the same JSON token; under a `float` field it is
// BamlValue::Float and under an `int` field it is BamlValue::Int, and BAML's
// numeric predicates distinguish the two. A collector that read the domain off
// the token could not tell them apart.
func TestConstraintStateScalarDomainComesFromTheSchema(t *testing.T) {
	floatT := constraintStateCheck(schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveFloat}, "big", "this > 1.0")
	boolT := constraintStateCheck(schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool}, "set", "this == true")
	cls := constraintStateClass("Reading",
		constraintStateField("ratio", "", floatT),
		constraintStateField("whole", "", floatT),
		constraintStateField("count", "", constraintStateCheck(constraintStateIntType(), "positive", "this > 0")),
		constraintStateField("ok", "", boolT),
	)
	b := constraintStateBundle(t, constraintStateClassType("Reading"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"ratio":1.5,"whole":2,"count":2,"ok":true}`)

	if got, want := constraintStateDescribe(run.Root.Canonical),
		`class:Reading{ratio=float:1.5,whole=float:2,count=int:2,ok=bool:true}`; got != want {
		t.Fatalf("root canonical = %s, want %s", got, want)
	}
	// `whole` and `count` carry the SAME token and land in DIFFERENT BAML
	// domains — the discriminator for "the schema chose, not the token".
	whole := requireConstraintStateNode(t, run, "$.whole")
	count := requireConstraintStateNode(t, run, "$.count")
	if got, want := whole.Canonical.Kind(), ConstraintKindFloat; got != want {
		t.Errorf("$.whole kind = %s, want %s", got, want)
	}
	if got, want := count.Canonical.Kind(), ConstraintKindInt; got != want {
		t.Errorf("$.count kind = %s, want %s", got, want)
	}
	if string(whole.CanonicalJSON) != string(count.CanonicalJSON) {
		t.Errorf("fixture does not discriminate: whole=%s count=%s must be the same token",
			whole.CanonicalJSON, count.CanonicalJSON)
	}
	requireConstraintStateEvents(t, requireConstraintStateNode(t, run, "$.ratio"),
		[]string{`type_meta/check/"big"/this > 1.0=true`})
	requireConstraintStateEvents(t, requireConstraintStateNode(t, run, "$.ok"),
		[]string{`type_meta/check/"set"/this == true=true`})
}

// ---------------------------------------------------------------------------
// ASYMMETRY 1 — bare string return skips constraints
// ---------------------------------------------------------------------------

// TestConstraintStateBareStringReturnSkipsBothLevels pins the first recorded
// asymmetry with POSITIVE evidence on both halves.
//
// The return type is a bare `string` carrying a FALSE @check and a FALSE
// @assert. Stock skips constraints on that route: the check collection is empty
// and the false assertion does not reject. The collector records
// SkipBareStringReturn BEFORE normal node evaluation, and records for each
// predicate the COUNTERFACTUAL outcome — so the test proves the evaluator was
// reached and returned false, and that the value was still not rejected. An
// empty Events list on its own would prove nothing; see
// [TestConstraintStateNestedStringDoesNotSkip] for the control.
func TestConstraintStateBareStringReturnSkipsBothLevels(t *testing.T) {
	target := constraintStateAssert(
		constraintStateCheck(constraintStateStringType(), "nonempty", `this == "expected"`),
		"shape", `this == "also_expected"`,
	)
	b := constraintStateBundle(t, target, nil, nil)
	// Production refuses this bundle like any other constrained one — the gate walks
	// b.Target. The collector runs anyway: it is not gated, and the skip it records
	// here is a STOCK BAML behaviour (the bare-string return route), which the
	// evaluator slice models whether or not native is allowed to serve the shape.
	run := collectConstraintStateFixture(t, b, `"actual"`)

	root := run.Root
	if got, want := root.Disposition, constraintDispositionSkipBareStringReturn; got != want {
		t.Fatalf("root disposition = %s, want %s", got, want)
	}
	if got := constraintStateDescribe(root.Canonical); got != `string:"actual"` {
		t.Errorf("root canonical = %s, want string:\"actual\"", got)
	}
	// The check collection is EMPTY — the observable half of the asymmetry.
	requireConstraintStateEvents(t, root, nil)
	// The false ASSERT did not reject.
	if root.AssertFailed {
		t.Error("AssertFailed is true on a bare string return; stock does not reject there")
	}
	// POSITIVE EVIDENCE: both predicates were reached and both decided FALSE.
	requireConstraintStateSkipped(t, root, []string{
		`type_meta/check/"nonempty"/this == "expected"~would-be-false`,
		`type_meta/assert/"shape"/this == "also_expected"~would-be-false`,
	})
	if root.SkipReason == "" {
		t.Error("SkipBareStringReturn recorded no reason")
	}
}

// TestConstraintStateNestedStringDoesNotSkip is the CONTROL for the test above,
// and the reason its empty-event assertion is not vacuous.
//
// It is the SAME constrained string type with the SAME input and the SAME two
// false predicates, moved one level down into a class field. There the
// constraints DO run: two events, both false, and the false @assert DOES set
// AssertFailed. So the skip is a property of the bare-string RETURN ROUTE, not
// of constrained strings, and "no events" at the root is a decision rather than
// a collector that never evaluates anything.
func TestConstraintStateNestedStringDoesNotSkip(t *testing.T) {
	field := constraintStateAssert(
		constraintStateCheck(constraintStateStringType(), "nonempty", `this == "expected"`),
		"shape", `this == "also_expected"`,
	)
	cls := constraintStateClass("Wrapper", constraintStateField("note", "", field))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"note":"actual"}`)

	note := requireConstraintStateNode(t, run, "$.note")
	if got, want := note.Disposition, constraintDispositionEvaluated; got != want {
		t.Fatalf("$.note disposition = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, note, []string{
		`type_meta/check/"nonempty"/this == "expected"=false`,
		`type_meta/assert/"shape"/this == "also_expected"=false`,
	})
	if !note.AssertFailed {
		t.Error("$.note AssertFailed is false; a false @assert on a non-bare-string node must record the failure")
	}
	requireConstraintStateSkipped(t, note, nil)
	// And the root is NOT the bare-string route.
	if got := run.Root.Disposition; got == constraintDispositionSkipBareStringReturn {
		t.Error("the class root took the bare-string-return route")
	}
}

func TestConstraintStateBareStringRouteConditionIsNarrow(t *testing.T) {
	cases := []struct {
		name string
		typ  schema.Type
		want bool
	}{
		{"bare-string", constraintStateStringType(), true},
		{"optional-string", constraintStateOptional(constraintStateStringType()), false},
		{"int", constraintStateIntType(), false},
		{"class", constraintStateClassType("X"), false},
		{"list-of-string", schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateStringType())}, false},
	}
	requireConstraintStateCount(t, len(cases), 5, "route cases")
	for _, c := range cases {
		if got := constraintStateIsBareStringReturn(c.typ); got != c.want {
			t.Errorf("%s: isBareStringReturn = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ASYMMETRY 2 — duplicate labels are two ordered events
// ---------------------------------------------------------------------------

// TestConstraintStateDuplicateLabelsStayTwoOrderedEvents pins the second
// recorded asymmetry. Two @check predicates share the label "range" and disagree
// on the value. Folding them by label would keep exactly one and silently decide
// the wire question Slice 7.2b owns, so both are recorded, in declaration order,
// with their own results.
func TestConstraintStateDuplicateLabelsStayTwoOrderedEvents(t *testing.T) {
	field := constraintStateCheck(
		constraintStateCheck(constraintStateIntType(), "range", "this > 0"),
		"range", "this > 100",
	)
	cls := constraintStateClass("Score", constraintStateField("value", "", field))
	b := constraintStateBundle(t, constraintStateClassType("Score"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"value":5}`)

	v := requireConstraintStateNode(t, run, "$.value")
	requireConstraintStateEvents(t, v, []string{
		`type_meta/check/"range"/this > 0=true`,
		`type_meta/check/"range"/this > 100=false`,
	})
	// The two events share a label AND disagree — the exact pair a map fold
	// would collapse to one.
	if v.Events[0].Label != v.Events[1].Label {
		t.Fatalf("fixture is not a duplicate-label case: %q vs %q", v.Events[0].Label, v.Events[1].Label)
	}
	if v.Events[0].Outcome == v.Events[1].Outcome {
		t.Fatalf("fixture does not discriminate: both events are %s", v.Events[0].Outcome)
	}
	if v.AssertFailed {
		t.Error("AssertFailed set by a failing @check; only @assert may set it")
	}
	if got, want := v.Disposition, constraintDispositionEvaluated; got != want {
		t.Errorf("disposition = %s, want %s", got, want)
	}
}

// TestConstraintStateEventsAreOrderedNotFolded pins the SHAPE the test above
// depends on. If a refactor changed Events to a map keyed by label the two
// events would silently become one and the assertions above would be rewritten
// to match; this fails first, structurally.
func TestConstraintStateEventsAreOrderedNotFolded(t *testing.T) {
	st := reflect.TypeOf(constraintCoercionState{})
	names := []string{"Events", "Skipped", "Children"}
	requireConstraintStateCount(t, len(names), 3, "ordered fields")
	for _, name := range names {
		f, ok := st.FieldByName(name)
		if !ok {
			t.Fatalf("constraintCoercionState has no %s field", name)
		}
		if f.Type.Kind() != reflect.Slice {
			t.Errorf("%s is a %s; it must stay an ordered slice (duplicate labels and traversal order are load-bearing)", name, f.Type.Kind())
		}
	}
}

// ---------------------------------------------------------------------------
// ASYMMETRY 3 — aliases canonicalize, and the alias is retained as metadata
// ---------------------------------------------------------------------------

// TestConstraintStateEnumAliasEvaluatesCanonicalVariant pins the third recorded
// asymmetry for enums. The model wrote the ALIAS `hearts_alias`; the predicate
// sees the CANONICAL variant `Hearts`, and the alias survives only as witness
// metadata.
//
// The two predicates are the discriminator: `this == "Hearts"` is TRUE and
// `this == "hearts_alias"` is FALSE. If the value model leaked the alias into
// the predicate both would flip.
func TestConstraintStateEnumAliasEvaluatesCanonicalVariant(t *testing.T) {
	suit := schema.EnumDef{
		Name: schema.Name{Name: "Suit"},
		Values: []schema.EnumValue{
			{Name: schema.Name{Name: "Hearts", Alias: strPtrConstraintState("hearts_alias")}},
			{Name: schema.Name{Name: "Spades"}},
		},
	}
	enumT := constraintStateCheck(
		constraintStateCheck(schema.Type{Kind: schema.TypeEnum, Name: "Suit"}, "canonical", `this == "Hearts"`),
		"alias", `this == "hearts_alias"`,
	)
	cls := constraintStateClass("Hand", constraintStateField("suit", "", enumT))
	b := constraintStateBundle(t, constraintStateClassType("Hand"), []schema.ClassDef{cls}, []schema.EnumDef{suit})
	run := collectConstraintStateFixture(t, b, `{"suit":"hearts_alias"}`)

	suitNode := requireConstraintStateNode(t, run, "$.suit")
	if got, want := constraintStateDescribe(suitNode.Canonical), "enum:Suit=Hearts"; got != want {
		t.Fatalf("$.suit canonical = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, suitNode, []string{
		`type_meta/check/"canonical"/this == "Hearts"=true`,
		`type_meta/check/"alias"/this == "hearts_alias"=false`,
	})
	// The alias is RETAINED, exactly and only as metadata.
	if suitNode.Original.EnumAlias == nil {
		t.Fatal("$.suit recorded no alias origin for an alias-routed enum")
	}
	if got, want := *suitNode.Original.EnumAlias, (constraintStateAliasOrigin{
		Canonical: "Hearts", Rendered: "hearts_alias", Observed: "hearts_alias",
	}); got != want {
		t.Errorf("$.suit alias origin = %+v, want %+v", got, want)
	}
	// The canonical document carries the canonical variant, never the alias.
	if got, want := string(run.Root.CanonicalJSON), `{"suit":"Hearts"}`; got != want {
		t.Errorf("CanonicalJSON = %s, want %s", got, want)
	}
	// CONTROL: the same enum reached by its CANONICAL spelling records NO alias
	// origin, so the metadata above is a real routing observation rather than a
	// field that is always populated.
	suit2 := schema.EnumDef{
		Name:   schema.Name{Name: "Suit"},
		Values: []schema.EnumValue{{Name: schema.Name{Name: "Hearts"}}, {Name: schema.Name{Name: "Spades"}}},
	}
	cls2 := constraintStateClass("Hand", constraintStateField("suit", "",
		constraintStateCheck(schema.Type{Kind: schema.TypeEnum, Name: "Suit"}, "canonical", `this == "Hearts"`)))
	b2 := constraintStateBundle(t, constraintStateClassType("Hand"), []schema.ClassDef{cls2}, []schema.EnumDef{suit2})
	run2 := collectConstraintStateFixture(t, b2, `{"suit":"Hearts"}`)
	if got := requireConstraintStateNode(t, run2, "$.suit").Original.EnumAlias; got != nil {
		t.Errorf("canonically-spelled enum recorded an alias origin: %+v", *got)
	}
}

// TestConstraintStateFieldAliasEvaluatesCanonicalField is the class half of the
// third asymmetry: the input key is the alias `qty`, the canonical class entry
// is `amount`, and a predicate reaches the value through the CANONICAL field
// name only.
//
// The alias spelling `this.qty` does not merely evaluate FALSE — it is
// UNDECIDABLE, because the mapping has no such key and the operator gate refuses
// the resulting mixed-kind comparison. Both facts are pinned.
func TestConstraintStateFieldAliasEvaluatesCanonicalField(t *testing.T) {
	order := constraintStateClass("Order", constraintStateField("amount", "qty", constraintStateIntType()))
	order.Constraints = []schema.Constraint{
		{Level: schema.ConstraintCheck, Expression: "this.amount == 3", Label: strPtrConstraintState("canonical")},
	}
	wrapper := constraintStateClass("Wrapper", constraintStateField("order", "",
		constraintStateCheck(constraintStateClassType("Order"), "alias", "this.qty == 3")))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"),
		[]schema.ClassDef{wrapper, order}, nil)
	run := collectConstraintStateFixture(t, b, `{"order":{"qty":3}}`)

	orderNode := requireConstraintStateNode(t, run, "$.order")
	if got, want := constraintStateDescribe(orderNode.Canonical), "class:Order{amount=int:3}"; got != want {
		t.Fatalf("$.order canonical = %s, want %s", got, want)
	}
	if got, want := string(orderNode.CanonicalJSON), `{"amount":3}`; got != want {
		t.Errorf("$.order CanonicalJSON = %s, want %s", got, want)
	}
	// The DECLARATION-site predicate reaching the canonical field is TRUE; the
	// type-node predicate reaching the alias spelling is undecidable. Both are
	// recorded with their origin, in declaration-then-type-node order.
	requireConstraintStateEvents(t, orderNode, []string{
		`declaration/check/"canonical"/this.amount == 3=true`,
		`type_meta/check/"alias"/this.qty == 3=unsupported`,
	})
	if got, want := orderNode.Disposition, constraintDispositionUnsupportedExpression; got != want {
		t.Errorf("$.order disposition = %s, want %s", got, want)
	}
	// The alias is retained as metadata, in schema order.
	requireConstraintStateCount(t, len(orderNode.Original.FieldAliases), 1, "$.order field aliases")
	if got, want := orderNode.Original.FieldAliases[0], (constraintStateAliasOrigin{
		Canonical: "amount", Rendered: "qty", Observed: "qty",
	}); got != want {
		t.Errorf("field alias = %+v, want %+v", got, want)
	}
	// CONTROL: an unaliased field records no alias metadata, so the entry above
	// is a real routing observation.
	plain := constraintStateClass("Plain", constraintStateField("amount", "", constraintStateIntType()))
	plain.Constraints = []schema.Constraint{
		{Level: schema.ConstraintCheck, Expression: "this.amount == 3", Label: strPtrConstraintState("canonical")},
	}
	wrapper2 := constraintStateClass("Wrapper", constraintStateField("order", "", constraintStateClassType("Plain")))
	b2 := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper2, plain}, nil)
	run2 := collectConstraintStateFixture(t, b2, `{"order":{"amount":3}}`)
	if got := requireConstraintStateNode(t, run2, "$.order").Original.FieldAliases; len(got) != 0 {
		t.Errorf("unaliased class recorded field aliases: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Skipped child paths
// ---------------------------------------------------------------------------

// TestConstraintStateDroppedListElementIsASkippedPath proves a dropped element
// is recorded as a skipped path with POSITIVE evidence — the marker node exists
// at the element's INPUT index and carries a reason — rather than simply being
// missing from the state.
func TestConstraintStateDroppedListElementIsASkippedPath(t *testing.T) {
	elem := constraintStateCheck(constraintStateIntType(), "positive", "this > 0")
	listT := schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(elem)}
	cls := constraintStateClass("Bag", constraintStateField("nums", "", listT))
	b := constraintStateBundle(t, constraintStateClassType("Bag"), []schema.ClassDef{cls}, nil)
	// "oops" is a PROVEN BAML parse error for an int element, so coerce_list
	// records ArrayItemParseError and drops it while the list still succeeds.
	run := collectConstraintStateFixture(t, b, `{"nums":[1,"oops",3]}`)

	nums := requireConstraintStateNode(t, run, "$.nums")
	if got, want := constraintStateDescribe(nums.Canonical), "list[int:1,int:3]"; got != want {
		t.Fatalf("$.nums canonical = %s, want %s", got, want)
	}
	requireConstraintStateCount(t, len(nums.Children), 3, "$.nums children")
	wantPaths := []string{"$.nums[0]", "$.nums[1]", "$.nums[2]"}
	wantDisp := []constraintStateDisposition{
		constraintDispositionEvaluated,
		constraintDispositionSkippedPath,
		constraintDispositionEvaluated,
	}
	for i, c := range nums.Children {
		if got := c.Path.String(); got != wantPaths[i] {
			t.Errorf("child %d path = %s, want %s", i, got, wantPaths[i])
		}
		if got := c.Disposition; got != wantDisp[i] {
			t.Errorf("child %d disposition = %s, want %s", i, got, wantDisp[i])
		}
	}
	dropped := requireConstraintStateNode(t, run, "$.nums[1]")
	// POSITIVE EVIDENCE the skip was reached: no value was synthesized, the
	// production rule that dropped it is named, and the element's own predicate is
	// listed as not-run.
	if dropped.HasCanonical {
		t.Error("$.nums[1] synthesized a canonical value for a dropped element")
	}
	if !strings.Contains(dropped.SkipReason, "ArrayItemParseError") {
		t.Errorf("$.nums[1] reason = %q, want the ArrayItemParseError skip named", dropped.SkipReason)
	}
	requireConstraintStateEvents(t, dropped, nil)
	requireConstraintStateSkipped(t, dropped, []string{
		`type_meta/check/"positive"/this > 0~would-be-not-evaluated`,
	})
	// The kept elements DID evaluate — so "no events" on the dropped one is a
	// decision, not a collector that evaluates nothing.
	requireConstraintStateEvents(t, requireConstraintStateNode(t, run, "$.nums[0]"),
		[]string{`type_meta/check/"positive"/this > 0=true`})
	requireConstraintStateEvents(t, requireConstraintStateNode(t, run, "$.nums[2]"),
		[]string{`type_meta/check/"positive"/this > 0=true`})
}

// TestConstraintStateAbsentOptionalFieldIsASkippedPath proves an absent optional
// field is a skipped path: production omits the key, the canonical class value
// omits it too, and the state records the omission — with its declared predicate
// listed as not-run — at the field's path.
func TestConstraintStateAbsentOptionalFieldIsASkippedPath(t *testing.T) {
	nickname := constraintStateCheck(constraintStateOptional(constraintStateStringType()), "short", `this == "nick"`)
	cls := constraintStateClass("Profile",
		constraintStateField("name", "", constraintStateStringType()),
		constraintStateField("nickname", "", nickname),
	)
	b := constraintStateBundle(t, constraintStateClassType("Profile"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"name":"ada"}`)

	if got, want := constraintStateDescribe(run.Root.Canonical), `class:Profile{name=string:"ada"}`; got != want {
		t.Fatalf("root canonical = %s, want %s", got, want)
	}
	requireConstraintStateCount(t, len(run.Root.Children), 2, "root children")
	nick := requireConstraintStateNode(t, run, "$.nickname")
	if got, want := nick.Disposition, constraintDispositionSkippedPath; got != want {
		t.Errorf("$.nickname disposition = %s, want %s", got, want)
	}
	if nick.HasCanonical {
		t.Error("$.nickname synthesized a value for an absent optional field")
	}
	if !strings.Contains(nick.SkipReason, "absent optional") {
		t.Errorf("$.nickname reason = %q", nick.SkipReason)
	}
	requireConstraintStateEvents(t, nick, nil)
	requireConstraintStateSkipped(t, nick, []string{
		`type_meta/check/"short"/this == "nick"~would-be-not-evaluated`,
	})
	// CONTROL: with the field PRESENT the very same predicate evaluates, so the
	// not-run list above is the absence decision rather than a node type whose
	// constraints never run.
	run2 := collectConstraintStateFixture(t, b, `{"name":"ada","nickname":"nick"}`)
	present := requireConstraintStateNode(t, run2, "$.nickname")
	if got, want := present.Disposition, constraintDispositionEvaluated; got != want {
		t.Fatalf("present $.nickname disposition = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, present, []string{`type_meta/check/"short"/this == "nick"=true`})
}

// ---------------------------------------------------------------------------
// Maps
// ---------------------------------------------------------------------------

// TestConstraintStateMapKeepsInputOrderAndSkipsProvenBadValues pins map order
// (INPUT order, not sorted) and the partial-map skip.
func TestConstraintStateMapKeepsInputOrderAndSkipsProvenBadValues(t *testing.T) {
	valT := constraintStateCheck(constraintStateIntType(), "positive", "this > 0")
	mapT := schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateStringType()),
		Value: ptrConstraintStateType(valT),
	}
	cls := constraintStateClass("Board", constraintStateField("scores", "", mapT))
	b := constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{cls}, nil)
	// Keys deliberately NOT in sorted order, and "b" carries a proven-bad value.
	run := collectConstraintStateFixture(t, b, `{"scores":{"z":1,"b":"oops","a":2}}`)

	scores := requireConstraintStateNode(t, run, "$.scores")
	if got, want := constraintStateDescribe(scores.Canonical), "map{z=int:1,a=int:2}"; got != want {
		t.Fatalf("$.scores canonical = %s, want %s", got, want)
	}
	if got, want := string(scores.CanonicalJSON), `{"z":1,"a":2}`; got != want {
		t.Errorf("$.scores CanonicalJSON = %s, want %s", got, want)
	}
	requireConstraintStateCount(t, len(scores.Children), 3, "$.scores children")
	wantPaths := []string{`$.scores["z"]`, `$.scores["b"]`, `$.scores["a"]`}
	for i, c := range scores.Children {
		if got := c.Path.String(); got != wantPaths[i] {
			t.Errorf("child %d path = %s, want %s", i, got, wantPaths[i])
		}
	}
	dropped := requireConstraintStateNode(t, run, `$.scores["b"]`)
	if got, want := dropped.Disposition, constraintDispositionSkippedPath; got != want {
		t.Errorf("dropped entry disposition = %s, want %s", got, want)
	}
	if !strings.Contains(dropped.SkipReason, "MapValueParseError") {
		t.Errorf("dropped entry reason = %q", dropped.SkipReason)
	}
	if dropped.HasCanonical {
		t.Error("a dropped map entry synthesized a canonical value")
	}
	requireConstraintStateEvents(t, requireConstraintStateNode(t, run, `$.scores["z"]`),
		[]string{`type_meta/check/"positive"/this > 0=true`})
	requireConstraintStateEvents(t, requireConstraintStateNode(t, run, `$.scores["a"]`),
		[]string{`type_meta/check/"positive"/this > 0=true`})
}

// TestConstraintStateMapKeyConstraintsArePolicyDeclined pins that a constraint
// declared on a map KEY type is recorded and NOT evaluated. Map keys stay a
// negative-admission fixture in Slice 7.2a, so the witness must show the
// predicate present and refused rather than absent.
func TestConstraintStateMapKeyConstraintsArePolicyDeclined(t *testing.T) {
	keyT := constraintStateCheck(constraintStateStringType(), "keyshape", `this == "z"`)
	mapT := schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(keyT),
		Value: ptrConstraintStateType(constraintStateIntType()),
	}
	cls := constraintStateClass("Board", constraintStateField("scores", "", mapT))
	b := constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"scores":{"z":1,"a":2}}`)

	key := requireConstraintStateNode(t, run, "$.scores.<key>")
	if got, want := key.Disposition, constraintDispositionPolicyDeclined; got != want {
		t.Fatalf("key node disposition = %s, want %s", got, want)
	}
	if key.HasCanonical {
		t.Error("the map key node synthesized a canonical value")
	}
	requireConstraintStateEvents(t, key, nil)
	// POSITIVE EVIDENCE: the declined predicate is listed, with its exact source.
	requireConstraintStateSkipped(t, key, []string{
		`type_meta/check/"keyshape"/this == "z"~would-be-not-evaluated`,
	})
	if key.SkipReason == "" {
		t.Error("the policy-declined key node recorded no reason")
	}
	// The key node is FIRST among the map's children, before the entries.
	scores := requireConstraintStateNode(t, run, "$.scores")
	requireConstraintStateCount(t, len(scores.Children), 3, "$.scores children (key node + 2 entries)")
	if got := scores.Children[0].Path.String(); got != "$.scores.<key>" {
		t.Errorf("first child = %s, want the key node", got)
	}
	// CONTROL: an UNCONSTRAINED map key produces NO key node at all, so the node
	// above exists because a predicate was declared there.
	plainMap := schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateStringType()),
		Value: ptrConstraintStateType(constraintStateCheck(constraintStateIntType(), "positive", "this > 0")),
	}
	cls2 := constraintStateClass("Board", constraintStateField("scores", "", plainMap))
	b2 := constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{cls2}, nil)
	run2 := collectConstraintStateFixture(t, b2, `{"scores":{"z":1,"a":2}}`)
	if n := run2.Root.find("$.scores.<key>"); n != nil {
		t.Errorf("an unconstrained map key produced a key node: %+v", n.Path)
	}
}

// ---------------------------------------------------------------------------
// Unions
// ---------------------------------------------------------------------------

// TestConstraintStateUnionCollectsOnlyTheWinningArm pins that exactly one arm
// state exists, that it is the arm production selected, and that no losing
// candidate leaves a trace.
//
// The winning arm is an ENUM, so the arm state also demonstrates why a union
// needs the traversal: the union's canonical document is the bare string
// `"Hearts"`, from which nothing could recover "arm 1, the enum Suit".
func TestConstraintStateUnionCollectsOnlyTheWinningArm(t *testing.T) {
	suit := schema.EnumDef{
		Name:   schema.Name{Name: "Suit"},
		Values: []schema.EnumValue{{Name: schema.Name{Name: "Hearts"}}, {Name: schema.Name{Name: "Spades"}}},
	}
	unionT := constraintStateCheck(schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{
		Variants: []schema.Type{
			constraintStateIntType(),
			{Kind: schema.TypeEnum, Name: "Suit"},
		},
	}}, "winner", `this == "Hearts"`)
	cls := constraintStateClass("Cell", constraintStateField("v", "", unionT))
	b := constraintStateBundle(t, constraintStateClassType("Cell"), []schema.ClassDef{cls}, []schema.EnumDef{suit})
	run := collectConstraintStateFixture(t, b, `{"v":"Hearts"}`)

	v := requireConstraintStateNode(t, run, "$.v")
	// The union node's own value IS the winner's value — an ENUM, not a string.
	if got, want := constraintStateDescribe(v.Canonical), "enum:Suit=Hearts"; got != want {
		t.Fatalf("$.v canonical = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, v, []string{`type_meta/check/"winner"/this == "Hearts"=true`})
	if v.Original.Union == nil {
		t.Fatal("$.v recorded no union origin")
	}
	if got, want := *v.Original.Union, (constraintStateUnionOrigin{Index: 1, NullArm: false, Variants: 2}); got != want {
		t.Fatalf("$.v union origin = %+v, want %+v", got, want)
	}
	// EXACTLY ONE child: the winner. The int arm was offered and lost; it has no
	// state, and neither does any default.
	requireConstraintStateCount(t, len(v.Children), 1, "$.v children")
	arm := requireConstraintStateNode(t, run, "$.v|arm1")
	if got, want := constraintStateDescribe(arm.Canonical), "enum:Suit=Hearts"; got != want {
		t.Errorf("arm canonical = %s, want %s", got, want)
	}
	if got, want := arm.Disposition, constraintDispositionUnconstrained; got != want {
		t.Errorf("arm disposition = %s, want %s", got, want)
	}
	// No state exists for arm 0 — the assertion that the loser left no trace.
	if run.Root.find("$.v|arm0") != nil {
		t.Error("a losing union arm produced a state")
	}
	// CONTROL: an int input picks arm 0 and the same predicate then reads an int,
	// so the arm index above is a real selection rather than a constant.
	run2 := collectConstraintStateFixture(t, b, `{"v":7}`)
	v2 := requireConstraintStateNode(t, run2, "$.v")
	if v2.Original.Union == nil {
		t.Fatal("the control run recorded no union origin")
	}
	if got, want := *v2.Original.Union, (constraintStateUnionOrigin{Index: 0, NullArm: false, Variants: 2}); got != want {
		t.Fatalf("int input union origin = %+v, want %+v", got, want)
	}
	if got, want := constraintStateDescribe(v2.Canonical), "int:7"; got != want {
		t.Errorf("int input canonical = %s, want %s", got, want)
	}
	if run2.Root.find("$.v|arm1") != nil {
		t.Error("the enum arm produced a state when the int arm won")
	}
}

// TestConstraintStateOptionalNullWinnerHasNoArmState pins the null arm: BAML's
// JSON-null fast path produces a null value and NO arm state, because no variant
// was coerced.
func TestConstraintStateOptionalNullWinnerHasNoArmState(t *testing.T) {
	cls := constraintStateClass("Cell",
		constraintStateField("v", "", constraintStateOptional(constraintStateIntType())),
		constraintStateField("tag", "", constraintStateCheck(constraintStateStringType(), "tag", `this == "x"`)),
	)
	b := constraintStateBundle(t, constraintStateClassType("Cell"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"v":null,"tag":"x"}`)

	v := requireConstraintStateNode(t, run, "$.v")
	if got, want := constraintStateDescribe(v.Canonical), "null"; got != want {
		t.Fatalf("$.v canonical = %s, want %s", got, want)
	}
	if v.Original.Union == nil {
		t.Fatal("$.v recorded no union origin")
	}
	if got, want := *v.Original.Union, (constraintStateUnionOrigin{Index: 1, NullArm: true, Variants: 1}); got != want {
		t.Fatalf("$.v union origin = %+v, want %+v", got, want)
	}
	requireConstraintStateCount(t, len(v.Children), 0, "$.v children")
	// CONTROL: the same schema with a non-null input DOES coerce the arm, so "no
	// arm state" above is the null decision rather than a collector that never
	// descends into an optional.
	run2 := collectConstraintStateFixture(t, b, `{"v":7,"tag":"x"}`)
	v2 := requireConstraintStateNode(t, run2, "$.v")
	if v2.Original.Union == nil {
		t.Fatal("the control run recorded no union origin")
	}
	if got, want := *v2.Original.Union, (constraintStateUnionOrigin{Index: 0, NullArm: false, Variants: 1}); got != want {
		t.Fatalf("non-null $.v union origin = %+v, want %+v", got, want)
	}
	requireConstraintStateCount(t, len(v2.Children), 1, "non-null $.v children")
	arm := requireConstraintStateNode(t, run2, "$.v|arm0")
	if got, want := constraintStateDescribe(arm.Canonical), "int:7"; got != want {
		t.Errorf("arm canonical = %s, want %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Evaluator declines
// ---------------------------------------------------------------------------

// TestConstraintStateUnsupportedExpressionIsRecordedNotSwallowed pins that a
// predicate the fail-closed profile refuses becomes an explicit `unsupported`
// outcome carrying the ErrConstraintUnsupported chain — never a silent false and
// never a dropped error.
//
// The subject is `in` over a mapping, which the value model documents as an
// unreconcilable divergence and the operator gate therefore refuses.
func TestConstraintStateUnsupportedExpressionIsRecordedNotSwallowed(t *testing.T) {
	order := constraintStateClass("Order", constraintStateField("amount", "", constraintStateIntType()))
	wrapper := constraintStateClass("Wrapper", constraintStateField("order", "",
		constraintStateCheck(constraintStateClassType("Order"), "member", `"amount" in this`)))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper, order}, nil)
	run := collectConstraintStateFixture(t, b, `{"order":{"amount":3}}`)

	node := requireConstraintStateNode(t, run, "$.order")
	if got, want := node.Disposition, constraintDispositionUnsupportedExpression; got != want {
		t.Fatalf("$.order disposition = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, node, []string{
		`type_meta/check/"member"/"amount" in this=unsupported`,
	})
	if node.Events[0].Err == nil {
		t.Fatal("an unsupported event dropped its error")
	}
	if !errors.Is(node.Events[0].Err, ErrConstraintUnsupported) {
		t.Errorf("unsupported event error = %v; want the ErrConstraintUnsupported chain", node.Events[0].Err)
	}
	// An unsupported check is not a false check.
	if node.AssertFailed {
		t.Error("AssertFailed set by an undecidable predicate")
	}
}

// TestConstraintStateUnsupportedAssertDoesNotBecomeAFailure pins that an
// undecidable @assert is NOT an assertion failure: there is no boolean, so
// recording one would fabricate a verdict stock never produced.
func TestConstraintStateUnsupportedAssertDoesNotBecomeAFailure(t *testing.T) {
	order := constraintStateClass("Order", constraintStateField("amount", "", constraintStateIntType()))
	wrapper := constraintStateClass("Wrapper", constraintStateField("order", "",
		constraintStateAssert(constraintStateClassType("Order"), "member", `"amount" in this`)))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper, order}, nil)
	run := collectConstraintStateFixture(t, b, `{"order":{"amount":3}}`)
	node := requireConstraintStateNode(t, run, "$.order")
	requireConstraintStateEvents(t, node, []string{
		`type_meta/assert/"member"/"amount" in this=unsupported`,
	})
	if node.AssertFailed {
		t.Error("an undecidable @assert was recorded as a failed assertion")
	}
	// CONTROL: a DECIDABLE false @assert at the very same node DOES set the flag,
	// so the assertion above is about undecidability rather than a flag that is
	// never set.
	wrapper2 := constraintStateClass("Wrapper", constraintStateField("order", "",
		constraintStateAssert(constraintStateClassType("Order"), "shape", "this.amount == 4")))
	b2 := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper2, order}, nil)
	run2 := collectConstraintStateFixture(t, b2, `{"order":{"amount":3}}`)
	node2 := requireConstraintStateNode(t, run2, "$.order")
	requireConstraintStateEvents(t, node2, []string{
		`type_meta/assert/"shape"/this.amount == 4=false`,
	})
	if !node2.AssertFailed {
		t.Error("a decidable false @assert did not set AssertFailed")
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// TestConstraintStateDefaultFilledFieldCarriesFullState pins that a required
// field filled from TypeIR::default_value is a FULL state, not a gap.
//
// `nums` is absent, `list<int>` is defaultable, and coerce_class fills `[]` with
// DefaultFromNoValue. That is a SUCCESSFUL coercion whose value a predicate then
// runs against, so the node carries the canonical value, the provenance, and its
// evaluated events — leaving it stateless would make the witness blind exactly
// where a value came from somewhere other than the input.
func TestConstraintStateDefaultFilledFieldCarriesFullState(t *testing.T) {
	nums := constraintStateCheck(
		schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateIntType())},
		"empty", "this|length == 0")
	cls := constraintStateClass("Bag",
		constraintStateField("name", "", constraintStateStringType()),
		constraintStateField("nums", "", nums),
	)
	b := constraintStateBundle(t, constraintStateClassType("Bag"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"name":"x"}`)

	if got, want := constraintStateDescribe(run.Root.Canonical), `class:Bag{name=string:"x",nums=list[]}`; got != want {
		t.Fatalf("root canonical = %s, want %s", got, want)
	}
	if got, want := string(run.Root.CanonicalJSON), `{"name":"x","nums":[]}`; got != want {
		t.Errorf("root CanonicalJSON = %s, want %s", got, want)
	}
	filled := requireConstraintStateNode(t, run, "$.nums")
	if !filled.HasCanonical {
		t.Fatal("$.nums has no canonical value; a default fill is a successful coercion")
	}
	if got, want := constraintStateDescribe(filled.Canonical), "list[]"; got != want {
		t.Errorf("$.nums canonical = %s, want %s", got, want)
	}
	if filled.Original.DefaultFill == nil {
		t.Fatal("$.nums recorded no default provenance")
	}
	if got, want := *filled.Original.DefaultFill, (constraintStateDefaultOrigin{Rule: "DefaultFromNoValue", ObservedKind: ""}); got != want {
		t.Errorf("$.nums default origin = %+v, want %+v", got, want)
	}
	// The predicate RAN against the default value.
	if got, want := filled.Disposition, constraintDispositionEvaluated; got != want {
		t.Errorf("$.nums disposition = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, filled, []string{`type_meta/check/"empty"/this|length == 0=true`})
	requireConstraintStateSkipped(t, filled, nil)
	// CONTROL: the SAME field supplied by the input carries no default provenance
	// and the same predicate then decides differently, so the state above is a
	// real default observation rather than a field that always reports one.
	run2 := collectConstraintStateFixture(t, b, `{"name":"x","nums":[1]}`)
	supplied := requireConstraintStateNode(t, run2, "$.nums")
	if supplied.Original.DefaultFill != nil {
		t.Errorf("an input-supplied field recorded default provenance: %+v", *supplied.Original.DefaultFill)
	}
	if got, want := constraintStateDescribe(supplied.Canonical), "list[int:1]"; got != want {
		t.Errorf("supplied $.nums canonical = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, supplied, []string{`type_meta/check/"empty"/this|length == 0=false`})

	// A defaulted UNION field records WHICH arm TypeIR::default_value resolved
	// to, so it is as legible as a coerced one. `int` is not defaultable and
	// `list<int>` is, so the default comes from arm 1 — not arm 0, and not the
	// null arm.
	unionField := constraintStateCheck(schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{
		Variants: []schema.Type{
			constraintStateIntType(),
			{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateIntType())},
		},
	}}, "empty", "this|length == 0")
	unionCls := constraintStateClass("Mixed",
		constraintStateField("name", "", constraintStateStringType()),
		constraintStateField("v", "", unionField),
	)
	ub := constraintStateBundle(t, constraintStateClassType("Mixed"), []schema.ClassDef{unionCls}, nil)
	urun := collectConstraintStateFixture(t, ub, `{"name":"x"}`)
	uv := requireConstraintStateNode(t, urun, "$.v")
	if got, want := constraintStateDescribe(uv.Canonical), "list[]"; got != want {
		t.Fatalf("defaulted union $.v canonical = %s, want %s", got, want)
	}
	if uv.Original.Union == nil {
		t.Fatal("defaulted union $.v recorded no union origin")
	}
	if got, want := *uv.Original.Union, (constraintStateUnionOrigin{Index: 1, NullArm: false, Variants: 2}); got != want {
		t.Errorf("defaulted union $.v origin = %+v, want %+v", got, want)
	}
	if uv.Original.DefaultFill == nil {
		t.Fatal("defaulted union $.v recorded no default provenance")
	}
	if got, want := *uv.Original.DefaultFill, (constraintStateDefaultOrigin{Rule: "DefaultFromNoValue", ObservedKind: ""}); got != want {
		t.Errorf("defaulted union $.v default origin = %+v, want %+v", got, want)
	}
}

// TestConstraintStateMapDefaultFillCarriesFullState pins the other default rule:
// a PRESENT map field whose value coerce_map refuses (error_unexpected_type) is
// filled with {} under DefaultButHadUnparseableValue. The state records the
// canonical {}, the rule, AND the kind of the value that was there.
func TestConstraintStateMapDefaultFillCarriesFullState(t *testing.T) {
	tally := constraintStateCheck(schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateStringType()),
		Value: ptrConstraintStateType(constraintStateIntType()),
	}, "empty", "this|length == 0")
	cls := constraintStateClass("Ledger",
		constraintStateField("name", "", constraintStateStringType()),
		constraintStateField("tally", "", tally),
	)
	b := constraintStateBundle(t, constraintStateClassType("Ledger"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"name":"x","tally":3}`)

	if got, want := constraintStateDescribe(run.Root.Canonical), `class:Ledger{name=string:"x",tally=map{}}`; got != want {
		t.Fatalf("root canonical = %s, want %s", got, want)
	}
	filled := requireConstraintStateNode(t, run, "$.tally")
	if !filled.HasCanonical {
		t.Fatal("$.tally has no canonical value; the {} fill is a successful coercion")
	}
	if got, want := constraintStateDescribe(filled.Canonical), "map{}"; got != want {
		t.Errorf("$.tally canonical = %s, want %s", got, want)
	}
	if filled.Original.DefaultFill == nil {
		t.Fatal("$.tally recorded no default provenance")
	}
	// ObservedKind is the discriminator against the absent-field rule: the value
	// WAS there, and it was a number.
	if got, want := *filled.Original.DefaultFill, (constraintStateDefaultOrigin{Rule: "DefaultButHadUnparseableValue", ObservedKind: "number"}); got != want {
		t.Errorf("$.tally default origin = %+v, want %+v", got, want)
	}
	requireConstraintStateEvents(t, filled, []string{`type_meta/check/"empty"/this|length == 0=true`})
	requireConstraintStateCount(t, len(filled.Children), 0, "$.tally children")
	// CONTROL: a real object value produces real entries and no default provenance.
	run2 := collectConstraintStateFixture(t, b, `{"name":"x","tally":{"a":1}}`)
	real := requireConstraintStateNode(t, run2, "$.tally")
	if real.Original.DefaultFill != nil {
		t.Errorf("a coercible map recorded default provenance: %+v", *real.Original.DefaultFill)
	}
	if got, want := constraintStateDescribe(real.Canonical), "map{a=int:1}"; got != want {
		t.Errorf("coercible $.tally canonical = %s, want %s", got, want)
	}
	requireConstraintStateEvents(t, real, []string{`type_meta/check/"empty"/this|length == 0=false`})
}

// TestConstraintStateSingleFieldAbsorptionCarriesFullState pins coerce_class's
// two single-field absorptions, both of which are successful coercions that
// produce a value from something other than a matched key:
//
//   - INFERRED OBJECT (coerce_class.rs:295): a scalar becomes the lone field's
//     value;
//   - IMPLIED KEY (coerce_class.rs:224): an object whose keys matched nothing is
//     stringified into the lone field.
//
// The two are distinguished by [constraintStateImpliedOrigin.Inferred], and
// neither records a field ALIAS — the value was absorbed, not key-routed.
func TestConstraintStateSingleFieldAbsorptionCarriesFullState(t *testing.T) {
	cls := constraintStateClass("Note",
		constraintStateField("text", "", constraintStateCheck(constraintStateStringType(), "nonempty", `this == "x"`)))
	b := constraintStateBundle(t, constraintStateClassType("Note"), []schema.ClassDef{cls}, nil)

	inferred := collectConstraintStateFixture(t, b, `"x"`)
	if got, want := constraintStateDescribe(inferred.Root.Canonical), `class:Note{text=string:"x"}`; got != want {
		t.Fatalf("inferred-object canonical = %s, want %s", got, want)
	}
	if inferred.Root.Original.Implied == nil {
		t.Fatal("inferred-object recorded no absorption provenance")
	}
	if got, want := *inferred.Root.Original.Implied, (constraintStateImpliedOrigin{Field: "text", Inferred: true}); got != want {
		t.Errorf("inferred-object origin = %+v, want %+v", got, want)
	}
	if got := inferred.Root.Original.FieldAliases; len(got) != 0 {
		t.Errorf("an absorbed value recorded a field alias: %+v", got)
	}
	requireConstraintStateEvents(t, requireConstraintStateNode(t, inferred, "$.text"),
		[]string{`type_meta/check/"nonempty"/this == "x"=true`})

	implied := collectConstraintStateFixture(t, b, `{"other":"x"}`)
	// The WHOLE object is stringified into the lone field (JsonToString), which is
	// why the field value is not `"x"`.
	if got, want := constraintStateDescribe(implied.Root.Canonical), `class:Note{text=string:"{other: x}"}`; got != want {
		t.Fatalf("implied-key canonical = %s, want %s", got, want)
	}
	if implied.Root.Original.Implied == nil {
		t.Fatal("implied-key recorded no absorption provenance")
	}
	if got, want := *implied.Root.Original.Implied, (constraintStateImpliedOrigin{Field: "text", Inferred: false}); got != want {
		t.Errorf("implied-key origin = %+v, want %+v", got, want)
	}
	requireConstraintStateEvents(t, requireConstraintStateNode(t, implied, "$.text"),
		[]string{`type_meta/check/"nonempty"/this == "x"=false`})

	// CONTROL: an ordinarily key-matched value records NO absorption at all, so
	// the provenance above is a real routing observation.
	matched := collectConstraintStateFixture(t, b, `{"text":"x"}`)
	if got := matched.Root.Original.Implied; got != nil {
		t.Errorf("a key-matched class recorded absorption provenance: %+v", *got)
	}
}

// TestConstraintStateClassFromArrayCollectsTheWinningItem pins coerce_class's
// ARRAY branch: coerce_array_to_singular ranks the items and the class value IS
// the winner. The collector reuses production's own selector, collects state for
// the winner only, and records which index won out of how many.
//
// The fixture is discriminating: item 0 is missing the required non-defaultable
// field `b`, which is a PROVEN BAML class error and is excluded from the
// ranking, so the winner is index 1 rather than the first item.
func TestConstraintStateClassFromArrayCollectsTheWinningItem(t *testing.T) {
	pair := constraintStateClass("Pair",
		constraintStateField("a", "", constraintStateStringType()),
		constraintStateField("b", "", constraintStateStringType()),
	)
	wrapper := constraintStateClass("Wrapper", constraintStateField("pair", "",
		constraintStateCheck(constraintStateClassType("Pair"), "winner", `this.a == "p"`)))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper, pair}, nil)
	run := collectConstraintStateFixture(t, b, `{"pair":[{"a":"x"},{"a":"p","b":"q"}]}`)

	pairNode := requireConstraintStateNode(t, run, "$.pair")
	if got, want := constraintStateDescribe(pairNode.Canonical), `class:Pair{a=string:"p",b=string:"q"}`; got != want {
		t.Fatalf("$.pair canonical = %s, want %s", got, want)
	}
	if pairNode.Original.ArrayToSingular == nil {
		t.Fatal("$.pair recorded no array-to-singular provenance")
	}
	if got, want := *pairNode.Original.ArrayToSingular, (constraintStateArrayOrigin{Index: 1, Items: 2}); got != want {
		t.Fatalf("$.pair array origin = %+v, want %+v", got, want)
	}
	requireConstraintStateEvents(t, pairNode, []string{`type_meta/check/"winner"/this.a == "p"=true`})
	// EXACTLY ONE child: the winning item. The losing item left no state.
	requireConstraintStateCount(t, len(pairNode.Children), 1, "$.pair children")
	winner := requireConstraintStateNode(t, run, "$.pair[1]")
	if got, want := constraintStateDescribe(winner.Canonical), `class:Pair{a=string:"p",b=string:"q"}`; got != want {
		t.Errorf("winner canonical = %s, want %s", got, want)
	}
	if run.Root.find("$.pair[0]") != nil {
		t.Error("the losing array item produced a state")
	}
}

// TestConstraintStateRefusesTypeKindsOutsideTheModelledSet pins the ONLY
// remaining refusal: a type KIND outside §2's node list.
//
// These are not "a successful route the collector skipped".
// schema.Bundle.ValidateOutput rejects tuple/arrow/top/media before parsing, and
// a recursive alias is a separate admitted family with its own scored coercer
// (alias_coerce.go) whose canonicalization the collector would have to re-derive
// rather than delegate. Production coerce fails or is unreachable for each, so
// they are driven directly at the traversal seam — otherwise the refusal would
// be untestable and the guard could rot unnoticed.
func TestConstraintStateRefusesTypeKindsOutsideTheModelledSet(t *testing.T) {
	b := constraintStateBundle(t, constraintStateStringType(), nil, nil)
	c := &constraintStateCollector{bundle: b}
	path := constraintStatePath{{Kind: constraintPathRoot}}
	cases := []struct {
		name string
		typ  schema.Type
	}{
		{"tuple", schema.Type{Kind: schema.TypeTuple, Items: []schema.Type{constraintStateIntType()}}},
		{"arrow", schema.Type{Kind: schema.TypeArrow}},
		{"recursive-alias", schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JSON"}},
		{"top", schema.Type{Kind: schema.TypeTop}},
		{"media", schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveMedia}},
	}
	requireConstraintStateCount(t, len(cases), 5, "unmodelled type kinds")
	for _, tc := range cases {
		st := &constraintCoercionState{Path: path, Type: tc.typ}
		_, _, err := c.canonicalValue(tc.typ, value{kind: valNull}, nil, path, json.RawMessage("null"), false, st)
		if err == nil {
			t.Errorf("%s: canonicalValue produced a state for an unmodelled type kind", tc.name)
			continue
		}
		if !errors.Is(err, errConstraintStateUnmodelled) {
			t.Errorf("%s: error = %v; want errConstraintStateUnmodelled", tc.name, err)
		}
	}
	// CONTROL: a MODELLED kind at the same seam succeeds, so the refusals above
	// are about the kind rather than about the seam being unusable.
	st := &constraintCoercionState{Path: path, Type: constraintStateStringType()}
	cv, _, err := c.canonicalValue(constraintStateStringType(), value{kind: valString, strV: "x"}, nil, path, json.RawMessage(`"x"`), false, st)
	if err != nil {
		t.Fatalf("control (string primitive): %v", err)
	}
	if got, want := constraintStateDescribe(cv), `string:"x"`; got != want {
		t.Errorf("control canonical = %s, want %s", got, want)
	}
}

// TestConstraintStateDefaultConstraintValueMirrorsProductionDefaults pins the
// default DOMAIN mirror against production's own [defaultValue], type by type,
// including the union arm resolution — so the mirror cannot drift from the bytes
// it is checked against.
func TestConstraintStateDefaultConstraintValueMirrorsProductionDefaults(t *testing.T) {
	b := constraintStateBundle(t, constraintStateStringType(), nil, nil)
	c := &constraintStateCollector{bundle: b}
	path := constraintStatePath{{Kind: constraintPathRoot}}
	listT := schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateIntType())}
	mapT := schema.Type{Kind: schema.TypeMap, Key: ptrConstraintStateType(constraintStateStringType()), Value: ptrConstraintStateType(constraintStateIntType())}
	nullT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveNull}
	defaultable := []struct {
		name string
		typ  schema.Type
		want string
	}{
		{"list", listT, "list[]"},
		{"map", mapT, "map{}"},
		{"primitive-null", nullT, "null"},
		{"optional-int", constraintStateOptional(constraintStateIntType()), "null"},
		{"union-int-then-list", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{constraintStateIntType(), listT}}}, "list[]"},
	}
	requireConstraintStateCount(t, len(defaultable), 5, "defaultable types")
	for _, tc := range defaultable {
		raw, ok := defaultValue(tc.typ)
		if !ok {
			t.Errorf("%s: production defaultValue reports it non-defaultable; the mirror case is stale", tc.name)
			continue
		}
		cv, err := c.defaultConstraintValue(tc.typ, path)
		if err != nil {
			t.Errorf("%s: mirror refused a defaultable type: %v", tc.name, err)
			continue
		}
		if got := constraintStateDescribe(cv); got != tc.want {
			t.Errorf("%s: mirror = %s, want %s", tc.name, got, tc.want)
			continue
		}
		got, err := cv.MarshalJSON()
		if err != nil {
			t.Errorf("%s: serialize: %v", tc.name, err)
			continue
		}
		if diff, ok := constraintStateJSONEquivalent(got, raw); !ok {
			t.Errorf("%s: mirror %s does not match production default %s: %s", tc.name, got, raw, diff)
		}
	}
	// The NON-defaultable kinds must be refused by the mirror too, or a
	// non-defaultable field could be silently given a value BAML never fills.
	nonDefaultable := []struct {
		name string
		typ  schema.Type
	}{
		{"string", constraintStateStringType()},
		{"int", constraintStateIntType()},
		{"enum", schema.Type{Kind: schema.TypeEnum, Name: "Suit"}},
		{"class", constraintStateClassType("X")},
	}
	requireConstraintStateCount(t, len(nonDefaultable), 4, "non-defaultable types")
	for _, tc := range nonDefaultable {
		if _, ok := defaultValue(tc.typ); ok {
			t.Errorf("%s: production defaultValue reports it defaultable; the case is stale", tc.name)
			continue
		}
		if _, err := c.defaultConstraintValue(tc.typ, path); !errors.Is(err, errConstraintStateUnmodelled) {
			t.Errorf("%s: mirror returned %v; want errConstraintStateUnmodelled", tc.name, err)
		}
	}
}

// TestConstraintStateRefusesAFailedCoercion pins that the collector models a
// SUCCESSFUL canonical coercion only: when production declines the value there
// is no state to report, and the production error is propagated rather than
// masked.
func TestConstraintStateRefusesAFailedCoercion(t *testing.T) {
	order := constraintStateClass("Order", constraintStateField("amount", "", constraintStateIntType()))
	wrapper := constraintStateClass("Wrapper", constraintStateField("order", "",
		constraintStateCheck(constraintStateClassType("Order"), "c", "this.amount == 3")))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper, order}, nil)
	_, err := collectConstraintCoercionState(b, `{"order":{"amount":"not_a_number"}}`)
	if err == nil {
		t.Fatal("collector produced a state for a value production could not coerce")
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("error = %v; want the production decline propagated", err)
	}
	if !strings.Contains(err.Error(), "SUCCESSFUL canonical coercion only") {
		t.Errorf("error = %v; want the collector's own contract named", err)
	}
	// CONTROL: the same schema with a coercible value DOES produce a state, so
	// the refusal above is about the value rather than about the fixture.
	run := collectConstraintStateFixture(t, b, `{"order":{"amount":3}}`)
	if got, want := constraintStateDescribe(requireConstraintStateNode(t, run, "$.order").Canonical), "class:Order{amount=int:3}"; got != want {
		t.Errorf("control canonical = %s, want %s", got, want)
	}
}

// TestConstraintStateDuplicateClassFieldNamesAreRefused pins the class entry
// builder's duplicate-key rule: two identically-named canonical fields would
// silently lose one through any map-shaped consumer, so the collector refuses
// instead of overwriting.
func TestConstraintStateDuplicateClassFieldNamesAreRefused(t *testing.T) {
	// Built valid, indexed, THEN mutated: schema.Bundle.RebuildIndexes rejects the
	// duplicate outright, and the point here is that the COLLECTOR refuses it too,
	// independently of the index.
	cls := constraintStateClass("Dup",
		constraintStateField("a", "", constraintStateIntType()),
		constraintStateField("z", "", constraintStateIntType()),
	)
	wrapper := constraintStateClass("Wrapper", constraintStateField("dup", "",
		constraintStateCheck(constraintStateClassType("Dup"), "c", "this.a == 1")))
	b := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{wrapper, cls}, nil)
	// Control first: while the field names are distinct the collector is happy.
	if _, err := collectConstraintCoercionState(b, `{"dup":{"a":1,"z":2}}`); err != nil {
		t.Fatalf("control (distinct field names): %v", err)
	}
	b.Classes[1].Fields[1].Name = schema.Name{Name: "a", Alias: strPtrConstraintState("z")}
	_, err := collectConstraintCoercionState(b, `{"dup":{"a":1,"z":2}}`)
	if err == nil {
		t.Fatal("collector accepted a class declaring the same canonical field twice")
	}
	if !strings.Contains(err.Error(), "declares field") {
		t.Errorf("error = %v; want the duplicate-field refusal", err)
	}
}

// TestConstraintStateDuplicateMapKeysNeverReachTheBuilder pins the other half:
// coerce_map declines a duplicate input key outright, so a duplicate can never
// reach the map entry builder — and the collector propagates that decline rather
// than building a map with one key silently overwritten.
func TestConstraintStateDuplicateMapKeysNeverReachTheBuilder(t *testing.T) {
	mapT := schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateStringType()),
		Value: ptrConstraintStateType(constraintStateIntType()),
	}
	// The duplicate probe drives the map DIRECTLY as the target: nested under a
	// class field, coerce_class would reclassify coerce_map's decline as an
	// indeterminate FIELD and the duplicate-key reason would never surface. This
	// bundle carries no constraint and makes no claim about the support gate — it
	// is about coerce_map and the collector's entry builder.
	direct := constraintStateBundle(t, mapT, nil, nil)
	_, err := collectConstraintCoercionState(direct, `{"a":1,"a":2}`)
	if err == nil {
		t.Fatal("collector produced a state for a map with a duplicate input key")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("error = %v; want coerce_map's duplicate-key decline", err)
	}
	// CONTROL: distinct keys build a two-entry map in INPUT order, in a bundle
	// production still refuses.
	cls := constraintStateClass("Board", constraintStateField("scores", "",
		constraintStateCheck(mapT, "c", "this.a == 1")))
	b := constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{cls}, nil)
	run := collectConstraintStateFixture(t, b, `{"scores":{"b":1,"a":2}}`)
	if got, want := constraintStateDescribe(requireConstraintStateNode(t, run, "$.scores").Canonical), "map{b=int:1,a=int:2}"; got != want {
		t.Errorf("control canonical = %s, want %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// The production boundary
// ---------------------------------------------------------------------------

// TestConstraintStateConstrainedBundlesAreStillRefused is the boundary lock: the
// WHOLE non-admission invariant, with nothing carved out of it.
//
// It asserts, HARD, through the three gates that actually decide serving —
// `checkSupported` (the Parse gate), `SupportsNativeFinalBundle` (the admission
// predicate) and `ParseStaticBundle` (the static-final entry point) — that every
// constrained shape reachable from the RETURN TYPE, a class field, a class
// declaration or an enum declaration is REFUSED with the
// ErrDeBAMLParseUnsupported fallback sentinel. A state result must never be
// readable as admission changing.
//
// The three target-level rows arrived from the retired #662 tripwire: they were
// the one family the gate did not reach, and the decline-more fix that walks
// b.Target moved them here. Each row carries its OWN raw, chosen so the DECLINE is
// a gate decision rather than a coercion failure — the unconstrained-twin
// assertion below proves that by serving the very same bytes.
func TestConstraintStateConstrainedBundlesAreStillRefused(t *testing.T) {
	suit := schema.EnumDef{
		Name: schema.Name{Name: "Suit"},
		Values: []schema.EnumValue{
			{Name: schema.Name{Name: "Hearts", Alias: strPtrConstraintState("hearts_alias")}},
			{Name: schema.Name{Name: "Spades"}},
		},
	}
	constrainedEnum := schema.EnumDef{
		Name:        schema.Name{Name: "Level"},
		Values:      []schema.EnumValue{{Name: schema.Name{Name: "Low"}}, {Name: schema.Name{Name: "High"}}},
		Constraints: []schema.Constraint{{Level: schema.ConstraintCheck, Expression: `this == "Low"`}},
	}
	hand := constraintStateClass("Hand",
		constraintStateField("suit", "", constraintStateCheck(schema.Type{Kind: schema.TypeEnum, Name: "Suit"}, "c", `this == "Hearts"`)),
	)
	order := constraintStateClass("Order", constraintStateField("amount", "qty", constraintStateIntType()))
	order.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this.amount == 3"}}
	bag := constraintStateClass("Bag", constraintStateField("nums", "",
		schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateCheck(constraintStateIntType(), "c", "this > 0"))}))
	boardValue := constraintStateClass("Board", constraintStateField("scores", "", schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateStringType()),
		Value: ptrConstraintStateType(constraintStateCheck(constraintStateIntType(), "c", "this > 0")),
	}))
	boardKey := constraintStateClass("Board", constraintStateField("scores", "", schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateCheck(constraintStateStringType(), "c", `this == "a"`)),
		Value: ptrConstraintStateType(constraintStateIntType()),
	}))
	scalarField := constraintStateClass("Wrapper",
		constraintStateField("note", "", constraintStateCheck(constraintStateStringType(), "c", `this == "x"`)))
	optionalField := constraintStateClass("Wrapper",
		constraintStateField("note", "", constraintStateCheck(constraintStateOptional(constraintStateStringType()), "c", `this == "x"`)))

	// The raw the CLASS-rooted rows are driven with: one object carrying every
	// field any of them reads, so each row's stripped twin below coerces it
	// cleanly.
	const classRaw = `{"note":"x","suit":"Hearts","lvl":"Low","amount":3,"qty":3,"nums":[1],"scores":{"a":1}}`

	gated := []struct {
		name   string
		bundle *schema.Bundle
		raw    string
		// twinServed is the EXACT document the constraint-stripped twin serves for
		// raw, or "" when that twin still declines for a reason unrelated to
		// constraints — in which case twinDeclines names that reason and the row
		// asserts it instead. Exactly one of the two is set.
		twinServed   string
		twinDeclines string
	}{
		{"scalar-class-field", constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{scalarField}, nil), classRaw,
			"", "single string-absorbing-field root class"},
		{"optional-class-field", constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{optionalField}, nil), classRaw,
			"", "single string-absorbing-field root class"},
		{"enum-field", constraintStateBundle(t, constraintStateClassType("Hand"), []schema.ClassDef{hand}, []schema.EnumDef{suit}), classRaw,
			"", `required field "suit" provably fails to coerce`},
		{"enum-declaration", constraintStateBundle(t, constraintStateClassType("Hand"), []schema.ClassDef{constraintStateClass("Hand", constraintStateField("lvl", "", schema.Type{Kind: schema.TypeEnum, Name: "Level"}))}, []schema.EnumDef{constrainedEnum}), classRaw,
			`{"lvl":"Low"}`, ""},
		{"class-declaration", constraintStateBundle(t, constraintStateClassType("Order"), []schema.ClassDef{order}, nil), classRaw,
			`{"amount":3}`, ""},
		{"list-element", constraintStateBundle(t, constraintStateClassType("Bag"), []schema.ClassDef{bag}, nil), classRaw,
			`{"nums":[1]}`, ""},
		{"map-value", constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{boardValue}, nil), classRaw,
			"", "non-last unquoted-scalar class field or scalar map value"},
		{"map-key", constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{boardKey}, nil), classRaw,
			"", "non-last unquoted-scalar class field or scalar map value"},
		// The three rows the b.Target walk added — the retired #662 tripwire's own
		// fixtures, now ordinary members of the declining set. Their twins SERVE, so
		// they are also this change's no-over-decline regression fixtures: the exact
		// bytes native used to serve WITH the constraint are still served without it.
		{"target-assert", constraintStateBundle(t, constraintStateAssert(constraintStateStringType(), "shape", `this == "expected"`), nil, nil), `"actual"`,
			`"actual"`, ""},
		{"target-check", constraintStateBundle(t, constraintStateCheck(constraintStateStringType(), "shape", `this == "expected"`), nil, nil), `"actual"`,
			`"actual"`, ""},
		{"target-list-element", constraintStateBundle(t, schema.Type{
			Kind: schema.TypeList,
			Elem: ptrConstraintStateType(constraintStateAssert(constraintStateIntType(), "big", "this > 100")),
		}, nil, nil), `[1,2]`,
			`[1,2]`, ""},
	}
	requireConstraintStateCount(t, len(gated), 11, "gated constrained shapes")
	served := 0
	for _, g := range gated {
		if g.twinServed != "" {
			served++
		}
		t.Run(g.name, func(t *testing.T) {
			if (g.twinServed == "") == (g.twinDeclines == "") {
				t.Fatalf("the row sets neither or both of twinServed/twinDeclines; one outcome must be stated")
			}
			if err := checkSupported(g.bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("checkSupported = %v; want the ErrDeBAMLParseUnsupported fallback sentinel", err)
			}
			if err := SupportsNativeFinalBundle(g.bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("SupportsNativeFinalBundle = %v; want the fallback sentinel", err)
			}
			// The static-final entry point must refuse too.
			_, err := ParseStaticBundle(context.Background(), g.bundle, g.raw)
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("ParseStaticBundle = %v; want the fallback sentinel", err)
			}

			// NO OVER-DECLINE, and the reason the declines above are attributable to
			// the constraint at all. The twin is DERIVED from this very bundle by
			// deleting its constraints and nothing else, so the pair differs in
			// exactly that attribute: whatever the twin admits, the constraint alone
			// caused the gate to refuse it.
			twin, removed := constraintStateWithoutConstraints(t, g.bundle)
			if removed == 0 {
				t.Fatalf("the fixture declares no constraint; its decline proves nothing about constraints")
			}
			// checkSupported is the gate the b.Target walk changed, so every twin —
			// including the three target-level ones — must pass it.
			if err := checkSupported(twin); err != nil {
				t.Fatalf("stripped twin: checkSupported DECLINED (%v); removing the constraint must restore admission", err)
			}
			res, terr := ParseStaticBundle(context.Background(), twin, g.raw)
			if g.twinServed != "" {
				if terr != nil {
					t.Fatalf("stripped twin: ParseStaticBundle = %v; want it to SERVE %s", terr, g.twinServed)
				}
				if got := string(res.JSON); got != g.twinServed {
					t.Errorf("stripped twin served %s, want %s", got, g.twinServed)
				}
				return
			}
			// The twin passes the CONSTRAINT gate but still declines downstream, for
			// a reason this row names. Pinning it is what stops the arm from being a
			// silent "it declined, close enough": the reason must be the stated
			// non-constraint one, so nothing here can be read as the constraint gate
			// still firing.
			if terr == nil {
				t.Fatalf("stripped twin SERVED %s; the row claims it declines for %q", res.JSON, g.twinDeclines)
			}
			if !strings.Contains(terr.Error(), g.twinDeclines) {
				t.Errorf("stripped twin declined with %v; want the non-constraint reason %q", terr, g.twinDeclines)
			}
			if strings.Contains(terr.Error(), "constraint") {
				t.Errorf("stripped twin declined for a CONSTRAINT reason (%v) after every constraint was removed", terr)
			}
		})
	}
	requireConstraintStateCount(t, served, 6, "rows whose stripped twin serves")
}

// constraintStateWithoutConstraints returns a DEEP COPY of b with every declared
// @assert/@check removed — from the target's whole type tree, from each enum and
// class declaration, and from every field type — plus the number it removed.
//
// It is DERIVED rather than hand-written on purpose. A second, hand-built
// "unconstrained version" of a fixture can drift into a different shape, and then
// its admission says nothing about why the constrained one declined. Copying the
// subject and deleting exactly one attribute makes the pair differ in exactly that
// attribute, so the twin's admission isolates the constraint as the cause. The
// count is returned so a caller can refuse a vacuous comparison against a fixture
// that declared nothing.
func constraintStateWithoutConstraints(t *testing.T, b *schema.Bundle) (*schema.Bundle, int) {
	t.Helper()
	removed := 0
	var stripType func(schema.Type) schema.Type
	stripType = func(x schema.Type) schema.Type {
		removed += len(x.Meta.Constraints)
		x.Meta.Constraints = nil
		if x.Elem != nil {
			x.Elem = ptrConstraintStateType(stripType(*x.Elem))
		}
		if x.Key != nil {
			x.Key = ptrConstraintStateType(stripType(*x.Key))
		}
		if x.Value != nil {
			x.Value = ptrConstraintStateType(stripType(*x.Value))
		}
		if len(x.Items) > 0 {
			items := make([]schema.Type, len(x.Items))
			for i := range x.Items {
				items[i] = stripType(x.Items[i])
			}
			x.Items = items
		}
		if x.Union != nil {
			u := *x.Union
			u.Variants = make([]schema.Type, len(x.Union.Variants))
			for i := range x.Union.Variants {
				u.Variants[i] = stripType(x.Union.Variants[i])
			}
			x.Union = &u
		}
		return x
	}

	enums := make([]schema.EnumDef, len(b.Enums))
	for i := range b.Enums {
		enums[i] = b.Enums[i]
		removed += len(enums[i].Constraints)
		enums[i].Constraints = nil
	}
	classes := make([]schema.ClassDef, len(b.Classes))
	for i := range b.Classes {
		classes[i] = b.Classes[i]
		removed += len(classes[i].Constraints)
		classes[i].Constraints = nil
		fields := make([]schema.ClassField, len(b.Classes[i].Fields))
		for j := range b.Classes[i].Fields {
			fields[j] = b.Classes[i].Fields[j]
			fields[j].Type = stripType(fields[j].Type)
		}
		classes[i].Fields = fields
	}
	return constraintStateBundle(t, stripType(b.Target), classes, enums), removed
}

// ---------------------------------------------------------------------------
// The closed target-level gap — what declining actually bought
// ---------------------------------------------------------------------------

// constraintStateDeclaredConstraints renders a schema node's DECLARED
// constraints as `level/label/expression`, in declaration order, so a test can
// pin exactly what is attached where — count, level, label and the exact source
// bytes — rather than assuming it.
func constraintStateDeclaredConstraints(t schema.Type) []string {
	out := make([]string, 0, len(t.Meta.Constraints))
	for i := range t.Meta.Constraints {
		c := &t.Meta.Constraints[i]
		label := "-"
		if c.Label != nil {
			label = strconv.Quote(*c.Label)
		}
		out = append(out, fmt.Sprintf("%s/%s/%s", c.Level, label, c.Expression))
	}
	return out
}

// TestTargetLevelConstraintDeclineClassification is what the retired #662
// known-gap tripwire turned into once the gap closed.
//
// THE GAP THAT WAS. `checkSupported` -> `checkSupportedFields` iterated `b.Enums`
// and `b.Classes` and never walked `b.Target`, so a constraint declared on the
// RETURN TYPE itself — `function F() -> int @assert(...)`, or a constrained
// element of a target list — was never examined and all three gates ADMITTED it.
// The tripwire asserted that wrong behaviour on purpose and failed the moment
// [checkTypeNoConstraints] landed.
//
// WHAT THIS TEST IS FOR. The decline itself is pinned alongside every other
// constrained shape by [TestConstraintStateConstrainedBundlesAreStillRefused].
// What is pinned HERE is the other half of the differential: for each
// target-level shape, the exact value native SERVED while the gate admitted it.
// That value is the native leg of the out-claim question, and it is derived
// rather than quoted — from the constraint-stripped twin, which coerces
// identically because native's coercer never reads Meta.Constraints.
//
// WHERE THE STOCK LEG LIVES, AND WHY NOT HERE. Whether declining a shape REMOVED
// an out-claim depends on what stock BAML v0.223 does with it, and no unit test
// in this package can answer that: [EvaluateConstraint] is the native evaluator,
// and its verdict on a predicate is NOT stock's verdict on a parse. Stock is
// measured live, through CFFI, by
// TestStockTargetLevelConstraintDispositionAndNativeDecline in
// internal/bamlprofile/profileoracle, which parses these same shapes through the
// untouched v0.223 runtime and asserts the native decline beside each one. Its
// measurement is a THREE-way taxonomy, and every row below carries the class it
// landed in:
//
//   - RAISES — a bare `int` return and a LIST-LEVEL constraint reject the parse
//     with "Assertions failed.". Native served a value; BAML errors. A genuine
//     out-claim, and declining restores the erroring fallback.
//   - SERVES A DIFFERENT VALUE — a constrained list ELEMENT does not reject.
//     coerce_array DROPS each failing element, so stock returns `[]` where native
//     returned `[1,2]`. An out-claim in value rather than in outcome.
//   - SERVES THE SAME VALUE — a bare `string` return skips constraint evaluation
//     entirely, so native's served value MATCHED stock's. Declining it is NOT an
//     out-claim fix; it is plain over-decline, safe under the parity principle.
//     [TestConstraintStateBareStringReturnSkipsBothLevels] pins that skip on the
//     collector side and profileoracle's TestStockSkipsConstraintsOnBareStringReturn
//     measures it against stock. Calling this row a removed out-claim would
//     contradict both, so it is labelled for what it is.
//
// The taxonomy is asserted, not just described: the row set must contain at least
// one genuine out-claim and at least one over-decline, so it cannot quietly
// collapse into "they all decline, therefore they were all bugs".
func TestTargetLevelConstraintDeclineClassification(t *testing.T) {
	probes := []struct {
		name   string
		bundle *schema.Bundle
		raw    string
		// wouldHaveServed is what ParseStaticBundle returned for raw BEFORE the
		// gate walked b.Target. Asserted against the constraint-stripped twin, so
		// it is measured here rather than remembered.
		wouldHaveServed string
		// constrained returns the schema node the predicate is DECLARED on, and
		// wantDeclared is that node's complete constraint list, in order.
		constrained  func(*schema.Bundle) schema.Type
		wantDeclared []string
		// stockDoes and outClaim record profileoracle's LIVE measurement for this
		// shape. They are documentation of another test's result, which is why the
		// row set's composition is asserted below rather than these values being
		// re-derived from a native evaluator call.
		stockDoes string
		outClaim  bool
	}{
		{
			// The genuine out-claim: stock REJECTS the parse, native served 5.
			name:            "bare-int-return",
			bundle:          constraintStateBundle(t, constraintStateAssert(constraintStateIntType(), "f", "false"), nil, nil),
			raw:             `5`,
			wouldHaveServed: `5`,
			constrained:     func(b *schema.Bundle) schema.Type { return b.Target },
			wantDeclared:    []string{`assert/"f"/false`},
			stockDoes:       "raises (Assertions failed.)",
			outClaim:        true,
		},
		{
			// The constraint on the LIST ITSELF. Stock rejects this one too.
			name: "target-list-level",
			bundle: constraintStateBundle(t, constraintStateAssert(schema.Type{
				Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateIntType()),
			}, "f", "false"), nil, nil),
			raw:             `[1,2]`,
			wouldHaveServed: `[1,2]`,
			constrained:     func(b *schema.Bundle) schema.Type { return b.Target },
			wantDeclared:    []string{`assert/"f"/false`},
			stockDoes:       "raises (Assertions failed.)",
			outClaim:        true,
		},
		{
			// The constrained ELEMENT of a target list — one of the two retired
			// tripwire fixtures. Stock does NOT reject, so this is not the
			// "native serves where BAML raises" shape; it is still an out-claim,
			// in VALUE: coerce_array drops both failing elements and stock serves
			// `[]` where native served both.
			name: "target-list-element",
			bundle: constraintStateBundle(t, schema.Type{
				Kind: schema.TypeList,
				Elem: ptrConstraintStateType(constraintStateAssert(constraintStateIntType(), "big", "this > 100")),
			}, nil, nil),
			raw:             `[1,2]`,
			wouldHaveServed: `[1,2]`,
			constrained:     func(b *schema.Bundle) schema.Type { return *b.Target.Elem },
			wantDeclared:    []string{`assert/"big"/this > 100`},
			stockDoes:       "serves [] (coerce_array drops the failing elements)",
			outClaim:        true,
		},
		{
			// The other retired tripwire fixture, and the row that keeps this test
			// honest. Stock SKIPS constraints on a bare-string return, so it serves
			// exactly what native served. Over-decline, not a removed out-claim.
			name:            "bare-string-return",
			bundle:          constraintStateBundle(t, constraintStateAssert(constraintStateStringType(), "shape", `this == "expected"`), nil, nil),
			raw:             `"actual"`,
			wouldHaveServed: `"actual"`,
			constrained:     func(b *schema.Bundle) schema.Type { return b.Target },
			wantDeclared:    []string{`assert/"shape"/this == "expected"`},
			stockDoes:       "serves the same value (bare-string return skips constraints)",
			outClaim:        false,
		},
	}
	requireConstraintStateCount(t, len(probes), 4, "target-level probes")

	// THE TAXONOMY IS NOT ALLOWED TO COLLAPSE. Without a genuine out-claim the
	// gate would be removing nothing; without an over-decline row the bare-string
	// contradiction this test exists to reconcile would have quietly vanished.
	outClaims, overDeclines := 0, 0
	for _, p := range probes {
		if p.outClaim {
			outClaims++
		} else {
			overDeclines++
		}
	}
	if outClaims == 0 {
		t.Fatal("no probe is a genuine out-claim; the gate would be removing nothing")
	}
	if overDeclines == 0 {
		t.Fatal("no probe is an over-decline; the bare-string route's stock SKIP must stay represented")
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			// 1. Pin exactly what the probe declares, so nothing below can drift
			// from the fixture — a deleted, relabelled, downgraded or reworded
			// constraint fails right here.
			node := p.constrained(p.bundle)
			declared := constraintStateDeclaredConstraints(node)
			requireConstraintStateCount(t, len(declared), len(p.wantDeclared), "declared constraints on the probe's schema node")
			for i := range p.wantDeclared {
				if declared[i] != p.wantDeclared[i] {
					t.Fatalf("declared constraint %d = %s, want %s", i, declared[i], p.wantDeclared[i])
				}
			}

			// 2. NATIVE DECLINES, through all three gates, with the fallback
			// sentinel — so the call routes to BAML and native serves nothing.
			if err := checkSupported(p.bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("checkSupported = %v; want the ErrDeBAMLParseUnsupported fallback sentinel", err)
			}
			if err := SupportsNativeFinalBundle(p.bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("SupportsNativeFinalBundle = %v; want the fallback sentinel", err)
			}
			res, serr := ParseStaticBundle(context.Background(), p.bundle, p.raw)
			if !errors.Is(serr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("ParseStaticBundle = (%s, %v); want the fallback sentinel", res.JSON, serr)
			}
			if len(res.JSON) != 0 {
				t.Errorf("a declining ParseStaticBundle still returned %s; a decline must serve nothing", res.JSON)
			}

			// 3. WHAT NATIVE SERVED WHILE THE GATE ADMITTED THIS, measured. The
			// constraint-stripped twin differs from the probe in exactly one
			// attribute and native's coercer never reads that attribute, so the
			// twin's output IS what the constrained bundle used to serve. This is
			// also the row's no-over-decline control: removing the constraint must
			// restore both admission and the byte-identical served document.
			twin, removed := constraintStateWithoutConstraints(t, p.bundle)
			if removed == 0 {
				t.Fatalf("the probe declares no constraint; it cannot be about the target gate")
			}
			if err := checkSupported(twin); err != nil {
				t.Fatalf("stripped twin: checkSupported DECLINED (%v); removing the constraint must restore admission", err)
			}
			twinRes, terr := ParseStaticBundle(context.Background(), twin, p.raw)
			if terr != nil {
				t.Fatalf("stripped twin: ParseStaticBundle = %v; want it to SERVE %s", terr, p.wouldHaveServed)
			}
			if got := string(twinRes.JSON); got != p.wouldHaveServed {
				t.Fatalf("native serves %s for %s, not the recorded %s; the out-claim classification in "+
					"profileoracle's TestStockTargetLevelConstraintDispositionAndNativeDecline is derived "+
					"from this value and must be re-measured", got, p.raw, p.wouldHaveServed)
			}

			if p.outClaim {
				t.Logf("OUT-CLAIM REMOVED: native served %s; stock %s. The decline routes the call to BAML.",
					p.wouldHaveServed, p.stockDoes)
			} else {
				t.Logf("OVER-DECLINE (safe, not an out-claim fix): native served %s; stock %s.",
					p.wouldHaveServed, p.stockDoes)
			}
		})
	}

	// CONTRAST — the decline is about the CONSTRAINT, not about the target
	// position. The same target shapes with no constraint still admit and serve,
	// and the same constrained string one level down inside a class field declines
	// exactly as it always did.
	unconstrained := []struct {
		name       string
		bundle     *schema.Bundle
		raw        string
		wantServed string
	}{
		{"bare string target", constraintStateBundle(t, constraintStateStringType(), nil, nil), `"actual"`, `"actual"`},
		{"bare int target", constraintStateBundle(t, constraintStateIntType(), nil, nil), `5`, `5`},
		{"int list target", constraintStateBundle(t, schema.Type{
			Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateIntType()),
		}, nil, nil), `[1,2]`, `[1,2]`},
	}
	requireConstraintStateCount(t, len(unconstrained), 3, "unconstrained target controls")
	for _, u := range unconstrained {
		t.Run("still-served/"+u.name, func(t *testing.T) {
			if err := SupportsNativeFinalBundle(u.bundle); err != nil {
				t.Fatalf("SupportsNativeFinalBundle DECLINED an unconstrained target: %v", err)
			}
			res, err := ParseStaticBundle(context.Background(), u.bundle, u.raw)
			if err != nil {
				t.Fatalf("ParseStaticBundle = %v; an unconstrained target must still be served", err)
			}
			if got := string(res.JSON); got != u.wantServed {
				t.Errorf("served %s, want %s", got, u.wantServed)
			}
		})
	}

	nested := constraintStateBundle(t, constraintStateClassType("Wrapper"), []schema.ClassDef{
		constraintStateClass("Wrapper", constraintStateField("note", "",
			constraintStateAssert(constraintStateStringType(), "shape", `this == "expected"`))),
	}, nil)
	if err := checkSupported(nested); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("checkSupported on a constrained CLASS FIELD = %v; want the fallback sentinel — "+
			"the target walk must not have moved the field-level decline", err)
	}
	if err := SupportsNativeFinalBundle(nested); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeFinalBundle on a constrained CLASS FIELD = %v; want the fallback sentinel", err)
	}
	if _, err := ParseStaticBundle(context.Background(), nested, `{"note":"actual"}`); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("ParseStaticBundle on a constrained CLASS FIELD = %v; want the fallback sentinel", err)
	}
}

// TestConstraintStateEveryCollectorFixtureFamilyIsRefused pins that the
// collector's own fixture families — the bundles its state tests actually run —
// are all still refused by production, so no state result in this file can be
// read as admission moving.
//
// The bare-string RETURN family is included. It used to be the one family left
// out, because its constrained node cannot be nested (that IS the route under
// test) and the gate did not walk b.Target; now checkSupportedFields does, so the
// family is refused like every other and belongs in this list.
func TestConstraintStateEveryCollectorFixtureFamilyIsRefused(t *testing.T) {
	suit := schema.EnumDef{
		Name: schema.Name{Name: "Suit"},
		Values: []schema.EnumValue{
			{Name: schema.Name{Name: "Hearts", Alias: strPtrConstraintState("hearts_alias")}},
			{Name: schema.Name{Name: "Spades"}},
		},
	}
	hand := constraintStateClass("Hand",
		constraintStateField("suit", "", constraintStateCheck(schema.Type{Kind: schema.TypeEnum, Name: "Suit"}, "c", `this == "Hearts"`)),
	)
	order := constraintStateClass("Order", constraintStateField("amount", "qty", constraintStateIntType()))
	order.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this.amount == 3"}}
	bag := constraintStateClass("Bag", constraintStateField("nums", "",
		schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateCheck(constraintStateIntType(), "c", "this > 0"))}))
	board := constraintStateClass("Board", constraintStateField("scores", "", schema.Type{
		Kind:  schema.TypeMap,
		Key:   ptrConstraintStateType(constraintStateStringType()),
		Value: ptrConstraintStateType(constraintStateCheck(constraintStateIntType(), "c", "this > 0")),
	}))
	// A MULTI-field class and a list that DROPS an element. Neither adds an
	// assertion here — they exist so the collector's own divergence refusal has
	// something to refuse: a reversed field order or a kept-dropped element would
	// change the document, and only the wired [constraintStateJSONEquivalent]
	// check can notice inside a test that asserts nothing about order.
	pair := constraintStateClass("Pair",
		constraintStateField("first", "", constraintStateCheck(constraintStateIntType(), "c", "this > 0")),
		constraintStateField("second", "", constraintStateIntType()),
	)
	// A default-filled required field and an implied-key absorption, so the newly
	// modelled successful routes are inside the boundary lock too.
	defaulted := constraintStateClass("Sack",
		constraintStateField("name", "", constraintStateStringType()),
		constraintStateField("nums", "", constraintStateCheck(
			schema.Type{Kind: schema.TypeList, Elem: ptrConstraintStateType(constraintStateIntType())}, "c", "this|length == 0")),
	)
	note := constraintStateClass("Note",
		constraintStateField("text", "", constraintStateCheck(constraintStateStringType(), "c", `this == "x"`)))
	ledger := constraintStateClass("Ledger",
		constraintStateField("name", "", constraintStateStringType()),
		constraintStateField("tally", "", constraintStateCheck(schema.Type{
			Kind:  schema.TypeMap,
			Key:   ptrConstraintStateType(constraintStateStringType()),
			Value: ptrConstraintStateType(constraintStateIntType()),
		}, "c", "this|length == 0")),
	)

	fixtures := []struct {
		name   string
		bundle *schema.Bundle
		raw    string
	}{
		{"enum-alias-field", constraintStateBundle(t, constraintStateClassType("Hand"), []schema.ClassDef{hand}, []schema.EnumDef{suit}), `{"suit":"hearts_alias"}`},
		{"class-declaration-constraint", constraintStateBundle(t, constraintStateClassType("Order"), []schema.ClassDef{order}, nil), `{"qty":3}`},
		{"list-element-under-a-field", constraintStateBundle(t, constraintStateClassType("Bag"), []schema.ClassDef{bag}, nil), `{"nums":[1,2]}`},
		{"list-with-a-dropped-element", constraintStateBundle(t, constraintStateClassType("Bag"), []schema.ClassDef{bag}, nil), `{"nums":[1,"oops",3]}`},
		{"multi-field-class-in-reverse-input-order", constraintStateBundle(t, constraintStateClassType("Pair"), []schema.ClassDef{pair}, nil), `{"second":2,"first":1}`},
		{"map-value-under-a-field", constraintStateBundle(t, constraintStateClassType("Board"), []schema.ClassDef{board}, nil), `{"scores":{"a":1}}`},
		{"default-filled-required-field", constraintStateBundle(t, constraintStateClassType("Sack"), []schema.ClassDef{defaulted}, nil), `{"name":"x"}`},
		{"single-field-implied-key", constraintStateBundle(t, constraintStateClassType("Note"), []schema.ClassDef{note}, nil), `{"other":"x"}`},
		{"map-default-fill", constraintStateBundle(t, constraintStateClassType("Ledger"), []schema.ClassDef{ledger}, nil), `{"name":"x","tally":3}`},
		// The bare-string RETURN family, the fixture of
		// [TestConstraintStateBareStringReturnSkipsBothLevels] — declined since the
		// gate started walking b.Target.
		{"bare-string-return", constraintStateBundle(t, constraintStateAssert(
			constraintStateCheck(constraintStateStringType(), "nonempty", `this == "expected"`),
			"shape", `this == "also_expected"`), nil, nil), `"actual"`},
	}
	requireConstraintStateCount(t, len(fixtures), 10, "collector fixture families")
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			run, err := collectConstraintCoercionState(f.bundle, f.raw)
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			// The collector produced a real state...
			if !run.Root.HasCanonical {
				t.Fatalf("no canonical state produced")
			}
			// ...and production still refuses the very same bundle.
			requireConstraintStateStillDeclines(t, run)
		})
	}
}
