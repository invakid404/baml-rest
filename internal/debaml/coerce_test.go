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
	run("coerceLiteralUnion-counting", "literal type missing value", func() (interface{}, error) {
		// A literal variant with a nil payload is a hard invariant failure.
		bad := schema.Type{Kind: schema.TypeLiteral, Literal: nil}
		return coerceLiteralUnion([]schema.Type{bad, bad}, value{kind: valString, strV: "x"}, false, nil)
	})
}

// TestMatchMapKeyUncertaintyMarksFlags pins the CR-MAPKEY signal-propagation
// fix: a non-ASCII case-fold-uncertain map key DECLINES the map AND marks
// cf.uncertain (so a future union counter that admits a map arm would decline
// the whole union), and the *coerceFlags parameter is nil-safe.
func TestMatchMapKeyUncertaintyMarksFlags(t *testing.T) {
	// String-literal map key "é"(U+00E9): the input key "É"(U+00C9) only matches
	// via the uncertain case fold (NFKD leaves the accent, so it is not
	// accent-fold-connected), and 'É' is non-ASCII and not IsLower -> uncertain.
	litKey := schema.Type{
		Kind:    schema.TypeLiteral,
		Literal: &schema.LiteralValue{Kind: schema.LiteralString, String: "é"},
	}

	cf := &coerceFlags{}
	if err := matchMapKey(nil, litKey, "É", cf); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("uncertain map key: want ErrDeBAMLParseUnsupported, got %v", err)
	}
	if !cf.isUncertain() {
		t.Error("uncertain map key did not mark cf.uncertain")
	}

	// Nil-safe: the same uncertain key with a nil accumulator must not panic.
	if err := matchMapKey(nil, litKey, "É", nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil cf: want ErrDeBAMLParseUnsupported, got %v", err)
	}

	// A CERTAIN (exact) key accepts and leaves cf.uncertain unset.
	clean := &coerceFlags{}
	if err := matchMapKey(nil, litKey, "é", clean); err != nil {
		t.Fatalf("exact key: want accept (nil), got %v", err)
	}
	if clean.isUncertain() {
		t.Error("exact key wrongly marked cf.uncertain")
	}
}

// TestWrapperDeclinesValueVerdict confirms the no-regression side of CR1: a
// VALUE-verdict child error (here typeMismatch — a scalar where a multi-field
// class is required) still makes the wrapper DECLINE (fall back), so BAML's
// partial-list behavior is deferred, not claimed.
func TestWrapperDeclinesValueVerdict(t *testing.T) {
	// Root{ items: Pair[] }, Pair{ a, b }. A non-object list element makes
	// coerceClass return typeMismatch (value-verdict) -> coerceList declines.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("items", &bamlutils.DynamicProperty{
			Type:  "list",
			Items: &bamlutils.DynamicTypeSpec{Ref: "Pair"},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Pair", &bamlutils.DynamicClass{
				Properties: props(kv("a", strProp()), kv("b", strProp())),
			}),
		),
	}
	requireUnsupported(t, s, `{"items":[5]}`)
}
