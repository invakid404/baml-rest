package debaml

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// TestPairGuard_CoerceCircularReference proves native's Class::coerce pair-guard: a
// one-field Loop{next Loop?} fed an object with NO matching key drives BAML's
// implied-key recursion (the whole object is re-fed into `next` → Loop → the SAME
// (ClassKey, value) pair), which native detects as a CIRCULAR REFERENCE and returns
// a CLAIMED parse error — NOT the ErrDeBAMLParseUnsupported fallback sentinel — with
// no depth cap, matching BAML's circular-reference parsing error. This is the direct
// native-coerce leg of the pair-guard regression; the route leg (Loop declines
// pre-claim) is TestParseStaticBundle_LoopDeclines + the admission fingerprint.
func TestPairGuard_CoerceCircularReference(t *testing.T) {
	b, err := schema.FromStaticDescriptor(loopDescriptor())
	if err != nil {
		t.Skipf("Loop descriptor rejected at lowering (also a valid decline): %v", err)
	}
	// A bare scalar string → single-field InferedObject: the whole value is re-fed into
	// the lone `next Loop?` field, re-entering Loop with the SAME value. Stock BAML
	// v0.223 Parse.StaticRecursiveLoop circular-references on this same input (proven in
	// the static_oracle differential's pair-guard row), so the two legs agree.
	input := strVv("x")
	_, cerr := coerce(b, b.Target, input, nil, &coerceCtx{})
	if cerr == nil {
		t.Fatal("expected a circular-reference parse error, got nil (guard did not fire)")
	}
	if errors.Is(cerr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("circular reference must be a CLAIMED error, not the fallback sentinel: %v", cerr)
	}
	if !strings.Contains(cerr.Error(), "circular reference") {
		t.Fatalf("expected a circular-reference error, got: %v", cerr)
	}
}

// TestPairGuard_TryCastSeparateSet proves the try_cast active set is SEPARATE from
// the coerce set: enterCoerce records a pair on the coerce chain only, and
// enterTryCast on the try_cast chain only, so a class on the coerce path never
// blocks the same (ClassKey, value) on a try_cast path and vice-versa.
func TestPairGuard_TryCastSeparateSet(t *testing.T) {
	k := schema.ClassKey{Name: "Loop", Mode: schema.NonStreaming}
	v := objVal(fld("foo", strVv("bar")))
	var base *coerceCtx // nil == empty
	c := base.enterCoerce(k, v)
	if !c.coerceHas(k, v) {
		t.Fatal("coerce chain must contain the entered pair")
	}
	if c.tryCastHas(k, v) {
		t.Fatal("try_cast chain must NOT see a coerce-set pair (separate sets)")
	}
	tc := c.enterTryCast(k, v)
	if !tc.tryCastHas(k, v) {
		t.Fatal("try_cast chain must contain the entered pair")
	}
	// The coerce pair is still visible (inherited), the try_cast pair is new.
	if !tc.coerceHas(k, v) {
		t.Fatal("try_cast child must still inherit the ancestor coerce pair")
	}
}

// rootTwoClassFieldsDescriptor is a NON-recursive DAG: Root{left C; right C} with
// C{v string}. It is the path-locality witness — the same class C appears in two
// SIBLING subtrees with structurally-EQUAL values.
func rootTwoClassFieldsDescriptor() schemadescriptor.Bundle {
	classRef := func(n string) schemadescriptor.Type {
		return schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: n, Mode: schemadescriptor.NonStreaming}
	}
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "Root",
		Target:  classRef("Root"),
		Classes: []schemadescriptor.ClassDef{
			{
				Name: schemadescriptor.Name{Name: "Root"}, Mode: schemadescriptor.NonStreaming,
				Fields: []schemadescriptor.ClassField{
					{Name: schemadescriptor.Name{Name: "left"}, Type: classRef("C")},
					{Name: schemadescriptor.Name{Name: "right"}, Type: classRef("C")},
				},
			},
			{
				Name: schemadescriptor.Name{Name: "C"}, Mode: schemadescriptor.NonStreaming,
				Fields: []schemadescriptor.ClassField{
					{Name: schemadescriptor.Name{Name: "v"}, Type: sdString()},
				},
			},
		},
	}
}

// TestPairGuard_PathLocalSiblings proves the guard is PATH-LOCAL, not a global mutable
// map: two sibling fields carrying the SAME (ClassKey, value) both coerce cleanly —
// the right sibling derives its context from the parent, NOT from the left sibling,
// so left's pair never poisons right. A global/shared set would false-positive the
// second sibling as a circular reference.
func TestPairGuard_PathLocalSiblings(t *testing.T) {
	b := lowerOrFatal(t, rootTwoClassFieldsDescriptor())
	raw := `{"left":{"v":"x"},"right":{"v":"x"}}`
	res, err := ParseStaticBundle(context.Background(), b, raw)
	if err != nil {
		t.Fatalf("sibling classes with equal values must both coerce (path-local guard), got: %v", err)
	}
	want := `{"left":{"v":"x"},"right":{"v":"x"}}`
	if string(res.JSON) != want {
		t.Fatalf("path-local sibling JSON mismatch:\n got: %s\nwant: %s", res.JSON, want)
	}
}

// TestPairGuard_NoDepthCap proves the admitted recursive family coerces a DEEP finite
// chain (N=32) with no native depth cap: the pair-guard never fires because each edge
// receives a proper finite subtree that is structurally distinct from its ancestors.
func TestPairGuard_NoDepthCap(t *testing.T) {
	b := lowerOrFatal(t, nodeDescriptor())
	// Build depth-32 explicit-null-terminated Node chain.
	const depth = 32
	raw := "null"
	for i := depth; i >= 1; i-- {
		raw = `{"value":"n` + strconv.Itoa(i) + `","next":` + raw + `}`
	}
	res, err := ParseStaticBundle(context.Background(), b, raw)
	if err != nil {
		t.Fatalf("deep finite recursive chain must coerce with no depth cap, got: %v", err)
	}
	if string(res.JSON) != raw {
		t.Fatalf("deep chain JSON mismatch:\n got: %s\nwant: %s", res.JSON, raw)
	}
}
