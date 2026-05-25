package hacks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
)

// dynamicOrderSonicVersion pins the github.com/bytedance/sonic module
// the generated serde/ordered_fields.go imports. The version matches
// bamlutils/go.mod so the patched fork resolves the same sonic
// implementation the source-of-truth OrderedMap is tested against.
const dynamicOrderSonicVersion = "v1.15.1"

// ApplyDynamicOrderFix patches the BAML module the runtime resolves
// at module-cache or workspace-replace level so DynamicClass.Fields
// preserves CFFI insertion order. The wrapper mirrors
// ApplyRuntimeDeadlockFix: it resolves the BAML module dir, copies it
// out of GOMODCACHE into a writable cache dir if needed, applies the
// transform via ApplyDynamicOrderFixToDir, and installs a go.work
// replace directive when the writable copy was created.
func ApplyDynamicOrderFix(bamlVersion string) error {
	requestedVersion := strings.TrimSpace(bamlVersion)
	if requestedVersion != "" {
		requestedVersion = bamlutils.NormalizeVersion(requestedVersion)
	}

	resolvedVersion, err := bamlModuleVersion()
	if err != nil {
		return err
	}
	moduleDir, err := bamlModuleDir()
	if err != nil {
		return err
	}

	version := resolvedVersion
	if requestedVersion != "" && bamlutils.CompareVersions(requestedVersion, resolvedVersion) != 0 {
		usesLocalReplace, err := moduleUsesLocalReplace(moduleDir)
		if err != nil {
			return err
		}
		if usesLocalReplace {
			version = requestedVersion
		}
	}

	moduleDir, usingPatchedCopy, err := preparePatchedBamlModuleDir(moduleDir, version)
	if err != nil {
		return err
	}

	if err := ApplyDynamicOrderFixToDir(version, moduleDir); err != nil {
		return err
	}

	if usingPatchedCopy {
		if err := setGoWorkReplace("github.com/boundaryml/baml", moduleDir); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDynamicOrderFixToDir applies the dynamic-order patch to a BAML
// module rooted at moduleDir. moduleDir must be a writable BAML source
// tree; no module resolution or go.work editing is performed. The
// transform is split into discrete steps that each fail closed with
// a labeled error so re-runs land on already-patched trees with a
// recognisable signature.
func ApplyDynamicOrderFixToDir(bamlVersion, moduleDir string) error {
	version := strings.TrimSpace(bamlVersion)
	if version != "" {
		version = bamlutils.NormalizeVersion(version)
	}
	if version == "" {
		return fmt.Errorf("baml version is required to apply the dynamic-order fix")
	}
	if moduleDir == "" {
		return fmt.Errorf("module directory is required to apply the dynamic-order fix")
	}

	family, err := detectDecodeFamily(moduleDir)
	if err != nil {
		return fmt.Errorf("detecting BAML serde shape in %s: %w", moduleDir, err)
	}

	if err := writeOrderedFieldsFile(moduleDir); err != nil {
		return fmt.Errorf("writing serde/ordered_fields.go in %s: %w", moduleDir, err)
	}

	if err := patchDecodeGo(moduleDir, family); err != nil {
		return fmt.Errorf("patching serde/decode.go in %s: %w", moduleDir, err)
	}

	if err := patchEncodeGo(moduleDir, family); err != nil {
		return fmt.Errorf("patching serde/encode.go in %s: %w", moduleDir, err)
	}

	if err := patchRawObjectMaps(moduleDir, family); err != nil {
		return fmt.Errorf("patching pkg/rawobjects_*.go in %s: %w", moduleDir, err)
	}

	if err := patchPkgLibGo(moduleDir, family); err != nil {
		return fmt.Errorf("patching pkg/lib.go in %s: %w", moduleDir, err)
	}

	if err := ensureSonicRequire(moduleDir); err != nil {
		return fmt.Errorf("ensuring sonic require in %s/go.mod: %w", moduleDir, err)
	}

	if err := runGoModTidy(moduleDir); err != nil {
		return fmt.Errorf("running go mod tidy on patched module at %s: %w", moduleDir, err)
	}

	fmt.Printf("  Applied dynamic-order fix (family %s) under %s\n", family, moduleDir)
	return nil
}

// decodeFamily names the serde shape variant the patch must produce
// code against. Three observed shapes:
//
//   - familyA (v0.204.x): Decode returns reflect.Value; list field is
//     Values; DynamicUnion uses VariantName.
//   - familyB (v0.215.x): Decode returns (reflect.Value, reflect.Type);
//     list field is Items; DynamicClass body stores value.Elem().
//   - familyC (v0.219+, including v0.222): same return as familyB, but
//     DynamicClass body has scalar-preserving switch logic.
type decodeFamily string

const (
	familyA decodeFamily = "v0.204"
	familyB decodeFamily = "v0.215"
	familyC decodeFamily = "v0.219+"
)

func decodeGoPath(moduleDir string) string {
	return filepath.Join(moduleDir, "engine", "language_client_go", "baml_go", "serde", "decode.go")
}

func encodeGoPath(moduleDir string) string {
	return filepath.Join(moduleDir, "engine", "language_client_go", "baml_go", "serde", "encode.go")
}

func rawObjectsPath(moduleDir, name string) string {
	return filepath.Join(moduleDir, "engine", "language_client_go", "pkg", name)
}

func orderedFieldsGoPath(moduleDir string) string {
	return filepath.Join(moduleDir, "engine", "language_client_go", "baml_go", "serde", "ordered_fields.go")
}

func pkgLibGoPath(moduleDir string) string {
	return filepath.Join(moduleDir, "engine", "language_client_go", "pkg", "lib.go")
}

// detectDecodeFamily parses serde/decode.go and infers the BAML
// version family from the Decode function signature plus the
// DynamicClass.Decode body shape. Returns an error when none of the
// three known shapes match — the orchestrator then refuses to patch
// the fork; a silent rewrite would be worse than a loud failure.
func detectDecodeFamily(moduleDir string) (decodeFamily, error) {
	path := decodeGoPath(moduleDir)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}

	var decodeReturns int
	var dynamicClassBody *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name == nil {
			continue
		}
		if fn.Recv == nil && fn.Name.Name == "Decode" {
			if fn.Type.Results != nil {
				decodeReturns = len(fn.Type.Results.List)
			}
		}
		if fn.Recv != nil && fn.Name.Name == "Decode" {
			if len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != "DynamicClass" {
				continue
			}
			dynamicClassBody = fn.Body
		}
	}

	if decodeReturns == 0 {
		return "", fmt.Errorf("no top-level Decode function found in %s", path)
	}
	if dynamicClassBody == nil {
		return "", fmt.Errorf("no DynamicClass.Decode method found in %s", path)
	}

	if decodeReturns == 1 {
		return familyA, nil
	}
	if decodeReturns != 2 {
		return "", fmt.Errorf("unexpected Decode return arity %d in %s; supported BAML serde shapes return either 1 or 2 results", decodeReturns, path)
	}

	// familyB stores value.Elem() directly; familyC has a switch on goType.
	hasSwitch := false
	ast.Inspect(dynamicClassBody, func(n ast.Node) bool {
		if _, ok := n.(*ast.SwitchStmt); ok {
			hasSwitch = true
			return false
		}
		return true
	})
	if hasSwitch {
		return familyC, nil
	}
	return familyB, nil
}

// writeOrderedFieldsFile reads bamlutils/orderedmap.go (resolved
// relative to the cmd/hacks/hacks package at runtime via the embedded
// constant below), rewrites the package clause and a handful of error
// strings, and writes the result alongside an OrderedFields type
// alias into serde/ordered_fields.go.
func writeOrderedFieldsFile(moduleDir string) error {
	src, err := readOrderedMapSource()
	if err != nil {
		return err
	}

	body := string(src)
	body = strings.Replace(body, "package bamlutils", "package serde", 1)
	body = strings.ReplaceAll(body, `errors.New("bamlutils:`, `errors.New("serde:`)

	body += "\n" + orderedFieldsAppend + "\n"

	dst := orderedFieldsGoPath(moduleDir)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(body), 0o644)
}

// orderedFieldsAppend is the small wrapper appended to the generated
// serde/ordered_fields.go so the patched runtime can reference the
// concrete OrderedMap[any] specialisation by a short, stable name.
const orderedFieldsAppend = `
// OrderedFields is the OrderedMap specialisation used by serde for
// dynamic class fields and CFFI map values. The alias keeps the
// rest of the serde package free of generic argument noise.
type OrderedFields = OrderedMap[any]

// NewOrderedFields constructs an empty OrderedFields with the
// supplied capacity hint. Mirrors the make(map[string]any, capacity)
// shape the unpatched runtime used to use.
func NewOrderedFields(capacity int) OrderedFields {
	return OrderedFields{
		keys: make([]string, 0, capacity),
		vals: make(map[string]any, capacity),
	}
}
`

// readOrderedMapSource resolves the canonical source of OrderedMap.
// orderedMapSourcePath is overridable by tests so fixture runs can
// point at a copy of orderedmap.go without depending on the live
// working directory.
var orderedMapSourcePath = ""

func readOrderedMapSource() ([]byte, error) {
	if orderedMapSourcePath != "" {
		return os.ReadFile(orderedMapSourcePath)
	}
	candidates := []string{
		"bamlutils/orderedmap.go",
	}
	cwd, err := os.Getwd()
	if err == nil {
		for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			path := filepath.Join(dir, "bamlutils", "orderedmap.go")
			if _, statErr := os.Stat(path); statErr == nil {
				return os.ReadFile(path)
			}
		}
	}
	for _, c := range candidates {
		if data, readErr := os.ReadFile(c); readErr == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("cannot locate bamlutils/orderedmap.go from %s", cwd)
}

// patchDecodeGo rewrites serde/decode.go to use OrderedFields for the
// DynamicClass struct field, replace the field initialisation, route
// nested decode through DecodeToOrderedValue, and append the
// version-family-specific DecodeToOrderedValue helper. The function
// fails closed when any expected anchor is missing so a future BAML
// release that drifts the shape surfaces a clear error.
func patchDecodeGo(moduleDir string, family decodeFamily) error {
	path := decodeGoPath(moduleDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := string(raw)

	// Strict sentinel: only short-circuit when the struct-field rewrite
	// AND the IsSinglePattern dispatch helper AND the per-family
	// isOrderableMapValueType helper AND the new ordered map decode
	// marker all landed. A cached patched-module tree from an earlier
	// hack revision (issue #365: dynamic-only patch) satisfies only the
	// first three markers; without the fourth the strict check forces a
	// re-pass so the new decodeMapValue body wires in. Each step below
	// handles its already-patched state idempotently, so a re-pass over
	// a fully or partially patched tree converges without
	// double-applying.
	if strings.Contains(body, "Fields OrderedFields") &&
		strings.Contains(body, "func isOrderableSinglePattern(") &&
		strings.Contains(body, "func isOrderableMapValueType(") &&
		strings.Contains(body, orderedMapDecodeMarker) {
		return nil
	}

	// 1. Replace the DynamicClass struct field type. BAML's upstream
	// source uses a single space; gofmt's tab-aligned struct field
	// layout in patched copies uses a tab. Match either. When the
	// declaration is already in its post-patch shape (cached patched
	// tree), the no-match branch falls through silently so the rest
	// of the rewrite can update the file in place.
	patched, ok := replaceFirstFieldDecl(body)
	if !ok {
		if !strings.Contains(body, "Fields OrderedFields") {
			return fmt.Errorf("could not find a `Fields map[string]any` declaration in %s; BAML serde shape may have changed", path)
		}
	} else {
		body = patched
	}

	// 2. Replace the DynamicClass.Decode body. The function body is
	// rewritten wholesale because each family has different scalar
	// handling; constructing a single AST-level transform that covers
	// all three is more brittle than emitting the canonical patched
	// body. The wrapping function declaration line is matched exactly
	// so a future signature change fails this transform loudly.
	dynamicClassDecl := dynamicClassDecodeDecl(family)
	idx := strings.Index(body, dynamicClassDecl)
	if idx < 0 {
		return fmt.Errorf("could not find DynamicClass.Decode signature %q in %s", dynamicClassDecl, path)
	}
	endIdx, err := findMatchingBraceEnd(body, idx+len(dynamicClassDecl)-1)
	if err != nil {
		return fmt.Errorf("locating DynamicClass.Decode body end: %w", err)
	}
	patchedBody := dynamicClassDecodePatched(family)
	body = body[:idx] + patchedBody + body[endIdx+1:]

	// 3. Replace the DynamicUnion.Decode body. Only the value-decode
	// line differs across families; using a body rewrite keeps the
	// three branches symmetric with DynamicClass.
	unionDecl := dynamicUnionDecodeDecl(family)
	uIdx := strings.Index(body, unionDecl)
	if uIdx < 0 {
		return fmt.Errorf("could not find DynamicUnion.Decode signature %q in %s", unionDecl, path)
	}
	uEnd, err := findMatchingBraceEnd(body, uIdx+len(unionDecl)-1)
	if err != nil {
		return fmt.Errorf("locating DynamicUnion.Decode body end: %w", err)
	}
	body = body[:uIdx] + dynamicUnionDecodePatched(family) + body[uEnd+1:]

	// 4. Patch decodeUnionValue's unknown-union branch so the nested
	// CFFI value flows through DecodeToOrderedValue. Without this,
	// dynamic unions whose Value is a CFFI map/class decode through
	// the standard Decode pipeline and lose key order before the
	// generated client wraps the result.
	patched, err = patchDecodeUnionValueDynamicBranch(body, family)
	if err != nil {
		return fmt.Errorf("patching decodeUnionValue dynamic branch in %s: %w", path, err)
	}
	body = patched

	// 5. Patch decodeUnionValue's optional/single-pattern branch so the
	// inner CFFI value flows through DecodeToOrderedValue. BAML uses this
	// branch for `T | null` shapes, so a non-null `optional(map<...>)`
	// dynamic field would otherwise reach decodeMapValue and become a
	// plain Go map before UnwrapDynamicValue could preserve key order.
	patched, err = patchDecodeUnionValueIsSinglePatternBranch(body, family)
	if err != nil {
		return fmt.Errorf("patching decodeUnionValue IsSinglePattern branch in %s: %w", path, err)
	}
	body = patched

	// 6. Patch decodeMapValue itself so every CFFI map node is decoded
	// into an OrderedFields carrier in valueMap.Entries order. The
	// per-family bodies preserve the upstream signatures (familyA's
	// single-return shape, familyB/C's two-return shape) and familyC's
	// INTERNAL.nil scalar-preserving branch; only the storage target
	// changes from a native Go map to OrderedFields. Generated client
	// conversion helpers (added in D8-D11) then rebuild typed
	// baml.OrderedMap[T] from the carrier without losing CFFI key order.
	patched, err = patchDecodeMapValue(body, family)
	if err != nil {
		return fmt.Errorf("patching decodeMapValue in %s: %w", path, err)
	}
	body = patched

	// 7. Patch convertFieldTypeToGoType's map arm so it advertises the
	// ordered carrier type for map nodes. Optional/checked/stream-state
	// wrappers consult convertFieldTypeToGoType to allocate their
	// pointer/holder targets; without this rewrite the wrapper allocates
	// a `*map[K]V` and panics when the inner decode hands back the
	// OrderedFields carrier.
	patched, err = patchConvertFieldTypeToGoTypeMapArm(body, family)
	if err != nil {
		return fmt.Errorf("patching convertFieldTypeToGoType map arm in %s: %w", path, err)
	}
	body = patched

	// 8. Append the family-specific DecodeToOrderedValue helper, the
	// isOrderableSinglePattern kind-dispatch helper, and the
	// isOrderableMapValueType ValueType-discriminator helper. Each
	// append is idempotent: skip when the function name is already
	// present so a re-run over a cached patched tree does not stack
	// duplicate declarations at end-of-file.
	if !strings.Contains(body, "func DecodeToOrderedValue(") {
		body = strings.TrimRight(body, "\n") + "\n\n" + decodeToOrderedValueFunc(family) + "\n"
	}
	if !strings.Contains(body, "func isOrderableSinglePattern(") {
		body = strings.TrimRight(body, "\n") + "\n\n" + isOrderableSinglePatternFunc(family) + "\n"
	}
	if !strings.Contains(body, "func isOrderableMapValueType(") {
		body = strings.TrimRight(body, "\n") + "\n\n" + isOrderableMapValueTypeFunc(family) + "\n"
	}

	return os.WriteFile(path, []byte(body), 0o644)
}

// replaceFirstFieldDecl rewrites the first `Fields<ws>map[string]any`
// pattern in body to `Fields OrderedFields`, where <ws> is one or more
// space/tab characters. gofmt's struct-field alignment uses tabs after
// the patched module is import-rewritten and printed, while the
// upstream module-cache source uses a single space; the helper accepts
// both shapes so the hack runs cleanly against either layout.
func replaceFirstFieldDecl(body string) (string, bool) {
	const ident = "Fields"
	const typ = "map[string]any"
	idx := 0
	for {
		j := strings.Index(body[idx:], ident)
		if j < 0 {
			return body, false
		}
		start := idx + j
		// Ensure the match is at the start of a struct field
		// (preceded by whitespace, newline, or '{').
		if start > 0 {
			prev := body[start-1]
			if prev != '\n' && prev != '\t' && prev != ' ' && prev != '{' {
				idx = start + len(ident)
				continue
			}
		}
		k := start + len(ident)
		// Skip whitespace.
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if !strings.HasPrefix(body[k:], typ) {
			idx = start + len(ident)
			continue
		}
		end := k + len(typ)
		return body[:start] + "Fields OrderedFields" + body[end:], true
	}
}

// findMatchingBraceEnd returns the index of the closing '}' that
// matches the '{' at openIdx, scanning forward and respecting string
// and comment literals at a coarse level. The serde sources we patch
// are gofmt-clean and do not contain '{' inside line/block comments
// inside the function bodies we target, so this lightweight scanner
// is sufficient and avoids reparsing the file just to locate a
// closing brace.
func findMatchingBraceEnd(src string, openIdx int) (int, error) {
	if openIdx >= len(src) || src[openIdx] != '{' {
		return -1, fmt.Errorf("findMatchingBraceEnd: char at %d is %q, not '{'", openIdx, src[openIdx])
	}
	depth := 0
	i := openIdx
	for i < len(src) {
		c := src[i]
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		case '"':
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '"' {
					break
				}
				i++
			}
		case '`':
			i++
			for i < len(src) && src[i] != '`' {
				i++
			}
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i++
			}
		}
		i++
	}
	return -1, fmt.Errorf("findMatchingBraceEnd: unbalanced braces starting at %d", openIdx)
}

// dynamicClassDecodeDecl returns the exact signature line the
// detector locates DynamicClass.Decode by. All three families share
// the same signature; the family parameter is reserved for future
// drift.
func dynamicClassDecodeDecl(_ decodeFamily) string {
	return "func (d *DynamicClass) Decode(holder *cffi.CFFIValueClass, typeMap TypeMap) {"
}

func dynamicUnionDecodeDecl(_ decodeFamily) string {
	return "func (d *DynamicUnion) Decode(holder *cffi.CFFIValueUnionVariant, typeMap TypeMap) {"
}

// dynamicClassDecodePatched emits the canonical post-patch body. The
// initialisation switches to NewOrderedFields, each field value flows
// through the family-specific DecodeToOrderedValue helper, and writes
// land via OrderedFields.Set.
func dynamicClassDecodePatched(family decodeFamily) string {
	return `func (d *DynamicClass) Decode(holder *cffi.CFFIValueClass, typeMap TypeMap) {
	typeName := holder.Name
	if typeName == nil {
		panic(fmt.Sprintf("DynamicClass.Decode: typeName is nil, holder=%+v", holder))
	}
	d.Name = string(typeName.Name)
	fieldCount := len(holder.Fields)
	d.Fields = NewOrderedFields(fieldCount)
	for i := 0; i < fieldCount; i++ {
		field := holder.Fields[i]
		if field == nil {
			panic(fmt.Sprintf("DynamicClass.Decode: field[%d] is nil, holder.Fields=%+v", i, holder.Fields))
		}
		key := field.Key
		valueHolder := field.Value
		_ = d.Fields.Set(key, DecodeToOrderedValue(valueHolder, typeMap))
	}
}`
}

// dynamicUnionDecodePatched emits the canonical post-patch body. The
// variant name accessor differs across families (VariantName in
// v0.204, ValueOptionName in v0.215+).
func dynamicUnionDecodePatched(family decodeFamily) string {
	variantField := "ValueOptionName"
	if family == familyA {
		variantField = "VariantName"
	}
	return fmt.Sprintf(`func (d *DynamicUnion) Decode(holder *cffi.CFFIValueUnionVariant, typeMap TypeMap) {
	d.Variant = string(holder.%s)
	d.Value = DecodeToOrderedValue(holder.Value, typeMap)
}`, variantField)
}

// patchDecodeUnionValueDynamicBranch rewrites the unknown-union (fully
// dynamic) branch in decodeUnionValue so the nested CFFI value is
// decoded through DecodeToOrderedValue (the plain Decode path would
// flatten a CFFI map or class holder into an unordered Go map). The
// generated client then preserves insertion order — the scope D2
// requirement that union nested values use the ordered decoder.
//
// The branch is located by per-family code-token anchors: a unique
// start-of-span line, a discriminator that must appear between the
// anchors, and an end-of-span line. Comment-text matching is avoided
// so upstream comment rewording, blank-line drift, and indentation
// drift within a known family all stay tolerated; only the matched
// code span is replaced. Unrecognised shapes still fail closed: a
// missing anchor, an ambiguous start, or a missing discriminator each
// returns an error naming the family.
func patchDecodeUnionValueDynamicBranch(body string, family decodeFamily) (string, error) {
	spec, ok := decodeUnionValueBranchSpec(family)
	if !ok {
		return body, fmt.Errorf("no decodeUnionValue dynamic-branch spec for family %s", family)
	}

	startMatches := spec.startRe.FindAllStringSubmatchIndex(body, -1)
	if len(startMatches) == 0 {
		// Idempotency: a cached patched-module tree already carries the
		// after-image. The struct-literal marker only appears in this
		// branch's after-image, never in the IsSinglePattern branch, so
		// it pins "this branch was patched" without false-positives.
		if strings.Contains(body, "Value:   DecodeToOrderedValue(valueUnion.Value, typeMap),") {
			return body, nil
		}
		return body, fmt.Errorf("could not locate decodeUnionValue dynamic-branch start anchor for family %s", family)
	}
	if len(startMatches) > 1 {
		return body, fmt.Errorf("decodeUnionValue dynamic-branch start anchor matched %d times for family %s; expected exactly 1", len(startMatches), family)
	}
	start := startMatches[0]
	indent := body[start[2]:start[3]]

	endLoc := spec.endRe.FindStringIndex(body[start[1]:])
	if endLoc == nil {
		return body, fmt.Errorf("could not locate decodeUnionValue dynamic-branch end anchor for family %s after start anchor", family)
	}
	spanEnd := start[1] + endLoc[1]

	middle := body[start[0]:spanEnd]
	if !strings.Contains(middle, spec.discriminator) {
		return body, fmt.Errorf("decodeUnionValue dynamic-branch discriminator %q missing between anchors for family %s", spec.discriminator, family)
	}

	afterImage := strings.ReplaceAll(spec.afterTemplate, "{I}", indent)
	return body[:start[0]] + afterImage + body[spanEnd:], nil
}

// decodeUnionBranchSpec describes how to locate and rewrite the
// unknown-union code span for a single BAML serde family. startRe must
// match exactly once in the file and capture the leading whitespace as
// group 1. endRe is searched only after the start match and locates the
// last line of the span (consumes its trailing newline). discriminator
// is a substring expected to occur between the anchors; its absence
// flags a shape the spec was not designed for. afterTemplate is the
// replacement, with "{I}" placeholders for the leading indentation
// captured from the start match.
type decodeUnionBranchSpec struct {
	startRe       *regexp.Regexp
	endRe         *regexp.Regexp
	discriminator string
	// discriminatorAlts holds additional acceptable substrings that
	// also pin the spec to its branch. Used by the IsSinglePattern
	// patcher so a cached patched-module tree carrying a stale
	// post-patch shape (commit-era 6f8464e50: unconditional ordered
	// routing) can be recognised and rewritten in-place. Empty for
	// the dynamic-branch patcher where the discriminator alone is
	// authoritative.
	discriminatorAlts []string
	afterTemplate     string
}

func decodeUnionValueBranchSpec(family decodeFamily) (decodeUnionBranchSpec, bool) {
	spec, ok := decodeUnionBranchSpecs[family]
	return spec, ok
}

// decodeUnionBranchSpecs holds the per-family token anchors and
// after-image templates. The startRe regexes anchor on the first
// distinctive line of the dynamic-union branch:
//
//   - familyA: `dynamicUnion := DynamicUnion{` — the discriminator
//     `Decode(valueUnion.Value, typeMap).Interface()` pins this to the
//     v0.204 shape so a hypothetical reuse elsewhere cannot match.
//   - familyB: `value, _ := Decode(valueUnion.Value, typeMap)` —
//     paired with the `value.Elem()` discriminator.
//   - familyC: `value, goType := Decode(valueUnion.Value, typeMap)` —
//     paired with the scalar `switch goType` discriminator.
//
// endRe locates the trailing line of the span (the family's final
// `return`), consuming its newline so the after-image substitution
// preserves the surrounding line layout.
var decodeUnionBranchSpecs = map[decodeFamily]decodeUnionBranchSpec{
	familyA: {
		startRe:       regexp.MustCompile(`(?m)^([ \t]+)dynamicUnion := DynamicUnion\{[ \t]*$`),
		endRe:         regexp.MustCompile(`(?m)^[ \t]+return reflect\.ValueOf\(dynamicUnion\)[ \t]*\n`),
		discriminator: "Decode(valueUnion.Value, typeMap).Interface()",
		afterTemplate: "{I}dynamicUnion := DynamicUnion{\n" +
			"{I}\tVariant: unionName,\n" +
			"{I}\tValue:   DecodeToOrderedValue(valueUnion.Value, typeMap),\n" +
			"{I}}\n" +
			"{I}return reflect.ValueOf(dynamicUnion)\n",
	},
	familyB: {
		startRe:       regexp.MustCompile(`(?m)^([ \t]+)value, _ := Decode\(valueUnion\.Value, typeMap\)[ \t]*$`),
		endRe:         regexp.MustCompile(`(?m)^[ \t]+return value, goType[ \t]*\n`),
		discriminator: "value.Elem()",
		afterTemplate: "{I}dynamicUnion := DynamicUnion{\n" +
			"{I}\tVariant: valueUnion.Name.Name,\n" +
			"{I}\tValue:   DecodeToOrderedValue(valueUnion.Value, typeMap),\n" +
			"{I}}\n" +
			"{I}value := reflect.ValueOf(dynamicUnion)\n" +
			"{I}goType = reflect.TypeOf(DynamicUnion{})\n" +
			"{I}return value, goType\n",
	},
	familyC: {
		startRe:       regexp.MustCompile(`(?m)^([ \t]+)value, goType := Decode\(valueUnion\.Value, typeMap\)[ \t]*$`),
		endRe:         regexp.MustCompile(`(?m)^[ \t]+return value, goType[ \t]*\n`),
		discriminator: "switch goType",
		afterTemplate: "{I}dynamicUnion := DynamicUnion{\n" +
			"{I}\tVariant: valueUnion.Name.Name,\n" +
			"{I}\tValue:   DecodeToOrderedValue(valueUnion.Value, typeMap),\n" +
			"{I}}\n" +
			"{I}value := reflect.ValueOf(dynamicUnion)\n" +
			"{I}goType = reflect.TypeOf(DynamicUnion{})\n" +
			"{I}return value, goType\n",
	},
}

// patchDecodeUnionValueIsSinglePatternBranch rewrites the optional /
// single-pattern (`T | null`) branch of decodeUnionValue so the inner
// CFFI value is decoded through DecodeToOrderedValue. BAML drives this
// branch for `optional(T)` dynamic fields; with the upstream code the
// inner value reached decodeMapValue and became a plain Go map, so
// UnwrapDynamicValue lost its chance to preserve CFFI key order.
//
// Family A's optional-pattern branch is structurally distinct from
// family B/C's `IsSinglePattern` branch (it is gated on a locally
// computed `isOptionalPattern` boolean and returns a single
// reflect.Value), so the per-family specs differ in start anchor,
// return arity, and after-image. Fail-closed semantics mirror the
// dynamic-branch patch: a missing anchor, an ambiguous anchor, or a
// missing discriminator each surfaces a family-named error.
func patchDecodeUnionValueIsSinglePatternBranch(body string, family decodeFamily) (string, error) {
	spec, ok := decodeUnionIsSinglePatternBranchSpecs[family]
	if !ok {
		return body, fmt.Errorf("no decodeUnionValue IsSinglePattern-branch spec for family %s", family)
	}

	// Idempotency: the dispatch line `if isOrderableSinglePattern(...)`
	// is unique to this branch's after-image. When it is present the
	// branch has already been rewritten and the regex match below
	// would otherwise re-replace the post-patch span with itself,
	// which is safe but wastes work and obscures unexpected drift.
	if strings.Contains(body, "if isOrderableSinglePattern(valueUnion.Value, typeMap)") {
		return body, nil
	}

	startMatches := spec.startRe.FindAllStringSubmatchIndex(body, -1)
	if len(startMatches) == 0 {
		return body, fmt.Errorf("could not locate decodeUnionValue IsSinglePattern-branch start anchor for family %s", family)
	}
	if len(startMatches) > 1 {
		return body, fmt.Errorf("decodeUnionValue IsSinglePattern-branch start anchor matched %d times for family %s; expected exactly 1", len(startMatches), family)
	}
	start := startMatches[0]
	indent := body[start[2]:start[3]]

	endLoc := spec.endRe.FindStringIndex(body[start[1]:])
	if endLoc == nil {
		return body, fmt.Errorf("could not locate decodeUnionValue IsSinglePattern-branch end anchor for family %s after start anchor", family)
	}
	spanEnd := start[1] + endLoc[1]

	middle := body[start[0]:spanEnd]
	if !discriminatorMatches(middle, spec) {
		return body, fmt.Errorf("decodeUnionValue IsSinglePattern-branch discriminator %q missing between anchors for family %s", spec.discriminator, family)
	}

	afterImage := strings.ReplaceAll(spec.afterTemplate, "{I}", indent)
	return body[:start[0]] + afterImage + body[spanEnd:], nil
}

// discriminatorMatches reports whether the candidate span carries the
// spec's primary discriminator or any of its alternates. The
// alternates list is empty for branch specs whose pre-patch shape is
// the only acceptable form; the IsSinglePattern spec lists the stale
// post-patch action line so a re-run over a cached patched-module
// tree can recognise the prior broken patch as a recoverable state.
func discriminatorMatches(span string, spec decodeUnionBranchSpec) bool {
	if strings.Contains(span, spec.discriminator) {
		return true
	}
	for _, alt := range spec.discriminatorAlts {
		if strings.Contains(span, alt) {
			return true
		}
	}
	return false
}

// decodeUnionIsSinglePatternBranchSpecs holds per-family token anchors
// and after-image templates for the optional-pattern branch. The
// startRe regexes anchor on the first distinctive line of the branch:
//
//   - familyA: `if isOptionalPattern {` — the discriminator
//     `value := valueUnion.Value` pins this as the v0.204 shape.
//   - familyB / familyC: `} else if valueUnion.IsSinglePattern {` —
//     paired with the `Decode(valueUnion.Value, typeMap)` discriminator
//     so the same regex would not silently match an unrelated
//     `IsSinglePattern` reference elsewhere.
//
// The after-image keeps the branch's opening line intact and rewrites
// only its body to dispatch on the inner CFFI holder kind. Lists,
// scalars, unions, and statically-typed maps stay on the plain Decode
// pipeline so the generated client receives the concrete element /
// value types its static fields assert against (e.g. `*[]string` for
// an `optional(list<string>)` field). Dynamic class values and
// dynamic-value maps route through DecodeToOrderedValue so OrderedFields
// propagates up. The kind discriminator lives in the appended helper
// `isOrderableSinglePattern` so the branch body stays narrow.
var decodeUnionIsSinglePatternBranchSpecs = map[decodeFamily]decodeUnionBranchSpec{
	familyA: {
		startRe: regexp.MustCompile(`(?m)^([ \t]+)if isOptionalPattern \{[ \t]*$`),
		// Accept either the pre-patch end line or the stale post-patch
		// end line so a cached patched tree from a prior hack revision
		// is also rewritten in place.
		endRe:             regexp.MustCompile(`(?m)^[ \t]+return (?:Decode\(value, typeMap\)|reflect\.ValueOf\(decoded\))[ \t]*\n`),
		discriminator:     "value := valueUnion.Value",
		discriminatorAlts: []string{"decoded := DecodeToOrderedValue(valueUnion.Value, typeMap)"},
		afterTemplate: "{I}if isOptionalPattern {\n" +
			"{I}\tif isOrderableSinglePattern(valueUnion.Value, typeMap) {\n" +
			"{I}\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n" +
			"{I}\t\tif decoded == nil {\n" +
			"{I}\t\t\treturn reflect.ValueOf(nil)\n" +
			"{I}\t\t}\n" +
			"{I}\t\treturn reflect.ValueOf(decoded)\n" +
			"{I}\t}\n" +
			"{I}\treturn Decode(valueUnion.Value, typeMap)\n",
	},
	familyB: {
		startRe: regexp.MustCompile(`(?m)^([ \t]+)\} else if valueUnion\.IsSinglePattern \{[ \t]*$`),
		// Accept either the pre-patch end line or the stale post-patch
		// end line so a cached patched tree from a prior hack revision
		// is also rewritten in place.
		endRe:             regexp.MustCompile(`(?m)^[ \t]+return (?:Decode\(valueUnion\.Value, typeMap\)|rv, rv\.Type\(\))[ \t]*\n`),
		discriminator:     "Decode(valueUnion.Value, typeMap)",
		discriminatorAlts: []string{"decoded := DecodeToOrderedValue(valueUnion.Value, typeMap)"},
		afterTemplate: "{I}} else if valueUnion.IsSinglePattern {\n" +
			"{I}\tif isOrderableSinglePattern(valueUnion.Value, typeMap) {\n" +
			"{I}\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n" +
			"{I}\t\tif decoded == nil {\n" +
			"{I}\t\t\treturn reflect.ValueOf(nil), nil\n" +
			"{I}\t\t}\n" +
			"{I}\t\trv := reflect.ValueOf(decoded)\n" +
			"{I}\t\treturn rv, rv.Type()\n" +
			"{I}\t}\n" +
			"{I}\treturn Decode(valueUnion.Value, typeMap)\n",
	},
	familyC: {
		startRe: regexp.MustCompile(`(?m)^([ \t]+)\} else if valueUnion\.IsSinglePattern \{[ \t]*$`),
		// Accept either the pre-patch end line or the stale post-patch
		// end line so a cached patched tree from a prior hack revision
		// is also rewritten in place.
		endRe:             regexp.MustCompile(`(?m)^[ \t]+return (?:Decode\(valueUnion\.Value, typeMap\)|rv, rv\.Type\(\))[ \t]*\n`),
		discriminator:     "Decode(valueUnion.Value, typeMap)",
		discriminatorAlts: []string{"decoded := DecodeToOrderedValue(valueUnion.Value, typeMap)"},
		afterTemplate: "{I}} else if valueUnion.IsSinglePattern {\n" +
			"{I}\tif isOrderableSinglePattern(valueUnion.Value, typeMap) {\n" +
			"{I}\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n" +
			"{I}\t\tif decoded == nil {\n" +
			"{I}\t\t\treturn reflect.ValueOf(nil), nil\n" +
			"{I}\t\t}\n" +
			"{I}\t\trv := reflect.ValueOf(decoded)\n" +
			"{I}\t\treturn rv, rv.Type()\n" +
			"{I}\t}\n" +
			"{I}\treturn Decode(valueUnion.Value, typeMap)\n",
	},
}

// decodeToOrderedValueFunc emits the version-family-specific
// DecodeToOrderedValue helper. The helper walks the CFFI holder tree
// producing OrderedFields for class and map nodes while delegating
// arrays and scalars to the existing per-family decode paths so
// scalar preservation semantics stay in sync with Decode.
func decodeToOrderedValueFunc(family decodeFamily) string {
	switch family {
	case familyA:
		return `// DecodeToOrderedValue walks the CFFI holder tree producing
// insertion-ordered OrderedFields for class and map nodes while
// delegating arrays and scalars to the existing Decode pipeline. It
// is the entry point generated @@dynamic clients call so dynamic
// outputs preserve LLM/CFFI key order before reaching baml-rest.
func DecodeToOrderedValue(holder *cffi.CFFIValueHolder, typeMap TypeMap) any {
	if holder == nil {
		return nil
	}
	switch v := holder.Value.(type) {
	case *cffi.CFFIValueHolder_ClassValue:
		decoded := decodeClassValue(v.ClassValue, typeMap)
		return decoded.Interface()
	case *cffi.CFFIValueHolder_MapValue:
		if v.MapValue == nil {
			return NewOrderedFields(0)
		}
		out := NewOrderedFields(len(v.MapValue.Entries))
		for _, entry := range v.MapValue.Entries {
			_ = out.Set(entry.Key, DecodeToOrderedValue(entry.Value, typeMap))
		}
		return out
	case *cffi.CFFIValueHolder_ListValue:
		if v.ListValue == nil {
			return []any{}
		}
		items := make([]any, 0, len(v.ListValue.Values))
		for _, item := range v.ListValue.Values {
			items = append(items, DecodeToOrderedValue(item, typeMap))
		}
		return items
	case *cffi.CFFIValueHolder_UnionVariantValue:
		if v.UnionVariantValue == nil {
			return nil
		}
		decoded := decodeUnionValue(v.UnionVariantValue, typeMap)
		if !decoded.IsValid() {
			return nil
		}
		return decoded.Interface()
	default:
		decoded := Decode(holder, typeMap)
		if !decoded.IsValid() {
			return nil
		}
		return decoded.Interface()
	}
}`
	case familyB:
		return `// DecodeToOrderedValue walks the CFFI holder tree producing
// insertion-ordered OrderedFields for class and map nodes while
// delegating arrays and scalars to the existing Decode pipeline. It
// is the entry point generated @@dynamic clients call so dynamic
// outputs preserve LLM/CFFI key order before reaching baml-rest.
func DecodeToOrderedValue(holder *cffi.CFFIValueHolder, typeMap TypeMap) any {
	if holder == nil {
		return nil
	}
	switch v := holder.Value.(type) {
	case *cffi.CFFIValueHolder_ClassValue:
		decoded, _ := decodeClassValue(v.ClassValue, typeMap)
		return decoded.Interface()
	case *cffi.CFFIValueHolder_MapValue:
		if v.MapValue == nil {
			return NewOrderedFields(0)
		}
		out := NewOrderedFields(len(v.MapValue.Entries))
		for _, entry := range v.MapValue.Entries {
			_ = out.Set(entry.Key, DecodeToOrderedValue(entry.Value, typeMap))
		}
		return out
	case *cffi.CFFIValueHolder_ListValue:
		if v.ListValue == nil {
			return []any{}
		}
		items := make([]any, 0, len(v.ListValue.Items))
		for _, item := range v.ListValue.Items {
			items = append(items, DecodeToOrderedValue(item, typeMap))
		}
		return items
	case *cffi.CFFIValueHolder_UnionVariantValue:
		if v.UnionVariantValue == nil {
			return nil
		}
		decoded, _ := decodeUnionValue(v.UnionVariantValue, typeMap)
		if !decoded.IsValid() {
			return nil
		}
		return decoded.Interface()
	default:
		decoded, _ := Decode(holder, typeMap)
		if !decoded.IsValid() {
			return nil
		}
		return decoded.Interface()
	}
}`
	case familyC:
		return `// DecodeToOrderedValue walks the CFFI holder tree producing
// insertion-ordered OrderedFields for class and map nodes while
// delegating arrays and scalars to the existing Decode pipeline. It
// is the entry point generated @@dynamic clients call so dynamic
// outputs preserve LLM/CFFI key order before reaching baml-rest.
func DecodeToOrderedValue(holder *cffi.CFFIValueHolder, typeMap TypeMap) any {
	if holder == nil {
		return nil
	}
	switch v := holder.Value.(type) {
	case *cffi.CFFIValueHolder_ClassValue:
		decoded, _ := decodeClassValue(v.ClassValue, typeMap)
		return decoded.Interface()
	case *cffi.CFFIValueHolder_MapValue:
		if v.MapValue == nil {
			return NewOrderedFields(0)
		}
		out := NewOrderedFields(len(v.MapValue.Entries))
		for _, entry := range v.MapValue.Entries {
			_ = out.Set(entry.Key, DecodeToOrderedValue(entry.Value, typeMap))
		}
		return out
	case *cffi.CFFIValueHolder_ListValue:
		if v.ListValue == nil {
			return []any{}
		}
		items := make([]any, 0, len(v.ListValue.Items))
		for _, item := range v.ListValue.Items {
			items = append(items, DecodeToOrderedValue(item, typeMap))
		}
		return items
	case *cffi.CFFIValueHolder_UnionVariantValue:
		if v.UnionVariantValue == nil {
			return nil
		}
		decoded, _ := decodeUnionValue(v.UnionVariantValue, typeMap)
		if !decoded.IsValid() {
			return nil
		}
		return decoded.Interface()
	default:
		decoded, goType := Decode(holder, typeMap)
		if !decoded.IsValid() {
			return nil
		}
		switch goType {
		case reflect.TypeOf(int64(0)):
			return decoded.Int()
		case reflect.TypeOf(float64(0)):
			return decoded.Float()
		case reflect.TypeOf(false):
			return decoded.Bool()
		default:
			return decoded.Interface()
		}
	}
}`
	default:
		return ""
	}
}

// isOrderableSinglePatternFunc emits the kind-discriminator helper the
// IsSinglePattern after-image dispatches against. The helper returns
// true only for shapes where plain Decode would lose key order: a CFFI
// class value (whose decodeClassValue path produces DynamicClass with
// OrderedFields for the dynamic case and a typed struct for the
// static case; either way the resulting reflect.Value is shape-
// compatible with the static field) and a CFFI map value whose
// declared ValueType is structurally dynamic. Lists, scalars, enums,
// nested unions, and statically-typed maps return false so plain
// Decode hands back a concretely typed reflect.Value the optional
// wrapper can Set into `reflect.New(pointerToConcrete)`.
//
// The map-value decision is delegated to isOrderableMapValueType so
// the kind-switch here is body-stable across families; the per-family
// difference lives in that helper alone (different TypeMap surface
// and different per-variant inner field names on the *type-descriptor*
// side of CFFI).
//
// On the *value* side (`*cffi.CFFIValueMap`, what `v.MapValue` is)
// the declared value type is named `ValueType` consistently across
// all observed families — the proto field rename was on the
// `CFFIFieldTypeMap` (the *type* descriptor), not on `CFFIValueMap`.
// The body therefore reads the same `v.MapValue.ValueType` accessor
// regardless of family.
func isOrderableSinglePatternFunc(family decodeFamily) string {
	body := `// isOrderableSinglePattern reports whether decodeUnionValue's
// optional / single-pattern branch should route the inner CFFI
// holder through DecodeToOrderedValue. The branch is shared by
// static fields like ` + "`optional(list<string>)`" + ` that need a
// concretely typed reflect.Value out of Decode and dynamic
// fields like ` + "`optional(map<string, dynamic>)`" + ` that need
// OrderedFields to preserve CFFI key order. Returning true here
// commits the caller to the ordered pipeline; returning false
// keeps the value on the plain Decode pipeline. Lists, scalars,
// enums, nested unions, and statically-typed maps all return
// false so the IsOptional wrapper can ` + "`Set(value)`" + ` into a
// ` + "`reflect.New(concreteGoType)`" + ` without a type-assertion
// mismatch.
func isOrderableSinglePattern(holder *cffi.CFFIValueHolder, typeMap TypeMap) bool {
	if holder == nil {
		return false
	}
	switch v := holder.Value.(type) {
	case *cffi.CFFIValueHolder_ClassValue:
		return v.ClassValue != nil
	case *cffi.CFFIValueHolder_MapValue:
		if v.MapValue == nil {
			return false
		}
		return isOrderableMapValueType(v.MapValue.ValueType, typeMap)
	default:
		return false
	}
}`
	// Family-stable body; per-family differences are absorbed entirely
	// by isOrderableMapValueType. Reject unknown families explicitly
	// so a future enum value surfaces as a loud no-op (empty string —
	// the caller's `strings.Contains` postcondition then fails).
	switch family {
	case familyA, familyB, familyC:
		return body
	default:
		return ""
	}
}

// isOrderableMapValueTypeFunc emits the per-family ValueType
// discriminator the _MapValue arm of isOrderableSinglePattern delegates
// to. The helper inspects the declared CFFI value type of a map and
// returns true only when the type is structurally dynamic (would
// otherwise produce a plain Go map under the standard Decode pipeline
// and lose CFFI key order):
//
//   - AnyType / NullType resolve to the unconstrained `interface{}`
//     value type at the runtime layer (NullType under serde's
//     `INTERNAL.nil` interpretation), so the map's value column is
//     untyped — route ordered.
//   - UnionVariantType is dynamic only when the variant name is unset
//     OR the variant name is absent from the generated TypeMap; a
//     concrete user union whose name is registered must stay on plain
//     Decode so the typed struct comes through unchanged.
//   - OptionalType / CheckedType / StreamStateType wrap an inner
//     value type; recurse so an `optional(any)` or
//     `streamstate(map<string, any>)` value column still routes
//     through the ordered pipeline.
//   - Concrete scalar / class / enum / list / map / type-alias
//     variants are static — return false so plain Decode produces a
//     concretely typed reflect.Value.
//
// familyA carries an additional TupleType variant; tuples are
// concrete in BAML's value-type sense so they return false as well.
// The trailing `default: return false` arm fails closed on any
// hypothetical future variant: a BAML release that introduces a new
// shape lands on plain Decode (the safe direction — the ordered
// pipeline would otherwise silently swallow shapes whose decode
// semantics have not been audited).
//
// Per-family differences captured here:
//
//   - TypeMap surface for the UnionVariantType lookup. v0.204 has
//     no GetType method on TypeMap; the existing
//     `convertFieldTypeToGoType` resolves union names via
//     ` + "`typeMap[namespace.Enum().String() + \".\" + name]`" + ` —
//     mirror that form. v0.215 has a GetType method but
//     `convertFieldTypeToGoType` still uses the direct-index form —
//     mirror it for consistency. v0.219+ wraps TypeMap in a struct
//     with `.GetType(name)` as the only stable accessor.
//   - familyA additionally enumerates the `TupleType` variant in
//     the switch so the case appears explicitly under the static
//     verdict; familyB / familyC let the `default` arm cover the
//     same outcome because the variant does not exist in their
//     proto descriptors.
func isOrderableMapValueTypeFunc(family decodeFamily) string {
	header := `// isOrderableMapValueType reports whether a CFFI value-type
// holder describes a structurally dynamic value. Only structurally
// dynamic types route the surrounding _MapValue through
// DecodeToOrderedValue; concrete scalar, class, enum, list, map,
// and type-alias variants stay on the plain Decode pipeline so a
// statically-typed map<K, V> field still decodes to map[K]V.
func isOrderableMapValueType(vt *cffi.CFFIFieldTypeHolder, typeMap TypeMap) bool {
	if vt == nil {
		return false
	}
	switch t := vt.Type.(type) {
	case *cffi.CFFIFieldTypeHolder_AnyType:
		return true
	case *cffi.CFFIFieldTypeHolder_NullType:
		return true
	case *cffi.CFFIFieldTypeHolder_UnionVariantType:
		if t.UnionVariantType == nil || t.UnionVariantType.Name == nil {
			return true
		}
%s
	case *cffi.CFFIFieldTypeHolder_OptionalType:
		if t.OptionalType == nil {
			return false
		}
		return isOrderableMapValueType(t.OptionalType.Value, typeMap)
	case *cffi.CFFIFieldTypeHolder_CheckedType:
		if t.CheckedType == nil {
			return false
		}
		return isOrderableMapValueType(t.CheckedType.Value, typeMap)
	case *cffi.CFFIFieldTypeHolder_StreamStateType:
		if t.StreamStateType == nil {
			return false
		}
		return isOrderableMapValueType(t.StreamStateType.Value, typeMap)
%s
	default:
		return false
	}
}`

	// Union-variant lookup body (per-family). A concrete user union
	// returns false (the static path); an unknown variant returns true
	// (the dynamic path mirrors what convertFieldTypeToGoType would
	// produce — a DynamicUnion wrapper).
	var unionLookup string
	switch family {
	case familyA:
		unionLookup = "\t\tname := t.UnionVariantType.Name\n" +
			"\t\tkey := name.Namespace.Enum().String() + \".\" + name.Name\n" +
			"\t\t_, ok := typeMap[key]\n" +
			"\t\treturn !ok"
	case familyB:
		unionLookup = "\t\tname := t.UnionVariantType.Name\n" +
			"\t\tkey := name.Namespace.Enum().String() + \".\" + name.Name\n" +
			"\t\t_, ok := typeMap[key]\n" +
			"\t\treturn !ok"
	case familyC:
		unionLookup = "\t\t_, ok := typeMap.GetType(t.UnionVariantType.Name)\n" +
			"\t\treturn !ok"
	default:
		return ""
	}

	// familyA-only TupleType case; rendered as an explicit
	// static-verdict arm so the verdict is reviewable in the helper
	// itself — the `default` fall-through covers the same outcome but
	// hides the verdict behind a catch-all. familyB / familyC omit
	// this arm (no TupleType variant exists in those proto
	// descriptors); the %s substitution would otherwise leave a stray
	// blank line, so render an explicit placeholder marker that the
	// caller strips after sprintf.
	tupleCase := "\t_ = struct{}{}"
	if family == familyA {
		tupleCase = "\tcase *cffi.CFFIFieldTypeHolder_TupleType:\n" +
			"\t\treturn false"
	}

	rendered := fmt.Sprintf(header, unionLookup, tupleCase)
	// Strip the placeholder line for non-familyA so the resulting
	// switch body has no stray dead statement. Targeted on a private
	// sentinel so unrelated `_ = struct{}{}` occurrences (none in the
	// templates above) are unaffected.
	rendered = strings.Replace(rendered, "\t_ = struct{}{}\n", "", 1)
	return rendered
}

// patchPkgLibGo augments the BAML pkg facade with three things the
// patched runtime exposes to generated clients: an alias for
// serde.OrderedFields, a NewOrderedFields constructor, a
// DecodeToOrderedValue entry point, and an EncodeClassOrdered helper
// that accepts the new field type while leaving the original
// EncodeClass(map[string]any) signature intact for unaffected
// callers.
func patchPkgLibGo(moduleDir string, family decodeFamily) error {
	path := pkgLibGoPath(moduleDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := string(raw)

	// Strict sentinel: only short-circuit when both the OrderedFields
	// alias AND the generic OrderedMap alias have landed. A cached
	// patched-module tree from an earlier hack revision (issue #365)
	// carries only OrderedFields; without the OrderedMap alias the
	// generated client's typed static-map fields fail to resolve. The
	// append step below is split into independent fragments, so a
	// re-pass over a partially patched tree converges without
	// duplicating the OrderedFields-era block.
	hasOrderedFields := strings.Contains(body, "type OrderedFields = serde.OrderedFields")
	hasOrderedMap := strings.Contains(body, "type OrderedMap[V any] = serde.OrderedMap[V]")
	if hasOrderedFields && hasOrderedMap {
		return nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, []byte(body), parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if file.Name == nil || file.Name.Name != "baml" {
		return fmt.Errorf("expected package baml in %s, got %s", path, file.Name.Name)
	}

	// Ensure reflect is imported (familyB lib.go in particular may not
	// already declare it). The append below uses reflect.TypeOf for the
	// scalar fast-path in DecodeToOrderedValue.
	EnsureImport(file, "reflect")

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		return fmt.Errorf("re-printing %s: %w", path, err)
	}
	body = buf.String()

	if !hasOrderedFields {
		body = strings.TrimRight(body, "\n") + "\n\n" + pkgFacadeAppend(family) + "\n"
	}
	if !hasOrderedMap {
		body = strings.TrimRight(body, "\n") + "\n\n" + pkgOrderedMapAppend() + "\n"
	}

	return os.WriteFile(path, []byte(body), 0o644)
}

// pkgOrderedMapAppend returns the family-stable block that exposes the
// generic OrderedMap[V] alias and its constructors. The block is
// independent of family because serde.OrderedMap is the same generic
// type across BAML versions; the family parameter only matters for
// EncodeClassOrdered and its scalar fast-path companions, both already
// covered by pkgFacadeAppend.
func pkgOrderedMapAppend() string {
	return `// OrderedMap is the public alias for the generic ordered map carrier
// used by statically-typed map<string, T> fields in the generated
// client. Generated code spells it ` + "`baml.OrderedMap[T]`" + `; the
// value type stays concrete while iteration order matches BAML's CFFI
// emission order.
type OrderedMap[V any] = serde.OrderedMap[V]

// OrderedKV constructs a single ordered-map entry. Provided so
// generated initialisers can build ` + "`baml.OrderedMap[T]`" + ` literals
// without importing serde directly.
func OrderedKV[V any](key string, value V) serde.OrderedEntry[V] {
	return serde.OrderedKV(key, value)
}

// NewOrderedMap builds a typed ordered map from entries in source
// order. Returns the wrapped serde error on duplicate keys.
func NewOrderedMap[V any](entries ...serde.OrderedEntry[V]) (OrderedMap[V], error) {
	return serde.NewOrderedMap(entries...)
}`
}

// pkgFacadeAppend returns the family-specific block of additions to
// the BAML pkg/lib.go. Each family's block is a self-contained string
// to keep the rewrite small and obvious; the cross-family commonality
// is the OrderedFields alias and the EncodeClassOrdered helper. The
// DecodeToOrderedValue facade selects the family-appropriate scalar
// fast-path so generated clients can call a single name regardless of
// BAML version.
func pkgFacadeAppend(family decodeFamily) string {
	switch family {
	case familyA:
		return `// OrderedFields is the public alias for the ordered field map
// the patched runtime uses inside DynamicClass and CFFI map values.
// Generated @@dynamic clients reference this through baml.OrderedFields
// so dynamic outputs surface insertion order to baml-rest.
type OrderedFields = serde.OrderedFields

// NewOrderedFields allocates an empty OrderedFields with the
// supplied capacity hint.
func NewOrderedFields(capacity int) OrderedFields {
	return serde.NewOrderedFields(capacity)
}

// DecodeToOrderedValue is the order-preserving counterpart to
// DecodeToValue. Generated @@dynamic clients call this for each
// LLM-added property so nested dynamic class and CFFI map values
// retain CFFI insertion order before baml-rest assembles the
// response.
func DecodeToOrderedValue(holder *cffi.CFFIValueHolder) any {
	return serde.DecodeToOrderedValue(holder, typeMap)
}

// EncodeClassOrdered mirrors EncodeClass but accepts the ordered
// field map generated clients now use for DynamicProperties. The
// underlying serde.EncodeClass call still takes a map[string]any, so
// the helper materialises a plain map by iterating in insertion
// order — encode semantics do not depend on map order, but the
// iteration shape keeps the helper trivially auditable.
//
// v0.204-family serde.EncodeClass takes a nameEncoder callback
// (func() *cffi.CFFITypeName), not a string; the wrapper passes the
// callback through unchanged so generated clients can keep emitting
// the BamlEncodeName method reference as the first argument.
func EncodeClassOrdered(nameEncoder func() *cffi.CFFITypeName, fields map[string]any, dynamicFields *OrderedFields) (*cffi.CFFIValueHolder, error) {
	if dynamicFields == nil {
		return serde.EncodeClass(nameEncoder, fields, nil)
	}
	flattened := make(map[string]any, dynamicFields.Len())
	dynamicFields.Range(func(k string, v any) bool {
		flattened[k] = v
		return true
	})
	return serde.EncodeClass(nameEncoder, fields, &flattened)
}
`
	default:
		return `// OrderedFields is the public alias for the ordered field map
// the patched runtime uses inside DynamicClass and CFFI map values.
// Generated @@dynamic clients reference this through baml.OrderedFields
// so dynamic outputs surface insertion order to baml-rest.
type OrderedFields = serde.OrderedFields

// NewOrderedFields allocates an empty OrderedFields with the
// supplied capacity hint.
func NewOrderedFields(capacity int) OrderedFields {
	return serde.NewOrderedFields(capacity)
}

// DecodeToOrderedValue is the order-preserving counterpart to
// DecodeToValue. Generated @@dynamic clients call this for each
// LLM-added property so nested dynamic class and CFFI map values
// retain CFFI insertion order before baml-rest assembles the
// response.
func DecodeToOrderedValue(holder *cffi.CFFIValueHolder) any {
	return serde.DecodeToOrderedValue(holder, typeMap)
}

// EncodeClassOrdered mirrors EncodeClass but accepts the ordered
// field map generated clients now use for DynamicProperties. The
// underlying serde.EncodeClass call still takes a map[string]any, so
// the helper materialises a plain map by iterating in insertion
// order — encode semantics do not depend on map order, but the
// iteration shape keeps the helper trivially auditable.
func EncodeClassOrdered(name string, fields map[string]any, dynamicFields *OrderedFields) (*cffi.HostValue, error) {
	if dynamicFields == nil {
		return serde.EncodeClass(name, fields, nil)
	}
	flattened := make(map[string]any, dynamicFields.Len())
	dynamicFields.Range(func(k string, v any) bool {
		flattened[k] = v
		return true
	})
	return serde.EncodeClass(name, fields, &flattened)
}
`
	}
}

// orderedMapDecodeMarker is the distinctive comment line embedded in
// every per-family decodeMapValue after-image. The decode.go sentinel
// in patchDecodeGo requires this marker so a re-run over a tree
// patched by the pre-issue-366 hack (which left decodeMapValue
// returning a native Go map) re-enters the patcher and lands the new
// ordered body. The marker survives gofmt unchanged and does not
// appear in any upstream BAML source, so a substring check pins
// "this file carries the ordered map decode after-image" without
// false positives.
const orderedMapDecodeMarker = "// ordered map decode: build OrderedFields in CFFI entry order"

// patchDecodeMapValue rewrites serde/decode.go::decodeMapValue per
// family so every map node decodes into an OrderedFields carrier in
// `valueMap.Entries` order. The transform is body-only: the upstream
// signature is preserved (familyA single-return; familyB/C two-return)
// so external call sites (Decode's _MapValue arm; rawobjects_*) keep
// their compile shape. Idempotent: a tree already carrying the marker
// returns unchanged so re-runs converge without double-applying.
func patchDecodeMapValue(body string, family decodeFamily) (string, error) {
	if strings.Contains(body, orderedMapDecodeMarker) {
		return body, nil
	}
	decl := "func decodeMapValue(valueMap *cffi.CFFIValueMap, typeMap TypeMap)"
	idx := strings.Index(body, decl)
	if idx < 0 {
		return body, fmt.Errorf("could not find decodeMapValue signature in serde/decode.go")
	}
	braceStart := strings.IndexByte(body[idx:], '{')
	if braceStart < 0 {
		return body, fmt.Errorf("could not locate decodeMapValue opening brace")
	}
	openIdx := idx + braceStart
	endIdx, err := findMatchingBraceEnd(body, openIdx)
	if err != nil {
		return body, fmt.Errorf("locating decodeMapValue body end: %w", err)
	}
	replacement := decodeMapValuePatched(family)
	if replacement == "" {
		return body, fmt.Errorf("no decodeMapValue after-image for family %s", family)
	}
	return body[:idx] + replacement + body[endIdx+1:], nil
}

// decodeMapValuePatched emits the canonical post-patch body for each
// family. All bodies share two invariants: every entry lands in the
// returned OrderedFields via `values.Set(entry.Key, ...)` so insertion
// order matches the CFFI entry order, and the orderedMapDecodeMarker
// comment is embedded at the top of the body so the sentinel in
// patchDecodeGo can recognise the post-patch shape on re-runs.
func decodeMapValuePatched(family decodeFamily) string {
	switch family {
	case familyA:
		return `func decodeMapValue(valueMap *cffi.CFFIValueMap, typeMap TypeMap) reflect.Value {
	if valueMap == nil {
		panic("decodeMapValue: valueMap is nil")
	}
	debugLog("decodeMapValue: valueMap=%+v\n", valueMap)
	keyType := valueMap.KeyType
	valueType := valueMap.ValueType
	goKeyType := convertFieldTypeToGoType(keyType, typeMap)
	goValueType := convertFieldTypeToGoType(valueType, typeMap)

	debugLog("goKeyType: %v\n", goKeyType)
	debugLog("goValueType: %v\n", goValueType)
	_ = goKeyType
	_ = goValueType

	` + orderedMapDecodeMarker + ` so generated client
	// conversion preserves BAML emission order. The returned reflect.Value
	// wraps OrderedFields; static-map call sites convert via the typed
	// helper emitted by cmd/hacks/hacks/dynamic_order_client.go.
	values := NewOrderedFields(len(valueMap.Entries))
	for _, entry := range valueMap.Entries {
		key := entry.Key
		value := entry.Value
		decodedValue := Decode(value, typeMap)
		var boxed any
		if decodedValue.IsValid() {
			boxed = decodedValue.Interface()
		}
		_ = values.Set(key, boxed)
	}
	return reflect.ValueOf(values)
}`
	case familyB:
		return `func decodeMapValue(valueMap *cffi.CFFIValueMap, typeMap TypeMap) (reflect.Value, reflect.Type) {
	if valueMap == nil {
		panic("decodeMapValue: valueMap is nil")
	}
	keyType := valueMap.KeyType
	valueType := valueMap.ValueType
	goKeyType := convertFieldTypeToGoType(keyType, typeMap)
	goValueType := convertFieldTypeToGoType(valueType, typeMap)
	_ = goKeyType
	_ = goValueType

	` + orderedMapDecodeMarker + ` so generated client
	// conversion preserves BAML emission order. The returned reflect.Value
	// wraps OrderedFields; static-map call sites convert via the typed
	// helper emitted by cmd/hacks/hacks/dynamic_order_client.go.
	values := NewOrderedFields(len(valueMap.Entries))
	for _, entry := range valueMap.Entries {
		key := entry.Key
		value := entry.Value
		decodedValue, _ := Decode(value, typeMap)
		var boxed any
		if decodedValue.IsValid() {
			boxed = decodedValue.Interface()
		}
		_ = values.Set(key, boxed)
	}
	rv := reflect.ValueOf(values)
	return rv, rv.Type()
}`
	case familyC:
		return `func decodeMapValue(valueMap *cffi.CFFIValueMap, typeMap TypeMap) (reflect.Value, reflect.Type) {
	if valueMap == nil {
		panic("decodeMapValue: valueMap is nil")
	}
	keyType := valueMap.KeyType
	valueType := valueMap.ValueType
	goKeyType := convertFieldTypeToGoType(keyType, typeMap)
	goValueType := convertFieldTypeToGoType(valueType, typeMap)
	debugLog("goValueType: %+v\n", goValueType)
	debugLog("typeMap.typeMap[\"INTERNAL.nil\"]: %+v\n", typeMap.typeMap["INTERNAL.nil"])
	_ = goKeyType

	` + orderedMapDecodeMarker + ` so generated client
	// conversion preserves BAML emission order. The INTERNAL.nil branch
	// keeps scalar-preserving assignments (int64/float64/bool surface
	// through reflect.Value's typed accessors) so dynamic-value maps
	// hand back faithfully typed scalars; the concrete branch routes
	// each value through Decode and boxes the result so generated
	// client conversion can rebuild baml.OrderedMap[T] without losing
	// CFFI key order.
	values := NewOrderedFields(len(valueMap.Entries))
	if goValueType == typeMap.typeMap["INTERNAL.nil"] {
		for _, entry := range valueMap.Entries {
			key := entry.Key
			value := entry.Value
			decodedValue, goType := Decode(value, typeMap)
			switch goType {
			case reflect.TypeOf(int64(0)):
				_ = values.Set(key, decodedValue.Int())
			case reflect.TypeOf(float64(0)):
				_ = values.Set(key, decodedValue.Float())
			case reflect.TypeOf(false):
				_ = values.Set(key, decodedValue.Bool())
			default:
				if decodedValue.IsValid() {
					_ = values.Set(key, decodedValue.Interface())
				} else {
					_ = values.Set(key, nil)
				}
			}
		}
	} else {
		for _, entry := range valueMap.Entries {
			key := entry.Key
			value := entry.Value
			decodedValue, _ := Decode(value, typeMap)
			var boxed any
			if decodedValue.IsValid() {
				boxed = decodedValue.Interface()
			}
			_ = values.Set(key, boxed)
		}
	}
	rv := reflect.ValueOf(values)
	return rv, rv.Type()
}`
	default:
		return ""
	}
}

// patchConvertFieldTypeToGoTypeMapArm rewrites the MapType arm of
// convertFieldTypeToGoType so it advertises the ordered carrier type
// (reflect.TypeOf(OrderedFields{})); the upstream return value
// reflect.MapOf(K, V) becomes the ordered carrier under this patch.
// Optional/checked/stream-state wrappers call this to allocate their
// destination via reflect.New; without this rewrite a wrapper around a
// map field allocates `*map[K]V` and panics when the inner decode
// produces an OrderedFields carrier. The reflect.Type advertised here
// matches decodeMapValue's return type so the type-assertion at the
// wrapper level lines up byte-for-byte.
func patchConvertFieldTypeToGoTypeMapArm(body string, family decodeFamily) (string, error) {
	spec, ok := convertFieldTypeMapArmSpecs[family]
	if !ok {
		return body, fmt.Errorf("no convertFieldTypeToGoType map-arm spec for family %s", family)
	}
	// Idempotency: the after-image's distinctive comment pins
	// "this arm was rewritten" so a re-run skips cleanly.
	if strings.Contains(body, orderedMapTypeMarker) {
		return body, nil
	}
	loc := spec.re.FindStringSubmatchIndex(body)
	if loc == nil {
		return body, fmt.Errorf("could not locate convertFieldTypeToGoType map arm for family %s", family)
	}
	// Group 1 captures the leading indentation of the matched line so
	// the after-image preserves layout regardless of upstream tab/space
	// drift across BAML revisions.
	indent := body[loc[2]:loc[3]]
	after := strings.ReplaceAll(spec.afterTemplate, "{I}", indent)
	return body[:loc[0]] + after + body[loc[1]:], nil
}

// orderedMapTypeMarker is the comment line embedded in the rewritten
// map arm of convertFieldTypeToGoType. Acts as both an idempotency
// pin and a per-family reviewable marker.
const orderedMapTypeMarker = "// ordered map type: advertise OrderedFields carrier"

// convertFieldTypeMapArmSpec describes how to locate and rewrite the
// map arm of convertFieldTypeToGoType for a single BAML serde family.
// re must match exactly once in the file and capture the leading
// indentation in group 1. afterTemplate is the replacement, with "{I}"
// placeholders for the leading indentation captured from the match.
type convertFieldTypeMapArmSpec struct {
	re            *regexp.Regexp
	afterTemplate string
}

// convertFieldTypeMapArmSpecs holds the per-family token anchors and
// after-image templates for the map arm of convertFieldTypeToGoType.
// The patterns target the inline `return reflect.MapOf(...)` shape
// upstream uses; the after-image returns the OrderedFields reflect.Type
// instead so optional/checked/stream-state wrappers allocate the
// ordered carrier directly.
//
// familyA (v0.204):
//
//	`return reflect.MapOf(convertFieldTypeToGoType(mapType.Key, typeMap), convertFieldTypeToGoType(mapType.Value, typeMap))`
//
// familyB/C (v0.215+):
//
//	`goKeyType := convertFieldTypeToGoType(mapType.KeyType, typeMap)`
//	`goValueType := convertFieldTypeToGoType(mapType.ValueType, typeMap)`
//	`return reflect.MapOf(goKeyType, goValueType)`
var convertFieldTypeMapArmSpecs = map[decodeFamily]convertFieldTypeMapArmSpec{
	familyA: {
		re: regexp.MustCompile(`(?m)^([ \t]+)return reflect\.MapOf\(convertFieldTypeToGoType\(mapType\.Key, typeMap\), convertFieldTypeToGoType\(mapType\.Value, typeMap\)\)[ \t]*\n`),
		afterTemplate: "{I}" + orderedMapTypeMarker + "; the ordered\n" +
			"{I}// runtime decoder produces an OrderedFields and the generated\n" +
			"{I}// client converts it back to baml.OrderedMap[T] at the static-map\n" +
			"{I}// surface. Allocating a native map type here would mismatch the\n" +
			"{I}// reflect.Value handed back by decodeMapValue.\n" +
			"{I}_ = convertFieldTypeToGoType(mapType.Key, typeMap)\n" +
			"{I}_ = convertFieldTypeToGoType(mapType.Value, typeMap)\n" +
			"{I}return reflect.TypeOf(OrderedFields{})\n",
	},
	familyB: {
		re: regexp.MustCompile(`(?m)^([ \t]+)return reflect\.MapOf\(convertFieldTypeToGoType\(mapType\.KeyType, typeMap\), convertFieldTypeToGoType\(mapType\.ValueType, typeMap\)\)[ \t]*\n`),
		afterTemplate: "{I}" + orderedMapTypeMarker + "; the ordered\n" +
			"{I}// runtime decoder produces an OrderedFields and the generated\n" +
			"{I}// client converts it back to baml.OrderedMap[T] at the static-map\n" +
			"{I}// surface. Allocating a native map type here would mismatch the\n" +
			"{I}// reflect.Value handed back by decodeMapValue.\n" +
			"{I}_ = convertFieldTypeToGoType(mapType.KeyType, typeMap)\n" +
			"{I}_ = convertFieldTypeToGoType(mapType.ValueType, typeMap)\n" +
			"{I}return reflect.TypeOf(OrderedFields{})\n",
	},
	familyC: {
		re: regexp.MustCompile(`(?m)^([ \t]+)goKeyType := convertFieldTypeToGoType\(mapType\.KeyType, typeMap\)\n[ \t]+goValueType := convertFieldTypeToGoType\(mapType\.ValueType, typeMap\)\n[ \t]+if goValueType == typeMap\.typeMap\["INTERNAL\.nil"\] \{\n[ \t]+return reflect\.TypeOf\(map\[string\]any\{\}\)\n[ \t]+\}\n[ \t]+return reflect\.MapOf\(goKeyType, goValueType\)[ \t]*\n`),
		afterTemplate: "{I}" + orderedMapTypeMarker + "; the ordered\n" +
			"{I}// runtime decoder produces an OrderedFields and the generated\n" +
			"{I}// client converts it back to baml.OrderedMap[T] at the static-map\n" +
			"{I}// surface. Allocating a native map type here would mismatch the\n" +
			"{I}// reflect.Value handed back by decodeMapValue. Both the\n" +
			"{I}// INTERNAL.nil dynamic-valued branch and the concrete-valued\n" +
			"{I}// branch collapse to OrderedFields because decodeMapValue\n" +
			"{I}// stores either shape through the same carrier.\n" +
			"{I}_ = convertFieldTypeToGoType(mapType.KeyType, typeMap)\n" +
			"{I}_ = convertFieldTypeToGoType(mapType.ValueType, typeMap)\n" +
			"{I}return reflect.TypeOf(OrderedFields{})\n",
	},
}

// orderedMapEncodeMarker pins the after-image of patchEncodeGo so the
// patcher recognises an already-patched tree and a re-run converges
// without double-applying the dispatch.
const orderedMapEncodeMarker = "// ordered map encode: materialize OrderedFields"

// orderedRangerInterfaceSnippet is the small interface definition the
// encode dispatch tests against, plus the pointer-unwrap helper the
// per-family insertions call. Embedded at the bottom of encode.go once
// per file; both per-family encodeValue insertions reference it by
// name so the patch stays minimal.
const orderedRangerInterfaceSnippet = `// orderedRanger captures the structural shape both
// OrderedFields (= OrderedMap[any]) and any typed
// OrderedMap[T] satisfy through bamlutils' RangeAny method.
// Used by encodeValue so static-map inputs (now typed as
// baml.OrderedMap[T]) reach the BAML map-encoding pipeline
// without losing key/value resolution.
type orderedRanger interface {
	RangeAny(func(string, any) bool)
	Len() int
}

// unwrapEncodeOrderedCarrier normalises ordered-map carriers the
// generated client may pass into encodeValue. A value receiver
// orderedMap[V] satisfies orderedRanger directly; a pointer to one
// (used by optional map fields) is unwrapped via reflect so a nil
// pointer surfaces as no match — the RangeAny invocation on a nil
// pointer would otherwise panic. Returns nil for any other shape so
// the caller falls through to the standard kind dispatch.
func unwrapEncodeOrderedCarrier(value any) orderedRanger {
	if value == nil {
		return nil
	}
	if r, ok := value.(orderedRanger); ok {
		return r
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		if r, ok := rv.Elem().Interface().(orderedRanger); ok {
			return r
		}
	}
	return nil
}`

// patchEncodeGo inserts an orderedRanger dispatch into encodeValue per
// family and appends the orderedRanger interface definition. The
// dispatch materialises the OrderedFields / OrderedMap[V] into a
// map[string]any and recurses through encodeValue so the existing
// map-encoding pipeline (with per-family value_type plumbing) is
// reused unchanged. Idempotent on the marker.
func patchEncodeGo(moduleDir string, family decodeFamily) error {
	path := encodeGoPath(moduleDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := string(raw)

	hasMarker := strings.Contains(body, orderedMapEncodeMarker)
	hasInterface := strings.Contains(body, "type orderedRanger interface")
	if hasMarker && hasInterface {
		return nil
	}

	if !hasMarker {
		spec, ok := encodeDispatchSpecs[family]
		if !ok {
			return fmt.Errorf("no encodeValue dispatch spec for family %s", family)
		}
		loc := spec.anchorRe.FindStringSubmatchIndex(body)
		if loc == nil {
			return fmt.Errorf("could not locate encodeValue dispatch anchor for family %s in %s", family, path)
		}
		// The captured group is the leading indentation of the anchor
		// line; the after-image preserves layout across upstream
		// tab/space drift by substituting the same indent into the
		// inserted block.
		indent := body[loc[2]:loc[3]]
		insertion := strings.ReplaceAll(spec.insertion, "{I}", indent)
		// The anchor regex matches *up to and including* the anchor
		// line; the insertion goes BEFORE that line. loc[0] is the
		// start of the matched span.
		body = body[:loc[0]] + insertion + body[loc[0]:]
	}

	if !hasInterface {
		body = strings.TrimRight(body, "\n") + "\n\n" + orderedRangerInterfaceSnippet + "\n"
	}

	return os.WriteFile(path, []byte(body), 0o644)
}

// encodeDispatchSpec describes how to find the insertion anchor in
// encodeValue for a single family and what to insert. The anchorRe
// must match exactly once in the file and capture the line's
// indentation in group 1; insertion is prepended to the matched span.
type encodeDispatchSpec struct {
	anchorRe  *regexp.Regexp
	insertion string
}

// encodeDispatchSpecs holds per-family encode-side patch anchors. The
// dispatch lives at a slightly different point in each family: familyA
// computes value_type from encodeFieldType at the top of encodeValue,
// which errors on OrderedMap's struct kind — so the dispatch must
// land BEFORE the value_type computation. familyB/C have no upfront
// value_type, so the dispatch lands right before the `switch rv.Kind()`
// kind dispatch (after the receiver/serializer/Checked/StreamState
// checks).
//
// The dispatch body itself is family-stable: it materialises the
// OrderedFields / OrderedMap[V] into a `map[string]any` (RangeAny
// captures order at the source) and recurses through encodeValue, so
// the existing map-encoding pipeline (with per-family Type/ValueType
// plumbing) is reused unchanged. Order is preserved at the materialise
// step because RangeAny walks the carrier in insertion order; the
// downstream encodeMap may reshuffle native-map iteration, but
// generated input encoding does not require strict order — input
// shape, not key sequence, is what BAML's runtime asserts against.
var encodeDispatchSpecs = map[decodeFamily]encodeDispatchSpec{
	familyA: {
		// Anchor: the very first line inside encodeValue. familyA's
		// value_type computation happens immediately after; the
		// dispatch must precede it so OrderedMap's struct kind never
		// reaches encodeFieldType.
		anchorRe: regexp.MustCompile(`(?m)^([ \t]+)value_type, err := encodeFieldType\(reflect\.TypeOf\(value\),\)[ \t]*\n`),
		insertion: "{I}" + orderedMapEncodeMarker + " into a native\n" +
			"{I}// map[string]any and recurse. The downstream encodeMap path then\n" +
			"{I}// computes the CFFIValueMap KeyType/ValueType correctly even though\n" +
			"{I}// the carrier was a struct on entry. Pointer-wrapped ordered carriers\n" +
			"{I}// (used by optional map fields) are unwrapped via reflect so a nil\n" +
			"{I}// optional flows to the existing nil-value handling below.\n" +
			"{I}{\n" +
			"{I}\tif probe := unwrapEncodeOrderedCarrier(value); probe != nil {\n" +
			"{I}\t\tmaterialized := make(map[string]any, probe.Len())\n" +
			"{I}\t\tprobe.RangeAny(func(k string, v any) bool {\n" +
			"{I}\t\t\tmaterialized[k] = v\n" +
			"{I}\t\t\treturn true\n" +
			"{I}\t\t})\n" +
			"{I}\t\treturn encodeValue(materialized)\n" +
			"{I}\t}\n" +
			"{I}}\n",
	},
	familyB: {
		// Anchor: the kind-dispatch line near the bottom of encodeValue.
		// Inserting before it leaves the upfront serializer/Checked
		// branches intact.
		anchorRe: regexp.MustCompile(`(?m)^([ \t]+)// Handle primitive kinds and collections using reflection value rv \(points to underlying value\)[ \t]*\n`),
		insertion: "{I}" + orderedMapEncodeMarker + " into a native\n" +
			"{I}// map[string]any and recurse. The downstream encodeMap path then\n" +
			"{I}// produces the HostMapValue with the right structure. Pointer-wrapped\n" +
			"{I}// ordered carriers (used by optional map fields) are unwrapped via\n" +
			"{I}// reflect so a nil optional flows to the existing nil-value handling.\n" +
			"{I}if probe := unwrapEncodeOrderedCarrier(value); probe != nil {\n" +
			"{I}\tmaterialized := make(map[string]any, probe.Len())\n" +
			"{I}\tprobe.RangeAny(func(k string, v any) bool {\n" +
			"{I}\t\tmaterialized[k] = v\n" +
			"{I}\t\treturn true\n" +
			"{I}\t})\n" +
			"{I}\treturn encodeValue(materialized)\n" +
			"{I}}\n" +
			"\n",
	},
	familyC: {
		anchorRe: regexp.MustCompile(`(?m)^([ \t]+)// Handle primitive kinds and collections using reflection value rv \(points to underlying value\)[ \t]*\n`),
		insertion: "{I}" + orderedMapEncodeMarker + " into a native\n" +
			"{I}// map[string]any and recurse. The downstream encodeMap path then\n" +
			"{I}// produces the HostMapValue with the right structure. Pointer-wrapped\n" +
			"{I}// ordered carriers (used by optional map fields) are unwrapped via\n" +
			"{I}// reflect so a nil optional flows to the existing nil-value handling.\n" +
			"{I}if probe := unwrapEncodeOrderedCarrier(value); probe != nil {\n" +
			"{I}\tmaterialized := make(map[string]any, probe.Len())\n" +
			"{I}\tprobe.RangeAny(func(k string, v any) bool {\n" +
			"{I}\t\tmaterialized[k] = v\n" +
			"{I}\t\treturn true\n" +
			"{I}\t})\n" +
			"{I}\treturn encodeValue(materialized)\n" +
			"{I}}\n" +
			"\n",
	},
}

// orderedMapRawObjectMarker pins the after-image of the raw-object map
// conversion helper appended to each affected rawobjects_* file.
const orderedMapRawObjectMarker = "// ordered map compat: convert OrderedFields back to native"

// rawObjectMapCompatFiles is the per-family set of pkg/rawobjects_*.go
// files whose public methods return native `map[string]string` or
// `map[string]any` shapes. Their decode paths now hand back OrderedFields;
// without the compat shim the .(map[string]T) type assertion panics.
//
// The same three files cover all three families today; the per-family
// dispatch exists so a future family can add or remove files without
// requiring a new patcher signature.
var rawObjectMapCompatFiles = map[decodeFamily][]string{
	familyA: {"rawobjects_http_request.go", "rawobjects_http_response.go", "rawobjects_function_log.go"},
	familyB: {"rawobjects_http_request.go", "rawobjects_http_response.go", "rawobjects_function_log.go"},
	familyC: {"rawobjects_http_request.go", "rawobjects_http_response.go", "rawobjects_function_log.go"},
}

// patchRawObjectMaps walks the per-family rawobjects_* file list and
// rewrites every `.(map[string]string)` and `.(map[string]any)` type
// assertion to flow through orderedFieldsToStringMap /
// orderedFieldsToAnyMap helpers. The helpers handle both legacy native
// maps (compat shim for callers that still produce them) and the new
// OrderedFields carrier. Missing files are tolerated quietly: a family
// that introduces a new file later does not block the patch. The
// helpers themselves live in a single sibling file
// (pkg/ordered_map_compat.go) so multiple rawobjects_*.go files share
// the same declaration without redeclaration errors.
func patchRawObjectMaps(moduleDir string, family decodeFamily) error {
	files, ok := rawObjectMapCompatFiles[family]
	if !ok {
		return fmt.Errorf("no raw-object map compat file list for family %s", family)
	}
	anyChanged := false
	for _, name := range files {
		path := rawObjectsPath(moduleDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(raw)
		newBody := rewriteRawObjectMapAsserts(body)
		if newBody == body {
			continue
		}
		if err := os.WriteFile(path, []byte(newBody), 0o644); err != nil {
			return err
		}
		anyChanged = true
	}

	// Emit the shared helper file once per module. The file is keyed by
	// path and contents so re-runs against an already-patched tree
	// produce a byte-stable file.
	helperPath := rawObjectsPath(moduleDir, "ordered_map_compat.go")
	if !anyChanged {
		// Even if no rewrites landed (e.g. a family that already had no
		// raw-object map asserts), still emit the helper file so the
		// patched module surface is uniform across families.
	}
	return os.WriteFile(helperPath, []byte(rawObjectsHelperFile()), 0o644)
}

// rewriteRawObjectMapAsserts rewrites every `<expr>.(map[string]string)`
// and `<expr>.(map[string]any)` substring into a helper call that
// accepts the OrderedFields carrier. The helpers return `(map, bool)`
// so the upstream `m, ok := <expr>.(...)` shape compiles unchanged.
// Pattern coverage is intentionally narrow — the upstream files use
// only these two assertion shapes for map returns — and the rewrite
// leaves any structurally similar expression that does not match the
// exact suffix untouched.
func rewriteRawObjectMapAsserts(body string) string {
	body = rewriteMapAssert(body, ".(map[string]string)", "orderedFieldsToStringMap")
	body = rewriteMapAssert(body, ".(map[string]any)", "orderedFieldsToAnyMap")
	return body
}

// rewriteMapAssert scans body for assertSuffix occurrences, walks
// backwards from each `.` to find the start of the primary expression
// being asserted on, and rewrites `<expr>.assertSuffix` into
// `helperName(<expr>)`. The walker tolerates nested parens, brackets,
// and dotted member chains so `foo.bar[i].baz.(...)` rewrites cleanly.
func rewriteMapAssert(body, assertSuffix, helperName string) string {
	for {
		idx := strings.Index(body, assertSuffix)
		if idx < 0 {
			return body
		}
		end := idx + len(assertSuffix)
		exprStart := walkBackPrimary(body, idx)
		expr := body[exprStart:idx]
		replacement := fmt.Sprintf("%s(%s)", helperName, expr)
		body = body[:exprStart] + replacement + body[end:]
	}
}

// walkBackPrimary walks backwards from a `.` at endIdx (the start of the
// assertion suffix) and returns the index of the start of the primary
// expression. Supports identifiers, dotted member chains, bracket
// indexing, and balanced parenthesised sub-expressions. Stops at the
// first non-expression character (whitespace, operator, comma, brace,
// statement boundary).
func walkBackPrimary(body string, endIdx int) int {
	i := endIdx
	parenDepth := 0
	brackDepth := 0
	for i > 0 {
		c := body[i-1]
		if parenDepth > 0 {
			switch c {
			case '(':
				parenDepth--
			case ')':
				parenDepth++
			}
			i--
			continue
		}
		if brackDepth > 0 {
			switch c {
			case '[':
				brackDepth--
			case ']':
				brackDepth++
			}
			i--
			continue
		}
		if c == ')' {
			parenDepth++
			i--
			continue
		}
		if c == ']' {
			brackDepth++
			i--
			continue
		}
		if c == '.' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			i--
			continue
		}
		break
	}
	return i
}

// rawObjectsHelperFile returns the full sibling file written into
// pkg/ordered_map_compat.go. The helpers return `(map, bool)` so the
// upstream `m, ok := <expr>.(...)` shape compiles unchanged. The bool
// is true on every successful conversion (native, legacy, or ordered
// carrier) and false only when the input is nil or an unrecognised
// shape — matching the original type-assertion semantics that returned
// `(nil, false)` on mismatch.
func rawObjectsHelperFile() string {
	return `// Code generated by cmd/hacks/hacks/dynamic_order_fix.go; DO NOT EDIT.
// ` + orderedMapRawObjectMarker + ` map shapes. The raw-object public
// methods promise map[string]string / map[string]any returns; the
// patched runtime decoder now hands back OrderedFields for every CFFI
// map node, so the .(map[string]T) type assertions previously inline
// would silently fail (comma-ok form: !ok branch surfaced as
// "unexpected type"). These helpers accept either the legacy native
// map (compat shim for callers that still build one) or the ordered
// carrier and produce the native map the public method advertises.
package baml

import "fmt"

// orderedFieldsToStringMap converts an arbitrary value that may be a
// native map[string]string, a map[string]any whose values stringify, or
// any ordered carrier (OrderedFields / OrderedMap[T] via RangeAny) into
// a native map[string]string. Returns (nil, false) for nil or
// unrecognised shapes so call sites that previously matched
// .(map[string]string) keep the same comma-ok contract.
func orderedFieldsToStringMap(value any) (map[string]string, bool) {
	if value == nil {
		return nil, false
	}
	if m, ok := value.(map[string]string); ok {
		return m, true
	}
	if m, ok := value.(map[string]any); ok {
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			} else if v != nil {
				out[k] = fmt.Sprintf("%v", v)
			}
		}
		return out, true
	}
	if ranger, ok := value.(interface {
		RangeAny(func(string, any) bool)
		Len() int
	}); ok {
		out := make(map[string]string, ranger.Len())
		ranger.RangeAny(func(k string, v any) bool {
			if s, ok := v.(string); ok {
				out[k] = s
			} else if v != nil {
				out[k] = fmt.Sprintf("%v", v)
			}
			return true
		})
		return out, true
	}
	return nil, false
}

// orderedFieldsToAnyMap converts an arbitrary value that may be a
// native map[string]any or any ordered carrier (OrderedFields /
// OrderedMap[T] via RangeAny) into a native map[string]any. Returns
// (nil, false) for nil or unrecognised shapes.
func orderedFieldsToAnyMap(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	if m, ok := value.(map[string]any); ok {
		return m, true
	}
	if ranger, ok := value.(interface {
		RangeAny(func(string, any) bool)
		Len() int
	}); ok {
		out := make(map[string]any, ranger.Len())
		ranger.RangeAny(func(k string, v any) bool {
			out[k] = v
			return true
		})
		return out, true
	}
	return nil, false
}
`
}

// runGoModTidy invokes `go mod tidy` inside moduleDir so the go.sum
// gains the transitive entries for github.com/bytedance/sonic the
// generated serde/ordered_fields.go now requires. The patched module
// is consumed both through go.work-managed replaces (where the parent
// go.sum supplies the entries) and as a standalone module (where the
// fork must own its own go.sum); tidying after the patch lands keeps
// both paths working.
//
// GOWORK is forced off so the tidy operates on the patched module in
// isolation, regardless of any workspace the operator may be running
// inside.
func runGoModTidy(moduleDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("go mod tidy timed out after 5m: %s", strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureSonicRequire adds `require github.com/bytedance/sonic <pin>`
// to the patched module's go.mod when absent. Sonic is the
// JSON-marshal backend the generated serde/ordered_fields.go inherits
// from bamlutils/orderedmap.go.
func ensureSonicRequire(moduleDir string) error {
	goModPath := filepath.Join(moduleDir, "go.mod")
	raw, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	contents := string(raw)
	if strings.Contains(contents, "github.com/bytedance/sonic") {
		return nil
	}
	requireLine := fmt.Sprintf("\nrequire github.com/bytedance/sonic %s\n", dynamicOrderSonicVersion)
	contents = strings.TrimRight(contents, "\n") + requireLine
	return os.WriteFile(goModPath, []byte(contents), 0o644)
}

