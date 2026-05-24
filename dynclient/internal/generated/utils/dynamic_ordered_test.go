package utils

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// fakeOrderedFields mirrors the same package-local stand-in used in
// adapters/common/utils. The package builds against the patched BAML
// fork in-repo, so serde.OrderedFields exists; the structural fake
// nevertheless keeps the test independent of the fork's exact
// identity and pins the interface contract.
type fakeOrderedFields struct {
	keys []string
	vals map[string]any
}

func newFakeOrderedFields(entries ...[2]any) *fakeOrderedFields {
	f := &fakeOrderedFields{vals: map[string]any{}}
	for _, e := range entries {
		k := e[0].(string)
		f.keys = append(f.keys, k)
		f.vals[k] = e[1]
	}
	return f
}

func (f *fakeOrderedFields) Len() int { return len(f.keys) }
func (f *fakeOrderedFields) Range(fn func(string, any) bool) {
	for _, k := range f.keys {
		if !fn(k, f.vals[k]) {
			return
		}
	}
}

func TestUnwrapDynamicValue_StructuralOrdered_DynclientMirror(t *testing.T) {
	value := newFakeOrderedFields(
		[2]any{"zulu", "z"},
		[2]any{"alpha", "a"},
	)
	got := UnwrapDynamicValue(value)
	out, ok := got.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("expected OrderedMap[any], got %T", got)
	}
	if want := []string{"zulu", "alpha"}; !reflect.DeepEqual(out.Keys(), want) {
		t.Fatalf("keys mismatch\n got: %v\nwant: %v", out.Keys(), want)
	}
}
