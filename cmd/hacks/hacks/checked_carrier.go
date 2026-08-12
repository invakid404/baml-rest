package hacks

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	Register(&CheckedCarrierHack{})
}

// bamlutilsPkgPath is the module path of the de-BAML carrier package the checked
// alias is re-pointed at.
const bamlutilsPkgPath = "github.com/invakid404/baml-rest/bamlutils"

// checkedCarrierEnv opts a generated client into the re-point. It is set ONLY by
// scripts/regen-staticserve-fixture.sh, for the STATIC serve fixture.
const checkedCarrierEnv = "BAML_HACKS_BAMLUTILS_CHECKED"

// CheckedCarrierHack re-points a generated client's `Checked[T]` alias from stock
// baml_go's carrier to [bamlutils.Checked] (de-BAML Slice 7.2b-2).
//
// # What it rewrites
//
// BAML v0.223.0's Go generator emits, in the client's types package:
//
//	type Checked[T any] = baml.Checked[T]
//
// and lowers a `@check`-bearing field to that alias, so a class like
// `StaticCheckedAnswer { answer string; confidence int @check(...) }` generates
// `Confidence Checked[int64]`. This hack rewrites the ALIAS TARGET — and nothing
// else — to `bamlutils.Checked[T]`, so every generated checked field resolves to the
// de-BAML carrier without the field declarations themselves being touched. That is
// the "generated alias to it" the 7.2b scope permits, applied at the one declaration
// that decides it.
//
// # Why the alias, and not the fields
//
// The two carriers have the SAME public shape and the SAME stock JSON names, so a
// field's declaration is identical either way. What differs is the BYTES: stock's
// plain struct under sonic (the worker's serializer) emits `checks` in Go map
// iteration order, which differs run to run, while [bamlutils.Checked] writes
// `value` then `checks` with the check keys in the recorded declaration order. A
// native claim has to be byte-reproducible, so the generated static path must carry
// the deterministic one.
//
// # Why it is OPT-IN
//
// Only the STATIC serve fixture needs it. The dynamic client (dynclient) has no
// constraint channel at all — DynamicOutputSchema cannot express a `@check` (the
// #572 ceiling) — so its `Checked` alias is unreachable, and rewriting it would
// churn a generated artifact for no behavioural reason. The hack therefore applies
// only when [checkedCarrierEnv] is set, exactly like the stock-static-map-decode
// selector next to it.
type CheckedCarrierHack struct{}

func (h *CheckedCarrierHack) Name() string { return "bamlutils-checked-carrier" }

func (h *CheckedCarrierHack) MinVersion() string { return "" }

func (h *CheckedCarrierHack) MaxVersion() string { return "" }

// CheckedCarrierBridgeFile is the helper file this hack ADDS to the client's types
// package. It is named here so the regeneration script can delete it before re-running
// BAML's generator, which refuses to run over a directory containing a file it did not
// itself produce.
const CheckedCarrierBridgeFile = "checked_carrier_bridge.go"

// Apply performs the three coordinated rewrites the re-point needs.
//
// The alias alone is not enough, and the reason is a hard constraint rather than a
// preference: stock's CFFI decoder builds a checked value by REFLECTION over the type
// the client's type map registers, and it always fills the `Checks` field with a
// `map[string]shared.Check` (baml_go/serde/decode.go decodeCheckedValue). That map is
// not assignable to [bamlutils.Checked]'s `map[string]bamlutils.Check`, so a client
// whose registered checked type were the de-BAML carrier would PANIC inside the CFFI
// callback the moment BAML parsed a checked value — and BAML is exactly what serves
// these routes while the checked-static seam is closed.
//
// So the three rewrites keep each decoder on the type it can actually build, while the
// FIELD — the thing the native static decode targets and the thing that gets
// serialized to the wire — becomes the de-BAML carrier:
//
//  1. `type Checked[T any] = baml.Checked[T]` becomes `= bamlutils.Checked[T]`, which
//     is what makes every generated `@check`-bearing field the de-BAML carrier;
//  2. the client's CHECKED_TYPES type-map entries are re-pointed at `StockChecked[T]`
//     (a preserved alias for stock's carrier), so stock's reflective decoder keeps
//     building the shape it hardcodes; and
//  3. each generated checked-field decode CONVERTS stock's decoded carrier into the
//     de-BAML one, so the assignment is well-typed and BAML's parse still works.
//
// It is a NO-OP unless opted in, and it FAILS when opted in but nothing was rewritten:
// a silently-skipped re-point would leave the generated static path on stock's
// non-deterministic carrier while every downstream proof kept passing against a type it
// no longer describes.
func (h *CheckedCarrierHack) Apply(bamlClientDir string) error {
	if os.Getenv(checkedCarrierEnv) != "1" {
		fmt.Printf("  Skipped (%s is not set; the dynamic client has no constraint channel)\n", checkedCarrierEnv)
		return nil
	}
	aliases, decodes, registrations := 0, 0, 0
	typesDir := ""
	err := filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		a, dc, rg, perr := h.processFile(path)
		if perr != nil {
			return fmt.Errorf("processing %s: %w", path, perr)
		}
		if a > 0 {
			typesDir = filepath.Dir(path)
		}
		aliases += a
		decodes += dc
		registrations += rg
		if a+dc+rg > 0 {
			fmt.Printf("  Modified (bamlutils-checked-carrier): %s\n", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if aliases == 0 {
		return fmt.Errorf("%s=1 but no `type Checked[T any] = <pkg>.Checked[T]` alias was found under %s; "+
			"the generated static path would silently keep stock's non-deterministic carrier",
			checkedCarrierEnv, bamlClientDir)
	}
	// A client that declares the alias but decodes NO checked field would compile and
	// serve nothing checked; a client that decodes one but registers nothing would
	// panic in the CFFI callback. Requiring both together is what keeps the three
	// rewrites in step rather than leaving a half-applied client that only fails at
	// runtime.
	if decodes == 0 || registrations == 0 {
		return fmt.Errorf("%s=1 rewrote %d alias(es) but %d checked-field decode(s) and %d type-map "+
			"registration(s); all three must move together or BAML's own decoder panics on a checked value",
			checkedCarrierEnv, aliases, decodes, registrations)
	}
	return h.writeBridge(typesDir)
}

// writeBridge emits the converter the rewritten decodes call. It lives in the client's
// types package (beside the alias) so both the value and pointer forms can name it.
func (h *CheckedCarrierHack) writeBridge(typesDir string) error {
	if typesDir == "" {
		return fmt.Errorf("the Checked alias was rewritten but its package directory was not recorded")
	}
	const src = `// Code generated by cmd/hacks (bamlutils-checked-carrier); DO NOT EDIT.

package types

import (
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/bamlutils"
)

// StockChecked is stock BAML v0.223.0's own constraint carrier, preserved under a
// distinct name because the generated ` + "`Checked`" + ` alias now points at the de-BAML
// carrier.
//
// It is what the client's type map registers: stock's reflective CFFI decoder builds a
// checked value by setting a ` + "`map[string]shared.Check`" + ` into the registered type's
// ` + "`Checks`" + ` field, which only this shape accepts.
type StockChecked[T any] = baml.Checked[T]

// FromStockChecked converts stock's decoded carrier into the de-BAML one.
//
// NO ORDER IS INVENTED. Stock's carrier is a map, so the declaration order the CFFI
// list carried is already gone by the time this runs; the result therefore carries no
// recorded order and [bamlutils.Checked.MarshalJSON] takes its documented deterministic
// fallback (lexicographic by label) rather than Go's randomised map iteration. That is
// the whole point of the re-point: BAML's own parse of a checked value now serializes
// deterministically too.
func FromStockChecked[T any](in StockChecked[T]) Checked[T] {
	checks := make(map[string]bamlutils.Check, len(in.Checks))
	for label, c := range in.Checks {
		checks[label] = bamlutils.Check{Name: c.Name, Expression: c.Expression, Status: c.Status}
	}
	return Checked[T]{Value: in.Value, Checks: checks}
}

// FromStockCheckedPtr is [FromStockChecked] for the nullable/streaming pointer form. A
// nil input stays nil: an absent partial is not an empty carrier.
func FromStockCheckedPtr[T any](in *StockChecked[T]) *Checked[T] {
	if in == nil {
		return nil
	}
	out := FromStockChecked(*in)
	return &out
}
`
	return os.WriteFile(filepath.Join(typesDir, CheckedCarrierBridgeFile), []byte(src), 0o644)
}

// processFile applies whichever of the three rewrites this file needs, reporting how
// many of each it made.
func (h *CheckedCarrierHack) processFile(path string) (aliases, decodes, registrations int, err error) {
	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if perr != nil {
		return 0, 0, 0, fmt.Errorf("parsing file: %w", perr)
	}

	aliases = h.repointAlias(file)
	decodes = h.convertCheckedDecodes(file)
	registrations = h.repointTypeMap(file)
	if aliases+decodes+registrations == 0 {
		return 0, 0, 0, nil
	}
	if aliases > 0 {
		EnsureImport(file, bamlutilsPkgPath)
	}

	out, cerr := os.Create(path)
	if cerr != nil {
		return 0, 0, 0, fmt.Errorf("creating output file: %w", cerr)
	}
	defer out.Close()
	if werr := printer.Fprint(out, fset, file); werr != nil {
		return 0, 0, 0, fmt.Errorf("writing file: %w", werr)
	}
	return aliases, decodes, registrations, nil
}

// repointAlias rewrites `type Checked[T any] = <pkg>.Checked[T]` to name bamlutils.
//
// It rewrites an ALIAS only (`=`): a defined type would be a different declaration with
// different semantics, and re-pointing it would change what the generator meant rather
// than where it points.
func (h *CheckedCarrierHack) repointAlias(file *ast.File) int {
	n := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Checked" || !ts.Assign.IsValid() {
				continue
			}
			idx, ok := ts.Type.(*ast.IndexExpr)
			if !ok {
				continue
			}
			sel, ok := idx.X.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Checked" {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name == "bamlutils" {
				continue // absent, or already re-pointed: the transform is idempotent
			}
			pkg.Name = "bamlutils"
			n++
		}
	}
	return n
}

// convertCheckedDecodes rewrites each generated checked-field decode so stock's
// reflective decoder keeps producing stock's carrier and the CONVERTED value is what
// reaches the field:
//
//	c.F = baml.Decode(v).Interface().(Checked[int64])
//	  -> c.F = FromStockChecked(baml.Decode(v).Interface().(StockChecked[int64]))
//
//	c.F = baml.Decode(v).Interface().(*types.Checked[int64])
//	  -> c.F = types.FromStockCheckedPtr(baml.Decode(v).Interface().(*types.StockChecked[int64]))
//
// The generator emits every field decode as a plain assignment, so walking assignments
// is enough and is precise: the asserted TYPE selects the form, and a field of any
// other type is untouched.
func (h *CheckedCarrierHack) convertCheckedDecodes(file *ast.File) int {
	n := 0
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			wrapped, converted := h.convertCheckedAssert(rhs)
			if converted {
				assign.Rhs[i] = wrapped
				n++
			}
		}
		return true
	})
	return n
}

// convertCheckedAssert returns expr wrapped in the converter when it is a type
// assertion to the (now de-BAML) `Checked` alias, having first re-pointed the asserted
// type at the preserved `StockChecked` one.
func (h *CheckedCarrierHack) convertCheckedAssert(expr ast.Expr) (ast.Expr, bool) {
	assert, ok := expr.(*ast.TypeAssertExpr)
	if !ok || assert.Type == nil {
		return expr, false
	}
	pointer := false
	target := assert.Type
	if star, isStar := target.(*ast.StarExpr); isStar {
		pointer = true
		target = star.X
	}
	idx, ok := target.(*ast.IndexExpr)
	if !ok {
		return expr, false
	}
	// `Checked[T]` (declared in this package) or `types.Checked[T]` (the stream
	// package, which qualifies it).
	var qualifier *ast.Ident
	switch base := idx.X.(type) {
	case *ast.Ident:
		if base.Name != "Checked" {
			return expr, false
		}
		base.Name = "StockChecked"
	case *ast.SelectorExpr:
		if base.Sel.Name != "Checked" {
			return expr, false
		}
		pkg, isIdent := base.X.(*ast.Ident)
		if !isIdent {
			return expr, false
		}
		qualifier = ast.NewIdent(pkg.Name)
		base.Sel.Name = "StockChecked"
	default:
		return expr, false
	}

	converter := "FromStockChecked"
	if pointer {
		converter = "FromStockCheckedPtr"
	}
	var fun ast.Expr = ast.NewIdent(converter)
	if qualifier != nil {
		fun = &ast.SelectorExpr{X: qualifier, Sel: ast.NewIdent(converter)}
	}
	return &ast.CallExpr{Fun: fun, Args: []ast.Expr{assert}}, true
}

// repointTypeMap rewrites the client's CHECKED_TYPES registrations from the (now
// de-BAML) `Checked` alias to the preserved `StockChecked` one.
//
// This is the registration stock's decodeCheckedValue reflects over, so it must keep
// naming the shape stock can build; the FIELD the decoded value is assigned to is what
// the alias re-point changed. The registrations are package-level `var` entries rather
// than statements, so this walks the whole file.
func (h *CheckedCarrierHack) repointTypeMap(file *ast.File) int {
	n := 0
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok || lit.Type == nil {
			return true
		}
		idx, ok := lit.Type.(*ast.IndexExpr)
		if !ok {
			return true
		}
		sel, ok := idx.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Checked" {
			return true
		}
		sel.Sel.Name = "StockChecked"
		n++
		return true
	})
	return n
}
