package codegen

import (
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/fixtures"
	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils"
)

// TestPoolLifecycle is the runtime complement to TestCompileMatrix.
// The compile matrix proves the rendered output type-checks; this
// harness EXECUTES the rendered output against a controlled fake
// adapter and asserts pool checkout / release balance + the zero-on-
// Put invariant. It crosses the existing 21+cycle structural cells
// with four sync failure modes (success, fail_at_idx_0, fail_at_idx_2,
// fail_in_nested) plus three async barrier cells (one per nested
// shape).
//
// Lifecycle assertions per cell:
//   - The dispatch returns the expected outcome (nil on success, non-
//     nil on failure injection).
//   - poolaudit.Imbalanced() is empty.
//   - poolaudit.ZeroOnPutViolations() is empty.
//   - For b_ptr_elem success cells, poolaudit recorded zero pool
//     activity — pointer-element shapes must skip the pool entirely.
//
// Inner failures bubble up as the captured `go test` output pinned
// into the outer fatal message.
func TestPoolLifecycle(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go binary not on PATH: %v", err)
	}
	runLifecycleHarness(t, Options{EmitPoolAuditHooks: true}, false, "lifecycle harness")
}

// TestPoolLifecycle_RegressionSeeds flips one regression seed at a
// time and asserts the inner go test FAILS. A green inner run for any
// seed means either the bug class is no longer reproducible at all
// (so the harness lost coverage) or this seed has drifted out of sync
// with the relevant emission path.
func TestPoolLifecycle_RegressionSeeds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go binary not on PATH: %v", err)
	}

	seeds := []struct {
		name string
		opts Options
	}{
		{name: "Seed_OmitPhaseBRelease", opts: Options{EmitPoolAuditHooks: true, Seed_OmitPhaseBRelease: true}},
		{name: "Seed_OmitOwnedNestedThread", opts: Options{EmitPoolAuditHooks: true, Seed_OmitOwnedNestedThread: true}},
		{name: "Seed_OmitZeroLoop", opts: Options{EmitPoolAuditHooks: true, Seed_OmitZeroLoop: true}},
		{name: "Seed_OmitAsyncDefer", opts: Options{EmitPoolAuditHooks: true, Seed_OmitAsyncDefer: true}},
	}
	for _, s := range seeds {
		s := s
		t.Run(s.name, func(t *testing.T) {
			runLifecycleHarness(t, s.opts, true, s.name)
		})
	}
}

func runLifecycleHarness(t *testing.T, opts Options, expectFail bool, label string) {
	t.Helper()

	rendered, _ := emitLifecycleMatrix(t, opts)
	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, rendered, map[string]string{
		"lifecycle_test.go": lifecycleTestTemplate,
	})

	stdout, err := testharness.RunGoTest(t, tmp, "")
	if expectFail {
		if err == nil {
			t.Fatalf("%s: inner go test passed but a regression seed should have made it FAIL — the bug class either no longer reproduces or the seed has drifted\n--- inner output ---\n%s", label, stdout)
		}
		// Diagnostic only — surfaced under `go test -v` so seed
		// drift is visible without having to break the harness.
		t.Logf("%s caught expected failures in inner output:\n%s", label, summarizeInnerFails(stdout))
		return
	}
	if err != nil {
		t.Fatalf("%s: inner go test failed unexpectedly\n--- inner output ---\n%s\n--- error ---\n%v", label, stdout, err)
	}
}

// summarizeInnerFails extracts the first few FAIL lines from inner
// go test output so the regression-seed diagnostic stays compact.
// Full output is omitted to keep -v noise manageable. Recognises
// both runtime FAIL lines (imbalance / zero-on-Put / barrier) and
// build-step compiler errors (e.g. arg-count mismatches), since the
// ownedNested-thread seed surfaces as a compile failure rather than
// a runtime assertion.
func summarizeInnerFails(stdout string) string {
	const maxLines = 20
	var fails []string
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "--- FAIL:"),
			strings.Contains(trimmed, "imbalance"),
			strings.Contains(trimmed, "zero-on-Put"),
			strings.Contains(trimmed, "release fired before"),
			strings.Contains(trimmed, "not enough arguments"),
			strings.Contains(trimmed, "cannot use"),
			strings.Contains(trimmed, "[build failed]"):
		default:
			continue
		}
		fails = append(fails, trimmed)
		if len(fails) >= maxLines {
			fails = append(fails, "(truncated)")
			break
		}
	}
	if len(fails) == 0 {
		return "(no recognised failure lines — inner stdout may have a new format)"
	}
	return strings.Join(fails, "\n")
}

// lifecycleCellMeta is the test-author view of one rendered cell.
// Returned by emitLifecycleMatrix and consumed by emitCellRegistry to
// build the SyncLifecycleCells / AsyncLifecycleCells var blocks in the
// rendered matrix.
type lifecycleCellMeta struct {
	Name string
	// FailMode tags the cell with its configured adapter failure
	// mode: "success", "fail_at_idx_0", "fail_at_idx_2",
	// "fail_in_nested", or "async_late_consume" (async cells).
	FailMode    string
	ExpectError bool
	// ExpectNoPoolForType, when non-empty, asserts the given audit
	// type name does NOT appear in poolaudit.Checkouts after the
	// dispatch returns. Set to "ContentPartB" for b_ptr_elem cells
	// under success mode — the pointer-element fallback path must
	// not touch the nested pool. (The OUTER MessageB slice is
	// still pooled because its element type is a value struct;
	// only ContentPartB is exempt.)
	ExpectNoPoolForType string
	Async               bool
	DispatchName        string
	InputStructName     string
	NestedShapeTag      string
}

type paramSpec struct {
	name string
	ty   reflect.Type
}

func emitLifecycleMatrix(t *testing.T, opts Options) (string, []lifecycleCellMeta) {
	t.Helper()

	pkgs := DefaultPackageConfig()
	pkgs.BamlPkg = reflect.TypeOf(fixtures.Image{}).PkgPath()
	pkgs.OutputPkg = "github.com/invakid404/baml-rest/adapters/common/codegen/matrixtest"
	pkgs.OutputPkgName = "matrix"

	out := jen.NewFilePathName(pkgs.OutputPkg, pkgs.OutputPkgName)
	tracker := newMirrorStructTracker()
	pools := newSlicePoolTracker(pkgs, opts.EmitPoolAuditHooks)
	pools.seedOmitZeroLoop = opts.Seed_OmitZeroLoop

	// Precompute ownedNested needs across the whole reachable graph.
	// Same shape as the compile-matrix path. Without this the cycle
	// cell's emission would race itself.
	var precomputeRoots []reflect.Type
	for _, ty := range []reflect.Type{
		reflect.TypeOf(fixtures.MessageA{}),
		reflect.TypeOf(fixtures.MessageB{}),
		reflect.TypeOf(fixtures.MessageC{}),
		reflect.TypeOf(fixtures.ClassA{}),
		reflect.TypeOf(fixtures.ClassB{}),
		reflect.TypeOf(fixtures.ClassC{}),
		reflect.TypeOf(fixtures.OtherA{}),
		reflect.TypeOf(fixtures.OtherB{}),
		reflect.TypeOf(fixtures.OtherC{}),
		reflect.TypeOf(fixtures.CycleA{}),
		reflect.TypeOf(fixtures.CycleB{}),
	} {
		precomputeRoots = append(precomputeRoots, ty)
	}
	tracker.precomputeOwnedNestedNeeds(precomputeRoots, pkgs)

	type nestedShape struct {
		tag       string
		messageTy reflect.Type
		classTy   reflect.Type
		otherTy   reflect.Type
		ptrElem   bool
	}
	nestedShapes := []nestedShape{
		{tag: testharness.NestedValueElem.String(), messageTy: reflect.TypeOf(fixtures.MessageA{}), classTy: reflect.TypeOf(fixtures.ClassA{}), otherTy: reflect.TypeOf(fixtures.OtherA{})},
		{tag: testharness.NestedPtrElem.String(), messageTy: reflect.TypeOf(fixtures.MessageB{}), classTy: reflect.TypeOf(fixtures.ClassB{}), otherTy: reflect.TypeOf(fixtures.OtherB{}), ptrElem: true},
		{tag: testharness.NestedTwoPooledTypes.String(), messageTy: reflect.TypeOf(fixtures.MessageC{}), classTy: reflect.TypeOf(fixtures.ClassC{}), otherTy: reflect.TypeOf(fixtures.OtherC{})},
	}

	type topShape struct {
		name        string
		buildParams func(messageTy, classTy, otherTy reflect.Type) []paramSpec
	}
	topShapes := []topShape{
		{name: "1_pooled_baseline", buildParams: func(messageTy, _, _ reflect.Type) []paramSpec {
			return []paramSpec{{name: "messages", ty: reflect.SliceOf(messageTy)}}
		}},
		{name: "2_two_pooled_params", buildParams: func(messageTy, classTy, _ reflect.Type) []paramSpec {
			return []paramSpec{
				{name: "messages", ty: reflect.SliceOf(messageTy)},
				{name: "classes", ty: reflect.SliceOf(classTy)},
			}
		}},
		{name: "3_pooled_plus_slice_of_ptr", buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
			return []paramSpec{
				{name: "messages", ty: reflect.SliceOf(messageTy)},
				{name: "extra", ty: reflect.SliceOf(reflect.PointerTo(otherTy))},
			}
		}},
		{name: "4_pooled_plus_ptr_to_slice", buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
			return []paramSpec{
				{name: "messages", ty: reflect.SliceOf(messageTy)},
				{name: "extra", ty: reflect.PointerTo(reflect.SliceOf(otherTy))},
			}
		}},
		{name: "5_pooled_plus_ptr", buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
			return []paramSpec{
				{name: "messages", ty: reflect.SliceOf(messageTy)},
				{name: "extra", ty: reflect.PointerTo(otherTy)},
			}
		}},
		{name: "6_pooled_plus_direct", buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
			return []paramSpec{
				{name: "messages", ty: reflect.SliceOf(messageTy)},
				{name: "extra", ty: otherTy},
			}
		}},
		{name: "7_single_non_pooled_ptr", buildParams: func(_, _, otherTy reflect.Type) []paramSpec {
			return []paramSpec{{name: "extra", ty: reflect.PointerTo(otherTy)}}
		}},
	}

	failModes := []string{
		testharness.FailureSuccess.String(),
		testharness.FailureAtIdx0.String(),
		testharness.FailureAtIdx2.String(),
		testharness.FailureInNested.String(),
	}

	var cells []lifecycleCellMeta

	for topIdx, top := range topShapes {
		for nestedIdx, nested := range nestedShapes {
			for _, failMode := range failModes {
				cellIdx := topIdx*len(nestedShapes) + nestedIdx
				cellName := fmt.Sprintf("cell_%02d_%s_%s_%s", cellIdx, top.name, nested.tag, failMode)
				params := top.buildParams(nested.messageTy, nested.classTy, nested.otherTy)
				meta := emitLifecycleSyncCell(out, tracker, pools, pkgs, opts, cellName, params, nested.tag, nested.ptrElem, failMode)
				cells = append(cells, meta)
			}
		}
	}

	for _, failMode := range failModes {
		meta := emitLifecycleCycleCell(out, tracker, pools, pkgs, opts, failMode)
		cells = append(cells, meta)
	}

	for nestedIdx, nested := range nestedShapes {
		cellName := fmt.Sprintf("cell_async_%02d_%s", nestedIdx, nested.tag)
		params := topShapes[0].buildParams(nested.messageTy, nested.classTy, nested.otherTy)
		meta := emitLifecycleAsyncCell(out, tracker, pools, pkgs, opts, cellName, params, nested.tag, nested.ptrElem)
		cells = append(cells, meta)
	}

	out.Func().Id("makeOptionsFromAdapter").
		Params(jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter")).
		Params(jen.Index().Any(), jen.Error()).
		Block(jen.Return(jen.Nil(), jen.Nil()))

	emitCellRegistry(out, pkgs, cells)

	return out.GoString(), cells
}

func emitLifecycleSyncCell(
	out *jen.File,
	tracker *mirrorStructTracker,
	pools *slicePoolTracker,
	pkgs PackageConfig,
	opts Options,
	cellName string,
	params []paramSpec,
	nestedTag string,
	nestedIsPtrElem bool,
	failMode string,
) lifecycleCellMeta {
	smps := make([]structMediaParam, 0, len(params))
	paramTypes := make([]reflect.Type, 0, len(params))
	for _, p := range params {
		mirrorName := tracker.ensureMirrorStruct(out, p.ty, pkgs, pools)
		smps = append(smps, structMediaParam{
			paramName:   p.name,
			mirrorName:  mirrorName,
			convertFunc: "convert" + mirrorName,
			paramType:   p.ty,
		})
		paramTypes = append(paramTypes, p.ty)
	}

	me := newLifecycleMethodEmitter(out, tracker, pools, pkgs, opts, cellName, smps, paramTypes)
	preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")

	tail := []jen.Code{jen.Id("_").Op("=").Id("options")}
	for _, smp := range smps {
		tail = append(tail, jen.Id("_").Op("=").Id("__struct_"+smp.paramName))
	}
	if me.hasReleaseConverted {
		// Mirror what production's body callback / goroutine
		// achieves via the deferred release at its successful exit.
		// Phase B errors call __releaseConverted() explicitly via
		// releaseThenReturnError; this is the happy-path exit.
		tail = append(tail, jen.Id("__releaseConverted").Call())
	}
	tail = append(tail, jen.Return(jen.Nil()))

	body := append([]jen.Code{}, preamble...)
	body = append(body, tail...)

	dispatchName := cellName + "_dispatch"
	out.Func().Id(dispatchName).
		Params(
			jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
		).
		Error().
		Block(body...)

	meta := lifecycleCellMeta{
		Name:            cellName,
		FailMode:        failMode,
		ExpectError:     failMode != testharness.FailureSuccess.String(),
		Async:           false,
		DispatchName:    dispatchName,
		InputStructName: cellName + "Input",
		NestedShapeTag:  nestedTag,
	}
	if nestedIsPtrElem && failMode == testharness.FailureSuccess.String() {
		meta.ExpectNoPoolForType = "ContentPartB"
	}
	return meta
}

func emitLifecycleCycleCell(
	out *jen.File,
	tracker *mirrorStructTracker,
	pools *slicePoolTracker,
	pkgs PackageConfig,
	opts Options,
	failMode string,
) lifecycleCellMeta {
	cycleAType := reflect.TypeOf(fixtures.CycleA{})
	mirrorName := tracker.ensureMirrorStruct(out, cycleAType, pkgs, pools)
	paramType := reflect.SliceOf(cycleAType)
	smps := []structMediaParam{{
		paramName:   "cycles",
		mirrorName:  mirrorName,
		convertFunc: "convert" + mirrorName,
		paramType:   paramType,
	}}
	cellName := fmt.Sprintf("cell_cycle_mutually_recursive_%s", failMode)
	me := newLifecycleMethodEmitter(out, tracker, pools, pkgs, opts, cellName, smps, []reflect.Type{paramType})
	preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")
	tail := []jen.Code{
		jen.Id("_").Op("=").Id("options"),
		jen.Id("_").Op("=").Id("__struct_cycles"),
	}
	if me.hasReleaseConverted {
		tail = append(tail, jen.Id("__releaseConverted").Call())
	}
	tail = append(tail, jen.Return(jen.Nil()))
	body := append([]jen.Code{}, preamble...)
	body = append(body, tail...)

	dispatchName := cellName + "_dispatch"
	out.Func().Id(dispatchName).
		Params(
			jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
		).
		Error().
		Block(body...)

	return lifecycleCellMeta{
		Name:            cellName,
		FailMode:        failMode,
		ExpectError:     failMode != testharness.FailureSuccess.String(),
		Async:           false,
		DispatchName:    dispatchName,
		InputStructName: cellName + "Input",
		NestedShapeTag:  testharness.NestedCycle.String(),
	}
}

func emitLifecycleAsyncCell(
	out *jen.File,
	tracker *mirrorStructTracker,
	pools *slicePoolTracker,
	pkgs PackageConfig,
	opts Options,
	cellName string,
	params []paramSpec,
	nestedTag string,
	nestedIsPtrElem bool,
) lifecycleCellMeta {
	smps := make([]structMediaParam, 0, len(params))
	paramTypes := make([]reflect.Type, 0, len(params))
	for _, p := range params {
		mirrorName := tracker.ensureMirrorStruct(out, p.ty, pkgs, pools)
		smps = append(smps, structMediaParam{
			paramName:   p.name,
			mirrorName:  mirrorName,
			convertFunc: "convert" + mirrorName,
			paramType:   p.ty,
		})
		paramTypes = append(paramTypes, p.ty)
	}

	me := newLifecycleMethodEmitter(out, tracker, pools, pkgs, opts, cellName, smps, paramTypes)
	preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")

	preambleTail := []jen.Code{jen.Id("_").Op("=").Id("options")}
	for _, smp := range smps {
		preambleTail = append(preambleTail, jen.Id("_").Op("=").Id("__struct_"+smp.paramName))
	}

	// AsyncWait is the test's signal/wait barrier — the goroutine
	// closes the adapter's `asyncStart` channel, then blocks on
	// `asyncCont`. The test reads from asyncStart to know the
	// goroutine entered, snapshots release counts (asserts 0),
	// then closes asyncCont. Correct emission defers
	// __releaseConverted inside the goroutine; the bug seed fires
	// it synchronously before the goroutine starts so the snapshot
	// already sees a release.
	barrierWait := []jen.Code{
		jen.If(
			jen.List(jen.Id("b"), jen.Id("ok")).Op(":=").Id("adapter").Assert(jen.Id("asyncBarrierAdapter")),
			jen.Id("ok"),
		).Block(
			jen.Id("b").Dot("AsyncWait").Call(),
		),
	}

	// The dispatch takes the `done` channel from the caller so the
	// preamble's Phase B `return fmt.Errorf(...)` paths match the
	// outer signature (single error return). The goroutine closes
	// `done` after the barrier so the test can synchronise on
	// completion.
	goroutineBody := []jen.Code{
		jen.Defer().Id("close").Call(jen.Id("done")),
	}
	if me.hasReleaseConverted && !opts.Seed_OmitAsyncDefer {
		goroutineBody = append(goroutineBody, jen.Defer().Id("__releaseConverted").Call())
	}
	goroutineBody = append(goroutineBody, barrierWait...)

	asyncBody := append([]jen.Code{}, preamble...)
	asyncBody = append(asyncBody, preambleTail...)
	if me.hasReleaseConverted && opts.Seed_OmitAsyncDefer {
		// Buggy ordering — release fires before the goroutine reads
		// the converted slice. The barrier test reports the release
		// count is non-zero after asyncStart, catching the bug.
		asyncBody = append(asyncBody, jen.Id("__releaseConverted").Call())
	}
	asyncBody = append(asyncBody,
		jen.Go().Func().Params().Block(goroutineBody...).Call(),
		jen.Return(jen.Nil()),
	)

	dispatchName := cellName + "_async_dispatch"
	out.Func().Id(dispatchName).
		Params(
			jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("done").Chan().Struct(),
		).
		Error().
		Block(asyncBody...)

	meta := lifecycleCellMeta{
		Name:            cellName,
		FailMode:        testharness.FailureAsyncLateConsume.String(),
		ExpectError:     false,
		Async:           true,
		DispatchName:    dispatchName,
		InputStructName: cellName + "Input",
		NestedShapeTag:  nestedTag,
	}
	if nestedIsPtrElem {
		meta.ExpectNoPoolForType = "ContentPartB"
	}
	return meta
}

// newLifecycleMethodEmitter constructs a synthetic methodEmitter whose
// generator carries the regression-seed Options. The matrix uses
// methodEmitter directly because the lifecycle dispatcher reuses the
// production Phase A / Phase B preamble emission unchanged — what
// differs is the dispatch wrapper around it and the seed flags
// methodEmitter consults via me.g.opts.
func newLifecycleMethodEmitter(
	out *jen.File,
	tracker *mirrorStructTracker,
	pools *slicePoolTracker,
	pkgs PackageConfig,
	opts Options,
	cellName string,
	smps []structMediaParam,
	paramTypes []reflect.Type,
) *methodEmitter {
	merged := opts
	merged.SupportsWithClient = true
	merged.Packages = pkgs
	merged.Introspection = RootIntrospection()
	g := &generator{
		opts:                 merged,
		pkgs:                 pkgs,
		intro:                RootIntrospection(),
		out:                  out,
		supportsWithClient:   true,
		mirrors:              tracker,
		emittedUnwrapHelpers: map[string]bool{},
		slicePools:           pools,
	}
	args := make([]string, 0, len(smps))
	for _, smp := range smps {
		args = append(args, smp.paramName)
	}
	me := &methodEmitter{
		g:                 g,
		methodName:        cellName,
		args:              args,
		syncFuncType:      synthSyncFuncType(paramTypes),
		methodMediaParams: map[string]bamlutils.MediaKind{},
		structMediaParams: smps,
		inputStructName:   cellName + "Input",
	}
	me.structMediaParamSet = make(map[string]bool, len(smps))
	for _, smp := range smps {
		me.structMediaParamSet[smp.paramName] = true
	}
	var fields []jen.Code
	for _, smp := range smps {
		fields = append(fields, jen.Id(upperCamelForMatrix(smp.paramName)).Add(mirrorFieldType(smp.paramType, smp.mirrorName)))
	}
	out.Type().Id(me.inputStructName).Struct(fields...)
	return me
}

// emitCellRegistry writes the asyncBarrierAdapter interface, the
// SyncLifecycleCell / AsyncLifecycleCell record types, and the two
// iteration tables. lifecycle_test.go consumes them by name.
func emitCellRegistry(out *jen.File, pkgs PackageConfig, cells []lifecycleCellMeta) {
	out.Type().Id("asyncBarrierAdapter").Interface(
		jen.Id("AsyncWait").Params(),
	)

	out.Type().Id("SyncLifecycleCell").Struct(
		jen.Id("Name").String(),
		jen.Id("FailMode").String(),
		jen.Id("ExpectError").Bool(),
		jen.Id("ExpectNoPoolForType").String(),
		jen.Id("NestedShapeTag").String(),
		jen.Id("Dispatch").Func().Params(jen.Qual(pkgs.InterfacesPkg, "Adapter"), jen.Any()).Error(),
		jen.Id("InputType").Qual("reflect", "Type"),
	)

	out.Type().Id("AsyncLifecycleCell").Struct(
		jen.Id("Name").String(),
		jen.Id("ExpectError").Bool(),
		jen.Id("ExpectNoPoolForType").String(),
		jen.Id("NestedShapeTag").String(),
		jen.Id("Dispatch").Func().Params(jen.Qual(pkgs.InterfacesPkg, "Adapter"), jen.Any(), jen.Chan().Struct()).Error(),
		jen.Id("InputType").Qual("reflect", "Type"),
	)

	var syncEntries, asyncEntries []jen.Code
	for _, c := range cells {
		if c.Async {
			asyncEntries = append(asyncEntries, jen.Values(jen.Dict{
				jen.Id("Name"):                jen.Lit(c.Name),
				jen.Id("ExpectError"):         jen.Lit(c.ExpectError),
				jen.Id("ExpectNoPoolForType"): jen.Lit(c.ExpectNoPoolForType),
				jen.Id("NestedShapeTag"):      jen.Lit(c.NestedShapeTag),
				jen.Id("Dispatch"):            jen.Id(c.DispatchName),
				jen.Id("InputType"):           jen.Qual("reflect", "TypeOf").Call(jen.Id(c.InputStructName).Values()),
			}))
		} else {
			syncEntries = append(syncEntries, jen.Values(jen.Dict{
				jen.Id("Name"):                jen.Lit(c.Name),
				jen.Id("FailMode"):            jen.Lit(c.FailMode),
				jen.Id("ExpectError"):         jen.Lit(c.ExpectError),
				jen.Id("ExpectNoPoolForType"): jen.Lit(c.ExpectNoPoolForType),
				jen.Id("NestedShapeTag"):      jen.Lit(c.NestedShapeTag),
				jen.Id("Dispatch"):            jen.Id(c.DispatchName),
				jen.Id("InputType"):           jen.Qual("reflect", "TypeOf").Call(jen.Id(c.InputStructName).Values()),
			}))
		}
	}

	out.Var().Id("SyncLifecycleCells").Op("=").Index().Id("SyncLifecycleCell").Values(syncEntries...)
	out.Var().Id("AsyncLifecycleCells").Op("=").Index().Id("AsyncLifecycleCell").Values(asyncEntries...)
}

// lifecycleTestTemplate is the source for lifecycle_test.go shipped
// into the temp module. It declares a fakeAdapter modelled on
// bamlutils/buildrequest/resolve_test.go's mockAdapter, the reflect-
// driven input builder, and the two table-driven test bodies.
const lifecycleTestTemplate = `package matrix

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/fixtures"
	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/poolaudit"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// fakeAdapter satisfies bamlutils.Adapter for the lifecycle tests.
// The only methods the generated converters reach are
// NewMediaFromURL / NewMediaFromBase64 (via bamlutils.ConvertMedia);
// the rest of the surface returns trivially-safe defaults.
type fakeAdapter struct {
	context.Context
	mu        sync.Mutex
	callCount int
	failAt    int
	// asyncStart fires when the goroutine has entered AsyncWait.
	// The test reads it to gate the pre-barrier snapshot, avoiding
	// the sleep-based timing that goes flaky under -race.
	asyncStart chan struct{}
	// asyncCont is closed by the test to let the goroutine proceed
	// past the barrier and exit.
	asyncCont chan struct{}
}

func newFakeAdapter(failAt int) *fakeAdapter {
	return &fakeAdapter{Context: context.Background(), failAt: failAt}
}

func newAsyncFakeAdapter() *fakeAdapter {
	return &fakeAdapter{
		Context:    context.Background(),
		asyncStart: make(chan struct{}),
		asyncCont:  make(chan struct{}),
	}
}

func (a *fakeAdapter) AsyncWait() {
	close(a.asyncStart)
	<-a.asyncCont
}

func (a *fakeAdapter) NewMediaFromURL(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	a.mu.Lock()
	a.callCount++
	c := a.callCount
	a.mu.Unlock()
	if a.failAt > 0 && c == a.failAt {
		return nil, errors.New("fakeAdapter: injected failure")
	}
	return fixtures.Image{}, nil
}

func (a *fakeAdapter) NewMediaFromBase64(kind bamlutils.MediaKind, b64 string, mt *string) (any, error) {
	return a.NewMediaFromURL(kind, "data:"+b64, mt)
}

func (a *fakeAdapter) SetClientRegistry(*bamlutils.ClientRegistry) error    { return nil }
func (a *fakeAdapter) SetTypeBuilder(*bamlutils.TypeBuilder) error          { return nil }
func (a *fakeAdapter) SetStreamMode(bamlutils.StreamMode)                   {}
func (a *fakeAdapter) StreamMode() bamlutils.StreamMode                     { return 0 }
func (a *fakeAdapter) SetLogger(bamlutils.Logger)                           {}
func (a *fakeAdapter) Logger() bamlutils.Logger                             { return nil }
func (a *fakeAdapter) SetRetryConfig(*bamlutils.RetryConfig)                {}
func (a *fakeAdapter) RetryConfig() *bamlutils.RetryConfig                  { return nil }
func (a *fakeAdapter) SetIncludeReasoning(bool)                             {}
func (a *fakeAdapter) IncludeReasoning() bool                               { return false }
func (a *fakeAdapter) ClientRegistryProvider() string                       { return "" }
func (a *fakeAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry    { return nil }
func (a *fakeAdapter) HTTPClient() *llmhttp.Client                          { return nil }
func (a *fakeAdapter) SetHTTPClient(*llmhttp.Client)                        {}
func (a *fakeAdapter) SetBuildRequestConfig(bamlutils.BuildRequestConfig)   {}
func (a *fakeAdapter) BuildRequestConfig() bamlutils.BuildRequestConfig {
	return bamlutils.BuildRequestConfig{}
}
func (a *fakeAdapter) SetRoundRobinAdvancer(bamlutils.RoundRobinAdvancer)   {}
func (a *fakeAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer     { return nil }

// TestPoolLifecycle drives the sync cell table.
func TestPoolLifecycle(t *testing.T) {
	for _, cell := range SyncLifecycleCells {
		cell := cell
		t.Run(cell.Name, func(t *testing.T) {
			poolaudit.Reset()
			t.Cleanup(poolaudit.Reset)

			failAt := pickFailAt(cell.FailMode)
			adapter := newFakeAdapter(failAt)
			rawInput := buildInputFor(cell.InputType, 3)

			err := cell.Dispatch(adapter, rawInput)
			if cell.ExpectError && err == nil {
				t.Errorf("cell %q (failMode=%s): expected error, got nil", cell.Name, cell.FailMode)
			}
			if !cell.ExpectError && err != nil {
				t.Errorf("cell %q (failMode=%s): expected success, got %v", cell.Name, cell.FailMode, err)
			}

			if imbalances := poolaudit.Imbalanced(); len(imbalances) > 0 {
				t.Errorf("cell %q: pool imbalance: %v", cell.Name, imbalances)
			}
			if violations := poolaudit.ZeroOnPutViolations(); len(violations) > 0 {
				t.Errorf("cell %q: zero-on-Put violations: %v", cell.Name, violations)
			}
			if cell.ExpectNoPoolForType != "" {
				snap := poolaudit.Snapshot()
				if _, ok := snap.Checkouts[cell.ExpectNoPoolForType]; ok {
					t.Errorf("cell %q: expected no pool activity for type %q, got %v", cell.Name, cell.ExpectNoPoolForType, snap.Checkouts)
				}
			}
		})
	}
}

// pickFailAt maps the failure-mode tag to the 1-based ConvertMedia
// call index the fake adapter fails on. 0 disables injection.
func pickFailAt(failMode string) int {
	switch failMode {
	case "fail_at_idx_0":
		return 1
	case "fail_at_idx_2":
		return 3
	case "fail_in_nested":
		// Cells with nested converters exercise their inner pools
		// via ConvertMedia too — failing on call 3 falls inside a
		// per-message inner loop for the 3-element builder.
		return 3
	}
	return 0
}

// TestPoolLifecycle_Async drives the async barrier cells.
func TestPoolLifecycle_Async(t *testing.T) {
	for _, cell := range AsyncLifecycleCells {
		cell := cell
		t.Run(cell.Name, func(t *testing.T) {
			poolaudit.Reset()
			t.Cleanup(poolaudit.Reset)

			adapter := newAsyncFakeAdapter()
			rawInput := buildInputFor(cell.InputType, 3)

			done := make(chan struct{})
			err := cell.Dispatch(adapter, rawInput, done)
			if err != nil {
				t.Fatalf("cell %q: async dispatch error: %v", cell.Name, err)
			}

			<-adapter.asyncStart

			snap := poolaudit.Snapshot()
			totalReleases := 0
			for _, n := range snap.Releases {
				totalReleases += n
			}
			// For b_ptr_elem cells the nested pool isn't touched,
			// but the outer MessageB pool still is. The barrier
			// invariant is: no RELEASE has fired (still in the
			// goroutine waiting). totalReleases must be 0 in both
			// shapes — ExpectNoPoolForType is incidental here.
			if totalReleases > 0 {
				t.Errorf("cell %q: release fired before async barrier signal: releases=%v", cell.Name, snap.Releases)
			}

			close(adapter.asyncCont)
			<-done

			if imbalances := poolaudit.Imbalanced(); len(imbalances) > 0 {
				t.Errorf("cell %q: pool imbalance after async dispatch: %v", cell.Name, imbalances)
			}
			if violations := poolaudit.ZeroOnPutViolations(); len(violations) > 0 {
				t.Errorf("cell %q: zero-on-Put violations: %v", cell.Name, violations)
			}
		})
	}
}

// buildInputFor reflects over the cell's input struct and populates
// it with sliceN elements per slice field, each leaf
// *bamlutils.MediaInput pointing at a stub URL so the generated
// converter's ConvertMedia call returns a non-nil image.
func buildInputFor(t reflect.Type, sliceN int) any {
	v := reflect.New(t)
	buildMirrorValue(v.Elem(), sliceN, 0)
	return v.Interface()
}

// maxBuildDepth is the recursion cap for the reflect-driven input
// builder. Set tight enough to cut the mutually-recursive cycle
// fixture (CycleA → CycleB → CycleA) at the second hop while still
// reaching the leaf *bamlutils.MediaInput pointer in the deeper
// pooled-nested shapes. The depth budget counts both
// populateField -> buildMirrorValue and populateSliceElem ->
// buildMirrorValue transitions, so a value: Input(0) → Messages
// field(1) → MessageA elem(2) → Parts field(3) → ContentPartA
// elem(4) → Img field(5) reaches the media leaf only when the cap
// is >= 4.
const maxBuildDepth = 4

func buildMirrorValue(v reflect.Value, sliceN, depth int) {
	if depth > maxBuildDepth {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		populateField(v.Field(i), sliceN, depth+1)
	}
}

func populateField(fv reflect.Value, sliceN, depth int) {
	t := fv.Type()
	switch t.Kind() {
	case reflect.Ptr:
		elem := t.Elem()
		switch elem.Kind() {
		case reflect.Slice:
			slice := reflect.MakeSlice(elem, sliceN, sliceN)
			for i := 0; i < sliceN; i++ {
				populateSliceElem(slice.Index(i), depth)
			}
			ptr := reflect.New(elem)
			ptr.Elem().Set(slice)
			fv.Set(ptr)
		case reflect.Struct:
			if isMediaInputType(elem) {
				url := "https://example.test/img.png"
				fv.Set(reflect.ValueOf(&bamlutils.MediaInput{URL: &url}))
				return
			}
			ptr := reflect.New(elem)
			buildMirrorValue(ptr.Elem(), sliceN, depth)
			fv.Set(ptr)
		}
	case reflect.Slice:
		slice := reflect.MakeSlice(t, sliceN, sliceN)
		for i := 0; i < sliceN; i++ {
			populateSliceElem(slice.Index(i), depth)
		}
		fv.Set(slice)
	case reflect.Struct:
		if isMediaInputType(t) {
			url := "https://example.test/img.png"
			fv.Set(reflect.ValueOf(bamlutils.MediaInput{URL: &url}))
			return
		}
		buildMirrorValue(fv, sliceN, depth)
	}
}

func populateSliceElem(ev reflect.Value, depth int) {
	switch ev.Kind() {
	case reflect.Ptr:
		elem := ev.Type().Elem()
		ptr := reflect.New(elem)
		if elem.Kind() == reflect.Struct {
			buildMirrorValue(ptr.Elem(), 1, depth+1)
		}
		ev.Set(ptr)
	case reflect.Struct:
		buildMirrorValue(ev, 1, depth+1)
	}
}

func isMediaInputType(t reflect.Type) bool {
	return t.Name() == "MediaInput" && t.PkgPath() == "github.com/invakid404/baml-rest/bamlutils"
}
`
