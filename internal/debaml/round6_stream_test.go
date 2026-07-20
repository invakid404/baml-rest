package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Round-6 PURE-GO regression coverage for the streaming-cadence fixes and the §11
// narrows. The EXPECTED values are BAML v0.223's live-captured outputs (recorded by the
// generative differential); this asserts native reproduces them WITHOUT needing live
// BAML, so the fixes are guarded even when the Docker-gated differential runs only in CI.
func TestRound6_StreamCadenceAndNarrows(t *testing.T) {
	str := func() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "string"} }
	intp := func() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "int"} }
	sc := func(kv ...bamlutils.OrderedEntry[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
		return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(kv...)}
	}
	part := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		j, e := ParseNativeStreamPartial(context.Background(), s, raw)
		if e != nil || j == nil {
			return "", false
		}
		return string(j), true
	}
	fin := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		j, e := ParseNativeStreamFinal(context.Background(), s, raw)
		if e != nil {
			return "", false
		}
		return string(j), true
	}
	ss := sc(bamlutils.OrderedKV("a", str()), bamlutils.OrderedKV("b", str()))
	na := sc(bamlutils.OrderedKV("name", str()), bamlutils.OrderedKV("age", intp()))

	// A NON-string number into a string target streams its serde Display in the PARTIAL
	// path (previously "JsonToString deferred" -> no-emit), byte-identical to the final.
	for _, mode := range []struct {
		name string
		fn   func(*bamlutils.DynamicOutputSchema, string) (string, bool)
	}{{"partial", part}, {"final", fin}} {
		got, ok := mode.fn(ss, `{"a":"x","b":5e0}`)
		if !ok || got != `{"a":"x","b":"5.0"}` {
			t.Errorf("num-in-string %s: got (%q,%v), want {\"a\":\"x\",\"b\":\"5.0\"}", mode.name, got, ok)
		}
	}
	// A LAST unquoted scalar with a trailing comma before the close emits in BOTH the
	// partial and final paths (the '}' trailing-comma exception in parseUnquotedScalar
	// AND parseUnquotedScalarStream).
	for _, mode := range []struct {
		name string
		fn   func(*bamlutils.DynamicOutputSchema, string) (string, bool)
	}{{"partial", part}, {"final", fin}} {
		got, ok := mode.fn(na, `{"name":"x","age":36,}`)
		if !ok || got != `{"name":"x","age":36}` {
			t.Errorf("trailing-comma %s: got (%q,%v), want {\"name\":\"x\",\"age\":36}", mode.name, got, ok)
		}
	}

	// §11 narrows: each disallowed shape declines pre-transport; int-last stays admitted.
	// #555 Slice 2: field @description ADMITS (doesn't change key matching). #583 teardown:
	// a NON-colliding field @alias now ADMITS too — native's rendered-name-only matcher is
	// byte-exact vs static BAML v0.223's alias-only jsonish coercer; only a fuzzy alias/canonical
	// rendered-name COLLISION stays declined (order-dependent BAML resolution unpinned, #583).
	dp := func(p *bamlutils.DynamicProperty) *bamlutils.DynamicOutputSchema {
		return sc(bamlutils.OrderedKV("name", str()), bamlutils.OrderedKV("v", p))
	}
	mapProp := &bamlutils.DynamicProperty{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "string"}}
	for _, c := range []struct {
		name string
		s    *bamlutils.DynamicOutputSchema
		want bool // true = expect DECLINE
	}{
		{"bool", dp(&bamlutils.DynamicProperty{Type: "bool"}), true},
		{"float", dp(&bamlutils.DynamicProperty{Type: "float"}), true},
		{"map", dp(mapProp), true},
		{"listbool", dp(&bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "bool"}}), true},
		{"field_alias", sc(bamlutils.OrderedKV("title", &bamlutils.DynamicProperty{Type: "string", Alias: "ÉTAT"}), bamlutils.OrderedKV("last", str())), false},
		{"field_alias_collision", sc(bamlutils.OrderedKV("a", &bamlutils.DynamicProperty{Type: "string", Alias: "Foo"}), bamlutils.OrderedKV("bb", &bamlutils.DynamicProperty{Type: "string", Alias: "foo"})), true},
		{"field_desc", sc(bamlutils.OrderedKV("title", &bamlutils.DynamicProperty{Type: "string", Description: "the title"}), bamlutils.OrderedKV("last", str())), false},
		{"str_int_admitted", na, false},
		{"str_liststr_admitted", sc(bamlutils.OrderedKV("name", str()), bamlutils.OrderedKV("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}})), false},
	} {
		err := SupportsNativeStream(c.s)
		declined := err != nil
		if declined != c.want {
			t.Errorf("shape %q: declined=%v want=%v (err=%v)", c.name, declined, c.want, err)
		}
		if declined && !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("shape %q: decline error is not the fallback sentinel: %v", c.name, err)
		}
	}
}
