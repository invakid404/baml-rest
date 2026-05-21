package codegen

import (
	"reflect"
	"sync"

	"github.com/dave/jennifer/jen"
)

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
	mu       sync.Mutex
	entries  map[reflect.Type]*slicePoolNames
	emitOrd  int
	pkgs     PackageConfig
	jenSync  string
}

func newSlicePoolTracker(pkgs PackageConfig) *slicePoolTracker {
	return &slicePoolTracker{
		entries: make(map[reflect.Type]*slicePoolNames),
		pkgs:    pkgs,
		jenSync: "sync",
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

	// func get<Name>Slice(n int) *[]Elem { ... }
	out.Func().Id(names.getFunc).
		Params(jen.Id("n").Int()).
		Op("*").Index().Add(names.elemQual.Clone()).
		Block(
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

	// func put<Name>Slice(sp *[]Elem) { ... }
	out.Func().Id(names.putFunc).
		Params(jen.Id("sp").Op("*").Index().Add(names.elemQual.Clone())).
		Block(
			jen.If(jen.Id("sp").Op("==").Nil()).Block(jen.Return()),
			jen.If(jen.Cap(jen.Op("*").Id("sp")).Op(">").Lit(maxCap)).Block(jen.Return()),
			jen.Id("used").Op(":=").Parens(jen.Op("*").Id("sp")).Index(jen.Lit(0), jen.Len(jen.Op("*").Id("sp")), jen.Cap(jen.Op("*").Id("sp"))),
			jen.For(jen.Id("i").Op(":=").Range().Id("used")).Block(
				jen.Id("used").Index(jen.Id("i")).Op("=").Add(names.elemQual.Clone()).Values(),
			),
			jen.Op("*").Id("sp").Op("=").Parens(jen.Op("*").Id("sp")).Index(jen.Empty(), jen.Lit(0)),
			jen.Id(names.poolVar).Dot("Put").Call(jen.Id("sp")),
		)

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
