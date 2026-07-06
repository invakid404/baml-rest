package debaml

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// TestDeclinableChildError pins the seam-contract classification: only the
// ErrDeBAMLParseUnsupported fallback sentinel and a value-verdict mismatchError
// are DECLINABLE; every other error is a HARD failure that must propagate.
func TestDeclinableChildError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sentinel-unsupported", unsupported("x"), true},
		{"sentinel-declineCoerce", declineCoerce("enum target", value{kind: valNumber}), true},
		{"sentinel-wrapped", fmt.Errorf("outer: %w", unsupported("inner")), true},
		{"verdict-typeMismatch", typeMismatch("object", value{kind: valString}), true},
		{"verdict-ambiguous", ambiguousMatch("enum X", "cat dog"), true},
		{"verdict-wrapped", fmt.Errorf("outer: %w", typeMismatch("object", value{kind: valString})), true},
		{"hard-unknown-enum", fmt.Errorf("debaml: unknown enum %q", "Ghost"), false},
		{"hard-plain", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := declinableChildError(c.err); got != c.want {
			t.Errorf("%s: declinableChildError(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// TestWrappersPropagateHardErrors proves CR1: a HARD/invariant child error (an
// unknown enum/class ref, a missing literal payload — none reachable through a
// VALIDATED schema, hence exercised directly) PROPAGATES through every
// container/union child-wrapper instead of being masked as the
// ErrDeBAMLParseUnsupported fallback sentinel.
func TestWrappersPropagateHardErrors(t *testing.T) {
	// An empty bundle: FindEnum/FindClass always miss -> coerceEnum/coerceClass
	// return their "unknown ..." hard errors.
	b := &schema.Bundle{}
	strKey := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	ghostEnum := schema.Type{Kind: schema.TypeEnum, Name: "Ghost"}
	ghostClass := schema.Type{Kind: schema.TypeClass, Name: "Ghost"}
	obj := value{kind: valObject, objV: []field{{key: "k", val: value{kind: valString, strV: "v"}}}}
	arr := value{kind: valArray, arrV: []value{{kind: valString, strV: "x"}}}

	run := func(name, wantSubstr string, fn func() (interface{}, error)) {
		_, err := fn()
		if err == nil {
			t.Errorf("%s: expected propagated hard error, got nil", name)
			return
		}
		if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: hard error MASKED as fallback sentinel: %v", name, err)
			return
		}
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("%s: expected propagated %q, got %v", name, wantSubstr, err)
		}
	}

	run("coerceList", "unknown enum", func() (interface{}, error) {
		return coerceList(b, &ghostEnum, arr, nil)
	})
	run("coerceMap-value", "unknown enum", func() (interface{}, error) {
		return coerceMap(b, &strKey, &ghostEnum, obj, nil)
	})
	run("coerceUnionSafe-optional-arm", "unknown enum", func() (interface{}, error) {
		u := &schema.UnionType{Variants: []schema.Type{ghostEnum}, Nullable: true}
		return coerceUnionSafe(b, u, value{kind: valString, strV: "x"}, nil)
	})
	run("coerceFlatClassUnion-counting", "unknown class", func() (interface{}, error) {
		// Bypasses checkSupportedUnionShape (a unit-level wrapper probe): the
		// counting loop hits FindClass and must propagate the hard error.
		return coerceFlatClassUnion(b, []schema.Type{ghostClass, ghostClass}, obj, false, nil)
	})
	run("coerceScalarLeafUnion-counting", "literal type missing value", func() (interface{}, error) {
		// A literal variant with a nil payload is a hard invariant failure surfaced
		// by phase-1 tryCastScalarArm before the scored coerce loop runs.
		bad := schema.Type{Kind: schema.TypeLiteral, Literal: nil}
		return coerceScalarLeafUnion(b, []schema.Type{bad, bad}, value{kind: valString, strV: "x"}, false, nil)
	})
}

// strLitType builds a string-literal schema type.
func strLitType(s string) schema.Type {
	return schema.Type{Kind: schema.TypeLiteral, Literal: &schema.LiteralValue{Kind: schema.LiteralString, String: s}}
}

// litUnionType builds a non-nullable union of the given string literals.
func litUnionType(lits ...string) schema.Type {
	vs := make([]schema.Type, len(lits))
	for i, l := range lits {
		vs[i] = strLitType(l)
	}
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: vs}}
}

// TestCoerceMapKey covers every coerceMapKey branch (string primitive,
// string-literal, string-literal-union, enum), pinning the TWO-way outcome:
//
//   - ACCEPT: err=nil — a clean match_string match (any arm, for a union), or any
//     string-primitive key.
//   - DECLINE the whole map: err=ErrDeBAMLParseUnsupported — a case-fold-UNCERTAIN
//     verdict (marks cf.uncertain, nil-safely), the mixed-union guard, or a
//     certain MISS. A KEY never yields a partial skip: the dynamic bridge keeps
//     non-matching enum/literal/literal-union keys leniently (full map), so a
//     miss is a DEFERRED Mcoerce-d keep and native declines the whole map.
//
// It also pins the string-literal-union ACCEPT-ANY-ARM rule (a clean arm accepts
// even if an EARLIER arm was uncertain). The runes are 'é'(U+00E9, IsLower ->
// certain) and 'É'(U+00C9, not IsLower -> uncertain once it reaches the case-fold
// attempt).
func TestCoerceMapKey(t *testing.T) {
	// An indexed bundle with an (ASCII) enum key type — enum uncertainty is
	// driven by the non-ASCII INPUT key, so the enum values stay ASCII.
	enumB, err := schema.FromDynamicOutputSchema(mapEnumKeySchema(), schema.BuildOptions{})
	if err != nil {
		t.Fatalf("build enum bundle: %v", err)
	}
	enumKey := schema.Type{Kind: schema.TypeEnum, Name: enumB.Enums[0].Name.Name}
	strKey := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}

	mixedUnion := schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{
		strLitType("é"),
		{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt},
	}}}

	cases := []struct {
		name          string
		b             *schema.Bundle
		keyT          schema.Type
		key           string
		wantAccept    bool // true = nil (accepted), false = whole-map decline (err)
		wantUncertain bool
	}{
		// String primitive: any key coerces -> ACCEPT.
		{"string-any", nil, strKey, "whatever", true, false},
		// String literal: exact -> ACCEPT; certain miss -> DECLINE (dynamic keeps);
		// uncertain case fold -> DECLINE + mark.
		{"literal-exact", nil, strLitType("é"), "é", true, false},
		{"literal-certain-miss-decline", nil, strLitType("abc"), "xyz", false, false},
		{"literal-uncertain-decline", nil, strLitType("é"), "É", false, true},
		// String-literal union.
		{"union-normal-accept", nil, litUnionType("é", "beta"), "beta", true, false},
		{"union-certain-miss-decline", nil, litUnionType("abc", "def"), "xyz", false, false},
		// UNCERTAIN-ONLY match: key "É" matches literal "é" only via the case-fold
		// pass (matchString returns matchOne AND uncertain) -> DECLINE + mark, NOT
		// accept.
		{"union-uncertain-only-match", nil, litUnionType("é"), "É", false, true},
		// No arm cleanly matches, but an arm's verdict was uncertain -> DECLINE.
		{"union-uncertain-no-clean", nil, litUnionType("abc", "def"), "É", false, true},
		// ACCEPT-ANY-ARM rescue: arm "abc" is uncertain for "É", but arm "É"
		// matches EXACTLY (before the case-fold attempt, so certain) -> accept,
		// and DON'T mark uncertain.
		{"union-accept-any-arm", nil, litUnionType("abc", "É"), "É", true, false},
		// Mixed union (string literal | int): declined by the guard, no scan.
		{"mixed-union-guard", nil, mixedUnion, "É", false, false},
		// Enum: exact -> ACCEPT; a MISS (dynamic keeps non-members) -> DECLINE the
		// whole map; uncertain -> DECLINE + mark.
		{"enum-exact", enumB, enumKey, "A", true, false},
		{"enum-miss-decline", enumB, enumKey, "ZZZ", false, false},
		{"enum-uncertain-decline", enumB, enumKey, "É", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cf := &coerceFlags{}
			err := coerceMapKey(c.b, c.keyT, c.key, cf)
			if c.wantAccept {
				if err != nil {
					t.Fatalf("want accept (nil), got %v", err)
				}
			} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("want ErrDeBAMLParseUnsupported decline, got %v", err)
			}
			if cf.isUncertain() != c.wantUncertain {
				t.Errorf("cf.uncertain = %v, want %v", cf.isUncertain(), c.wantUncertain)
			}
		})
	}

	// Nil-safe: an uncertain key with a nil accumulator must not panic.
	if err := coerceMapKey(nil, strLitType("é"), "É", nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil cf: want ErrDeBAMLParseUnsupported, got %v", err)
	}
}
