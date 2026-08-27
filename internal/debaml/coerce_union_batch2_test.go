package debaml

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Batch 2 — union-residual slice. These tests pin the three changes and their
// mandatory positive proof matrix + negative/out-claim controls (scope §5): the
// optional-field class-cast score, the typed all-arms-failed union verdict at its
// three enclosing positions, and the proven-failing union list-element drop. Each
// asserts a fact that would REGRESS under the exact mutations scope §5 names.

// abcClassUnionSchema builds Root{u: A | C} with A carrying `aFields` and C
// carrying `cFields`, for the optional-field class-union tests.
func abcClassUnionSchema(aFields, cFields bamlutils.OrderedMap[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "C"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: aFields}),
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: cFields}),
		),
	}
}

// TestClassUnion_OptionalField_PresentAbsentNull pins tryCastClass's three optional
// cases inside a scored union (Class::try_cast, ir_ref/coerce_class.rs): a PRESENT
// optional recurses into the member, an EXPLICIT-null optional is a score-0 null, and
// an ABSENT optional is a typed null at OptionalDefaultFromNoValue (score 1). The C
// arm has a DIFFERENT key set, so A must win each time.
func TestClassUnion_OptionalField_PresentAbsentNull(t *testing.T) {
	// A{a int, b string?} | C{q int} — C's `q` never matches, so A is the only arm.
	s := abcClassUnionSchema(
		props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"}))),
		props(kv("q", intProp())),
	)
	// PRESENT: b recurses through tryCastArm's union dispatch → the string arm.
	mustParse(t, s, `{"u":{"a":1,"b":"x"}}`, `{"u":{"a":1,"b":"x"}}`)
	// EXPLICIT null: the nullable-union null fast path → score-0 null (a present value,
	// NOT OptionalDefaultFromNoValue).
	mustParse(t, s, `{"u":{"a":1,"b":null}}`, `{"u":{"a":1,"b":null}}`)
	// ABSENT: OptionalDefaultFromNoValue → typed null, score 1 — A still wins (C misses).
	mustParse(t, s, `{"u":{"a":1}}`, `{"u":{"a":1,"b":null}}`)
}

// TestClassUnion_OptionalField_ScoreObservable is the MUTATE-TO-DIVERGE control for
// the SCORE-0 regression (the no-match half is TestClassUnion_OptionalField_NoMatchWitness):
// it FAILS if OptionalDefaultFromNoValue is treated as score 0. A{a int, b string?} | C{a
// int} over {"a":1}: A's try_cast scores 1 (absent b), C's scores 0 (exact) — so C, the
// clean score-0 arm, is the union winner and native emits {"a":1}. If the absent optional
// were score 0, A (arm 0) would short-circuit as the first score-0 winner and native would
// emit {"a":1,"b":null} — a DIFFERENT byte output. (It does NOT catch the no-match
// mutation: with C's clean score-0 arm present, an absent optional that no-matched instead
// of scoring 1 still lets C win {"a":1} unchanged.) Order reversal and the index tiebreak
// are pinned too.
func TestClassUnion_OptionalField_ScoreObservable(t *testing.T) {
	// A(score 1) before C(score 0): the clean C wins on score.
	s := abcClassUnionSchema(
		props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"}))),
		props(kv("a", intProp())),
	)
	mustParse(t, s, `{"u":{"a":1}}`, `{"u":{"a":1}}`)

	// Order reversed: C{a int}(score 0) is now arm 0 — the clean arm still wins.
	sRev := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "C"}, {Ref: "A"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}),
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()))}),
		),
	}
	mustParse(t, sRev, `{"u":{"a":1}}`, `{"u":{"a":1}}`)

	// TIE: two arms whose ONLY score is a single absent optional (both score 1) — the
	// index tiebreak picks the lower-index arm, so its DISTINCT null field is emitted.
	sTie := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "Ap"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("x", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}),
			bamlutils.OrderedKV("Ap", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("y", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}),
		),
	}
	mustParse(t, sTie, `{"u":{"a":1}}`, `{"u":{"a":1,"x":null}}`)
}

// TestClassUnion_OptionalField_NoMatchWitness is the MUTATE-TO-DIVERGE control for the
// NO-MATCH regression: it FAILS if tryCastClass's absent-optional branch is replaced with
// an immediate strict no-match instead of the null-fill at OptionalDefaultFromNoValue
// (score 1). A{a int, b string?} | map<string,int> over {"a":1}: BOTH arms try_cast the
// object at score 1 in PHASE 1 — A fills the absent `b` (OptionalDefaultFromNoValue), the
// map arm does ObjectToMap — and pick_best breaks the score-1 tie by INDEX (class-vs-map
// hits no special case in array_helper.rs: neither is a JsonToString/FirstMatch scalar and
// they are not both classes), so the lower-index class arm A WINS and native emits
// {"a":1,"b":null}. If the absent optional no-matched instead, A would not try_cast at all,
// the map would be the SOLE phase-1 candidate, and native would emit {"a":1} — a DIFFERENT
// arm and output (and phase 2 never runs, so the lenient absent-optional fill cannot mask
// it). The absent-optional null-fill at score 1 is BAML v0.223's Class::try_cast behavior;
// verified by the frozen-corpus differential (class_union_arm_collection_class_field, whose
// absent optional BAML fills null at score 1) and, offline, by the mutation itself
// (applying the no-match branch to tryCastClass makes THIS test emit {"a":1} and fail).
func TestClassUnion_OptionalField_NoMatchWitness(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "A"},
				{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}),
		),
	}
	// A (class, score 1, index 0) beats the map (score 1, index 1) on the index tiebreak.
	mustParse(t, s, `{"u":{"a":1}}`, `{"u":{"a":1,"b":null}}`)
	// Reversed: map is index 0 now. BOTH arms still run — neither try_casts at score 0, so
	// tryCastUnion collects both (map score 1 via ObjectToMap, A score 1 via the absent
	// optional) and pickBest breaks the score-1 tie by INDEX: the map, at index 0, wins →
	// {"a":1}. This pins that the witness above depends on A being LOWER-index — it is the
	// phase-1 pick_best tie that carries the signal, not arm identity.
	sRev := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
				{Ref: "A"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}),
		),
	}
	mustParse(t, sRev, `{"u":{"a":1}}`, `{"u":{"a":1}}`)
}

// TestClassUnion_OptionalField_ClaimsAgainstScore1MapArm pins the absent optional
// competing against a NON-zero (map-bearing) class arm: A{a int, b string?} scores 1
// (absent b) and M{a int, m map<string,int>} scores 1 (its map is absent, filled {}
// at DefaultFromNoValue 100 — so M actually scores 100, losing to A). This exercises
// the "score-1 map/list-bearing class arm" row of the proof matrix.
func TestClassUnion_OptionalField_ClaimsAgainstScore1MapArm(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "M"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}),
			bamlutils.OrderedKV("M", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("m", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"})))}),
		),
	}
	// {"a":1}: A fills b=null (score 1); M default-fills m={} (DefaultFromNoValue 100).
	// A's lower score wins.
	mustParse(t, s, `{"u":{"a":1}}`, `{"u":{"a":1,"b":null}}`)
}

// TestScalarUnion_AllArmsFailed_OrderAndNullable pins the typed all-arms-failed
// verdict at the TOP-LEVEL/required position (a class field): a non-nullable
// non-defaultable union with every arm proven-failed CLAIMS the BAML error, in BOTH
// arm orders. A NULLABLE sibling is NEVER an all-arms-failed error (the null
// candidate always survives), so it is left byte-identical to before Batch 2: it
// claims null when the arms are ALREADY-proven, and safely DECLINES when an arm is
// only native-can't-prove (the upgrade is deliberately gated to non-nullable unions).
func TestScalarUnion_AllArmsFailed_OrderAndNullable(t *testing.T) {
	requireClaimedError(t, primUnion("int", "bool"), `{"u":"hello"}`)
	requireClaimedError(t, primUnion("bool", "int"), `{"u":"hello"}`) // order reversed

	// NULLABLE, arms ALREADY-proven: (1|2)? over "7" — both literal_int arms are a
	// proven value mismatch, so they are excluded and the surviving null candidate
	// wins. Native CLAIMS null (NOT an error) — a nullable union has no all-arms-failed
	// verdict.
	nullableProven := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(1)},
		&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(2)},
		&bamlutils.DynamicTypeSpec{Type: "null"},
	)
	mustParse(t, nullableProven, `{"u":"7"}`, `{"u":null}`)

	// NULLABLE, arm native-can't-prove: (int|bool)? over "hello" — the int arm is a
	// native decline the upgrade does NOT touch for a nullable union, so it stays
	// indeterminate and native safely DECLINES (BAML returns null; a parity-decline).
	// This pins that the all-arms-failed CLAIM never fires for a nullable union.
	nullableIndeterminate := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "int"},
		&bamlutils.DynamicTypeSpec{Type: "bool"},
		&bamlutils.DynamicTypeSpec{Type: "null"},
	)
	requireUnsupported(t, nullableIndeterminate, `{"u":"hello"}`)
}

// TestListUnion_ProvenElementSkip_Position pins that a proven-failing union list
// element is DROPPED (ArrayItemParseError) at first / middle / last position while
// good siblings are preserved — coerceListChild consuming the typed union verdict.
// The element is `null` into string|map<string,int> (both arms reject null; the union
// is defaultable via the map's {}, so nothing here rescues it and BAML skips it).
func TestListUnion_ProvenElementSkip_Position(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "list",
		Items: &bamlutils.DynamicTypeSpec{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "string"},
				{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
			},
		},
	})
	mustParse(t, s, `{"u":[null]}`, `{"u":[]}`)                       // lone element dropped
	mustParse(t, s, `{"u":[null,"keep"]}`, `{"u":["keep"]}`)          // first dropped
	mustParse(t, s, `{"u":["a",null,{"k":1}]}`, `{"u":["a",{"k":1}]}`) // middle dropped
	mustParse(t, s, `{"u":["a",{"k":1},null]}`, `{"u":["a",{"k":1}]}`) // last dropped
}

// TestListUnion_NullSurvivingArmStaysFallback is the NEGATIVE control for the
// list-element drop: a union whose null-input error is NOT proven — string|list<int>
// over [null] — must DECLINE the whole list, not skip the element. The list arm
// SURVIVES the null (coerce_array wraps it as [] via SingleToArray), so BAML KEEPS the
// element as [[]] rather than skipping it; native cannot prove a skip and declines.
func TestListUnion_NullSurvivingArmStaysFallback(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "list",
		Items: &bamlutils.DynamicTypeSpec{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "string"},
				{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}},
			},
		},
	})
	requireUnsupported(t, s, `{"u":[null]}`)
}

// TestListUnion_IndeterminateElementStaysFallback is the second NEGATIVE control: a
// union list element native cannot PROVE fails (an "inf" string against int|float —
// BAML saturates, so it is NOT a proven parse error) must decline the whole list, not
// be skipped. This guards coerceListChild's union drop from consuming an indeterminate
// verdict.
func TestListUnion_IndeterminateElementStaysFallback(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "list",
		Items: &bamlutils.DynamicTypeSpec{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "float"}},
		},
	})
	requireUnsupported(t, s, `{"u":["inf"]}`)
}

// TestClassUnion_OptionalMemberOutOfFamilyDeclines pins the gate's NARROW admission:
// only a SINGLE-non-null optional whose member is itself in the union-arm family is
// admitted. A multi-arm optional, an optional nested-class, and an optional collection
// that leads back to a recursive class all still DECLINE at checkUnionClassField.
func TestClassUnion_OptionalMemberOutOfFamilyDeclines(t *testing.T) {
	// Optional MULTI-arm union field: (int|string)? is a nullable union with TWO
	// non-null variants — not a single-non-null wrapper.
	multi := abcClassUnionSchema(
		props(kv("a", intProp()), kv("b", &bamlutils.DynamicProperty{
			Type:  "optional",
			Inner: &bamlutils.DynamicTypeSpec{Type: "union", OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "string"}}},
		})),
		props(kv("q", intProp())),
	)
	requireUnsupported(t, multi, `{"u":{"a":1}}`)

	// Optional NESTED-CLASS field: D? — the member is a class, not a flat leaf /
	// collection, so it declines even though it is a single-non-null wrapper.
	nested := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "C"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Ref: "D"})))}),
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("q", intProp()))}),
			bamlutils.OrderedKV("D", &bamlutils.DynamicClass{Properties: props(kv("v", intProp()))}),
		),
	}
	requireUnsupported(t, nested, `{"u":{"a":1,"b":{"v":2}}}`)

	// Optional recursive cycle: A{items list<A>?} — the optional member is a list of A,
	// which the collection walk holds to the union-arm rules and the cycle guard
	// declines (A already on the walk).
	cyc := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "C"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("items", &bamlutils.DynamicProperty{
				Type:  "optional",
				Inner: &bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Ref: "A"}},
			}))}),
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("q", intProp()))}),
		),
	}
	requireUnsupported(t, cyc, `{"u":{"items":[]}}`)
}

// TestAliasUnion_NilContextEquivalence pins the change-4.3 nil-safety hardening: a
// nil *coerceCtx (a leaf/unit-probe caller) and an EMPTY &coerceCtx{} must produce
// byte-for-byte identical results from tryCastAliasUnion / coerceAliasUnion and must
// NOT panic. Before the fix the raw cctx.hint read panicked on a nil context; this is
// a ZERO-gain guard (a non-nil context is unchanged), so the two must be equal.
func TestAliasUnion_NilContextEquivalence(t *testing.T) {
	b := jsonAliasBundle(t)
	prof, ok := admittedRecursiveAliasProfile(b)
	if !ok {
		t.Fatal("JSON alias profile not admitted")
	}
	variants, err := aliasVariants(b, prof)
	if err != nil {
		t.Fatalf("aliasVariants: %v", err)
	}
	inputs := []value{
		numV("1"),
		strVv("x"),
		value{kind: valBool, boolV: true},
		arrVal(numV("1"), strVv("y")),
		objVal(fld("k", numV("2"))),
		nullVal(),
	}
	for _, in := range inputs {
		// tryCastAliasUnion: nil vs empty context must agree exactly.
		av0, f0, arm0, ok0, e0 := tryCastAliasUnion(b, prof, variants, in, nil)
		av1, f1, arm1, ok1, e1 := tryCastAliasUnion(b, prof, variants, in, &coerceCtx{})
		if !reflect.DeepEqual(av0, av1) || arm0 != arm1 || ok0 != ok1 || (e0 == nil) != (e1 == nil) || !reflect.DeepEqual(f0, f1) {
			t.Errorf("tryCastAliasUnion nil vs empty diverged for %v: (%v,%v,%v,%v) vs (%v,%v,%v,%v)", in.kind, av0, arm0, ok0, e0, av1, arm1, ok1, e1)
		}
		// coerceAliasUnion: nil vs empty context must agree exactly.
		cv0, cf0, carm0, ce0 := coerceAliasUnion(b, prof, variants, in, nil)
		cv1, cf1, carm1, ce1 := coerceAliasUnion(b, prof, variants, in, &coerceCtx{})
		if !reflect.DeepEqual(cv0, cv1) || carm0 != carm1 || (ce0 == nil) != (ce1 == nil) || !reflect.DeepEqual(cf0, cf1) {
			t.Errorf("coerceAliasUnion nil vs empty diverged for %v: (%v,%v,%v) vs (%v,%v,%v)", in.kind, cv0, carm0, ce0, cv1, carm1, ce1)
		}
	}
}
