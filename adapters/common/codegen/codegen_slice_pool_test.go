package codegen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/bamlutils"
)

// testContentPartMediaInput mimics the dynclient's
// Baml_Rest_ContentPartMediaInput shape: a value-type mirror struct
// that contains media fields (here represented by a string-typed
// stand-in — the codegen branches we exercise read field types, not
// the actual media interfaces).
type testContentPartMediaInput struct {
	Text *string
}

// testContentPartElem is the BAML-side element type the pool helpers
// would be generated for. The convert function's ownedNested parameter
// is typed against this element, so we use it as the `pooledInner`
// sentinel in the convertOwnedNested map.
type testContentPartElem struct {
	Text *string
}

// testMessageMediaInputA / testMessageMediaInputB are two distinct
// outer mirror types that both route through the pooled-slice path.
// The structMediaParams loop adds one entry per such param, so a
// method with two pooled slice params used to emit two
// `__releaseConverted := func()` declarations in the same scope —
// the F1 regression.
type testMessageMediaInputA struct {
	Role  string
	Parts *[]testContentPartMediaInput
}

type testMessageMediaInputB struct {
	Role  string
	Parts *[]testContentPartMediaInput
}

// testMessageElemA / testMessageElemB are the BAML-side outer
// element types matching the mirror inputs above. We only need their
// reflect.Type identity to plumb the slice-pool tracker and the
// convertOwnedNested map.
type testMessageElemA struct {
	Role  string
	Parts *[]testContentPartElem
}

type testMessageElemB struct {
	Role  string
	Parts *[]testContentPartElem
}

// newPooledMethodEmitter builds a minimal methodEmitter wired against
// `n` pooled slice params, each with paramType []testMessageElem<i>.
// Pre-populates the slice-pool tracker and convertOwnedNested map so
// the dispatch site takes the pooled branch without first running the
// mirror-struct emitter.
func newPooledMethodEmitter(t *testing.T, paramNames []string) (*methodEmitter, []reflect.Type) {
	t.Helper()
	if len(paramNames) == 0 || len(paramNames) > 2 {
		t.Fatalf("newPooledMethodEmitter: paramNames must have 1 or 2 entries, got %d", len(paramNames))
	}

	pkgs := DefaultPackageConfig()
	g := &generator{
		opts:                 Options{SupportsWithClient: true, Packages: pkgs, Introspection: RootIntrospection()},
		pkgs:                 pkgs,
		intro:                RootIntrospection(),
		out:                  common.MakeFile(),
		supportsWithClient:   true,
		mirrors:              newMirrorStructTracker(),
		emittedUnwrapHelpers: map[string]bool{},
		slicePools:           newSlicePoolTracker(pkgs),
	}

	elemTypes := []reflect.Type{
		reflect.TypeOf(testMessageElemA{}),
		reflect.TypeOf(testMessageElemB{}),
	}
	innerElem := reflect.TypeOf(testContentPartElem{})

	var smps []structMediaParam
	var paramTypes []reflect.Type
	for i, name := range paramNames {
		elem := elemTypes[i]
		sliceType := reflect.SliceOf(elem)
		smps = append(smps, structMediaParam{
			paramName:   name,
			mirrorName:  "TestMirror" + name,
			convertFunc: "convertTestMirror" + name,
			paramType:   sliceType,
		})
		paramTypes = append(paramTypes, sliceType)
		g.mirrors.convertOwnedNested[elem] = innerElem
	}

	me := &methodEmitter{
		g:                 g,
		methodName:        "TestMethod",
		args:              paramNames,
		syncFuncType:      synthSyncFuncType(paramTypes),
		methodMediaParams: map[string]bamlutils.MediaKind{}, // direct media-typed params: none for this fixture
		structMediaParams: smps,
		inputStructName:   "TestMethodInput",
	}
	me.structMediaParamSet = make(map[string]bool, len(me.structMediaParams))
	for _, smp := range me.structMediaParams {
		me.structMediaParamSet[smp.paramName] = true
	}
	return me, paramTypes
}

// synthSyncFuncType builds a reflect.Type approximating BAML's sync
// function signature: ctx, <one param per smp>, ...opts. The codegen
// loop only consults In(paramIdx) for the slice/ptr branches the test
// exercises, so we use string-typed sentinels for ctx and opts.
func synthSyncFuncType(paramTypes []reflect.Type) reflect.Type {
	in := []reflect.Type{
		reflect.TypeOf((*[0]byte)(nil)).Elem(), // ctx slot — replaced below
	}
	// reflect.FuncOf can't build a context.Context placeholder directly
	// from a non-interface; use any so In(0) has a value, and append
	// each param + a trailing variadic opts slot.
	in[0] = reflect.TypeOf((*any)(nil)).Elem()
	in = append(in, paramTypes...)
	// trailing variadic: reflect.FuncOf with isVariadic=true requires
	// the last param to be a slice. The codegen loop reads
	// `NumIn()-1` (exclusive variadic), so the slot just has to exist.
	in = append(in, reflect.TypeOf([]any{}))
	return reflect.FuncOf(in, []reflect.Type{reflect.TypeOf((*any)(nil)).Elem()}, true)
}

// TestPreambleHoistsSingleReleaseConvertedForMultiplePooledParams pins
// F1: a method with two pooled struct-media slice params must emit
// exactly ONE `__releaseConverted := func()` declaration. The pre-fix
// codegen emitted one per param in the same scope and failed to
// compile with "no new variables on left side of :=".
//
// We render the preamble and substring-count the redeclaration. We
// also assert each pooled param's per-name resources
// (`__messagesPtr_<name>`, `__ownedNested_<name>`) are declared so the
// hoisted closure body has every reference it expects.
func TestPreambleHoistsSingleReleaseConvertedForMultiplePooledParams(t *testing.T) {
	me, _ := newPooledMethodEmitter(t, []string{"messages", "context_messages"})
	preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")

	out := common.MakeFile()
	out.Func().Id("preambleHarness").Params().Block(preamble...)
	rendered := out.GoString()

	// F1 pin — exactly one `__releaseConverted := func()` line. Two
	// would re-declare the closure in the same scope and the file
	// would not compile.
	if got := strings.Count(rendered, "__releaseConverted := func()"); got != 1 {
		t.Errorf("expected exactly 1 `__releaseConverted := func()` declaration; got %d. Rendered:\n%s", got, rendered)
	}

	// Per-param resource declarations must use unique names so the
	// hoisted closure body's references stay unambiguous.
	for _, name := range []string{"messages", "context_messages"} {
		wantPtr := "__messagesPtr_" + name + " :="
		wantOwned := "var __ownedNested_" + name
		if !strings.Contains(rendered, wantPtr) {
			t.Errorf("expected per-param outer-slice ptr declaration %q; rendered:\n%s", wantPtr, rendered)
		}
		if !strings.Contains(rendered, wantOwned) {
			t.Errorf("expected per-param ownedNested declaration %q; rendered:\n%s", wantOwned, rendered)
		}
	}

	// hasReleaseConverted must be set so dispatch-site emitters defer
	// the closure at the right spot (inside the orchestration goroutine
	// for async paths, inside the body callback for legacy paths).
	if !me.hasReleaseConverted {
		t.Error("methodEmitter.hasReleaseConverted must be true after pooled preamble emission")
	}
}

// TestPreambleSingleParamStillEmitsHoistedRelease pins the trivial
// case: one pooled param also routes through the hoisted closure (the
// dispatch site can't tell whether there are 0/1/N pooled params, so
// the hoist is unconditional). Functionally identical pre/post-fix
// for this shape, but covered so a future "only-hoist-when-N>1"
// shortcut would surface here.
func TestPreambleSingleParamStillEmitsHoistedRelease(t *testing.T) {
	me, _ := newPooledMethodEmitter(t, []string{"messages"})
	preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")

	out := common.MakeFile()
	out.Func().Id("preambleHarness").Params().Block(preamble...)
	rendered := out.GoString()

	if got := strings.Count(rendered, "__releaseConverted := func()"); got != 1 {
		t.Errorf("expected exactly 1 `__releaseConverted := func()` declaration for single pooled param; got %d. Rendered:\n%s", got, rendered)
	}
	if !strings.Contains(rendered, "__messagesPtr_messages :=") {
		t.Errorf("expected `__messagesPtr_messages :=` declaration; rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "var __ownedNested_messages") {
		t.Errorf("expected `var __ownedNested_messages` declaration; rendered:\n%s", rendered)
	}
}

// Image is a stand-in for a BAML media type so the codegen's
// `isMediaReflectType` (which keys on the name + the configured BAML
// PkgPath) detects f2Inner as media-bearing. The F2 test below
// constructs a custom PackageConfig that points BamlPkg at this test
// package so the name match resolves.
type Image struct{}

// f2OuterStructPtrSliceOfPointers is the BAML-side mirror shape that
// triggered F2: a struct with a `*[]*Inner` field. Pre-fix, the
// pooledInner-detection branch would unwrap `*Inner` to `Inner` and
// record `convertOwnedNested[Outer] = Inner`, while
// nestedStructConversion would call `pools.ensure(out, *Inner, 256)`
// and produce a `*[]*Inner` pool — the resulting append
// `*ownedNested = append(*ownedNested, __partsPtr)` would not type-
// check (`*[]*[]Inner` vs `*[]*Inner`).
type f2Inner struct {
	Text *string
	Img  *Image
}

type f2OuterStructPtrSliceOfPointers struct {
	Role  string
	Parts *[]*f2Inner
}

// TestGenerateConversionFunc_PointerElementSliceSkipsPool pins F2: a
// `*[]*Inner` field on a mirror struct must take the legacy
// `make([]*Inner, n)` path inside nestedStructConversion AND must
// NOT register the outer struct in `convertOwnedNested` (which would
// add an unused `ownedNested` parameter the dispatch site can't pass
// matching arguments for).
//
// We drive the codegen against the synthetic mirror shape, render the
// convert function, and assert:
//
//  1. The convert function signature has 2 params (adapter, input) —
//     no `ownedNested *[]*[]X` parameter snuck in.
//  2. The body emits `__ptrSlice := make([]*` — the legacy pointer-
//     element path.
//  3. The body does NOT call any `getf2InnerMediaInputSlice` or
//     `putf2InnerMediaInputSlice` pool helper — the slice pool tracker
//     was never asked to emit a pool for the `*Inner` element type.
//  4. `tracker.convertOwnedNested` does NOT have an entry for the
//     outer type (the dispatch site keys off this map to decide
//     whether to thread `&__ownedNested_<name>` through).
func TestGenerateConversionFunc_PointerElementSliceSkipsPool(t *testing.T) {
	out := common.MakeFile()
	tracker := newMirrorStructTracker()
	// Point BamlPkg at this test package so `isMediaReflectType` resolves
	// the locally-declared `Image` type as a BAML media field. Without
	// this, `structContainsMedia(f2Inner)` returns false and the convert
	// emitter shortcuts to a direct `result.Parts = input.Parts` copy,
	// never exercising the nestedStructConversion `*[]*T` branch this
	// test targets.
	pkgs := DefaultPackageConfig()
	pkgs.BamlPkg = reflect.TypeOf(Image{}).PkgPath()
	pools := newSlicePoolTracker(pkgs)

	outerType := reflect.TypeOf(f2OuterStructPtrSliceOfPointers{})
	mirrorName := tracker.ensureMirrorStruct(out, outerType, pkgs, pools)
	if mirrorName == "" {
		t.Fatal("ensureMirrorStruct returned empty mirror name")
	}
	rendered := out.GoString()

	convertFunc := "convert" + mirrorName
	fnStart := strings.Index(rendered, "func "+convertFunc+"(")
	if fnStart < 0 {
		t.Fatalf("convert function %q not found in rendered source:\n%s", convertFunc, rendered)
	}
	nextFn := strings.Index(rendered[fnStart+1:], "\nfunc ")
	end := len(rendered)
	if nextFn >= 0 {
		end = fnStart + 1 + nextFn
	}
	body := rendered[fnStart:end]

	// 1. Signature must have exactly the legacy two-param shape. The
	// post-fix detection branch declines to register the outer type
	// in `convertOwnedNested`, so generateConversionFunc emits the
	// original (adapter, input) params and nothing else.
	if strings.Contains(body, "ownedNested") {
		t.Errorf("convert function for `*[]*T` field must NOT take ownedNested parameter; body:\n%s", body)
	}

	// 2. Body must use the legacy make path. We don't pin the exact
	// inner element name (the mirror-name suffix depends on the
	// synthetic input type), only that `make([]*` appears — the
	// hallmark of the pointer-element fallback inside
	// nestedStructConversion's `if isPtr { if innerAfterPtr.Kind() ==
	// Slice { ... } }` branch.
	if !strings.Contains(body, "__ptrSlice := make([]*") {
		t.Errorf("convert function for `*[]*T` field must use `__ptrSlice := make([]*...)` legacy path; body:\n%s", body)
	}

	// 3. No pool helper for the pointer element type. The pool
	// emitter would have written `var <name>SlicePool sync.Pool` at
	// the file level if pools.ensure was called; substring-match
	// against that signature scoped to the inner type name.
	if strings.Contains(rendered, "f2InnerSlicePool") {
		t.Errorf("pointer-element slice must NOT trigger a slice-pool emission for the inner element; rendered:\n%s", rendered)
	}

	// 4. convertOwnedNested must be unset for the outer type so the
	// dispatch site does not try to pass `&__ownedNested_*` into a
	// convert call that has no matching parameter.
	if got := tracker.convertOwnedNestedFor(outerType); got != nil {
		t.Errorf("convertOwnedNestedFor(%v) = %v, want nil — pointer-element nested slices must NOT register an ownedNested contract",
			outerType, got)
	}
}
