package codegen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/bamlutils"
)

// TestParseReflectType_OrderedMapNamedGeneric pins the contract D12
// describes: parseReflectType handles the named generic
// `bamlutils.OrderedMap[T]` shape that the issue #366 static-map pass
// surfaces to generated code. The output must compose into Go that
// renders the qualified generic without losing the type parameter.
//
// Pinning this here protects against future drift in parseReflectType's
// generic handling that would otherwise only surface when the
// dynclient regen runs against a schema with a concrete static map.
func TestParseReflectType_OrderedMapNamedGeneric(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "OrderedMap[string]",
			typ:  reflect.TypeOf(bamlutils.OrderedMap[string]{}),
			want: []string{"OrderedMap", "string"},
		},
		{
			name: "OrderedMap[int]",
			typ:  reflect.TypeOf(bamlutils.OrderedMap[int]{}),
			want: []string{"OrderedMap", "int"},
		},
		{
			name: "pointer to OrderedMap[string]",
			typ:  reflect.TypeOf((*bamlutils.OrderedMap[string])(nil)),
			want: []string{"*", "OrderedMap", "string"},
		},
		{
			name: "slice of OrderedMap[string]",
			typ:  reflect.TypeOf([]bamlutils.OrderedMap[string]{}),
			want: []string{"[]", "OrderedMap", "string"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseReflectType(tc.typ)
			if parsed.statement == nil {
				t.Fatalf("parseReflectType returned nil statement for %v", tc.typ)
			}
			rendered := jen.NewFile("scratch").Add(parsed.statement).GoString()
			// jen renders the statement embedded in a file; the actual
			// type expression appears verbatim. Each expected token
			// must show up so generic args are not silently dropped.
			for _, token := range tc.want {
				if !strings.Contains(rendered, token) {
					t.Errorf("parseReflectType(%v) rendered %q; missing token %q", tc.typ, rendered, token)
				}
			}
			// Generic args must be captured in the parsedReflectType
			// struct as a sibling slice — call sites read them to drive
			// .Interface().(T) assertions on the inner type.
			if len(parsed.generics) == 0 {
				t.Errorf("parseReflectType(%v) returned empty generics; expected at least 1", tc.typ)
			}
		})
	}
}
