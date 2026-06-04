package hacks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaticMapClientRewrite_NullValueMap reproduces the bamlfuzz
// nightly Static-target failure on BAML 0.219+: a `map<string, null>`
// union arm lowers to the Go type `map[string]*interface{}`, and the
// static-map pass mangled the `interface{}` value type into an invalid
// helper identifier (`bamlOrderedAs_map[stringPtr_interface{}`),
// producing "static-map rewrite produced invalid Go: expected ']',
// found newline".
//
// The fixture is the exact shape BAML 0.219 emits for a
// `(int | map<string, null> | string)` union (captured from
// `npx @boundaryml/baml@0.219.0 generate`). The rewrite must now
// succeed, route the decode assert through the ordered helper, and
// emit a helper that preserves null entries.
func TestStaticMapClientRewrite_NullValueMap(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const before = `package types

import (
	"encoding/json"
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type Union3IntOrMapStringKeyNullValueOrString struct {
	variant string

	variant_Int *int64

	variant_MapStringKeyNullValue *map[string]*interface{}

	variant_String *string
}

func (u *Union3IntOrMapStringKeyNullValueOrString) Decode(holder *cffi.CFFIValueUnionVariant, typeMap baml.TypeMap) {
	valueHolder := holder.Value
	variantName := holder.ValueOptionName
	switch variantName {
	case "int":
		u.variant = "Int"
		value := baml.Decode(valueHolder).Int()
		u.variant_Int = &value
	case "string":
		u.variant = "String"
		value := baml.Decode(valueHolder).Interface().(string)
		u.variant_String = &value
	case "Map__string_null":
		u.variant = "MapStringKeyNullValue"
		value := baml.Decode(valueHolder).Interface().(map[string]*interface{})
		u.variant_MapStringKeyNullValue = &value
	default:
		panic(fmt.Sprintf("invalid union variant: %s", variantName))
	}
}

func (u *Union3IntOrMapStringKeyNullValueOrString) AsMapStringKeyNullValue() *map[string]*interface{} {
	if u.variant != "MapStringKeyNullValue" {
		return nil
	}
	return u.variant_MapStringKeyNullValue
}

func (u Union3IntOrMapStringKeyNullValueOrString) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "MapStringKeyNullValue":
		return json.Marshal(u.variant_MapStringKeyNullValue)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
`
	path := filepath.Join(typesDir, "unions.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	// Before the fix this returned an "expected ']', found newline"
	// error; the regression assertion is simply that Apply succeeds.
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply (regression — null-value map should rewrite cleanly): %v", err)
	}

	after := readFileT(t, path)
	// The `*interface{}` value type rewrites to an ordered map in every
	// position (field, accessor return, __New/Set params elided here).
	for _, marker := range []string{
		"*baml.OrderedMap[*interface{}]",
		"bamlOrderedAs_OM_Ptr_Iface(baml.DecodeToOrderedValue(valueHolder))",
	} {
		if !strings.Contains(after, marker) {
			t.Fatalf("missing rewritten marker %q in patched file:\n%s", marker, after)
		}
	}
	// The corrupt pre-fix identifier must not appear.
	if strings.Contains(after, "bamlOrderedAs_map[") {
		t.Fatalf("patched file still contains the corrupt helper identifier:\n%s", after)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	for _, marker := range []string{
		"func bamlOrderedAs_OM_Ptr_Iface(value any) baml.OrderedMap[*interface{}] {",
		// Null map entries keep their key with a nil value rather than
		// panicking the `v.(*interface{})` assertion on an untyped nil.
		"_ = out.Set(k, nil)",
		"v.(*interface{})",
	} {
		if !strings.Contains(helperBody, marker) {
			t.Fatalf("helper file missing %q:\n%s", marker, helperBody)
		}
	}

	// Both rewritten files must parse as valid Go (Apply already
	// enforces this internally, but assert it explicitly here so a
	// future regression in the helper emitter is caught by this test).
	mustParseGo(t, path, after)
	mustParseGo(t, helperPath, helperBody)
}

// TestUnusedDecodeVarHack_AllNullClass reproduces the second BAML 0.219+
// Static-target failure: a class whose fields are all BAML `null`
// decodes every field as `(*interface{})(nil)` without reading the
// `valueHolder := field.Value` the generator always emits, so `go build`
// fails with "declared and not used: valueHolder".
//
// The fixture is self-contained (no imports) so go/types can verify the
// compile error exists before the hack and is gone after, while a mixed
// class that does read valueHolder is left untouched.
func TestUnusedDecodeVarHack_AllNullClass(t *testing.T) {
	dir := t.TempDir()
	clientDir := filepath.Join(dir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Self-contained stand-in for the generated decode shape: holderT /
	// fieldT model holder.Fields + field.Key/field.Value so the package
	// type-checks with no importer. AllNull mirrors a class all of whose
	// fields are BAML `null`; Mixed reads valueHolder for a non-null
	// field and must be left alone.
	const before = `package types

type fieldT struct {
	Key   string
	Value any
}

type holderT struct {
	Fields []fieldT
}

type AllNull struct {
	A *interface{}
	B *interface{}
}

func (c *AllNull) Decode(holder *holderT) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "a":
			c.A = (*interface{})(nil)
		case "b":
			c.B = (*interface{})(nil)
		}
	}
}

type Mixed struct {
	A *interface{}
	S *string
}

func (c *Mixed) Decode(holder *holderT) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "a":
			c.A = (*interface{})(nil)
		case "s":
			s, _ := valueHolder.(string)
			c.S = &s
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Precondition: the fixture genuinely has the bug.
	if errs := typeCheckErrors(before); !anyMentions(errs, "valueHolder") {
		t.Fatalf("fixture precondition failed — expected a 'declared and not used: valueHolder' error, got: %v", errs)
	}

	hack := &UnusedDecodeVarHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, path)
	// AllNull's unused declaration is blanked; Mixed's used declaration
	// is not (exactly one blank assignment in the file).
	if got := strings.Count(after, "_ = valueHolder"); got != 1 {
		t.Fatalf("expected exactly one `_ = valueHolder` (AllNull only), got %d:\n%s", got, after)
	}

	// The blanked file now type-checks with no unused-variable error.
	if errs := typeCheckErrors(after); anyMentions(errs, "declared and not used") {
		t.Fatalf("unused-variable error survived the fix: %v\n%s", errs, after)
	}

	// Idempotent: a second pass adds nothing (the blank assignment
	// already counts as a use of valueHolder).
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply (second pass): %v", err)
	}
	after2 := readFileT(t, path)
	if got := strings.Count(after2, "_ = valueHolder"); got != 1 {
		t.Fatalf("hack is not idempotent — second pass produced %d `_ = valueHolder`:\n%s", got, after2)
	}
}

// mustParseGo fails the test if src does not parse as a Go file.
func mustParseGo(t *testing.T, name, src string) {
	t.Helper()
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, name, []byte(src), parser.ParseComments); err != nil {
		t.Fatalf("rewritten %s does not parse: %v\n%s", filepath.Base(name), err, src)
	}
}

// typeCheckErrors type-checks a single import-free Go source string and
// returns every error the checker reports. The source must not import
// any package (no importer is supplied), which holds for the
// self-contained decode fixture above.
func typeCheckErrors(src string) []error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", []byte(src), 0)
	if err != nil {
		return []error{err}
	}
	var errs []error
	conf := types.Config{Error: func(err error) { errs = append(errs, err) }}
	_, _ = conf.Check("types", fset, []*ast.File{file}, nil)
	return errs
}

func anyMentions(errs []error, substr string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), substr) {
			return true
		}
	}
	return false
}
