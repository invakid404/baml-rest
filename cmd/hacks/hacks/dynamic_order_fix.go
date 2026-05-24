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

	if strings.Contains(body, "Fields OrderedFields") {
		return nil
	}

	// 1. Replace the DynamicClass struct field type. BAML's upstream
	// source uses a single space; gofmt's tab-aligned struct field
	// layout in patched copies uses a tab. Match either.
	patched, ok := replaceFirstFieldDecl(body)
	if !ok {
		return fmt.Errorf("could not find a `Fields map[string]any` declaration in %s; BAML serde shape may have changed", path)
	}
	body = patched

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

	// 4. Append the family-specific DecodeToOrderedValue helper.
	body = strings.TrimRight(body, "\n") + "\n\n" + decodeToOrderedValueFunc(family) + "\n"

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

	if strings.Contains(body, "type OrderedFields = serde.OrderedFields") {
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

	body = strings.TrimRight(body, "\n") + "\n\n" + pkgFacadeAppend(family) + "\n"

	return os.WriteFile(path, []byte(body), 0o644)
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
func EncodeClassOrdered(name string, fields map[string]any, dynamicFields *OrderedFields) (*cffi.CFFIValueHolder, error) {
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

