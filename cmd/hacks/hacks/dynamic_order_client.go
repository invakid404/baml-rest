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
	return filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		changed, applyErr := patchGeneratedStaticMapFile(path)
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
func patchGeneratedStaticMapFile(path string) (bool, error) {
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

	newBody, rewroteAny, err := rewriteStaticMapTypesAndAsserts(body)
	if err != nil {
		return false, err
	}
	if !rewroteAny {
		return false, nil
	}

	// Validate the rewrite still parses.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, path, []byte(newBody), parser.ParseComments); err != nil {
		// Persist the bad output to a sibling .reject file for
		// post-mortem inspection so the test harness has something
		// concrete to diff against the original.
		_ = os.WriteFile(path+".reject", []byte(newBody), 0o644)
		return false, fmt.Errorf("static-map rewrite produced invalid Go: %w", err)
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
	if err := ensureStaticMapHelperFile(filepath.Dir(path), prettied); err != nil {
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
func rewriteStaticMapTypesAndAsserts(body string) (string, bool, error) {
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
	newBody, changedTypes := rewriteMapStringTypeExprs(body)
	if changedTypes {
		rewroteAny = true
		body = newBody
	}

	// Pass 2: rewrite Decode().Interface() casts.
	newBody, changedAsserts := rewriteStaticMapDecodeAsserts(body)
	if changedAsserts {
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
func rewriteMapStringTypeExprs(body string) (string, bool) {
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
		nextNonSpace := typeEnd
		for nextNonSpace < len(body) && (body[nextNonSpace] == ' ' || body[nextNonSpace] == '\t') {
			nextNonSpace++
		}
		if nextNonSpace < len(body) && body[nextNonSpace] == '{' {
			searchFrom = typeEnd
			continue
		}
		// Recurse into the inner type expression so nested maps are
		// rewritten first. The recursion produces a stable substring
		// the outer rewrite can splice in.
		innerRewritten, _ := rewriteMapStringTypeExprs(inner)
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
func rewriteStaticMapDecodeAsserts(body string) (string, bool) {
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
		// Walk back to find the Decode call start. Only the immediate
		// `baml.Decode(<arg>).Interface()` pattern qualifies; the
		// pre-marker substring must end with `).Interface()` (i.e. a
		// decode call followed by .Interface()). If the assert sits on
		// a different receiver, leave it alone.
		preMarker := body[:absIdx]
		const wantInterfaceCall = ".Interface()"
		if !strings.HasSuffix(preMarker, "") {
			// Tautological guard; the slice always satisfies the
			// suffix test below. Kept for symmetry with the matching
			// suffix check on the trailing side.
		}
		_ = wantInterfaceCall
		// Find `baml.Decode(` to the left.
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
		// Normalise the type expression to its `baml.OrderedMap[...]`
		// form (the pass-1 rewriter may have already done this; pass
		// it through again to be sure).
		normalised, _ := rewriteMapStringTypeExprs(typeExpr)
		helperName := staticMapHelperName(normalised)
		replacement := fmt.Sprintf("%s(baml.DecodeToOrderedValue(%s))", helperName, decodeArg)
		body = body[:decodeStart] + replacement + body[assertCloseIdx+1:]
		searchFrom = decodeStart + len(replacement)
		changed = true
	}
	return body, changed
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
// name. Nested maps and dotted/identifier types are encoded so the
// resulting identifier is unique per shape but stable across reruns.
func staticMapHelperName(typeExpr string) string {
	enc := typeExpr
	enc = strings.ReplaceAll(enc, "baml.OrderedMap[", "OM_")
	enc = strings.ReplaceAll(enc, "[", "_")
	enc = strings.ReplaceAll(enc, "]", "_")
	enc = strings.ReplaceAll(enc, ".", "_")
	enc = strings.ReplaceAll(enc, "*", "Ptr_")
	enc = strings.ReplaceAll(enc, " ", "")
	enc = strings.Trim(enc, "_")
	return "bamlOrderedAs_" + enc
}

// ensureStaticMapHelperFile writes / refreshes a per-package helper
// file alongside the generated client files. The file declares every
// `bamlOrderedAs_*` helper the rewritten code references, using a
// generic core helper that builds the typed `baml.OrderedMap[T]` from
// any runtime value the patched serde produces (OrderedFields directly,
// `*baml.OrderedMap[T]` pointer wrappers from optional fields, or a
// legacy native map as a compat fallback).
//
// The file is regenerated wholesale each time so a re-run after a
// rewrite-shape change does not stack stale helpers. The helper set is
// deduplicated by scanning the file just rewritten for
// `bamlOrderedAs_*` call sites.
func ensureStaticMapHelperFile(packageDir string, rewrittenSrc []byte) error {
	// Read every .go file in the package and collect all helper names
	// any of them references. The helper file is one declaration per
	// helper, defined at package scope so cross-file references resolve.
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return err
	}
	names := map[string]struct{}{}
	helperRe := regexp.MustCompile(`bamlOrderedAs_[A-Za-z0-9_]+`)
	collect := func(src []byte) {
		for _, m := range helperRe.FindAll(src, -1) {
			names[string(m)] = struct{}{}
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
	if len(names) == 0 {
		// No helpers referenced — remove any stale helper file from a
		// prior run so the package stays clean.
		path := filepath.Join(packageDir, staticMapHelperBaseName)
		if _, statErr := os.Stat(path); statErr == nil {
			return os.Remove(path)
		}
		return nil
	}

	// Determine the package name and the BAML `pkg` import path from a
	// sibling Go file. The import path varies by build context: the
	// regenerate-dynclient pipeline rewrites generated-client imports to
	// the patched-fork path AFTER the static-map pass, while the
	// cmd/build/build.sh integration pipeline leaves them on the
	// upstream `github.com/boundaryml/baml/...` path. Adopting whatever
	// the surrounding files use keeps the helper file resolvable in both
	// pipelines (and lets the post-pass rewriter retarget the helper
	// along with everything else when it runs).
	pkgName := ""
	bamlImportPath := ""
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
		if pkgName != "" && bamlImportPath != "" {
			break
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

	sortedNames := make([]string, 0, len(names))
	for n := range names {
		sortedNames = append(sortedNames, n)
	}
	// Stable order so re-runs produce byte-stable output.
	for i := 1; i < len(sortedNames); i++ {
		for j := i; j > 0 && sortedNames[j] < sortedNames[j-1]; j-- {
			sortedNames[j], sortedNames[j-1] = sortedNames[j-1], sortedNames[j]
		}
	}

	var buf strings.Builder
	buf.WriteString("// Code generated by cmd/hacks/hacks/dynamic_order_client.go; DO NOT EDIT.\n")
	buf.WriteString(staticMapOrderedHelperMarker + "\n\n")
	buf.WriteString("package " + pkgName + "\n\n")
	buf.WriteString("import (\n")
	fmt.Fprintf(&buf, "\tbaml %q\n", bamlImportPath)
	buf.WriteString(")\n\n")
	for _, name := range sortedNames {
		typeExpr, ok := staticMapHelperTypeFromName(name)
		if !ok {
			continue
		}
		buf.WriteString(emitStaticMapHelper(name, typeExpr))
		buf.WriteString("\n\n")
	}
	buf.WriteString(staticMapHelperCore())

	return os.WriteFile(filepath.Join(packageDir, staticMapHelperBaseName), []byte(buf.String()), 0o644)
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

// staticMapHelperTypeFromName reverses staticMapHelperName. Returns
// the `baml.OrderedMap[T]` type expression the helper produces.
// Returns false for unparseable encodings so a future drift in the
// encoder surfaces as a missing helper; the wrong-typed-helper failure
// mode would otherwise compile and crash at runtime.
func staticMapHelperTypeFromName(name string) (string, bool) {
	const prefix = "bamlOrderedAs_"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	enc := strings.TrimPrefix(name, prefix)
	// Reverse the per-token substitutions. The encoding starts with
	// either `OM_` (plain ordered map) or `Ptr_OM_` (optional/pointer
	// wrapped). Anything else is not a static-map helper this pass
	// emits — skip so a future helper convention does not silently
	// produce wrong-typed code here.
	if !strings.HasPrefix(enc, "OM_") && !strings.HasPrefix(enc, "Ptr_OM_") {
		return "", false
	}
	return decodeStaticMapEnc(enc), true
}

// decodeStaticMapEnc reverses the OM_/_/Ptr_ substitution. OM_ becomes
// `baml.OrderedMap[`; closing `]` is appended for every OM_; underscore
// is a generic separator that maps to dot when followed by an upper-
// case letter and bracket boundary otherwise. The encoding is
// engineered to survive Go-identifier constraints; the decoder uses a
// small state machine to reproduce the original expression.
func decodeStaticMapEnc(enc string) string {
	var out strings.Builder
	depth := 0
	i := 0
	for i < len(enc) {
		if strings.HasPrefix(enc[i:], "OM_") {
			out.WriteString("baml.OrderedMap[")
			depth++
			i += 3
			continue
		}
		if strings.HasPrefix(enc[i:], "Ptr_") {
			out.WriteString("*")
			i += 4
			continue
		}
		c := enc[i]
		if c == '_' {
			// End of an identifier segment. Either a generic close or
			// a dotted-name separator. The disambiguator: peek ahead.
			next := byte(0)
			if i+1 < len(enc) {
				next = enc[i+1]
			}
			if next == 0 || next == '_' || (next >= 'a' && next <= 'z') {
				// dotted separator (e.g. baml_OrderedMap → baml.OrderedMap),
				// but in practice only the legacy `pkg.Type` shape needs
				// this. Empty separator means end of expression.
				out.WriteString(".")
				i++
				continue
			}
			// Close generic.
			if depth > 0 {
				out.WriteString("]")
				depth--
			}
			i++
			continue
		}
		out.WriteByte(c)
		i++
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
func emitStaticMapHelper(name, typeExpr string) string {
	isPtr := strings.HasPrefix(typeExpr, "*")
	innerOrderedType := typeExpr
	if isPtr {
		innerOrderedType = strings.TrimPrefix(typeExpr, "*")
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
// the conversion recurses through the matching helper. For all other
// shapes the conversion uses a type assertion that mirrors what
// `.Interface().(T)` would produce.
func convertStaticMapValueExpr(varName, innerType string) string {
	if strings.HasPrefix(innerType, "baml.OrderedMap[") {
		helper := staticMapHelperName(innerType)
		return fmt.Sprintf("%s(%s)", helper, varName)
	}
	return fmt.Sprintf("%s.(%s)", varName, innerType)
}

// staticMapHelperCore returns the supporting bits the helpers share.
// Currently empty; reserved as a hook so a future shared utility
// (panic-on-mismatch instrumentation, telemetry, etc.) can land in one
// well-known place without revisiting every helper emission.
func staticMapHelperCore() string {
	return ""
}
