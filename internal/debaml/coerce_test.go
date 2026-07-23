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
		return coerceList(b, &ghostEnum, arr, nil, nil)
	})
	run("coerceMap-value", "unknown enum", func() (interface{}, error) {
		return coerceMap(b, &strKey, &ghostEnum, obj, nil, nil)
	})
	run("coerceUnionSafe-optional-arm", "unknown enum", func() (interface{}, error) {
		u := &schema.UnionType{Variants: []schema.Type{ghostEnum}, Nullable: true}
		return coerceUnionSafe(b, u, value{kind: valString, strV: "x"}, nil, nil)
	})
	run("coerceUnionSafeMulti-class-counting", "unknown class", func() (interface{}, error) {
		// Bypasses checkSupportedUnionShape (a unit-level wrapper probe): phase-1
		// tryCastUnion → tryCastClass hits FindClass and must propagate the hard error.
		return coerceUnionSafeMulti(b, []schema.Type{ghostClass, ghostClass}, obj, false, nil, nil)
	})
	run("coerceUnionSafeMulti-literal-counting", "literal type missing value", func() (interface{}, error) {
		// A literal variant with a nil payload is a hard invariant failure surfaced
		// by phase-1 tryCastArm before the scored coerce loop runs.
		bad := schema.Type{Kind: schema.TypeLiteral, Literal: nil}
		return coerceUnionSafeMulti(b, []schema.Type{bad, bad}, value{kind: valString, strV: "x"}, false, nil, nil)
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
//   - DECLINE the whole map: err=ErrDeBAMLParseUnsupported — a certain MISS. A KEY
//     never yields a partial skip: the dynamic bridge keeps non-matching
//     enum/literal/literal-union keys leniently (full map), so a miss is a
//     DEFERRED Mcoerce-d keep and native declines the whole map.
//
// #555 Slice 2: the match_string fold is now proven (bamlunicode == Rust
// str::to_lowercase), so a non-ASCII key that folds to a candidate CLAIMS the
// match — the runes 'é'(U+00E9) and 'É'(U+00C9) fold together, so key "É" now
// ACCEPTS literal "é" (was a case-fold-uncertain DECLINE). coerceMapKey marks NO
// uncertainty, so cf.isUncertain() is always false here.
func TestCoerceMapKey(t *testing.T) {
	// An indexed bundle with an (ASCII) enum key type — the non-ASCII INPUT key
	// exercises the fold against ASCII enum values.
	enumB, err := schema.FromDynamicOutputSchema(mapEnumKeySchema(), schema.BuildOptions{})
	if err != nil {
		t.Fatalf("build enum bundle: %v", err)
	}
	enumKey := schema.Type{Kind: schema.TypeEnum, Name: enumB.Enums[0].Name.Name}
	strKey := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}

	mixedUnion := schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{
		strLitType("\u00e9"),
		{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt},
	}}}

	cases := []struct {
		name       string
		b          *schema.Bundle
		keyT       schema.Type
		key        string
		wantAccept bool // true = nil (accepted), false = whole-map decline (err)
	}{
		// String primitive: any key coerces -> ACCEPT.
		{"string-any", nil, strKey, "whatever", true},
		// String literal: exact -> ACCEPT; certain miss -> DECLINE (dynamic keeps).
		{"literal-exact", nil, strLitType("\u00e9"), "\u00e9", true},
		{"literal-certain-miss-decline", nil, strLitType("abc"), "xyz", false},
		// #555 Slice 2: key "É" folds to "é" (proven) and now CLAIMS the match.
		{"literal-nonascii-casefold-now-accept", nil, strLitType("\u00e9"), "\u00c9", true},
		// String-literal union.
		{"union-normal-accept", nil, litUnionType("\u00e9", "beta"), "beta", true},
		{"union-certain-miss-decline", nil, litUnionType("abc", "def"), "xyz", false},
		// #555 Slice 2: the single non-ASCII arm now folds and ACCEPTS (was decline).
		{"union-nonascii-casefold-now-accept", nil, litUnionType("\u00e9"), "\u00c9", true},
		// "É" folds to "é" which matches NO ascii arm -> certain miss -> DECLINE.
		{"union-nonascii-no-arm-decline", nil, litUnionType("abc", "def"), "\u00c9", false},
		// A later EXACT arm accepts regardless (unchanged).
		{"union-accept-any-arm", nil, litUnionType("abc", "\u00c9"), "\u00c9", true},
		// Mixed union (string literal | int): declined by the guard, no scan.
		{"mixed-union-guard", nil, mixedUnion, "\u00c9", false},
		// Enum: exact -> ACCEPT; a MISS (dynamic keeps non-members) -> DECLINE.
		{"enum-exact", enumB, enumKey, "A", true},
		{"enum-miss-decline", enumB, enumKey, "ZZZ", false},
		// #555 Slice 2: "É" folds to "é" (proven), != the ASCII enum values -> a
		// CERTAIN miss -> DECLINE the whole map (dynamic keeps non-members).
		{"enum-nonascii-certain-miss-decline", enumB, enumKey, "\u00c9", false},
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
			// coerceMapKey marks no uncertainty after #555 Slice 2 (the fold is proven).
			if cf.isUncertain() {
				t.Errorf("cf.isUncertain() = true, want false (map-key coercion marks no uncertainty)")
			}
		})
	}

	// Nil-safe: a non-ASCII key with a nil accumulator must not panic; key "É"
	// folds to literal "é" and ACCEPTS.
	if err := coerceMapKey(nil, strLitType("\u00e9"), "\u00c9", nil); err != nil {
		t.Fatalf("nil cf: want accept (nil), got %v", err)
	}
}
