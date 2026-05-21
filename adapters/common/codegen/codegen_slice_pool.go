package codegen

import (
	"reflect"
	"sync"

	"github.com/dave/jennifer/jen"
)

// poolAuditPkg is the import path of the test-only poolaudit hooks
// package. Referenced only when slicePoolTracker.audit is true, which
// only the lifecycle harness flips. Sits behind a constant so the
// rendered import is identical across cells without threading a
// PackageConfig field through every test.
const poolAuditPkg = "github.com/invakid404/baml-rest/adapters/common/codegen/internal/poolaudit"

// slicePoolNames captures the symbol names emitted for a single pooled
// slice type. Tracked centrally so emitters and call sites stay in
// lockstep; one entry covers `[]T` where T is the unwrapped BAML
// struct type (e.g. types.Baml_Rest_Message).
type slicePoolNames struct {
	poolVar   string
	getFunc   string
	putFunc   string
	elemQual  *jen.Statement
	elemRefl  reflect.Type
	maxCap    int
	emittedAt int
}

// slicePoolTracker dedupes per-file pool emission for each pooled
// inner type. The lifecycle is:
//
//  1. emitter encounters a slice-of-struct conversion that benefits
//     from pooling and calls `ensure`, which lazily emits the
//     `sync.Pool` variable + `get/put` helpers into `out`.
//  2. emitter consumers (nested struct conversion, slice-param
//     preamble) read the returned names to assemble call-site code.
//
// Names are deterministic functions of the unwrapped BAML type name so
// repeat encounters reuse the same helpers across the file.
type slicePoolTracker struct {
	mu      sync.Mutex
	entries map[reflect.Type]*slicePoolNames
	emitOrd int
	pkgs    PackageConfig
	jenSync string
	// audit mirrors Options.EmitPoolAuditHooks. When true, ensure()
	// emits poolaudit.OnCheckout / CheckZeroPrePut / OnRelease calls
	// inside the generated get/put helpers so the lifecycle harness
	// can observe pool activity and zero-loop coverage. Production
	// codegen leaves this false, in which case helper emission is
	// byte-identical to the pre-audit baseline.
	audit bool
	// seedOmitZeroLoop mirrors Options.Seed_OmitZeroLoop — a
	// test-only switch the lifecycle harness flips to verify
	// CheckZeroPrePut still catches a missing zero loop in putXSlice.
	// Set only by the regression-seed renderer.
	seedOmitZeroLoop bool
}

func newSlicePoolTracker(pkgs PackageConfig, audit bool) *slicePoolTracker {
	return &slicePoolTracker{
		entries: make(map[reflect.Type]*slicePoolNames),
		pkgs:    pkgs,
		jenSync: "sync",
		audit:   audit,
	}
}

// ensure registers `elemType` for pooling with the supplied retention
// cap. On the first call for a given type, it emits the pool variable
// + get/put helpers into `out`. Subsequent calls are no-ops returning
// the same names. `maxCap` lower bounds at 1 — non-positive values are
// clamped so a misconfiguration cannot disable the cap check entirely.
func (t *slicePoolTracker) ensure(out *jen.File, elemType reflect.Type, maxCap int) *slicePoolNames {
	if elemType == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if existing, ok := t.entries[elemType]; ok {
		return existing
	}

	if maxCap < 1 {
		maxCap = 1
	}

	baseName := slicePoolBaseName(elemType)
	names := &slicePoolNames{
		poolVar:   baseName + "SlicePool",
		getFunc:   "get" + baseName + "Slice",
		putFunc:   "put" + baseName + "Slice",
		elemQual:  parseReflectType(elemType).statement,
		elemRefl:  elemType,
		maxCap:    maxCap,
		emittedAt: t.emitOrd,
	}
	t.emitOrd++
	t.entries[elemType] = names

	// var <pool> sync.Pool
	out.Var().Id(names.poolVar).Qual(t.jenSync, "Pool")

	// auditTypeLiteral is the literal type-name string the audit
	// hooks key counters on. Element type's plain name (no package
	// qualifier) matches what slicePoolBaseName already uses, so an
	// imbalance report points at the same name a developer sees in
	// the generated helper symbols.
	auditTypeLiteral := elemType.Name()

	// func get<Name>Slice(n int) *[]Elem { ... }
	getBody := []jen.Code{}
	if t.audit {
		getBody = append(getBody, jen.Qual(poolAuditPkg, "OnCheckout").Call(jen.Lit(auditTypeLiteral)))
	}
	getBody = append(getBody,
		jen.If(jen.Id("v").Op(":=").Id(names.poolVar).Dot("Get").Call(), jen.Id("v").Op("!=").Nil()).Block(
			jen.Id("sp").Op(":=").Id("v").Assert(jen.Op("*").Index().Add(names.elemQual.Clone())),
			jen.If(jen.Cap(jen.Op("*").Id("sp")).Op(">=").Id("n")).Block(
				jen.Op("*").Id("sp").Op("=").Parens(jen.Op("*").Id("sp")).Index(jen.Empty(), jen.Lit(0)),
				jen.Return(jen.Id("sp")),
			),
		),
		jen.Id("s").Op(":=").Make(jen.Index().Add(names.elemQual.Clone()), jen.Lit(0), jen.Id("n")),
		jen.Return(jen.Op("&").Id("s")),
	)
	out.Func().Id(names.getFunc).
		Params(jen.Id("n").Int()).
		Op("*").Index().Add(names.elemQual.Clone()).
		Block(getBody...)

	// func put<Name>Slice(sp *[]Elem) { ... }
	putBody := []jen.Code{
		jen.If(jen.Id("sp").Op("==").Nil()).Block(jen.Return()),
		jen.If(jen.Cap(jen.Op("*").Id("sp")).Op(">").Lit(maxCap)).Block(jen.Return()),
		jen.Id("used").Op(":=").Parens(jen.Op("*").Id("sp")).Index(jen.Lit(0), jen.Len(jen.Op("*").Id("sp")), jen.Cap(jen.Op("*").Id("sp"))),
	}
	if !t.seedOmitZeroLoop {
		// Production path. The seed flips this off so the lifecycle
		// harness can verify CheckZeroPrePut catches the missing
		// loop — every non-zero element in `used` becomes a
		// recorded violation.
		putBody = append(putBody,
			jen.For(jen.Id("i").Op(":=").Range().Id("used")).Block(
				jen.Id("used").Index(jen.Id("i")).Op("=").Add(names.elemQual.Clone()).Values(),
			),
		)
	}
	if t.audit {
		// CheckZeroPrePut runs AFTER the zero loop and BEFORE pool.Put
		// so a violation surfaces precisely when the loop is missing
		// or fails to cover a slot. The seed that omits the loop
		// makes every non-zero slot light up.
		putBody = append(putBody, jen.Qual(poolAuditPkg, "CheckZeroPrePut").Call(jen.Lit(auditTypeLiteral), jen.Id("used")))
	}
	putBody = append(putBody,
		jen.Op("*").Id("sp").Op("=").Parens(jen.Op("*").Id("sp")).Index(jen.Empty(), jen.Lit(0)),
		jen.Id(names.poolVar).Dot("Put").Call(jen.Id("sp")),
	)
	if t.audit {
		// OnRelease pairs 1:1 with OnCheckout. Placed after pool.Put
		// so a non-trivial failure between the zero loop and the
		// pool handoff still leaves the imbalance visible.
		putBody = append(putBody, jen.Qual(poolAuditPkg, "OnRelease").Call(jen.Lit(auditTypeLiteral)))
	}
	out.Func().Id(names.putFunc).
		Params(jen.Id("sp").Op("*").Index().Add(names.elemQual.Clone())).
		Block(putBody...)

	return names
}

func slicePoolBaseName(t reflect.Type) string {
	name := t.Name()
	if name == "" {
		// Fall back to a sanitized full string when the type has no
		// declared name (anonymous types should not reach here in the
		// generated paths, but guard so the codegen does not panic).
		name = "Anon"
	}
	return lowerFirst(name)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] = r[0] + ('a' - 'A')
	}
	return string(r)
}
