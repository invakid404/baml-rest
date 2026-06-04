package hacks

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func init() {
	Register(&DynamicOrderClientHack{})
}

// DynamicOrderClientHack rewrites generated @@dynamic client structs so
// `DynamicProperties` carries an ordered field map, not a Go map.
// Combined with the BAML-side ApplyDynamicOrderFix patch (serde
// DynamicClass.Fields + DecodeToOrderedValue), the client surfaces CFFI
// insertion order from the runtime up to baml-rest's unwrap pass.
//
// Targeted files: every .go file under baml_client/ that declares a
// struct field literally named `DynamicProperties` of type
// `map[string]any`. The hack edits the field type, the make-init, each
// index-assign in the Decode body, and the EncodeClass call site that
// passes the field by pointer.
type DynamicOrderClientHack struct{}

func (h *DynamicOrderClientHack) Name() string {
	return "dynamic-order-client-fix"
}

func (h *DynamicOrderClientHack) MinVersion() string {
	return ""
}

func (h *DynamicOrderClientHack) MaxVersion() string {
	return ""
}

func (h *DynamicOrderClientHack) Apply(bamlClientDir string) error {
	if err := filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		changed, applyErr := patchGeneratedDynamicFile(path)
		if applyErr != nil {
			return fmt.Errorf("processing %s: %w", path, applyErr)
		}
		if changed {
			fmt.Printf("  Modified (dynamic-order-client): %s\n", path)
		}
		return nil
	}); err != nil {
		return err
	}

	// Static-map pass (issue #366). Rewrites every concrete
	// `map[string]T` field/return/variant under types/, stream_types/,
	// unions, and top-level functions*.go into `baml.OrderedMap[T]`,
	// and replaces the `.Interface().(map[string]T)` cast emitted by
	// BAML's Decode pipeline with a generated typed conversion helper
	// that rebuilds the ordered carrier without losing CFFI key order.
	// Skips dynamic surfaces (`DynamicProperties`, `map[string]any`)
	// because they already flow through the OrderedFields path.
	//
	// The pass also detects recursive type aliases: when a map's value
	// type is a type alias whose transitive closure references the same
	// alias in another map element position (e.g. the integration
	// `JsonValue` alias union with a `map<string, JsonValue>` arm), the
	// rewrite to `baml.OrderedMap[T]` would trip a Go compiler ICE on
	// the recursive generic instantiation. Those arms are left as
	// native `map[string]T`; their decode/assert sites instead route
	// through a `bamlNativeMapAs_*` helper that converts the patched
	// ordered runtime carrier back to an unordered native map.
	pkgIndexCache := map[string]*pkgTypeIndex{}
	return filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		dir := filepath.Dir(path)
		idx, cached := pkgIndexCache[dir]
		if !cached {
			built, buildErr := newPkgTypeIndex(dir)
			if buildErr != nil {
				return fmt.Errorf("building type index for %s: %w", dir, buildErr)
			}
			idx = built
			pkgIndexCache[dir] = idx
		}
		changed, applyErr := patchGeneratedStaticMapFile(path, idx)
		if applyErr != nil {
			return fmt.Errorf("processing static-map pass on %s: %w", path, applyErr)
		}
		if changed {
			fmt.Printf("  Modified (static-map): %s\n", path)
		}
		return nil
	})
}

// patchGeneratedDynamicFile applies the DynamicProperties rewrite to a
// single generated file. The function returns false when the file has
// no DynamicProperties declaration, so the walker prints a clean log
// line only for files the hack actually rewrote. Text-level edits are
// used because the patterns are short, file-local, and the AST view of
// `map[string]any` and `baml.OrderedFields` would otherwise require
// rewriting selector expressions through reflection — the text
// approach keeps the rewrite auditable.
func patchGeneratedDynamicFile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	body := string(raw)

	if !strings.Contains(body, "DynamicProperties") {
		return false, nil
	}
	if !strings.Contains(body, "DynamicProperties map[string]any") {
		// Already patched, or the field is shaped differently in this
		// file. Avoid a partial rewrite that mixes types.
		return false, nil
	}

	original := body

	// 1. Field type: keep the struct tag (if any) intact.
	body = strings.ReplaceAll(
		body,
		"DynamicProperties map[string]any",
		"DynamicProperties baml.OrderedFields",
	)

	// 2. Allocation: support both forms BAML emits.
	body = strings.ReplaceAll(
		body,
		"DynamicProperties = make(map[string]any)",
		"DynamicProperties = baml.NewOrderedFields(0)",
	)
	body = replaceMakeWithCapacity(body)

	// 3. Index assignments in the Decode body. The BAML codegen only
	// writes through c.DynamicProperties[key] in a small set of shapes;
	// matching by suffix on the right-hand side keeps the rewrite
	// pinned to the assignment pattern so any sub-expression that
	// merely mentions DynamicProperties is left alone.
	body = rewriteDecodeAssignments(body)

	// 4. EncodeClass call site: the generated method passes
	// &c.DynamicProperties; route through EncodeClassOrdered which
	// accepts *baml.OrderedFields directly.
	body = strings.ReplaceAll(
		body,
		"baml.EncodeClass(",
		"baml.EncodeClass(",
	)
	body = rewriteEncodeClassCall(body)

	if body == original {
		return false, nil
	}

	// Validate that the patched file still parses; without this a
	// failed regex would silently produce a broken Go file.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, path, []byte(body), parser.ParseComments); err != nil {
		return false, fmt.Errorf("patched file does not parse: %w", err)
	}

	// Run gofmt-style pretty-print to keep diff noise low when this
	// file is later re-read by other hacks.
	prettied, err := gofmtBytes([]byte(body))
	if err != nil {
		return false, fmt.Errorf("gofmt patched file: %w", err)
	}

	if err := os.WriteFile(path, prettied, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// replaceMakeWithCapacity rewrites `DynamicProperties = make(map[string]any, N)`
// (any expression N) into `DynamicProperties = baml.NewOrderedFields(N)`.
// The previous string replace handles the capacity-less form; this one
// captures the capacity-bearing form BAML emits when the codegen has a
// hint.
func replaceMakeWithCapacity(body string) string {
	const marker = "DynamicProperties = make(map[string]any,"
	idx := 0
	for {
		j := strings.Index(body[idx:], marker)
		if j < 0 {
			break
		}
		start := idx + j
		// Locate the matching ')'.
		open := start + len(marker)
		depth := 1
		k := open
		for k < len(body) && depth > 0 {
			switch body[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
			k++
		}
		if depth != 0 {
			break
		}
		capArg := strings.TrimSpace(body[open : k-1])
		replacement := fmt.Sprintf("DynamicProperties = baml.NewOrderedFields(%s)", capArg)
		body = body[:start] + replacement + body[k:]
		idx = start + len(replacement)
	}
	return body
}

// rewriteDecodeAssignments rewrites
//
//	c.DynamicProperties[key] = baml.DecodeToValue(valueHolder)
//	c.DynamicProperties[key] = baml.Decode(valueHolder).Interface()
//	c.DynamicProperties[key] = <expr>
//
// into a single ordered-Set form that routes the value through
// DecodeToOrderedValue when the original expression was a Decode-style
// call, and leaves any other right-hand side untouched apart from the
// container call shape.
func rewriteDecodeAssignments(body string) string {
	const lhsMarker = ".DynamicProperties["
	idx := 0
	for {
		j := strings.Index(body[idx:], lhsMarker)
		if j < 0 {
			break
		}
		start := idx + j
		// Walk backwards to find the start of the receiver expression
		// (look for the closest preceding whitespace).
		recvStart := start
		for recvStart > 0 {
			c := body[recvStart-1]
			if c == ' ' || c == '\t' || c == '\n' {
				break
			}
			recvStart--
		}
		recv := body[recvStart:start]
		// Locate ']' that closes the index expression.
		keyOpen := start + len(lhsMarker)
		keyClose := -1
		depth := 1
		k := keyOpen
		for k < len(body) && depth > 0 {
			switch body[k] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					keyClose = k
				}
			}
			if depth == 0 {
				break
			}
			k++
		}
		if keyClose < 0 {
			break
		}
		// Skip whitespace and '=' to get the assignment value.
		eq := keyClose + 1
		for eq < len(body) && (body[eq] == ' ' || body[eq] == '\t') {
			eq++
		}
		if eq >= len(body) || body[eq] != '=' {
			idx = keyClose + 1
			continue
		}
		eq++
		for eq < len(body) && (body[eq] == ' ' || body[eq] == '\t') {
			eq++
		}
		// Locate the end of the assignment expression: the statement
		// ends at '\n' (no inline `;` in the generated code we patch).
		eol := strings.IndexByte(body[eq:], '\n')
		if eol < 0 {
			eol = len(body) - eq
		}
		rhs := strings.TrimSpace(body[eq : eq+eol])

		key := strings.TrimSpace(body[keyOpen:keyClose])
		newRHS := convertAssignmentRHS(rhs)
		replacement := fmt.Sprintf("%s.DynamicProperties.Set(%s, %s)", recv, key, newRHS)

		body = body[:recvStart] + replacement + body[eq+eol:]
		idx = recvStart + len(replacement)
	}
	return body
}

// convertAssignmentRHS rewrites known Decode-style right-hand sides
// into their ordered counterparts. Patterns left untouched fall
// through verbatim — the generator only emits a small set of shapes
// for DynamicProperties assignment.
func convertAssignmentRHS(rhs string) string {
	const dtv = "baml.DecodeToValue("
	if strings.HasPrefix(rhs, dtv) {
		return "baml.DecodeToOrderedValue(" + rhs[len(dtv):]
	}
	const decodePrefix = "baml.Decode("
	const decodeSuffix = ").Interface()"
	if strings.HasPrefix(rhs, decodePrefix) && strings.HasSuffix(rhs, decodeSuffix) {
		inner := rhs[len(decodePrefix) : len(rhs)-len(decodeSuffix)]
		return "baml.DecodeToOrderedValue(" + inner + ")"
	}
	return rhs
}

// rewriteEncodeClassCall walks every baml.EncodeClass(...) call site
// in the file and rewrites the form
//
//	baml.EncodeClass(<name>, <fields>, &c.DynamicProperties)
//
// into
//
//	baml.EncodeClassOrdered(<name>, <fields>, &c.DynamicProperties)
//
// The legacy two/three-argument forms with nil or *map[string]any
// continue to use baml.EncodeClass so non-dynamic classes are
// unaffected.
func rewriteEncodeClassCall(body string) string {
	const marker = "baml.EncodeClass("
	idx := 0
	for {
		j := strings.Index(body[idx:], marker)
		if j < 0 {
			break
		}
		start := idx + j
		open := start + len(marker)
		depth := 1
		k := open
		for k < len(body) && depth > 0 {
			switch body[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
			k++
		}
		if depth != 0 {
			break
		}
		args := body[open:k]
		if encodeClassCallPassesOrderedFields(args) {
			replacement := "baml.EncodeClassOrdered(" + args + ")"
			body = body[:start] + replacement + body[k+1:]
			idx = start + len(replacement)
			continue
		}
		idx = k + 1
	}
	return body
}

// encodeClassCallPassesOrderedFields returns true when the EncodeClass
// argument list ends with a pointer to a DynamicProperties field. The
// BAML generator emits this shape only for @@dynamic output structs;
// other call sites either pass nil or a typed map pointer and continue
// to use the original EncodeClass signature.
func encodeClassCallPassesOrderedFields(args string) bool {
	trimmed := strings.TrimSpace(args)
	idx := strings.LastIndex(trimmed, ",")
	if idx < 0 {
		return false
	}
	last := strings.TrimSpace(trimmed[idx+1:])
	if !strings.HasPrefix(last, "&") {
		return false
	}
	return strings.HasSuffix(last, ".DynamicProperties")
}

// gofmtBytes runs the gofmt pipeline on src and returns the result.
// Used by patchGeneratedDynamicFile so the rewritten file lands in a
// stable, minimum-diff shape regardless of the text editing path taken
// through the patterns above.
func gofmtBytes(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "patched.go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, file); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// staticMapOrderedHelperMarker is the comment line embedded at the top
// of every generated helper file. Lets re-runs detect "this file's
// helpers were emitted by the static-map pass" and skip re-emission.
const staticMapOrderedHelperMarker = "// static-map helpers: convert ordered carrier to baml.OrderedMap[T]"

// staticMapHelperBaseName is the file name the static-map pass writes
// the per-package conversion helpers into. Kept distinct from
// BAML-emitted files so a re-run that wipes the BAML-generated tree
// (the regen orchestrator does this each run) replaces the helper
// file cleanly.
const staticMapHelperBaseName = "ordered_map_static.go"

// patchGeneratedStaticMapFile applies the static-map rewrite to a
// single generated file. The file is parsed with go/parser so structural
// rewrites land on type expressions and call expressions without
// mistakenly matching identical substrings inside strings or comments.
// Returns true when the file's contents changed.
func patchGeneratedStaticMapFile(path string, pkgIdx *pkgTypeIndex) (bool, error) {
	base := filepath.Base(path)
	// Skip the helper file itself — it carries `baml.OrderedMap` and
	// `Interface().(...)` references that would otherwise be rewritten
	// recursively on re-runs.
	if base == staticMapHelperBaseName {
		return false, nil
	}
	// Skip BAML infrastructure files. These carry `map[string]X{...}`
	// composite literals for internal lookup tables (type registry,
	// source-file map, runtime wrappers); rewriting their type
	// expressions to `baml.OrderedMap[X]` breaks the composite-literal
	// shape and the runtime that consumes them. The static-map pass
	// targets schema-derived types and union variants, not these
	// generator-emitted infrastructure files.
	if isStaticMapInfraFile(base) {
		return false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	body := string(raw)

	// Cheap pre-filter: only files that mention a concrete
	// `map[string]<not-any>` token are eligible. The BAML emitter
	// produces map[string]any in EncodeClass init blocks; those must
	// pass through unchanged. The regex anchors on the substring before
	// any T identifier to skip files that have no concrete maps.
	if !staticMapCandidateRe.MatchString(body) {
		return false, nil
	}

	newBody, rewroteAny, err := rewriteStaticMapTypesAndAsserts(body, pkgIdx)
	if err != nil {
		return false, err
	}
	if !rewroteAny {
		return false, nil
	}

	// Validate the rewrite still parses.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, []byte(newBody), parser.ParseComments)
	if err != nil {
		// Persist the bad output to a sibling .reject file for
		// post-mortem inspection so the test harness has something
		// concrete to diff against the original.
		_ = os.WriteFile(path+".reject", []byte(newBody), 0o644)
		return false, fmt.Errorf("static-map rewrite produced invalid Go: %w", err)
	}

	// Ensure the rewritten file binds the `baml` selector to the BAML
	// pkg import. The rewriter emits `baml.OrderedMap[T]` and
	// `baml.DecodeToOrderedValue(...)`; BAML's schema-derived files
	// already import the pkg, but generator-emitted siblings
	// (cmd/embed's `embed.go`, ad-hoc generated files) only import
	// what they directly use. Adding the import after the rewrite
	// keeps the pass robust against future generator shapes without
	// hard-coding a per-filename skip list.
	if added, ensureErr := ensureBAMLAliasImport(file, path, pkgIdx); ensureErr != nil {
		return false, ensureErr
	} else if added {
		var buf strings.Builder
		cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
		if printErr := cfg.Fprint(&buf, fset, file); printErr != nil {
			return false, fmt.Errorf("re-printing static-map rewrite: %w", printErr)
		}
		newBody = buf.String()
	}

	prettied, err := gofmtBytes([]byte(newBody))
	if err != nil {
		return false, fmt.Errorf("gofmt static-map rewrite: %w", err)
	}
	if err := os.WriteFile(path, prettied, 0o644); err != nil {
		return false, err
	}

	// Emit / refresh the per-package helper file. Helpers are package-
	// scoped because the generated package import path varies (types,
	// stream_types, baml_client root).
	if err := ensureStaticMapHelperFile(filepath.Dir(path), prettied, pkgIdx); err != nil {
		return false, err
	}
	return true, nil
}

// staticMapCandidateRe matches `map[string]X` where X starts with a
// letter (concrete identifier). The pattern excludes `map[string]any`
// and `map[string]any{` literals so files that only mention the
// dynamic-surface shape are not visited by the rewriter.
var staticMapCandidateRe = regexp.MustCompile(`map\[string\][A-Za-z_*\[]`)

// staticMapInfraFiles names the BAML-emitted files that carry
// composite-literal lookup tables (type_map.go, baml_source_map.go)
// and the lazy-runtime wrapper. Rewriting `map[string]X` in these
// files to `baml.OrderedMap[X]` breaks the composite-literal shape
// the runtime consumes. They are not schema-derived types and never
// participate in CFFI deserialisation, so the static-map rewrite has
// no business touching them.
var staticMapInfraFiles = map[string]struct{}{
	"baml_source_map.go": {},
	"type_map.go":        {},
	"runtime.go":         {},
}

func isStaticMapInfraFile(base string) bool {
	_, ok := staticMapInfraFiles[base]
	return ok
}

// rewriteStaticMapTypesAndAsserts rewrites every concrete
// `map[string]T` type expression to `baml.OrderedMap[T]` and every
// `.Interface().(map[string]T)` cast to `bamlOrderedAs<helperKey>(...)`.
// The rewrite is text-based but uses an AST parse for shape validation
// (the caller verifies the rewritten body parses before writing). Each
// pattern is matched with a balanced-bracket walker so nested generics
// (`map[string]map[string]string`) and pointer-wrapped maps are handled
// recursively.
//
// The return values are:
//   - rewritten body
//   - whether any rewrite landed
//   - error when a structural malformation prevents safe rewriting
func rewriteStaticMapTypesAndAsserts(body string, pkgIdx *pkgTypeIndex) (string, bool, error) {
	rewroteAny := false

	// Pass 1: rewrite type expressions. Two scopes:
	//   a) Struct field declarations: `<Indent><Name>[ \t]+map[string]T`.
	//   b) Function return / parameter types: `map[string]T)` and
	//      `map[string]T,` and `map[string]T {`.
	//   c) Variable type assertions: `var x map[string]T`.
	//
	// Implementation: walk the body byte-by-byte; when we see
	// `map[string]` followed by a non-`any` and non-`map[string]any`
	// token, capture the type expression and rewrite.
	newBody, changedTypes := rewriteMapStringTypeExprs(body, pkgIdx)
	if changedTypes {
		rewroteAny = true
		body = newBody
	}

	// Pass 2: rewrite Decode().Interface() casts.
	newBody, changedAsserts := rewriteStaticMapDecodeAsserts(body, pkgIdx)
	if changedAsserts {
		rewroteAny = true
		body = newBody
	}

	// Pass 3: rewrite direct type assertions on top-level / parse /
	// stream result carriers. Pass 2 covers the `baml.Decode(...).Interface().(T)`
	// shape generated inside class Decode bodies, but the generator
	// also emits `(result.Data).(T)`, `(result).(T)`, and
	// `result.StreamData.(T)` from top-level call / parse / stream
	// functions. The patched runtime supplies an ordered carrier in
	// these `result` carriers too, so a direct assertion to the
	// (post-pass-1-rewritten) `baml.OrderedMap[T]` shape panics.
	// Routing the carrier through the typed conversion helper keeps
	// the ordered runtime value alive on the way out of the runtime.
	newBody, changedDirects := rewriteStaticMapDirectAsserts(body, pkgIdx)
	if changedDirects {
		rewroteAny = true
		body = newBody
	}

	return body, rewroteAny, nil
}

// rewriteMapStringTypeExprs scans body for every `map[string]<T>`
// occurrence where T is concrete (not `any`, not `interface{}`) and
// rewrites the whole `map[string]<T>` span to `baml.OrderedMap[<T>]`.
// Nested maps are rewritten recursively so `map[string]map[string]Foo`
// becomes `baml.OrderedMap[baml.OrderedMap[Foo]]`.
//
// The walker preserves pointer / list wrappers: `*map[string]Foo`
// → `*baml.OrderedMap[Foo]` and `[]map[string]Foo` →
// `[]baml.OrderedMap[Foo]`. Composite literals (`map[string]any{}`)
// are left untouched because their value type is `any`, which is
// filtered out by the concrete-type predicate.
func rewriteMapStringTypeExprs(body string, pkgIdx *pkgTypeIndex) (string, bool) {
	const prefix = "map[string]"
	changed := false
	for searchFrom := 0; ; {
		idx := strings.Index(body[searchFrom:], prefix)
		if idx < 0 {
			break
		}
		absIdx := searchFrom + idx
		// Skip if this occurrence sits inside a string literal or
		// composite-literal context where the type expression is not
		// part of a declaration (best-effort: substring in single line
		// comments is treated as code; the rewrite is still safe
		// because comments containing `map[string]T` are not generated
		// by BAML for static maps).
		if isInsideStringLiteral(body, absIdx) {
			searchFrom = absIdx + len(prefix)
			continue
		}
		typeStart := absIdx + len(prefix)
		typeEnd, ok := readGoTypeExpr(body, typeStart)
		if !ok {
			searchFrom = typeStart
			continue
		}
		inner := body[typeStart:typeEnd]
		// Skip dynamic-surface maps: `any`, `interface{}`,
		// `interface {}`, and anything inside an `any{...}` literal.
		if isDynamicValueType(inner) {
			searchFrom = typeEnd
			continue
		}
		// Skip composite-literal initialisers: `map[string]T{...}`.
		// Rewriting these to `baml.OrderedMap[T]{...}` is invalid Go
		// because OrderedMap's fields are unexported. Look ahead past
		// whitespace to detect a `{`.
		//
		// The function-return-type position (`func F() *map[string]T {`)
		// shares the same trailing-`{` shape — the `{` opens the body,
		// not a composite literal. Distinguish by checking whether the
		// byte preceding the map type (after walking back over pointer
		// and slice modifier prefixes plus whitespace) is the `)` that
		// closes a function's parameter list. When it is, treat this
		// as a function return-type and rewrite normally; otherwise
		// the trailing `{` belongs to a composite literal and we skip.
		nextNonSpace := typeEnd
		for nextNonSpace < len(body) && (body[nextNonSpace] == ' ' || body[nextNonSpace] == '\t') {
			nextNonSpace++
		}
		if nextNonSpace < len(body) && body[nextNonSpace] == '{' {
			if !isFuncReturnTypePosition(body, absIdx) {
				searchFrom = typeEnd
				continue
			}
		}
		// Skip `make(map[string]T, ...)` argument position. Runtime
		// map allocations are not BAML decode sites; rewriting the
		// argument to `baml.OrderedMap[T]` would feed `make` a struct
		// type (rejected at compile time) and any companion index
		// assignment (`m[k] = v`) on the variable would not compile
		// either because OrderedMap is a struct, not a map. The
		// generator-emitted `embed.go` produced by cmd/embed has
		// this shape; the BAML emitter never uses `make` for static
		// schema maps (it stages decoded carriers via type
		// assertions on the runtime value), so skipping `make` calls
		// here loses no real coverage.
		if isInsideMakeCall(body, absIdx) {
			searchFrom = typeEnd
			continue
		}
		// Recursive type-alias guard. The Go compiler ICEs on a
		// `baml.OrderedMap[T]` instantiation when T is a type alias
		// whose transitive closure reaches back to the same alias in
		// another map value position (the integration `JsonValue`
		// shape). Leaving the offending arm as native `map[string]T`
		// avoids the ICE; the trade-off is that arm loses CFFI
		// insertion order on the way out of the runtime — Pass 2 and
		// Pass 3 detect the same recursive case and route the
		// decode/assert site through a native-map helper that builds
		// `map[string]T` from the ordered carrier without claiming
		// to preserve order.
		if pkgIdx != nil && pkgIdx.isRecursiveMapElement(inner) {
			searchFrom = typeEnd
			continue
		}
		// Recurse into the inner type expression so nested maps are
		// rewritten first. The recursion produces a stable substring
		// the outer rewrite can splice in.
		innerRewritten, _ := rewriteMapStringTypeExprs(inner, pkgIdx)
		replacement := "baml.OrderedMap[" + innerRewritten + "]"
		body = body[:absIdx] + replacement + body[typeEnd:]
		searchFrom = absIdx + len(replacement)
		changed = true
	}
	return body, changed
}

// rewriteStaticMapDecodeAsserts rewrites every
// `baml.Decode(<expr>).Interface().(<typeExpr>)` substring into a
// helper-driven typed conversion when <typeExpr> is a static map type.
// The type-expression pass that ran before this one will have rewritten
// `map[string]T` to `baml.OrderedMap[T]`; we still tolerate the
// pre-rewrite shape so a future call order change does not silently
// break the assert rewrite.
//
// The generated helper takes the runtime any and returns the typed
// `baml.OrderedMap[T]`, ranging in CFFI insertion order. Per-key
// conversion is type-asserted on the element type; an inner map
// recurses through the same helper at the nested element T.
func rewriteStaticMapDecodeAsserts(body string, pkgIdx *pkgTypeIndex) (string, bool) {
	const interfaceMarker = ".Interface().("
	changed := false
	for searchFrom := 0; ; {
		idx := strings.Index(body[searchFrom:], interfaceMarker)
		if idx < 0 {
			break
		}
		absIdx := searchFrom + idx
		// The `(` of the assertion is the last byte of the marker;
		// findMatchingParen walks forward from there to the `)` that
		// closes the type expression.
		assertOpenIdx := absIdx + len(interfaceMarker) - 1
		assertCloseIdx := findMatchingParen(body, assertOpenIdx)
		if assertCloseIdx < 0 {
			searchFrom = absIdx + len(interfaceMarker)
			continue
		}
		typeStart := assertOpenIdx + 1
		typeEnd := assertCloseIdx
		typeExpr := strings.TrimSpace(body[typeStart:typeEnd])
		// Only act on static-map shapes; leave every other assertion
		// alone. The shape we want to catch is either the
		// pre-rewrite `*?map[string]T` form (if pass 1 missed it for
		// some reason) or the post-rewrite `*?baml.OrderedMap[T]` form.
		if !isStaticMapAssertType(typeExpr) {
			searchFrom = assertCloseIdx + 1
			continue
		}
		// Find `baml.Decode(` to the left.
		preMarker := body[:absIdx]
		decodeStart := strings.LastIndex(preMarker, "baml.Decode(")
		if decodeStart < 0 {
			searchFrom = assertCloseIdx + 1
			continue
		}
		decodeArgStart := decodeStart + len("baml.Decode(")
		decodeArgEnd := findMatchingParen(body, decodeArgStart-1)
		if decodeArgEnd < 0 {
			searchFrom = assertCloseIdx + 1
			continue
		}
		// The substring between `decodeArgEnd+1` and `absIdx` must be
		// empty: `.Interface()` is the prefix of `interfaceMarker`, so
		// a well-formed `baml.Decode(x).Interface().(T)` site has the
		// assertion marker starting immediately after the Decode
		// call's closing `)`. A non-empty gap means an unrelated
		// `.Interface()` call sits between the Decode and the
		// assertion, so we leave it alone.
		between := body[decodeArgEnd+1 : absIdx]
		if between != "" {
			searchFrom = assertCloseIdx + 1
			continue
		}
		decodeArg := body[decodeArgStart:decodeArgEnd]
		// Recursive type-alias case: pass 1 left the surface type as
		// native `map[string]T` to dodge the Go compiler ICE on
		// `baml.OrderedMap[T]`; the assertion result type still has
		// to match, so route through a native-map helper that builds
		// `map[string]T` from the patched runtime's ordered carrier.
		// The element-order trade-off documented above applies.
		if elemName, ok := recursiveMapAssertElement(typeExpr, pkgIdx); ok {
			isPtr := strings.HasPrefix(strings.TrimSpace(typeExpr), "*")
			helperName := nativeMapHelperName(elemName, isPtr)
			replacement := fmt.Sprintf("%s(baml.DecodeToOrderedValue(%s))", helperName, decodeArg)
			body = body[:decodeStart] + replacement + body[assertCloseIdx+1:]
			searchFrom = decodeStart + len(replacement)
			changed = true
			continue
		}
		// Normalise the type expression to its `baml.OrderedMap[...]`
		// form (the pass-1 rewriter may have already done this; pass
		// it through again to be sure).
		normalised, _ := rewriteMapStringTypeExprs(typeExpr, pkgIdx)
		helperName := staticMapHelperName(normalised)
		replacement := fmt.Sprintf("%s(baml.DecodeToOrderedValue(%s))", helperName, decodeArg)
		body = body[:decodeStart] + replacement + body[assertCloseIdx+1:]
		searchFrom = decodeStart + len(replacement)
		changed = true
	}
	return body, changed
}

// rewriteStaticMapDirectAsserts rewrites every remaining
// `<expr>.(<staticMapType>)` site after pass 2. The BAML generator
// emits direct assertions on `result.Data`, `result.StreamData`, and
// `result` itself in the top-level call, parse, and stream
// final/partial paths; those carriers also reach the assertion as an
// ordered runtime value, so the unrouted assertion panics. The
// receiver expression is captured by walking back through the chain
// of identifier / selector / paren-wrapping characters, then handed
// off to the typed conversion helper (which accepts `any` and
// produces the typed ordered map).
//
// The rewrite is intentionally narrow: it only fires when the type
// expression inside the assertion is a static-map shape
// (isStaticMapAssertType). Every other type assertion in the
// generated code (e.g. `result.Data.(types.Foo)`) is left untouched.
func rewriteStaticMapDirectAsserts(body string, pkgIdx *pkgTypeIndex) (string, bool) {
	const dotParen = ".("
	changed := false
	for searchFrom := 0; ; {
		idx := strings.Index(body[searchFrom:], dotParen)
		if idx < 0 {
			break
		}
		absIdx := searchFrom + idx
		if isInsideStringLiteral(body, absIdx) {
			searchFrom = absIdx + len(dotParen)
			continue
		}
		// Locate the closing `)` of the assertion.
		assertOpenIdx := absIdx + 1
		assertCloseIdx := findMatchingParen(body, assertOpenIdx)
		if assertCloseIdx < 0 {
			searchFrom = absIdx + len(dotParen)
			continue
		}
		typeStart := assertOpenIdx + 1
		typeEnd := assertCloseIdx
		typeExpr := strings.TrimSpace(body[typeStart:typeEnd])
		if !isStaticMapAssertType(typeExpr) {
			searchFrom = assertCloseIdx + 1
			continue
		}
		// Walk back from `.` to the start of the receiver expression.
		// Acceptable receiver characters: identifier bytes, `.` for
		// selectors, and matched `(...)` for paren wrapping. Anything
		// else terminates the walk.
		recvStart := findStaticMapReceiverStart(body, absIdx)
		if recvStart < 0 || recvStart == absIdx {
			searchFrom = assertCloseIdx + 1
			continue
		}
		receiver := strings.TrimSpace(body[recvStart:absIdx])
		stripped := trimOuterMatchedParens(receiver)
		if stripped == "" {
			searchFrom = assertCloseIdx + 1
			continue
		}
		// Recursive type-alias case mirrors pass 2: route through the
		// native-map helper so the un-rewritten `map[string]T` surface
		// is preserved and the runtime-produced ordered carrier is
		// flattened to a native Go map.
		if elemName, ok := recursiveMapAssertElement(typeExpr, pkgIdx); ok {
			isPtr := strings.HasPrefix(strings.TrimSpace(typeExpr), "*")
			helperName := nativeMapHelperName(elemName, isPtr)
			replacement := fmt.Sprintf("%s(%s)", helperName, stripped)
			body = body[:recvStart] + replacement + body[assertCloseIdx+1:]
			searchFrom = recvStart + len(replacement)
			changed = true
			continue
		}
		normalised, _ := rewriteMapStringTypeExprs(typeExpr, pkgIdx)
		helperName := staticMapHelperName(normalised)
		replacement := fmt.Sprintf("%s(%s)", helperName, stripped)
		body = body[:recvStart] + replacement + body[assertCloseIdx+1:]
		searchFrom = recvStart + len(replacement)
		changed = true
	}
	return body, changed
}

// findStaticMapReceiverStart walks back from `end` (the byte index of
// the `.` in a `.(` assertion) over the receiver expression. The walk
// accepts identifier characters, `.` for selectors, and matched
// `(...)` groups. It stops at the first non-receiver byte (whitespace,
// `=`, `,`, `{`, `;`, `+`, etc.). Returns the start byte index, or
// the same index as `end` when no receiver was found.
func findStaticMapReceiverStart(body string, end int) int {
	i := end
	for i > 0 {
		c := body[i-1]
		if c == '.' || c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			i--
			continue
		}
		if c == ')' {
			depth := 1
			j := i - 2
			matched := -1
			for j >= 0 {
				switch body[j] {
				case ')':
					depth++
				case '(':
					depth--
				}
				if depth == 0 {
					matched = j
					break
				}
				j--
			}
			if matched < 0 {
				break
			}
			i = matched
			continue
		}
		break
	}
	return i
}

// trimOuterMatchedParens removes a leading `(` / trailing `)` pair
// when they wrap the entire expression. Repeated wrappings (e.g.
// `((x))`) are stripped iteratively. Returns the input unchanged
// when the outer pair is not a single matched group.
func trimOuterMatchedParens(s string) string {
	for {
		t := strings.TrimSpace(s)
		if len(t) < 2 || t[0] != '(' || t[len(t)-1] != ')' {
			return t
		}
		inner := t[1 : len(t)-1]
		depth := 0
		matched := true
		for i := 0; i < len(inner); i++ {
			switch inner[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth < 0 {
					matched = false
				}
			}
			if !matched {
				break
			}
		}
		if !matched || depth != 0 {
			return t
		}
		s = inner
	}
}

// isStaticMapAssertType reports whether typeExpr (a Go type expression
// captured between `.(` and `)` of a type assertion) describes a
// concrete static-map carrier. Accepts both the pre-rewrite
// `*?map[string]T` (with T != any) and the post-rewrite
// `*?baml.OrderedMap[T]` shapes. Slice / array wrappers around either
// are also accepted so `[]baml.OrderedMap[T]` casts are picked up.
func isStaticMapAssertType(typeExpr string) bool {
	t := strings.TrimSpace(typeExpr)
	for {
		if strings.HasPrefix(t, "*") {
			t = strings.TrimSpace(t[1:])
			continue
		}
		if strings.HasPrefix(t, "[]") {
			t = strings.TrimSpace(t[2:])
			continue
		}
		break
	}
	if strings.HasPrefix(t, "baml.OrderedMap[") {
		return true
	}
	if strings.HasPrefix(t, "map[string]") {
		inner := strings.TrimPrefix(t, "map[string]")
		return !isDynamicValueType(inner)
	}
	return false
}

// isFuncReturnTypePosition reports whether the `map[string]` substring
// at body[mapStart:] sits in a function-declaration return-type position
// (e.g. `func F() *map[string]T {`) — the alternative shape is a
// composite-literal expression position (e.g. `var x = map[string]T{...}`).
//
// The two shapes share a trailing `{` byte — for the function decl, `{`
// opens the body; for the literal, `{` opens the value list. The
// distinguishing signal is the byte that precedes the map type: a
// function return-type position has a `)` (the closing paren of the
// parameter list) before the map type, after skipping pointer / slice
// modifier prefixes and whitespace. Literal expressions are preceded
// by `=`, `,`, `(`, `return`, etc.
//
// Walks left from mapStart over `*`, `[]` slice prefixes, and inline
// whitespace. The first non-modifier byte determines the verdict.
// Returns false on malformed input (no preceding context, mismatched
// brackets) so the fail-closed default stays composite-literal-safe.
func isFuncReturnTypePosition(body string, mapStart int) bool {
	i := mapStart
	for i > 0 {
		c := body[i-1]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i--
			continue
		case c == '*':
			i--
			continue
		case c == ']':
			depth := 1
			j := i - 2
			for j >= 0 && depth > 0 {
				switch body[j] {
				case ']':
					depth++
				case '[':
					depth--
				}
				if depth == 0 {
					break
				}
				j--
			}
			if depth != 0 || j < 0 {
				return false
			}
			i = j
			continue
		}
		return c == ')'
	}
	return false
}

// isInsideMakeCall reports whether body[mapStart:] sits in the
// argument position of a `make(...)` call. Walks left from mapStart
// over pointer / slice modifier prefixes plus inline whitespace; if
// the first non-modifier byte is `(` and the identifier immediately
// before it is `make`, the map type is a runtime allocation argument
// and rewriting it produces invalid Go (OrderedMap[T] is a struct,
// not a map type — `make` rejects it and any `m[k] = v` on the
// resulting variable also fails to compile).
//
// The walk mirrors isFuncReturnTypePosition's left-walk over `*` /
// `[]` wrappers so a hypothetical `make([]*map[string]T, n)` still
// resolves to the `make` argument. The check returns false on a
// malformed prefix (no preceding context, mismatched brackets,
// identifier byte preceding `make`) so the fail-closed default keeps
// the rewrite running for non-make positions.
func isInsideMakeCall(body string, mapStart int) bool {
	i := mapStart
	for i > 0 {
		c := body[i-1]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i--
			continue
		case c == '*':
			i--
			continue
		case c == ']':
			depth := 1
			j := i - 2
			for j >= 0 && depth > 0 {
				switch body[j] {
				case ']':
					depth++
				case '[':
					depth--
				}
				if depth == 0 {
					break
				}
				j--
			}
			if depth != 0 || j < 0 {
				return false
			}
			i = j
			continue
		}
		if c != '(' {
			return false
		}
		// Scan back across whitespace to find the identifier preceding `(`.
		j := i - 2
		for j >= 0 && (body[j] == ' ' || body[j] == '\t') {
			j--
		}
		if j < 3 {
			return false
		}
		if body[j-3:j+1] != "make" {
			return false
		}
		// Reject identifiers that end with `make` as a suffix (e.g.
		// `unmake(`) by checking the byte immediately preceding the
		// `m` is not an identifier continuation.
		if j-3 > 0 {
			prev := body[j-4]
			if (prev >= 'a' && prev <= 'z') ||
				(prev >= 'A' && prev <= 'Z') ||
				(prev >= '0' && prev <= '9') ||
				prev == '_' {
				return false
			}
		}
		return true
	}
	return false
}

// findMatchingParen finds the `)` matching the `(` at openIdx.
func findMatchingParen(body string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(body) || body[openIdx] != '(' {
		return -1
	}
	depth := 1
	i := openIdx + 1
	for i < len(body) && depth > 0 {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// readGoTypeExpr scans a Go type expression starting at start and
// returns the index past the last byte of the expression plus a
// success flag. Recognises pointer prefix, identifier/dotted name,
// bracket-delimited type parameters, and array/slice prefixes. Stops
// on whitespace, `{`, `(`, `,`, `;`, `=`, `\n`, or `]` at the outer
// nesting level (treated as the end of the type expression).
func readGoTypeExpr(body string, start int) (int, bool) {
	i := start
	// Pointer / slice prefix.
	for i < len(body) {
		c := body[i]
		if c == '*' {
			i++
			continue
		}
		if c == '[' {
			// Could be slice `[]` or array `[N]` — find matching `]`.
			depth := 1
			j := i + 1
			for j < len(body) && depth > 0 {
				switch body[j] {
				case '[':
					depth++
				case ']':
					depth--
				}
				j++
			}
			if depth != 0 {
				return 0, false
			}
			i = j
			continue
		}
		break
	}
	if i >= len(body) {
		return 0, false
	}
	// `map[K]V` recursion shortcut: detect `map[`.
	if strings.HasPrefix(body[i:], "map[") {
		// Skip `[K]`.
		depth := 1
		j := i + 4
		for j < len(body) && depth > 0 {
			switch body[j] {
			case '[':
				depth++
			case ']':
				depth--
			}
			j++
		}
		if depth != 0 {
			return 0, false
		}
		// Now we're past the `]`; the value-type expression follows.
		return readGoTypeExpr(body, j)
	}
	// Literal interface / struct type with a brace body. BAML emits
	// `interface{}` as the Go value type for the BAML `null` type, so a
	// `map<string, null>` lowers to `map[string]*interface{}`. The
	// identifier loop below stops at the `{`, truncating the type to
	// `interface`; consume the balanced brace body here so the caller
	// sees the whole `interface{}` (or `struct{...}`) expression.
	if end, ok := readBracedTypeKeyword(body, i); ok {
		return end, true
	}
	// Identifier / dotted name with optional generic args.
	for i < len(body) {
		c := body[i]
		if c == '.' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			i++
			continue
		}
		if c == '[' {
			depth := 1
			j := i + 1
			for j < len(body) && depth > 0 {
				switch body[j] {
				case '[':
					depth++
				case ']':
					depth--
				}
				j++
			}
			if depth != 0 {
				return 0, false
			}
			i = j
			continue
		}
		break
	}
	if i == start {
		return 0, false
	}
	return i, true
}

// readBracedTypeKeyword recognises a literal `interface{...}` or
// `struct{...}` type expression at body[start:] and returns the index
// past the closing `}`. The keyword must be followed (after optional
// inline whitespace) by a `{`, so a plain identifier that merely starts
// with the keyword spelling (`interfaceFoo`, `structName`) is not
// matched. Returns false when start does not open such a type.
//
// BAML emits `interface{}` as the Go representation of the BAML `null`
// type; `map<string, null>` therefore lowers to `map[string]*interface{}`.
// readGoTypeExpr's identifier scan would otherwise stop at the `{` and
// hand back a truncated `interface` type, so this helper keeps the
// `{}` (or any `struct` field list) attached to the type expression.
func readBracedTypeKeyword(body string, start int) (int, bool) {
	for _, kw := range [...]string{"interface", "struct"} {
		if !strings.HasPrefix(body[start:], kw) {
			continue
		}
		j := start + len(kw)
		// Require the keyword to be followed by `{` (after optional
		// inline whitespace); otherwise this is an identifier whose
		// spelling merely begins with the keyword.
		k := j
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if k >= len(body) || body[k] != '{' {
			continue
		}
		depth := 1
		m := k + 1
		for m < len(body) && depth > 0 {
			switch body[m] {
			case '{':
				depth++
			case '}':
				depth--
			}
			m++
		}
		if depth != 0 {
			return 0, false
		}
		return m, true
	}
	return 0, false
}

// isDynamicValueType reports whether the inner value type of a map is
// the dynamic surface (`any`, `interface{}`, `interface {}`). Used to
// skip the rewrite for `map[string]any` initialisers BAML emits in
// every EncodeClass body.
func isDynamicValueType(t string) bool {
	t = strings.TrimSpace(t)
	switch t {
	case "any", "interface{}", "interface {}":
		return true
	}
	return false
}

// isInsideStringLiteral reports whether absIdx falls inside a `"..."`
// or `` `...` `` string literal in body. Walks the body left-to-right
// counting literal boundaries; the walk is O(absIdx) but only runs
// once per `map[string]` candidate so total work stays linear.
func isInsideStringLiteral(body string, absIdx int) bool {
	inDouble := false
	inBacktick := false
	for i := 0; i < absIdx && i < len(body); i++ {
		c := body[i]
		if inBacktick {
			if c == '`' {
				inBacktick = false
			}
			continue
		}
		if inDouble {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '`':
			inBacktick = true
		}
	}
	return inDouble || inBacktick
}

// staticMapHelperName encodes a `baml.OrderedMap[T]` type expression
// into a stable Go identifier suffix. Used as the helper function
// name. The encoding is reversible by decodeStaticMapEnc: nested
// maps, pointer/slice wrappers, and dotted identifiers each map to
// fixed token sequences so a re-run that re-discovers a helper name
// in a generated file can reproduce the original type expression
// without an out-of-band manifest.
func staticMapHelperName(typeExpr string) string {
	return "bamlOrderedAs_" + encodeStaticMapType(typeExpr)
}

// convertListHelperName names the per-element-type helper that turns
// the runtime `[]any` produced by baml.DecodeToOrderedValue into a
// typed `[]T`. Each list helper is emitted alongside the ordered-map
// helpers and registered through the same worklist as the ordered-
// map helpers so the per-package helper file resolves regardless of
// emission order.
func convertListHelperName(typeExpr string) string {
	elem := strings.TrimPrefix(strings.TrimSpace(typeExpr), "[]")
	return "bamlConvertList_" + encodeStaticMapType(elem)
}

// encodeStaticMapType encodes a Go type expression into a Go-identifier-
// safe token sequence. The encoding is deterministic and decodable; the
// only reserved producer is decodeStaticMapEnc. Tokens:
//
//	"baml.OrderedMap[" -> "OM_"     (opens a generic; close is inferred
//	                                 at end-of-token-stream by depth)
//	"*"                -> "Ptr_"    (pointer prefix)
//	"[]"               -> "Slice_"  (slice prefix)
//	"interface{}"      -> "Iface"   (the empty interface, Go's spelling
//	                                 of the BAML `null` value type)
//	"."                -> "_dot_"   (package separator)
//	"]"                -> ""        (closes inferred by OM_ depth)
//	spaces             -> ""        (stripped)
//
// Identifier characters pass through unchanged, including any
// underscore that is part of a Go identifier (e.g. `stream_types`).
// All map types we encode have exactly one type parameter so the
// implicit-close-at-end rule is unambiguous.
//
// Known limitation: encodeStaticMapType reserves the token
// substrings `_dot_`, `Slice_`, `Ptr_`, `OM_`, and `Iface`.
// Identifiers whose own spelling contains any of those literal
// substrings (e.g. a Go type called `Slice_string`, a package
// selector `some_dot_type`, or a type named `IfaceThing`) are not
// round-trip-safe: the decoder would
// greedily consume the reserved token and rebuild a different
// Go type expression. BAML's generated Go identifiers are
// PascalCase/camelCase without those substrings today, so the
// collision class is theoretical for the current code generator,
// but callers and any future codegen extensions should avoid
// emitting identifiers containing those literals through this
// encoding. A future fully-reversible encoding (base32 or hex of
// the normalized type expression) would side-step the limitation
// entirely.
func encodeStaticMapType(typeExpr string) string {
	s := strings.ReplaceAll(typeExpr, " ", "")
	// The empty interface carries the only brace characters that reach
	// this encoder (BAML's `null` -> `*interface{}`). Fold it to a token
	// before the brace-free replacements run so no `{`/`}` survives into
	// the helper identifier; the space-strip above already collapsed the
	// `interface {}` spelling to `interface{}`.
	s = strings.ReplaceAll(s, "interface{}", "Iface")
	s = strings.ReplaceAll(s, "baml.OrderedMap[", "OM_")
	s = strings.ReplaceAll(s, "*", "Ptr_")
	s = strings.ReplaceAll(s, "[]", "Slice_")
	s = strings.ReplaceAll(s, ".", "_dot_")
	s = strings.ReplaceAll(s, "]", "")
	return s
}

// ensureStaticMapHelperFile writes / refreshes a per-package helper
// file alongside the generated client files. The file declares every
// `bamlOrderedAs_*` helper the rewritten code references plus the
// `bamlConvertList_*` helpers those ordered-map helpers transitively
// call for `map<string, list<T>>` values.
//
// The file is regenerated wholesale each time so a re-run after a
// rewrite-shape change does not stack stale helpers. Helper discovery
// is a worklist: seed names from a regex scan of every package file,
// then expand by examining each emitted helper's body for further
// `bamlOrderedAs_*` / `bamlConvertList_*` references. Without the
// transitive step, a package whose only static-map surface is a
// nested map (e.g. `NestedMap baml.OrderedMap[baml.OrderedMap[T]]`)
// would emit the outer helper but omit the inner helper the outer
// recursively calls, leaving the package with an undefined symbol.
//
// Imports are similarly derived from the emitted bodies: any
// `<pkg>.<Ident>` reference where `<pkg>` is not the local package
// and not the `baml` alias is treated as a sibling generated
// package, and its import path is resolved by inspecting the same
// surrounding files used to derive the BAML alias.
func ensureStaticMapHelperFile(packageDir string, rewrittenSrc []byte, pkgIdx *pkgTypeIndex) error {
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return err
	}
	seed := map[string]struct{}{}
	helperRefRe := regexp.MustCompile(`baml(OrderedAs|ConvertList|NativeMapAs)_[A-Za-z0-9_]+`)
	collect := func(src []byte) {
		for _, m := range helperRefRe.FindAll(src, -1) {
			seed[string(m)] = struct{}{}
		}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if e.Name() == staticMapHelperBaseName {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(packageDir, e.Name()))
		if readErr != nil {
			return readErr
		}
		collect(data)
	}
	collect(rewrittenSrc)
	if len(seed) == 0 {
		// No helpers referenced — remove any stale helper file from a
		// prior run so the package stays clean.
		path := filepath.Join(packageDir, staticMapHelperBaseName)
		if _, statErr := os.Stat(path); statErr == nil {
			return os.Remove(path)
		}
		return nil
	}

	// Determine the package name and the BAML `pkg` import path from a
	// sibling Go file. Also collect the full import map so the helper
	// file's imports can pick up sibling-package paths by name.
	pkgName := ""
	bamlImportPath := ""
	importsByName := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == staticMapHelperBaseName {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(packageDir, e.Name()))
		if readErr != nil {
			return readErr
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, e.Name(), data, parser.ImportsOnly)
		if parseErr != nil || file.Name == nil {
			continue
		}
		if pkgName == "" {
			pkgName = file.Name.Name
		}
		if bamlImportPath == "" {
			bamlImportPath = findBAMLPkgImportPath(file)
		}
		for name, path := range collectImportPathsByName(file) {
			if _, ok := importsByName[name]; !ok {
				importsByName[name] = path
			}
		}
	}
	if pkgName == "" {
		return fmt.Errorf("static-map helper: cannot determine package name in %s", packageDir)
	}
	if bamlImportPath == "" {
		// No sibling file declared the BAML `pkg` import — fall back to
		// the upstream module path. The cmd/regenerate-dynclient
		// pipeline runs RewriteGeneratedClientBAMLImports after this
		// pass and will retarget the fallback to the patched-fork path;
		// the cmd/build/build.sh pipeline keeps the upstream path
		// (the in-place patched module cache copy carries OrderedMap[T]).
		bamlImportPath = "github.com/boundaryml/baml/engine/language_client_go/pkg"
	}

	// Worklist expansion: emit each helper body and follow any
	// transitive `bamlOrderedAs_*` / `bamlConvertList_*` references
	// nested inside it.
	emitted := map[string]string{}
	worklist := make([]string, 0, len(seed))
	for n := range seed {
		worklist = append(worklist, n)
	}
	for len(worklist) > 0 {
		name := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		if _, done := emitted[name]; done {
			continue
		}
		body, ok := emitStaticHelperByName(name, pkgIdx)
		if !ok {
			continue
		}
		emitted[name] = body
		for _, m := range helperRefRe.FindAllString(body, -1) {
			if _, done := emitted[m]; !done {
				worklist = append(worklist, m)
			}
		}
	}
	if len(emitted) == 0 {
		// All seed names were unparseable encodings — leave the
		// package clean and drop any stale helper file.
		path := filepath.Join(packageDir, staticMapHelperBaseName)
		if _, statErr := os.Stat(path); statErr == nil {
			return os.Remove(path)
		}
		return nil
	}

	sortedNames := make([]string, 0, len(emitted))
	for n := range emitted {
		sortedNames = append(sortedNames, n)
	}
	// Stable order so re-runs produce byte-stable output.
	for i := 1; i < len(sortedNames); i++ {
		for j := i; j > 0 && sortedNames[j] < sortedNames[j-1]; j-- {
			sortedNames[j], sortedNames[j-1] = sortedNames[j-1], sortedNames[j]
		}
	}

	// Derive any sibling-package imports needed by the emitted bodies.
	// The local package and the `baml` alias are excluded; everything
	// else is treated as a sibling generated package and resolved
	// through the import map collected from the surrounding files.
	//
	// When the in-scope name (the `ref` the helper body emits as a
	// selector prefix) differs from the path's default last-segment
	// name, the helper file must reproduce the explicit alias used
	// by the surrounding files. Without the alias, the import would
	// bind the default name and the helper body's `ref.Foo` would
	// resolve to an undefined identifier.
	extraImports := map[string]string{} // import path -> alias ("" for default name)
	pkgRefRe := regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\.[A-Z][a-zA-Z0-9_]*`)
	for _, name := range sortedNames {
		body := emitted[name]
		for _, m := range pkgRefRe.FindAllStringSubmatch(body, -1) {
			ref := m[1]
			if ref == "baml" || ref == pkgName {
				continue
			}
			path, ok := importsByName[ref]
			if !ok {
				continue
			}
			alias := ""
			if ref != defaultImportName(path) {
				alias = ref
			}
			// Don't downgrade an existing explicit alias to the
			// default form on a later iteration.
			if existing, present := extraImports[path]; !present || existing == "" {
				extraImports[path] = alias
			}
		}
	}

	// The native-map-only helper bodies (emitted exclusively for
	// recursive type aliases) never reference the `baml` selector
	// in code — they keep the field type as a native `map[string]T`.
	// Skip the baml import when no helper has a `baml.` token
	// outside of `//` comments so the helper file does not fail
	// the `imported and not used` check.
	needsBAMLImport := false
	for _, name := range sortedNames {
		if helperBodyReferencesBAML(emitted[name]) {
			needsBAMLImport = true
			break
		}
	}

	var buf strings.Builder
	buf.WriteString("// Code generated by cmd/hacks/hacks/dynamic_order_client.go; DO NOT EDIT.\n")
	buf.WriteString(staticMapOrderedHelperMarker + "\n\n")
	buf.WriteString("package " + pkgName + "\n\n")
	if needsBAMLImport || len(extraImports) > 0 {
		buf.WriteString("import (\n")
		if needsBAMLImport {
			fmt.Fprintf(&buf, "\tbaml %q\n", bamlImportPath)
		}
		// Stable import order: sort by path.
		if len(extraImports) > 0 {
			paths := make([]string, 0, len(extraImports))
			for p := range extraImports {
				paths = append(paths, p)
			}
			for i := 1; i < len(paths); i++ {
				for j := i; j > 0 && paths[j] < paths[j-1]; j-- {
					paths[j], paths[j-1] = paths[j-1], paths[j]
				}
			}
			for _, p := range paths {
				if alias := extraImports[p]; alias != "" {
					fmt.Fprintf(&buf, "\t%s %q\n", alias, p)
				} else {
					fmt.Fprintf(&buf, "\t%q\n", p)
				}
			}
		}
		buf.WriteString(")\n\n")
	}
	for _, name := range sortedNames {
		buf.WriteString(emitted[name])
		buf.WriteString("\n\n")
	}
	buf.WriteString(staticMapHelperCore())

	return os.WriteFile(filepath.Join(packageDir, staticMapHelperBaseName), []byte(buf.String()), 0o644)
}

// emitStaticHelperByName dispatches helper-body emission by name
// prefix. Returns the rendered body and a success flag; an
// unparseable name (e.g. an unrelated `bamlOrderedAs_` reference
// from user code) yields false so the worklist skips it without
// emitting a wrong-typed stub.
func emitStaticHelperByName(name string, pkgIdx *pkgTypeIndex) (string, bool) {
	if strings.HasPrefix(name, "bamlOrderedAs_") {
		typeExpr, ok := staticMapHelperTypeFromName(name)
		if !ok {
			return "", false
		}
		return emitStaticMapHelper(name, typeExpr, pkgIdx), true
	}
	if strings.HasPrefix(name, "bamlConvertList_") {
		listType, ok := convertListHelperTypeFromName(name)
		if !ok {
			return "", false
		}
		return emitConvertListHelper(name, listType, pkgIdx), true
	}
	if strings.HasPrefix(name, "bamlNativeMapAs_") {
		elemName, isPtr, ok := nativeMapHelperElementFromName(name)
		if !ok {
			return "", false
		}
		// Default to nilable when no package index is available. The
		// native-map helper is generated for recursive type aliases,
		// which in BAML's emitted code are always pointer aliases
		// (e.g. `type JsonValue = *Union6...`); the BAML runtime hands
		// untyped nil into the ranger callback for null holders. A
		// missing index would happen only in tests that bypass the
		// package walk — treating "unknown" as nilable preserves the
		// key in the produced map, which is the conservative call when
		// the alternative is silent data loss.
		nilable := pkgIdx == nil || pkgIdx.isNilableElement(elemName)
		return emitNativeMapHelper(name, elemName, isPtr, nilable), true
	}
	return "", false
}

// defaultImportName returns the default in-scope name Go binds for
// an unaliased import of the given path — the last `/`-separated
// segment. Used to decide whether a sibling-import in the helper
// file needs an explicit alias to keep the helper body's selector
// references resolvable.
func defaultImportName(path string) string {
	slash := strings.LastIndex(path, "/")
	if slash < 0 {
		return path
	}
	return path[slash+1:]
}

// collectImportPathsByName returns the file's imports keyed by the
// in-scope name they bind. The name is the explicit alias when
// present, otherwise the last segment of the import path — matching
// how the Go compiler resolves selector expressions back to imports.
// Blank and dot imports are skipped because they bind no selectable
// name.
func collectImportPathsByName(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range file.Imports {
		if imp == nil || imp.Path == nil {
			continue
		}
		raw := imp.Path.Value
		if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
			continue
		}
		path := raw[1 : len(raw)-1]
		name := ""
		if imp.Name != nil {
			switch imp.Name.Name {
			case "_", ".":
				continue
			default:
				name = imp.Name.Name
			}
		} else {
			name = defaultImportName(path)
		}
		if name == "" {
			continue
		}
		out[name] = path
	}
	return out
}

// findBAMLPkgImportPath returns the module-qualified import path used
// by file for the BAML `pkg` package the rewrite targets, or the empty
// string when no such import is present. Returning the exact path the
// surrounding files declare lets the helper file resolve under both the
// regenerate-dynclient build (which later rewrites to the patched
// fork) and the cmd/build/build.sh integration build (which keeps the
// upstream module path).
//
// Selection order:
//  1. An import explicitly aliased `baml` — this is the alias the
//     rewrite emits `baml.OrderedMap[T]` against, so adopting its
//     module path is the most direct way to keep the helper in lockstep.
//  2. Any import path ending with `/engine/language_client_go/pkg` —
//     covers default-aliased BAML imports in surrounding files.
func findBAMLPkgImportPath(file *ast.File) string {
	const suffix = "/engine/language_client_go/pkg"
	var fallback string
	for _, imp := range file.Imports {
		if imp == nil || imp.Path == nil {
			continue
		}
		raw := imp.Path.Value
		if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
			continue
		}
		path := raw[1 : len(raw)-1]
		if imp.Name != nil && imp.Name.Name == "baml" {
			return path
		}
		if fallback == "" && strings.HasSuffix(path, suffix) {
			fallback = path
		}
	}
	return fallback
}

// ensureBAMLAliasImport adds an explicit `baml "<path>"` import to a
// rewritten file when no existing import already binds the `baml`
// selector. The static-map rewrite emits `baml.OrderedMap[T]` and
// `baml.DecodeToOrderedValue(...)`, so any file that received a
// rewrite must bind `baml`. Returns (added, err) where `added` is
// true when an import was inserted (and the caller must re-print the
// file) and false when the file already had a compatible binding.
//
// Import-path resolution order, mirroring findBAMLPkgImportPath:
//  1. The rewritten file itself — the rewrite never removes an
//     existing baml import, so a file that already binds baml needs
//     no further action.
//  2. The pkgTypeIndex cache populated during package-level scanning
//     — sibling files (other generated package members) declare the
//     same fork/upstream path the helper file uses.
//  3. As a last resort, the file-level neighbour scan via
//     collectImportPathsByName against the on-disk directory, which
//     covers the case where pkgIdx was not built (idx == nil).
//  4. The upstream `github.com/boundaryml/baml/...` path — matches
//     the helper-file fallback so a downstream
//     RewriteGeneratedClientBAMLImports retargets both consistently.
func ensureBAMLAliasImport(file *ast.File, path string, pkgIdx *pkgTypeIndex) (bool, error) {
	for _, imp := range file.Imports {
		if imp == nil || imp.Path == nil {
			continue
		}
		rawPath := strings.Trim(imp.Path.Value, `"`)
		if imp.Name != nil {
			switch imp.Name.Name {
			case "_", ".":
				continue
			case "baml":
				return false, nil
			}
		} else if defaultImportName(rawPath) == "baml" {
			return false, nil
		}
	}
	bamlPath := findBAMLPkgImportPath(file)
	if bamlPath == "" && pkgIdx != nil {
		bamlPath = pkgIdx.bamlImportPath
	}
	if bamlPath == "" {
		bamlPath = neighbourBAMLImportPath(filepath.Dir(path))
	}
	if bamlPath == "" {
		bamlPath = "github.com/boundaryml/baml/engine/language_client_go/pkg"
	}
	spec := &ast.ImportSpec{
		Name: ast.NewIdent("baml"),
		Path: &ast.BasicLit{Kind: token.STRING, Value: `"` + bamlPath + `"`},
	}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		// Force block form so the printer emits the spec on its own
		// line; gofmt later normalises grouping.
		if !gd.Lparen.IsValid() {
			gd.Lparen = gd.Pos()
			gd.Rparen = gd.End()
		}
		gd.Specs = append(gd.Specs, spec)
		file.Imports = append(file.Imports, spec)
		return true, nil
	}
	gd := &ast.GenDecl{
		Tok:    token.IMPORT,
		Lparen: token.Pos(1),
		Rparen: token.Pos(1),
		Specs:  []ast.Spec{spec},
	}
	file.Decls = append([]ast.Decl{gd}, file.Decls...)
	file.Imports = append(file.Imports, spec)
	return true, nil
}

// neighbourBAMLImportPath scans the .go files in dir (skipping the
// helper file the static-map pass owns) for a BAML pkg import and
// returns the first path that matches the same selection order as
// findBAMLPkgImportPath. Empty string when no neighbour file declares
// the import. Used as a fallback when pkgTypeIndex was not populated
// (callers that hand in a nil index, or a test harness that loads
// files in isolation).
func neighbourBAMLImportPath(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if e.Name() == staticMapHelperBaseName {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), e.Name(), data, parser.ImportsOnly)
		if parseErr != nil || file == nil {
			continue
		}
		if path := findBAMLPkgImportPath(file); path != "" {
			return path
		}
	}
	return ""
}

// staticMapHelperTypeFromName reverses staticMapHelperName. Returns
// the wrapped `baml.OrderedMap[T]` type expression the helper
// produces. Returns false for unparseable encodings so a future
// drift in the encoder surfaces as a missing helper; this fails
// closed where a wrong-typed helper would compile and crash at runtime.
//
// Accepted encodings: any sequence of `Ptr_` / `Slice_` prefixes
// followed by an `OM_` opener — e.g. `OM_T`, `Ptr_OM_T`,
// `Slice_OM_T`, `Ptr_Slice_OM_T`, `Slice_Ptr_OM_T`. The static-map
// assert recogniser (isStaticMapAssertType) accepts the same set,
// so every helper-name the rewrite emits must reach a defined
// helper here or the generated package fails to compile with an
// undefined symbol.
func staticMapHelperTypeFromName(name string) (string, bool) {
	const prefix = "bamlOrderedAs_"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	enc := strings.TrimPrefix(name, prefix)
	rest := enc
	for {
		switch {
		case strings.HasPrefix(rest, "Ptr_"):
			rest = strings.TrimPrefix(rest, "Ptr_")
		case strings.HasPrefix(rest, "Slice_"):
			rest = strings.TrimPrefix(rest, "Slice_")
		case strings.HasPrefix(rest, "OM_"):
			return decodeStaticMapEnc(enc), true
		default:
			return "", false
		}
	}
}

// convertListHelperTypeFromName reverses convertListHelperName. The
// emitted helper signature returns `[]<elementType>`; the element
// type is recovered by decoding the suffix and prepending `[]`.
func convertListHelperTypeFromName(name string) (string, bool) {
	const prefix = "bamlConvertList_"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	enc := strings.TrimPrefix(name, prefix)
	if enc == "" {
		return "", false
	}
	return "[]" + decodeStaticMapEnc(enc), true
}

// decodeStaticMapEnc reverses encodeStaticMapType. Token recognition
// (`OM_`, `Ptr_`, `Slice_`, `Iface`, `_dot_`) is greedy and
// unambiguous; any other byte is appended verbatim, which preserves
// underscores that live inside Go identifiers such as `stream_types`.
// Generic closes
// are inferred at end-of-stream by closing every `OM_` that was
// opened — every static-map type expression we encode has exactly
// one type parameter, so the close set is always trailing.
func decodeStaticMapEnc(enc string) string {
	var out strings.Builder
	depth := 0
	for i := 0; i < len(enc); {
		switch {
		case strings.HasPrefix(enc[i:], "OM_"):
			out.WriteString("baml.OrderedMap[")
			depth++
			i += 3
		case strings.HasPrefix(enc[i:], "Ptr_"):
			out.WriteString("*")
			i += 4
		case strings.HasPrefix(enc[i:], "Slice_"):
			out.WriteString("[]")
			i += 6
		case strings.HasPrefix(enc[i:], "Iface"):
			out.WriteString("interface{}")
			i += 5
		case strings.HasPrefix(enc[i:], "_dot_"):
			out.WriteString(".")
			i += 5
		default:
			out.WriteByte(enc[i])
			i++
		}
	}
	for depth > 0 {
		out.WriteString("]")
		depth--
	}
	return out.String()
}

// emitStaticMapHelper renders one helper function. The helper body
// dispatches on the runtime carrier type:
//   - OrderedFields (= OrderedMap[any]) is the common case from
//     decodeMapValue; iterate via RangeAny and convert each value to
//     the typed T.
//   - *OrderedMap[T] (pointer-wrapped optional carrier) is unwrapped.
//   - native map[string]V is supported as a compat fall-through so
//     legacy generated code that still produces native maps does not
//     panic when fed through the new helper.
//
// Nested map values are converted recursively by calling the helper
// for the nested type; the inner helper is also emitted by the same
// pass so the call resolves at compile time.
//
// When the surface type is `*baml.OrderedMap[T]` (used for optional
// map fields), the helper allocates an OrderedMap[T] from the
// carrier and returns the address; a nil carrier returns nil so the
// optional field stays nil for absent values.
func emitStaticMapHelper(name, typeExpr string, pkgIdx *pkgTypeIndex) string {
	t := strings.TrimSpace(typeExpr)
	// Slice-wrapped variants (`[]baml.OrderedMap[T]`,
	// `*[]baml.OrderedMap[T]`, `[]*baml.OrderedMap[T]`, …) peel
	// the outer slice and recurse through the element-type helper.
	// `isStaticMapAssertType` accepts these shapes, so the
	// rewrite emits `bamlOrderedAs_Slice_*` calls that must
	// reach a defined helper here.
	if strings.HasPrefix(t, "[]") || strings.HasPrefix(t, "*[]") {
		return emitSliceWrappedStaticMapHelper(name, t, pkgIdx)
	}
	isPtr := strings.HasPrefix(t, "*")
	innerOrderedType := t
	if isPtr {
		innerOrderedType = strings.TrimPrefix(t, "*")
	}
	if !strings.HasPrefix(innerOrderedType, "baml.OrderedMap[") {
		// Unrecognised shape — emit a stub that returns the zero
		// value so a future encoding drift surfaces as a runtime
		// "always-empty"; emitting a compile error here would block
		// the entire regen on an isolated encoding glitch.
		var stub strings.Builder
		fmt.Fprintf(&stub, "// %s is a static-map helper stub; the type expression\n", name)
		fmt.Fprintf(&stub, "// %q does not match the expected baml.OrderedMap[T] shape.\n", typeExpr)
		fmt.Fprintf(&stub, "func %s(value any) %s {\n", name, typeExpr)
		fmt.Fprintf(&stub, "\tvar out %s\n", typeExpr)
		stub.WriteString("\t_ = value\n")
		stub.WriteString("\treturn out\n")
		stub.WriteString("}")
		return stub.String()
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(innerOrderedType, "baml.OrderedMap["), "]")

	var body strings.Builder
	fmt.Fprintf(&body, "// %s converts the runtime ordered carrier produced\n", name)
	fmt.Fprintf(&body, "// by baml.DecodeToOrderedValue into a typed %s while\n", typeExpr)
	body.WriteString("// preserving CFFI insertion order.\n")
	fmt.Fprintf(&body, "func %s(value any) %s {\n", name, typeExpr)
	body.WriteString("\tif value == nil {\n")
	if isPtr {
		body.WriteString("\t\treturn nil\n")
	} else {
		fmt.Fprintf(&body, "\t\tvar zero %s\n", typeExpr)
		body.WriteString("\t\treturn zero\n")
	}
	body.WriteString("\t}\n")
	// Pointer-wrapped optional carrier identity short-circuit.
	if isPtr {
		fmt.Fprintf(&body, "\tif ptr, ok := value.(%s); ok {\n", typeExpr)
		body.WriteString("\t\treturn ptr\n")
		body.WriteString("\t}\n")
	} else {
		fmt.Fprintf(&body, "\tif ptr, ok := value.(*%s); ok {\n", typeExpr)
		body.WriteString("\t\tif ptr == nil {\n")
		fmt.Fprintf(&body, "\t\t\tvar zero %s\n", typeExpr)
		body.WriteString("\t\t\treturn zero\n")
		body.WriteString("\t\t}\n")
		body.WriteString("\t\treturn *ptr\n")
		body.WriteString("\t}\n")
	}
	// Ordered carrier via RangeAny.
	fmt.Fprintf(&body, "\tvar out %s\n", innerOrderedType)
	body.WriteString("\tif ranger, ok := value.(interface {\n")
	body.WriteString("\t\tRangeAny(func(string, any) bool)\n")
	body.WriteString("\t\tLen() int\n")
	body.WriteString("\t}); ok {\n")
	body.WriteString("\t\tranger.RangeAny(func(k string, v any) bool {\n")
	// Null-holder preservation mirrors the native-map helper: when
	// the element type is nilable and the conversion is a raw
	// `v.(T)` assertion, untyped nil from the BAML runtime would
	// panic the assertion before reaching `out.Set`. Emit a guard
	// that records the key with a nil value so the carrier's CFFI
	// insertion order survives a null entry. Helper-routed inner
	// types (nested `baml.OrderedMap[T]`, lists, pointer-wrapped
	// ordered maps) handle nil internally and need no guard here.
	emitStaticMapElementGuard(&body, "\t\t\t", "v", inner, pkgIdx, "_ = out.Set(k, nil)\n\t\t\t\treturn true\n")
	fmt.Fprintf(&body, "\t\t\t_ = out.Set(k, %s)\n", convertStaticMapValueExpr("v", inner))
	body.WriteString("\t\t\treturn true\n")
	body.WriteString("\t\t})\n")
	if isPtr {
		body.WriteString("\t\treturn &out\n")
	} else {
		body.WriteString("\t\treturn out\n")
	}
	body.WriteString("\t}\n")
	// Native map compat (only when T is convertible from any).
	fmt.Fprintf(&body, "\tif native, ok := value.(map[string]%s); ok {\n", inner)
	body.WriteString("\t\tfor k, v := range native {\n")
	body.WriteString("\t\t\t_ = out.Set(k, v)\n")
	body.WriteString("\t\t}\n")
	if isPtr {
		body.WriteString("\t\treturn &out\n")
	} else {
		body.WriteString("\t\treturn out\n")
	}
	body.WriteString("\t}\n")
	if isPtr {
		body.WriteString("\treturn nil\n")
	} else {
		body.WriteString("\treturn out\n")
	}
	body.WriteString("}")
	return body.String()
}

// convertStaticMapValueExpr returns a Go expression that converts the
// `any`-typed value v to the typed element T. For nested ordered maps
// (with or without a pointer wrapper) the conversion recurses through
// the matching ordered-map helper; for lists (`[]E`) it recurses
// through a per-element-type list helper that walks the `[]any`
// carrier produced by baml.DecodeToOrderedValue. Other shapes fall
// back to a direct type assertion that mirrors what
// `.Interface().(T)` would produce on the upstream pre-ordered-decode
// path.
func convertStaticMapValueExpr(varName, innerType string) string {
	t := strings.TrimSpace(innerType)
	if strings.HasPrefix(t, "baml.OrderedMap[") {
		helper := staticMapHelperName(t)
		return fmt.Sprintf("%s(%s)", helper, varName)
	}
	if strings.HasPrefix(t, "*baml.OrderedMap[") {
		// Pointer-wrapped ordered map. The matching helper has a
		// `Ptr_OM_*` encoding and accepts the ordered carrier
		// directly; route through it. A direct
		// `v.(*baml.OrderedMap[T])` assertion would not match the
		// ordered carrier.
		helper := staticMapHelperName(t)
		return fmt.Sprintf("%s(%s)", helper, varName)
	}
	if strings.HasPrefix(t, "[]") {
		helper := convertListHelperName(t)
		return fmt.Sprintf("%s(%s)", helper, varName)
	}
	return fmt.Sprintf("%s.(%s)", varName, t)
}

// emitSliceWrappedStaticMapHelper renders a helper for a slice-
// wrapped ordered-map type — `[]baml.OrderedMap[T]`,
// `*[]baml.OrderedMap[T]`, `[]*baml.OrderedMap[T]`, etc. The body
// peels the outer pointer (when present), unpacks the `[]any`
// carrier baml.DecodeToOrderedValue produces for list nodes, and
// recurses through convertStaticMapValueExpr on the element type
// so the inner ordered-map (or further-wrapped element) flows
// through its own typed helper.
//
// The shape mirrors emitConvertListHelper so a slice of ordered
// maps and a slice of plain typed values share the same recovery
// rules: nil → nil, identity short-circuit on the already-typed
// slice (and its pointer), and a fall-through that returns nil
// when neither the ordered nor the typed shape applies (rather
// than panicking on an unexpected carrier).
func emitSliceWrappedStaticMapHelper(name, typeExpr string, pkgIdx *pkgTypeIndex) string {
	t := strings.TrimSpace(typeExpr)
	isPtr := strings.HasPrefix(t, "*")
	sliceType := t
	if isPtr {
		sliceType = strings.TrimPrefix(t, "*")
	}
	if !strings.HasPrefix(sliceType, "[]") {
		// Shouldn't happen given the caller's prefix check; emit
		// an empty-return stub so a future drift surfaces as
		// always-empty data; a compile error would be louder but
		// the stub keeps the pass fail-closed without breaking
		// callers that already type-assert against the slice
		// return shape.
		var stub strings.Builder
		fmt.Fprintf(&stub, "// %s is a slice-wrapped static-map helper stub;\n", name)
		fmt.Fprintf(&stub, "// the type expression %q does not start with `[]` or `*[]`.\n", typeExpr)
		fmt.Fprintf(&stub, "func %s(value any) %s {\n", name, typeExpr)
		fmt.Fprintf(&stub, "\tvar out %s\n", typeExpr)
		stub.WriteString("\t_ = value\n")
		stub.WriteString("\treturn out\n")
		stub.WriteString("}")
		return stub.String()
	}
	elem := strings.TrimPrefix(sliceType, "[]")

	var b strings.Builder
	fmt.Fprintf(&b, "// %s converts the runtime []any carrier produced\n", name)
	fmt.Fprintf(&b, "// by baml.DecodeToOrderedValue into a typed %s,\n", typeExpr)
	b.WriteString("// recursing through the matching ordered-map or list helper\n")
	b.WriteString("// for each element so CFFI insertion order is preserved.\n")
	fmt.Fprintf(&b, "func %s(value any) %s {\n", name, typeExpr)
	b.WriteString("\tif value == nil {\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t}\n")
	// Identity short-circuit: the runtime carrier may already be
	// the exact return type (re-decoded by a previous helper, or
	// fed from a non-CFFI path).
	fmt.Fprintf(&b, "\tif typed, ok := value.(%s); ok {\n", typeExpr)
	b.WriteString("\t\treturn typed\n")
	b.WriteString("\t}\n")
	if isPtr {
		// For `*[]T` returns, also accept a bare `[]T` and take its
		// address so the optional wrapper sits over the rebuilt slice.
		fmt.Fprintf(&b, "\tif typed, ok := value.(%s); ok {\n", sliceType)
		b.WriteString("\t\treturn &typed\n")
		b.WriteString("\t}\n")
	} else {
		// For `[]T` returns, also accept a `*[]T` and dereference.
		fmt.Fprintf(&b, "\tif ptr, ok := value.(*%s); ok {\n", sliceType)
		b.WriteString("\t\tif ptr == nil {\n")
		b.WriteString("\t\t\treturn nil\n")
		b.WriteString("\t\t}\n")
		b.WriteString("\t\treturn *ptr\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("\tlistVal, ok := value.([]any)\n")
	b.WriteString("\tif !ok {\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, "\tout := make(%s, len(listVal))\n", sliceType)
	b.WriteString("\tfor i, ev := range listVal {\n")
	// Mirror the ordered-map helper's null-holder preservation: a
	// nil slot in the `[]any` carrier must be recorded as nil in
	// the typed slice when the element is nilable. The raw
	// assertion path otherwise panics on an untyped nil.
	emitStaticMapElementGuard(&b, "\t\t", "ev", elem, pkgIdx, "out[i] = nil\n\t\t\tcontinue\n")
	fmt.Fprintf(&b, "\t\tout[i] = %s\n", convertStaticMapValueExpr("ev", elem))
	b.WriteString("\t}\n")
	if isPtr {
		b.WriteString("\treturn &out\n")
	} else {
		b.WriteString("\treturn out\n")
	}
	b.WriteString("}")
	return b.String()
}

// emitConvertListHelper renders the per-element-type list conversion
// helper. The body normalises the `[]any` carrier
// baml.DecodeToOrderedValue produces for list nodes into a typed
// `[]E`, recursing into ordered-map / list helpers for composite
// element types. A direct `[]E` carrier from the legacy Decode path
// is also accepted as a compat fall-through so a generated client
// fed through the new helper from a path that still produced a
// concrete slice does not panic.
func emitConvertListHelper(name, listType string, pkgIdx *pkgTypeIndex) string {
	elem := strings.TrimPrefix(strings.TrimSpace(listType), "[]")

	var b strings.Builder
	fmt.Fprintf(&b, "// %s converts the runtime []any carrier produced\n", name)
	fmt.Fprintf(&b, "// by baml.DecodeToOrderedValue into a typed %s while\n", listType)
	b.WriteString("// preserving element order. Composite element types recurse\n")
	b.WriteString("// through the matching ordered-map or list helper.\n")
	fmt.Fprintf(&b, "func %s(value any) %s {\n", name, listType)
	b.WriteString("\tif value == nil {\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t}\n")
	// Pointer wrapping is not used for list fields today, but accept
	// `*[]E` defensively so an optional-list wrapper from a future
	// generator shape does not panic here.
	fmt.Fprintf(&b, "\tif ptr, ok := value.(*%s); ok {\n", listType)
	b.WriteString("\t\tif ptr == nil {\n")
	b.WriteString("\t\t\treturn nil\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\treturn *ptr\n")
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, "\tif typed, ok := value.(%s); ok {\n", listType)
	b.WriteString("\t\treturn typed\n")
	b.WriteString("\t}\n")
	b.WriteString("\tlistVal, ok := value.([]any)\n")
	b.WriteString("\tif !ok {\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, "\tout := make(%s, len(listVal))\n", listType)
	b.WriteString("\tfor i, ev := range listVal {\n")
	// Preserve nil slots in the `[]any` carrier when the element
	// type is nilable; without the guard a raw `ev.(T)` assertion
	// panics on untyped nil for pointer / alias-to-pointer / slice
	// / map / interface / chan / func elements.
	emitStaticMapElementGuard(&b, "\t\t", "ev", elem, pkgIdx, "out[i] = nil\n\t\t\tcontinue\n")
	fmt.Fprintf(&b, "\t\tout[i] = %s\n", convertStaticMapValueExpr("ev", elem))
	b.WriteString("\t}\n")
	b.WriteString("\treturn out\n")
	b.WriteString("}")
	return b.String()
}

// emitStaticMapElementGuard writes a `if <var> == nil { <onNil> }`
// guard into b when the inner-element conversion would otherwise be
// a raw `<var>.(T)` assertion AND the element type is nilable. The
// guard preserves nil entries from the BAML runtime by routing them
// through the caller-supplied onNil body (which sets the sink to nil
// and skips the typed assertion) before the assertion has a chance
// to panic on untyped nil.
//
// indent is the leading tabs that match the surrounding scope (so
// the rendered block lines up with sibling statements). onNil is the
// caller-supplied body inserted inside the guard — it carries both
// the sink assignment (`out[k] = nil` / `out[i] = nil`) and the
// continuation statement (`return true` for the ranger callback,
// `continue` for the for-loop). Indentation of onNil's first line
// is already accounted for by the caller (it must begin at indent +
// one tab); onNil's trailing newline is preserved so the closing
// brace lands on its own line.
//
// No-op when the inner type routes through a helper (nested ordered
// map, slice helper) — the helper handles its own nil case — or when
// the element type is non-nilable (struct, builtin value type).
func emitStaticMapElementGuard(b *strings.Builder, indent, varName, innerType string, pkgIdx *pkgTypeIndex, onNil string) {
	if !staticMapElementUsesRawAssertion(innerType) {
		return
	}
	if !pkgIdx.isNilableElementType(innerType) {
		return
	}
	fmt.Fprintf(b, "%sif %s == nil {\n", indent, varName)
	fmt.Fprintf(b, "%s\t%s", indent, onNil)
	fmt.Fprintf(b, "%s}\n", indent)
}

// staticMapElementUsesRawAssertion reports whether convertStaticMapValueExpr
// would emit a bare `v.(T)` assertion for the given inner type — as
// opposed to recursing through a helper. Helper-routed shapes
// (`baml.OrderedMap[T]`, `*baml.OrderedMap[T]`, `[]E`) handle nil
// themselves, so the per-element guard would be both redundant and
// structurally incorrect (the helper accepts a nil `any` and returns
// the right nil/zero, which is what `out.Set(k, helper(v))` already
// stores).
func staticMapElementUsesRawAssertion(innerType string) bool {
	t := strings.TrimSpace(innerType)
	if strings.HasPrefix(t, "baml.OrderedMap[") {
		return false
	}
	if strings.HasPrefix(t, "*baml.OrderedMap[") {
		return false
	}
	if strings.HasPrefix(t, "[]") {
		return false
	}
	return true
}

// staticMapHelperCore returns the supporting bits the helpers share.
// Currently empty; reserved as a hook so a future shared utility
// (panic-on-mismatch instrumentation, telemetry, etc.) can land in one
// well-known place without revisiting every helper emission.
func staticMapHelperCore() string {
	return ""
}

// pkgTypeIndex caches the type-declaration topology of a single
// generated package directory. It feeds the recursive-alias guard
// that protects the static-map rewrite from triggering a Go compiler
// ICE on `baml.OrderedMap[T]` instantiations where T is a recursive
// type alias (e.g. the integration `JsonValue = *Union6...` shape).
//
// Two maps are kept:
//
//   - aliases: every `type X = Y` declaration's RHS expression.
//     `JsonValue` lives here.
//   - defs: every non-alias `type X T` declaration's RHS (struct
//     types, interface types, named function types, etc.).
//     `Union6...` lives here.
//
// The split matters because only alias chains trigger the ICE; a
// recursive struct without an alias hop compiles fine. Restricting
// the recursion check to alias-rooted element types avoids
// over-skipping plain struct recursion.
type pkgTypeIndex struct {
	aliases map[string]ast.Expr
	defs    map[string]ast.Expr
	// bamlImportPath caches the BAML pkg import path declared by any
	// sibling file in the package directory. The static-map rewrite
	// reads this when the file under rewrite does not import the pkg
	// itself (generator-emitted siblings like cmd/embed's embed.go),
	// so the added `baml` alias adopts the same fork/upstream path
	// the surrounding files already bind.
	bamlImportPath string
}

// newPkgTypeIndex parses every .go file in dir and collects the
// alias / def graph. Parse errors are tolerated per file because the
// static-map pass walks unverified generator output; a corrupted
// sibling file should not block the rewriter for the rest of the
// package. Unparseable files contribute nothing to the index and the
// recursive-alias check fails-open (no skip).
func newPkgTypeIndex(dir string) (*pkgTypeIndex, error) {
	idx := &pkgTypeIndex{
		aliases: map[string]ast.Expr{},
		defs:    map[string]ast.Expr{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return idx, nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), e.Name(), data, parser.SkipObjectResolution)
		if parseErr != nil || file == nil {
			continue
		}
		if idx.bamlImportPath == "" {
			idx.bamlImportPath = findBAMLPkgImportPath(file)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil || ts.Type == nil {
					continue
				}
				if ts.Assign.IsValid() {
					idx.aliases[ts.Name.Name] = ts.Type
				} else {
					idx.defs[ts.Name.Name] = ts.Type
				}
			}
		}
	}
	return idx, nil
}

// isRecursiveMapElement reports whether elemExpr names a type whose
// transitive alias-and-struct closure references the same name in
// another map element position. The recursion check fires only when
// elemExpr is a bare identifier registered as an alias in the
// package: non-alias struct recursion does not trigger the Go compiler
// ICE on `baml.OrderedMap[T]` instantiations, so the broader
// definition would over-skip and lose order preservation on plain
// recursive structs unnecessarily.
//
// Pointer / slice wrappers on elemExpr are not unwrapped here; the
// caller passes the raw element type captured from `map[string]<T>`,
// and Go's recursive-alias hazard sits on the bare identifier path.
func (idx *pkgTypeIndex) isRecursiveMapElement(elemExpr string) bool {
	target := strings.TrimSpace(elemExpr)
	if !isSimpleGoIdent(target) {
		return false
	}
	if _, isAlias := idx.aliases[target]; !isAlias {
		return false
	}
	return idx.reachesMapValue(target, target, map[string]bool{})
}

// reachesMapValue walks the type-decl graph from `name` and returns
// true when any reachable map element position resolves back to
// `target`. The walk follows alias-RHS expressions and struct field
// types; idents inside those expressions are followed by recursion
// into reachesMapValue with the same `target`.
func (idx *pkgTypeIndex) reachesMapValue(name, target string, visited map[string]bool) bool {
	if visited[name] {
		return false
	}
	visited[name] = true
	expr := idx.exprFor(name)
	if expr == nil {
		return false
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if m, ok := n.(*ast.MapType); ok {
			if idx.valueRefersTo(m.Value, target, map[string]bool{}) {
				found = true
				return false
			}
		}
		return true
	})
	if found {
		return true
	}
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok {
			if idx.reachesMapValue(id.Name, target, visited) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// valueRefersTo reports whether the map-value expression resolves
// (through pointer / slice prefixes and alias hops) to the target
// identifier. The visited set guards against infinite loops on
// `type A = *B; type B = *A` style cycles that do not themselves
// involve maps.
func (idx *pkgTypeIndex) valueRefersTo(expr ast.Expr, target string, visited map[string]bool) bool {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
			continue
		case *ast.ArrayType:
			expr = t.Elt
			continue
		}
		break
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	if id.Name == target {
		return true
	}
	if visited[id.Name] {
		return false
	}
	visited[id.Name] = true
	if a, ok := idx.aliases[id.Name]; ok {
		return idx.valueRefersTo(a, target, visited)
	}
	return false
}

// isNilableElement reports whether elemName, used as a `map[string]T`
// element type in the surrounding package, resolves to a nilable Go
// type (pointer, slice, map, channel, function, interface). The
// native-map helper reads this to decide whether a null map value
// from the BAML runtime should preserve the key with a `nil` value
// (nilable types) or skip the entry (non-nilable types — the runtime
// never produces untyped nil for these, and writing the zero value
// would falsely advertise a present entry).
//
// Walks through alias hops; non-alias struct/named definitions in
// the package are treated as non-nilable. Identifiers that resolve
// to nothing in the package (Go builtins like `int`, `string`, or
// imports from sibling packages) are reported as non-nilable on the
// rationale that BAML's recursive-alias trap fires only on pointer
// aliases — the only known caller of the native-map helper today.
func (idx *pkgTypeIndex) isNilableElement(elemName string) bool {
	if idx == nil {
		return true
	}
	target := strings.TrimSpace(elemName)
	if !isSimpleGoIdent(target) {
		return false
	}
	return idx.isNilableIdent(target, map[string]bool{})
}

// isNilableElementType reports whether typeExprText, used as the
// element type T of a `baml.OrderedMap[T]` (or a `[]T` slice element),
// resolves to a nilable Go type. Unlike isNilableElement, the input
// is a free-form type expression where isNilableElement accepts only
// a bare identifier: the ordered-map static helper sees element
// types like `*ConcreteClass`, `AliasPtr` (an alias to a pointer),
// `*baml.OrderedMap[X]`, etc.
// Pointer / slice / map / chan / func / interface shapes are nilable
// directly; bare identifiers route through the package index's alias
// chain so a `type AliasPtr = *T` declaration surfaces as nilable.
//
// A nil receiver and an unparseable expression default to non-nilable
// (the conservative call for emitting the guard: only emit it when
// we are confident the assertion target can actually accept nil; a
// false positive would emit dead code on a non-nilable element, while
// a false negative would drop nil entries — but a false positive on
// a struct element triggers a compile error because `nil` is not
// assignable to a struct, so being strict here is the right default
// for unknown shapes).
func (idx *pkgTypeIndex) isNilableElementType(typeExprText string) bool {
	t := strings.TrimSpace(typeExprText)
	if t == "" {
		return false
	}
	expr, err := parser.ParseExpr(t)
	if err != nil || expr == nil {
		return false
	}
	if idx == nil {
		// Walker is independent of the receiver for structural shapes;
		// pointer / slice / map / chan / func / interface return true
		// without consulting any alias table. Only bare-identifier
		// inputs need the index, and a nil index treats them as
		// unresolved (non-nilable). This matches how the native-map
		// helper degrades when the package index is absent.
		dummy := &pkgTypeIndex{aliases: map[string]ast.Expr{}, defs: map[string]ast.Expr{}}
		return dummy.isNilableTypeExpr(expr, map[string]bool{})
	}
	return idx.isNilableTypeExpr(expr, map[string]bool{})
}

func (idx *pkgTypeIndex) isNilableIdent(name string, visited map[string]bool) bool {
	// Predeclared interface identifiers are nilable. `any` and `error`
	// are not registered in idx.aliases or idx.defs because the
	// package walk only collects user-declared type names; classifying
	// them here avoids the false-negative where a static map field
	// like map[string]any or map[string]error skips the nil-guard and
	// then panics on a single-value `v.(any)` / `v.(error)` assertion
	// against an untyped nil at decode time.
	if name == "any" || name == "error" {
		return true
	}
	if visited[name] {
		return false
	}
	visited[name] = true
	if a, ok := idx.aliases[name]; ok {
		return idx.isNilableTypeExpr(a, visited)
	}
	if d, ok := idx.defs[name]; ok {
		return idx.isNilableTypeExpr(d, visited)
	}
	return false
}

func (idx *pkgTypeIndex) isNilableTypeExpr(expr ast.Expr, visited map[string]bool) bool {
	switch t := expr.(type) {
	case *ast.StarExpr, *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return true
	case *ast.Ident:
		return idx.isNilableIdent(t.Name, visited)
	case *ast.ParenExpr:
		return idx.isNilableTypeExpr(t.X, visited)
	}
	return false
}

// exprFor returns the declared RHS expression for name when name is
// registered as either an alias or a struct/def in the package
// index. Aliases take precedence on the off chance a name appears in
// both maps (which the indexer prevents by construction, but the
// lookup stays safe regardless).
func (idx *pkgTypeIndex) exprFor(name string) ast.Expr {
	if a, ok := idx.aliases[name]; ok {
		return a
	}
	if d, ok := idx.defs[name]; ok {
		return d
	}
	return nil
}

// isSimpleGoIdent reports whether s is a Go identifier (a leading
// letter or `_` followed by letters / digits / underscores). The
// pkgTypeIndex check uses this to short-circuit on qualified names
// like `types.Foo` and on composite expressions like `*Foo`, neither
// of which the recursive-alias guard handles.
func isSimpleGoIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// recursiveMapAssertElement returns the element-type identifier when
// the given type-assertion expression names a recursive map element
// that pass 1 chose to leave as native `map[string]T`. Used by passes
// 2 and 3 to route the assert through a native-map helper — the
// OrderedMap helper would not match the un-rewritten field type.
// Returns the element identifier and true on a match; false otherwise.
//
// Accepts `map[string]T` and `*map[string]T` (slice wrappers around
// recursive maps are not generated by BAML today and stay out of
// scope until a concrete case appears).
func recursiveMapAssertElement(typeExpr string, idx *pkgTypeIndex) (string, bool) {
	if idx == nil {
		return "", false
	}
	t := strings.TrimSpace(typeExpr)
	t = strings.TrimPrefix(t, "*")
	t = strings.TrimSpace(t)
	const prefix = "map[string]"
	if !strings.HasPrefix(t, prefix) {
		return "", false
	}
	elem := strings.TrimSpace(strings.TrimPrefix(t, prefix))
	if !idx.isRecursiveMapElement(elem) {
		return "", false
	}
	return elem, true
}

// nativeMapHelperName names the per-element-type native-map helper
// the recursive-alias path routes through. The pointer variant has a
// `Ptr_` suffix mirroring the ordered-map helper naming scheme so
// the helper-file generator can dispatch by prefix.
func nativeMapHelperName(elemName string, isPtr bool) string {
	if isPtr {
		return "bamlNativeMapAs_Ptr_" + elemName
	}
	return "bamlNativeMapAs_" + elemName
}

// emitNativeMapHelper renders one native-map conversion helper. The
// helper turns the patched runtime's ordered carrier (or an already-
// native Go map) into a plain `map[string]T`. CFFI iteration order
// is consumed during conversion but the resulting Go map is
// unordered — the recursive-alias path explicitly trades order
// preservation for compilable output on the ICE-prone shape.
//
// When isPtr is true the helper returns `*map[string]T` so the call
// site that previously assigned `&value` to the variant field can
// stay structurally identical (the assert was on the non-pointer
// shape; only direct top-level returns ever assert to the pointer
// shape, but emitting both variants keeps the worklist symmetric).
func emitNativeMapHelper(name, elemName string, isPtr, elemNilable bool) string {
	nativeType := "map[string]" + elemName
	retType := nativeType
	if isPtr {
		retType = "*" + nativeType
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// %s converts the runtime ordered carrier produced\n", name)
	fmt.Fprintf(&b, "// by baml.DecodeToOrderedValue into a native %s.\n", retType)
	b.WriteString("// The recursive-type-alias path keeps the field as a native\n")
	b.WriteString("// Go map (CFFI insertion order is lost on this arm) so the\n")
	b.WriteString("// rewriter avoids the Go compiler ICE on baml.OrderedMap[T]\n")
	b.WriteString("// where T is a self-referential alias.\n")
	fmt.Fprintf(&b, "func %s(value any) %s {\n", name, retType)
	b.WriteString("\tif value == nil {\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t}\n")
	// Identity short-circuit for the return type.
	fmt.Fprintf(&b, "\tif typed, ok := value.(%s); ok {\n", retType)
	b.WriteString("\t\treturn typed\n")
	b.WriteString("\t}\n")
	if isPtr {
		fmt.Fprintf(&b, "\tif bare, ok := value.(%s); ok {\n", nativeType)
		b.WriteString("\t\treturn &bare\n")
		b.WriteString("\t}\n")
	} else {
		fmt.Fprintf(&b, "\tif ptr, ok := value.(*%s); ok {\n", nativeType)
		b.WriteString("\t\tif ptr == nil {\n")
		b.WriteString("\t\t\treturn nil\n")
		b.WriteString("\t\t}\n")
		b.WriteString("\t\treturn *ptr\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("\tif ranger, ok := value.(interface {\n")
	b.WriteString("\t\tRangeAny(func(string, any) bool)\n")
	b.WriteString("\t\tLen() int\n")
	b.WriteString("\t}); ok {\n")
	fmt.Fprintf(&b, "\t\tout := make(%s, ranger.Len())\n", nativeType)
	b.WriteString("\t\tranger.RangeAny(func(k string, v any) bool {\n")
	// Null-holder preservation: BAML's recursive aliases (`type
	// JsonValue = *Union6...`) include `null` as a variant. The
	// runtime decodes null map values to an untyped nil in the
	// ranger callback; a type assertion against the concrete element
	// type rejects untyped nil, so without this branch the key would
	// silently drop from the produced map — converting documented
	// order loss into undocumented data loss. For nilable element
	// types the explicit `out[k] = nil` keeps the key with a nil
	// value; non-nilable types skip the entry since the BAML decoder
	// does not produce untyped nil for them, and writing a zero
	// struct would falsely advertise a present-but-empty value.
	if elemNilable {
		b.WriteString("\t\t\tif v == nil {\n")
		b.WriteString("\t\t\t\tout[k] = nil\n")
		b.WriteString("\t\t\t\treturn true\n")
		b.WriteString("\t\t\t}\n")
	}
	fmt.Fprintf(&b, "\t\t\tif cv, ok := v.(%s); ok {\n", elemName)
	b.WriteString("\t\t\t\tout[k] = cv\n")
	b.WriteString("\t\t\t}\n")
	b.WriteString("\t\t\treturn true\n")
	b.WriteString("\t\t})\n")
	if isPtr {
		b.WriteString("\t\treturn &out\n")
	} else {
		b.WriteString("\t\treturn out\n")
	}
	b.WriteString("\t}\n")
	if isPtr {
		b.WriteString("\treturn nil\n")
	} else {
		b.WriteString("\treturn nil\n")
	}
	b.WriteString("}")
	return b.String()
}

// helperBodyReferencesBAML reports whether body has a `baml.`
// selector outside of `//` line comments. The static-map helper
// renderer drops the baml import from the helper file when no
// emitted helper actually needs it, but every helper still has a
// comment line that documents the `baml.DecodeToOrderedValue`
// runtime entrypoint. A naive substring check on the rendered
// helper would falsely flag the comment as a baml use; this walker
// strips `//` comment trailers per line first.
func helperBodyReferencesBAML(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, "baml.") {
			return true
		}
	}
	return false
}

// nativeMapHelperElementFromName reverses nativeMapHelperName. Returns
// the element identifier and whether the helper is the pointer
// variant. False on names that do not match the
// `bamlNativeMapAs_(Ptr_)?<Ident>` shape so the worklist skips
// unrelated references.
func nativeMapHelperElementFromName(name string) (string, bool, bool) {
	const prefix = "bamlNativeMapAs_"
	if !strings.HasPrefix(name, prefix) {
		return "", false, false
	}
	rest := strings.TrimPrefix(name, prefix)
	isPtr := false
	if strings.HasPrefix(rest, "Ptr_") {
		isPtr = true
		rest = strings.TrimPrefix(rest, "Ptr_")
	}
	if !isSimpleGoIdent(rest) {
		return "", false, false
	}
	return rest, isPtr, true
}
