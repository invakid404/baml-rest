package codegen

import (
	"context"
	"go/parser"
	"go/token"
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
		slicePools:           newSlicePoolTracker(pkgs, false, false),
	}

	elemTypes := []reflect.Type{
		reflect.TypeOf(testMessageElemA{}),
		reflect.TypeOf(testMessageElemB{}),
	}
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
		// Pre-populate the closure-context contract so the dispatch
		// site emits the `&__ownedNested_<name>` 3rd arg without
		// first generating the inner converter.
		g.mirrors.convertNeedsOwnedNested[elem] = true
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

// nonPooledMessageElem is a value-type mirror struct used to construct
// non-slice / pointer-element struct-media params in F3's fixture.
// It pairs with nonPooledContentPart (the would-be-nested type) so
// structContainsMedia would have returned true under a real
// PackageConfig — but the F3 test doesn't traverse ensureMirrorStruct,
// only methodEmitter.makePreambleWithArgs, which reads structMediaParams
// directly without re-deriving them from BAML media detection.
type nonPooledMessageElem struct {
	Role string
	Note *string
}

// newMixedMethodEmitter constructs a methodEmitter whose
// structMediaParams mix one pooled `[]ClassWithMedia` slice (param
// "messages") with one non-pooled struct-media param of the shape
// described by `nonPooledKind`:
//
//   - "ptr_slice"      → `*[]ClassWithMedia` field shape
//   - "ptr_struct"     → `*ClassWithMedia` field shape
//   - "direct"         →  `ClassWithMedia` direct field shape
//   - "slice_of_ptr"   → `[]*ClassWithMedia` (slice with pointer elements;
//     skipped from pooling per F2 guard)
//
// All four shapes hit a Phase-B error-return site that — pre-fix —
// did not call `__releaseConverted()` before propagating the
// conversion failure, leaking the pooled outer slice + ownedNested
// from param "messages".
func newMixedMethodEmitter(t *testing.T, nonPooledKind string) *methodEmitter {
	t.Helper()

	pkgs := DefaultPackageConfig()
	g := &generator{
		opts:                 Options{SupportsWithClient: true, Packages: pkgs, Introspection: RootIntrospection()},
		pkgs:                 pkgs,
		intro:                RootIntrospection(),
		out:                  common.MakeFile(),
		supportsWithClient:   true,
		mirrors:              newMirrorStructTracker(),
		emittedUnwrapHelpers: map[string]bool{},
		slicePools:           newSlicePoolTracker(pkgs, false, false),
	}

	pooledElem := reflect.TypeOf(testMessageElemA{})
	g.mirrors.convertNeedsOwnedNested[pooledElem] = true
	pooledType := reflect.SliceOf(pooledElem)

	nonPooledStruct := reflect.TypeOf(nonPooledMessageElem{})
	var nonPooledType reflect.Type
	switch nonPooledKind {
	case "ptr_slice":
		nonPooledType = reflect.PointerTo(reflect.SliceOf(nonPooledStruct))
	case "ptr_struct":
		nonPooledType = reflect.PointerTo(nonPooledStruct)
	case "direct":
		nonPooledType = nonPooledStruct
	case "slice_of_ptr":
		nonPooledType = reflect.SliceOf(reflect.PointerTo(nonPooledStruct))
	default:
		t.Fatalf("unknown nonPooledKind %q", nonPooledKind)
	}

	smps := []structMediaParam{
		{
			paramName:   "messages",
			mirrorName:  "TestMirrorMessages",
			convertFunc: "convertTestMirrorMessages",
			paramType:   pooledType,
		},
		{
			paramName:   "context",
			mirrorName:  "TestMirrorContext",
			convertFunc: "convertTestMirrorContext",
			paramType:   nonPooledType,
		},
	}

	me := &methodEmitter{
		g:                 g,
		methodName:        "TestMethod",
		args:              []string{"messages", "context"},
		syncFuncType:      synthSyncFuncType([]reflect.Type{pooledType, nonPooledType}),
		methodMediaParams: map[string]bamlutils.MediaKind{},
		structMediaParams: smps,
		inputStructName:   "TestMethodInput",
	}
	me.structMediaParamSet = make(map[string]bool, len(me.structMediaParams))
	for _, smp := range me.structMediaParams {
		me.structMediaParamSet[smp.paramName] = true
	}
	return me
}

// TestPreambleReleasesPooledResourcesOnNonPooledConversionError pins
// F3: every Phase-B conversion-error return must call
// `__releaseConverted()` before propagating the error when any pooled
// resources were allocated in Phase A. The pre-fix codegen only added
// the release call to the pooled-slice branch — non-pooled siblings'
// error paths leaked the outer slice + ownedNested.
//
// Drives one fixture per non-pooled shape (the four Phase-B branches
// that currently emit `return fmt.Errorf(...)`): `*[]C`, `*C`,
// direct `C`, and `[]*C`. The shared assertion is that
// `__releaseConverted()` is called immediately before the
// non-pooled param's `return fmt.Errorf("<paramName>: %w", ...)` /
// `return fmt.Errorf("<paramName>[%d]: %w", ...)` site. We match the
// exact two-statement block jen emits — release call, then the
// `return fmt.Errorf` literal — so a regression that drops the
// release call (or moves it elsewhere in the block) fails the
// assertion regardless of surrounding context.
func TestPreambleReleasesPooledResourcesOnNonPooledConversionError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		kind string
		// wantBlock is the literal two-statement block that must
		// appear in the rendered preamble for the non-pooled param's
		// error path: `__releaseConverted()` followed by the matching
		// `return fmt.Errorf(...)`. We anchor on the "context"
		// param-name fragment so the assertion targets the non-pooled
		// branch, not the pooled "messages" branch.
		wantBlock string
	}{
		{
			name:      "ptr_slice",
			kind:      "ptr_slice",
			wantBlock: "__releaseConverted()\n\t\t\t\treturn fmt.Errorf(\"context[%d]: %w\",",
		},
		{
			name:      "ptr_struct",
			kind:      "ptr_struct",
			wantBlock: "__releaseConverted()\n\t\t\treturn fmt.Errorf(\"context: %w\",",
		},
		{
			name:      "direct",
			kind:      "direct",
			wantBlock: "__releaseConverted()\n\t\treturn fmt.Errorf(\"context: %w\",",
		},
		{
			name:      "slice_of_ptr",
			kind:      "slice_of_ptr",
			wantBlock: "__releaseConverted()\n\t\t\t\treturn fmt.Errorf(\"context[%d]: %w\",",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			me := newMixedMethodEmitter(t, tc.kind)
			preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")

			out := common.MakeFile()
			out.Func().Id("preambleHarness").Params().Block(preamble...)
			rendered := out.GoString()

			// The pooled-slice param must have set hasReleaseConverted —
			// otherwise the assertion would be vacuously passing (no
			// release call would be expected anywhere).
			if !me.hasReleaseConverted {
				t.Fatalf("fixture invariant: pooled param must set hasReleaseConverted; rendered:\n%s", rendered)
			}

			if !strings.Contains(rendered, tc.wantBlock) {
				t.Errorf("non-pooled %q error path must call __releaseConverted() immediately before the error return; expected block:\n%s\n\nrendered:\n%s",
					tc.kind, tc.wantBlock, rendered)
			}
		})
	}
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
	pools := newSlicePoolTracker(pkgs, false, false)

	outerType := reflect.TypeOf(f2OuterStructPtrSliceOfPointers{})
	tracker.precomputeOwnedNestedNeeds([]reflect.Type{outerType}, pkgs)
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

	// 4. convertNeedsOwnedNested must be unset for the outer type so
	// the dispatch site does not try to pass `&__ownedNested_*` into
	// a convert call that has no matching parameter.
	if got := tracker.convertNeedsOwnedNestedFor(outerType); got {
		t.Errorf("convertNeedsOwnedNestedFor(%v) = true, want false — pointer-element nested slices must NOT register an ownedNested contract",
			outerType)
	}
}

// =====================================================================
// F1/F2 cold-review regression tests (closure-context ownedNested).
// =====================================================================

// f1MessageWithMedia is the cold-review F1 shape: a struct-media param
// with shape `*MessageWithMedia` (single pointer-to-struct, non-pooled
// at the top level) whose mirror's `Parts *[]ContentPart` field IS
// pooled. The dispatch top-level call site must pass the third
// `ownedNested` argument so the inner converter's signature matches.
// Pre-fix the call site emitted 2-arg, the converter emitted 3-param,
// and the file failed to compile.
type f1ContentPart struct {
	Text *string
	Img  *Image
}

type f1MessageWithMedia struct {
	Role  string
	Parts *[]f1ContentPart
}

// f1OuterWrapper has a direct nested struct field whose converter
// needs `ownedNested`. Used to assert nested helpers also thread the
// argument through.
type f1OuterWrapper struct {
	Inner f1MessageWithMedia
}

// TestPreambleThreadsOwnedNestedThroughNonPooledCallSites pins F1:
// every top-level dispatch call site whose target converter takes the
// closure-context `ownedNested` parameter must pass `&__ownedNested_<name>`
// as the third argument — including the non-pooled `*[]C`, `*C`,
// direct `C`, and `[]*C` shapes that pre-fix emitted 2-arg calls.
//
// We exercise the F1 brief's `*MessageWithMedia` shape: a single
// pointer-to-struct param whose inner converter requires
// `ownedNested *[]func()` because the inner struct has a pooled
// `*[]ContentPart` field. The rendered preamble must declare
// `var __ownedNested_<name> []func()` and the converter call must
// include `&__ownedNested_<name>` as its third arg.
func TestPreambleThreadsOwnedNestedThroughNonPooledCallSites(t *testing.T) {
	t.Parallel()

	pkgs := DefaultPackageConfig()
	// Point BamlPkg at this test package so `Image` (declared above)
	// is recognized as a BAML media type by `isMediaReflectType`,
	// which routes f1ContentPart through structContainsMedia.
	pkgs.BamlPkg = reflect.TypeOf(Image{}).PkgPath()

	g := &generator{
		opts:                 Options{SupportsWithClient: true, Packages: pkgs, Introspection: RootIntrospection()},
		pkgs:                 pkgs,
		intro:                RootIntrospection(),
		out:                  common.MakeFile(),
		supportsWithClient:   true,
		mirrors:              newMirrorStructTracker(),
		emittedUnwrapHelpers: map[string]bool{},
		slicePools:           newSlicePoolTracker(pkgs, false, false),
	}

	// Run the analysis pass first so generateConversionFunc sees
	// the final convertNeedsOwnedNested value. The mirror-struct
	// emission then queries it from the (already-populated) map.
	outerType := reflect.TypeOf(f1MessageWithMedia{})
	g.mirrors.precomputeOwnedNestedNeeds([]reflect.Type{outerType}, pkgs)
	mirrorName := g.mirrors.ensureMirrorStruct(g.out, outerType, pkgs, g.slicePools)
	if !g.mirrors.convertNeedsOwnedNestedFor(outerType) {
		t.Fatalf("fixture invariant: convertNeedsOwnedNestedFor(%v) must be true after precompute — otherwise the dispatch site under test has nothing to thread", outerType)
	}

	// Set up a methodEmitter whose single struct-media param is
	// `*f1MessageWithMedia`, the F1 failing shape.
	paramType := reflect.PointerTo(outerType)
	convertFunc := "convert" + mirrorName
	me := &methodEmitter{
		g:            g,
		methodName:   "TestMethod",
		args:         []string{"extra"},
		syncFuncType: synthSyncFuncType([]reflect.Type{paramType}),
		structMediaParams: []structMediaParam{{
			paramName:   "extra",
			mirrorName:  mirrorName,
			convertFunc: convertFunc,
			paramType:   paramType,
		}},
		methodMediaParams: map[string]bamlutils.MediaKind{},
		inputStructName:   "TestMethodInput",
	}
	me.structMediaParamSet = map[string]bool{"extra": true}

	preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")
	out := common.MakeFile()
	out.Func().Id("preambleHarness").Params().Block(preamble...)
	rendered := out.GoString()

	// 1. The dispatch must declare a per-param closure slice so the
	// converter has somewhere to deposit its release closures.
	if !strings.Contains(rendered, "var __ownedNested_extra []func()") {
		t.Errorf("expected `var __ownedNested_extra []func()` declaration in dispatch preamble; rendered:\n%s", rendered)
	}

	// 2. The call site for the non-pooled `*Mirror` shape must pass
	// `&__ownedNested_extra` as the third argument. Pre-fix the
	// dispatch emitted only `(adapter, input.Extra)` and the file
	// did not compile.
	wantCall := convertFunc + "(adapter, input.Extra, &__ownedNested_extra)"
	if !strings.Contains(rendered, wantCall) {
		t.Errorf("expected non-pooled call site %q in dispatch preamble; rendered:\n%s", wantCall, rendered)
	}

	// 3. The hoisted release closure must drain the per-param
	// closure slice — the F1 fix's reason for existence.
	if !strings.Contains(rendered, "for _, __release := range __ownedNested_extra") {
		t.Errorf("hoisted release closure must iterate __ownedNested_extra; rendered:\n%s", rendered)
	}
}

// TestPreambleThreadsOwnedNestedThroughNestedHelpers pins F1's nested
// arm: when a converter's body contains another converter call (via
// `nestedStructConversion`'s Direct / *Struct / slice / *[]Struct
// branches), and the inner converter takes `ownedNested`, the outer
// converter must thread its own `ownedNested` parameter into the inner
// call. With the closure-context shape this is uniform: every
// converter that propagates pooled state takes the same
// `*[]func()` parameter regardless of the specific inner element
// types its descendants pool.
//
// We exercise this by generating mirror converters for the outer
// `f1OuterWrapper`, whose Inner field is a value-type
// `f1MessageWithMedia` (the F1 type above — its converter needs
// ownedNested). The outer converter's rendered body must contain a
// 3-arg call to the inner converter.
func TestPreambleThreadsOwnedNestedThroughNestedHelpers(t *testing.T) {
	t.Parallel()

	pkgs := DefaultPackageConfig()
	pkgs.BamlPkg = reflect.TypeOf(Image{}).PkgPath()

	out := common.MakeFile()
	tracker := newMirrorStructTracker()
	pools := newSlicePoolTracker(pkgs, false, false)

	outerType := reflect.TypeOf(f1OuterWrapper{})
	tracker.precomputeOwnedNestedNeeds([]reflect.Type{outerType}, pkgs)
	outerMirror := tracker.ensureMirrorStruct(out, outerType, pkgs, pools)
	innerType := reflect.TypeOf(f1MessageWithMedia{})
	if !tracker.convertNeedsOwnedNestedFor(innerType) {
		t.Fatal("fixture invariant: inner converter must need ownedNested")
	}
	if !tracker.convertNeedsOwnedNestedFor(outerType) {
		t.Fatal("outer converter must transitively need ownedNested (its body calls the inner one)")
	}

	rendered := out.GoString()

	// The outer converter's signature must propagate ownedNested.
	wantOuterSig := "func convert" + outerMirror + "(adapter bamlutils.Adapter, input *" + outerMirror + ", ownedNested *[]func())"
	if !strings.Contains(rendered, wantOuterSig) {
		t.Errorf("outer converter signature must take ownedNested; expected:\n%s\nrendered:\n%s", wantOuterSig, rendered)
	}

	// The outer converter's body must call the inner with the 3-arg
	// shape — passing its own `ownedNested` through unchanged. Pre-
	// fix the nested helpers emitted 2-arg calls and the file did
	// not compile.
	innerMirror := "convert" + tracker.generated[innerType]
	wantInnerCall := innerMirror + "(adapter, &input.Inner, ownedNested)"
	if !strings.Contains(rendered, wantInnerCall) {
		t.Errorf("outer converter must thread ownedNested through nested helper call; expected fragment %q; rendered:\n%s", wantInnerCall, rendered)
	}
}

// f2MultiPooledMixed is the cold-review F2 shape: a mirror struct with
// TWO distinct `*[]Struct` value-element fields whose nested
// converters target DIFFERENT BAML element types. Pre-fix the
// converter signature carried a single `ownedNested *[]*[]T` and the
// second field's `append(*ownedNested, *[]OtherT)` was a type-error.
// Post-fix (closure context) both fields append `func()` closures
// to the same `*[]func()` slice.
type f2ContentPart struct {
	Text *string
	Img  *Image
}

type f2ToolPart struct {
	Name *string
	Doc  *Image
}

type f2MultiPooledMixed struct {
	Role  string
	Parts *[]f2ContentPart
	Tools *[]f2ToolPart
}

// TestConvertFuncHandlesMultiplePooledNestedTypes pins F2: a mirror
// converter with two distinct `*[]Struct` value-element fields must
// generate code that compiles cleanly. The closure-context shape
// resolves the type mismatch — both fields append `func()` closures
// to the same `ownedNested *[]func()` slice. Pre-fix the converter's
// signature carried a single concrete `*[]*[]T` and the second
// append site was a type error.
//
// Compile-checks the rendered output via go/parser plus a focused
// textual assertion that both pool sites use the closure shape.
func TestConvertFuncHandlesMultiplePooledNestedTypes(t *testing.T) {
	t.Parallel()

	pkgs := DefaultPackageConfig()
	pkgs.BamlPkg = reflect.TypeOf(Image{}).PkgPath()

	out := common.MakeFile()
	tracker := newMirrorStructTracker()
	pools := newSlicePoolTracker(pkgs, false, false)

	outerType := reflect.TypeOf(f2MultiPooledMixed{})
	tracker.precomputeOwnedNestedNeeds([]reflect.Type{outerType}, pkgs)
	mirrorName := tracker.ensureMirrorStruct(out, outerType, pkgs, pools)
	if mirrorName == "" {
		t.Fatal("ensureMirrorStruct returned empty mirror name")
	}
	if !tracker.convertNeedsOwnedNestedFor(outerType) {
		t.Fatal("converter must need ownedNested when it pools any nested slice")
	}

	rendered := out.GoString()

	// Signature must use closure-context shape — exactly one slice,
	// regardless of how many distinct nested element types are
	// pooled.
	wantSig := "func convert" + mirrorName + "(adapter bamlutils.Adapter, input *" + mirrorName + ", ownedNested *[]func())"
	if !strings.Contains(rendered, wantSig) {
		t.Errorf("converter signature must use *[]func() closure context; expected:\n%s\nrendered:\n%s", wantSig, rendered)
	}

	// Each pool site must append a closure capturing its specific
	// inner `put<X>Slice` call. Substring-match the two distinct
	// inner-type names (lowerFirst applied) — the closure-context
	// shape means both share the same `*ownedNested` slice but
	// invoke different put helpers.
	for _, innerName := range []string{"f2ContentPart", "f2ToolPart"} {
		wantAppend := "*ownedNested = append(*ownedNested, func() {\n\t\t\tput" + innerName + "Slice(__partsPtr)"
		if !strings.Contains(rendered, wantAppend) {
			t.Errorf("expected closure append for inner type %q; want fragment:\n%s\nrendered:\n%s", innerName, wantAppend, rendered)
		}
	}

	// The rendered code must parse as valid Go. Pre-fix the second
	// pooled site emitted a `*[]*[]f2ToolPart` append into
	// `*[]*[]f2ContentPart`, which the type checker rejects (parser
	// would still accept it; we add an explicit textual sanity
	// check to catch the regression even when parsing alone passes).
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "multi_pooled_synth.go", rendered, parser.AllErrors); err != nil {
		t.Errorf("generated source does not parse as valid Go: %v\n--- rendered ---\n%s", err, rendered)
	}
}

func TestEmitMethodsNilSlicePoolsKeepsOwnedNestedDisabled(t *testing.T) {
	t.Parallel()

	pkgs := DefaultPackageConfig()
	pkgs.BamlPkg = reflect.TypeOf(Image{}).PkgPath()

	outerType := reflect.TypeOf(f1OuterWrapper{})
	intro := Introspection{
		SyncMethods:        map[string][]string{"TestMethod": {"extra"}},
		SyncFuncs:          map[string]any{"TestMethod": func(context.Context, f1OuterWrapper, ...any) (string, error) { return "", nil }},
		ParseStreamMethods: map[string]struct{}{"TestMethod": {}},
		MediaParams:        map[string]map[string]bamlutils.MediaKind{"TestMethod": {}},
	}
	g := &generator{
		opts:                 Options{SupportsWithClient: true, Packages: pkgs, Introspection: intro},
		pkgs:                 pkgs,
		intro:                intro,
		out:                  common.MakeFile(),
		supportsWithClient:   true,
		mirrors:              newMirrorStructTracker(),
		emittedUnwrapHelpers: map[string]bool{},
		// Nil slicePools exercises the legacy non-pooling emission mode.
		slicePools: nil,
	}

	g.emitMethods()
	rendered := g.out.GoString()

	if g.mirrors.convertNeedsOwnedNestedFor(outerType) {
		t.Fatal("nil slicePools mode must not precompute ownedNested requirements")
	}
	if strings.Contains(rendered, "ownedNested") {
		t.Fatalf("nil slicePools mode must not emit ownedNested signatures or call-site args; rendered:\n%s", rendered)
	}
}
