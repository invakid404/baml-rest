package bamlfuzz

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// StaticBamlSource is the lowered form of a FuzzSchema for the static
// prompt oracle. The integration test wires FunctionName into the
// /call/<FunctionName> URL; ClassNames / EnumNames go into the failure
// envelope so a build failure points at the offending symbols.
type StaticBamlSource struct {
	// Source is the emitted .baml text. Safe to drop directly into a
	// generated baml_src/ alongside the existing integration fixtures —
	// the lowering emits classes/enums/function only, no client block.
	Source string
	// FunctionName is the BAML function declared by Source. Always
	// equals "FuzzFn_" + CaseID.
	FunctionName string
	// CaseID is the suffix added to every generated symbol. Tests pass
	// this through to compose subtest names and replay paths.
	CaseID string
	// ClassNames are the suffixed class declarations in the emitted
	// source, in declaration order.
	ClassNames []string
	// EnumNames are the suffixed enum declarations in the emitted
	// source, in declaration order.
	EnumNames []string
	// RootClass is the suffixed name of the function's return type.
	RootClass string
}

// caseIDPattern is the contract LowerToBamlSource enforces on caseID:
// alphanumerics + underscore, must start with a letter. The pattern
// keeps the suffix safe to splice into a BAML identifier without
// further escaping.
var caseIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// LowerToBamlSource lowers a FuzzSchema into a .baml source string.
//
// Every generated symbol (classes, enums, the synthesized function) is
// suffixed with `_<caseID>` so multiple cases can be batched into one
// baml_src/ directory without name collisions. The returned
// StaticBamlSource exposes the mangled names so the failure envelope
// and the /call/<FunctionName> URL stay in sync with what landed in
// the BAML symbol table.
//
// Static lowering accepts self-referential and mutually-recursive
// schemas: the static .baml path has no upstream limitation
// equivalent to the dynamic TypeBuilder's self-ref/mutual-cycle gates.
// Value-side termination is enforced earlier, at value generation.
//
// The emitted function declares `client TestClient`, expecting the
// integration test to supply a per-request `client_registry` override
// pointing TestClient at the scenario for this case.
func LowerToBamlSource(schema FuzzSchema, caseID string) (StaticBamlSource, error) {
	if !caseIDPattern.MatchString(caseID) {
		return StaticBamlSource{}, fmt.Errorf("bamlfuzz: invalid caseID %q (must match %s)", caseID, caseIDPattern)
	}
	if schema.RootClass == "" {
		return StaticBamlSource{}, fmt.Errorf("bamlfuzz: schema missing RootClass")
	}
	if _, ok := schema.FindClass(schema.RootClass); !ok {
		return StaticBamlSource{}, fmt.Errorf("bamlfuzz: root class %q not present in schema", schema.RootClass)
	}

	classNames := make(map[string]string, len(schema.Classes))
	enumNames := make(map[string]string, len(schema.Enums))
	classOrder := make([]string, 0, len(schema.Classes))
	enumOrder := make([]string, 0, len(schema.Enums))

	for _, cls := range schema.Classes {
		mangled := cls.Name + "_" + caseID
		classNames[cls.Name] = mangled
		classOrder = append(classOrder, mangled)
	}
	for _, enum := range schema.Enums {
		mangled := enum.Name + "_" + caseID
		enumNames[enum.Name] = mangled
		enumOrder = append(enumOrder, mangled)
	}

	var b strings.Builder
	for _, enum := range schema.Enums {
		if err := writeEnum(&b, enum, enumNames[enum.Name]); err != nil {
			return StaticBamlSource{}, fmt.Errorf("bamlfuzz: emit enum %s: %w", enum.Name, err)
		}
		b.WriteByte('\n')
	}
	for _, cls := range schema.Classes {
		if err := writeClass(&b, cls, classNames, enumNames); err != nil {
			return StaticBamlSource{}, fmt.Errorf("bamlfuzz: emit class %s: %w", cls.Name, err)
		}
		b.WriteByte('\n')
	}

	funcName := "FuzzFn_" + caseID
	rootMangled := classNames[schema.RootClass]
	if err := writeFunction(&b, funcName, rootMangled); err != nil {
		return StaticBamlSource{}, fmt.Errorf("bamlfuzz: emit function: %w", err)
	}

	return StaticBamlSource{
		Source:       b.String(),
		FunctionName: funcName,
		CaseID:       caseID,
		ClassNames:   classOrder,
		EnumNames:    enumOrder,
		RootClass:    rootMangled,
	}, nil
}

// writeEnum emits a BAML enum declaration. Values are written in the
// order the IR records them so failure replay produces byte-stable
// source across runs at the same seed.
func writeEnum(b *strings.Builder, enum FuzzEnum, mangledName string) error {
	if len(enum.Values) == 0 {
		return fmt.Errorf("enum %s has no values", enum.Name)
	}
	b.WriteString("enum ")
	b.WriteString(mangledName)
	b.WriteString(" {\n")
	for _, v := range enum.Values {
		b.WriteString("  ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return nil
}

// writeClass emits a BAML class declaration. Property order follows
// the IR; type spellings come from typeSpelling which consults the
// mangle maps for class/enum refs.
func writeClass(b *strings.Builder, cls FuzzClass, classNames, enumNames map[string]string) error {
	mangled, ok := classNames[cls.Name]
	if !ok {
		return fmt.Errorf("class %s not in mangle map", cls.Name)
	}
	b.WriteString("class ")
	b.WriteString(mangled)
	b.WriteString(" {\n")
	for _, prop := range cls.Properties {
		spell, err := typeSpelling(prop.Type, classNames, enumNames)
		if err != nil {
			return fmt.Errorf("property %s: %w", prop.Name, err)
		}
		b.WriteString("  ")
		b.WriteString(prop.Name)
		b.WriteByte(' ')
		b.WriteString(spell)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return nil
}

// writeFunction emits the synthesized BAML function. The function
// returns the suffixed root class and references the existing
// TestClient declared in the integration testdata baml_src/. Tests
// override TestClient via a per-request client_registry to point at
// the mockllm scenario for this case.
//
// The prompt includes {{ ctx.output_format }} so schema rendering is
// exercised in the upstream request.
func writeFunction(b *strings.Builder, funcName, rootMangled string) error {
	b.WriteString("function ")
	b.WriteString(funcName)
	b.WriteString("(input: string) -> ")
	b.WriteString(rootMangled)
	b.WriteString(" {\n")
	b.WriteString("  client TestClient\n")
	b.WriteString("  prompt #\"{{ ctx.output_format }}\n")
	b.WriteString("  {{ input }}\"#\n")
	b.WriteString("}\n")
	return nil
}

// typeSpelling returns the BAML source-level type spelling for a
// FuzzType. ClassRef/EnumRef targets are resolved through the mangle
// maps so the emitted source matches the declared symbol names.
func typeSpelling(t FuzzType, classNames, enumNames map[string]string) (string, error) {
	switch t.Kind {
	case KindString, KindInt, KindFloat, KindBool, KindNull:
		return string(t.Kind), nil
	case KindLiteral:
		if t.Literal == nil {
			return "", fmt.Errorf("literal kind missing payload")
		}
		return literalSpelling(t.Literal)
	case KindOptional:
		if t.Inner == nil {
			return "", fmt.Errorf("optional missing inner")
		}
		inner, err := typeSpelling(*t.Inner, classNames, enumNames)
		if err != nil {
			return "", err
		}
		// Wrap composite inners in parens so e.g. `int[]?` parses as
		// optional<list<int>>, not list<optional<int>>. Atomic
		// spellings don't need parens but adding them uniformly keeps
		// the precedence story unambiguous.
		return "(" + inner + ")?", nil
	case KindList:
		if t.Inner == nil {
			return "", fmt.Errorf("list missing inner")
		}
		inner, err := typeSpelling(*t.Inner, classNames, enumNames)
		if err != nil {
			return "", err
		}
		return "(" + inner + ")[]", nil
	case KindMap:
		if t.Key == nil || t.Inner == nil {
			return "", fmt.Errorf("map missing key or inner")
		}
		if t.Key.Kind != KindString {
			return "", fmt.Errorf("map key must be string in v1, got %q", t.Key.Kind)
		}
		inner, err := typeSpelling(*t.Inner, classNames, enumNames)
		if err != nil {
			return "", err
		}
		return "map<string, " + inner + ">", nil
	case KindClassRef:
		mangled, ok := classNames[t.Ref]
		if !ok {
			return "", fmt.Errorf("class ref %q not declared in schema", t.Ref)
		}
		return mangled, nil
	case KindEnumRef:
		mangled, ok := enumNames[t.Ref]
		if !ok {
			return "", fmt.Errorf("enum ref %q not declared in schema", t.Ref)
		}
		return mangled, nil
	default:
		return "", fmt.Errorf("unsupported kind %q", t.Kind)
	}
}

// literalSpelling renders a FuzzLiteral as its BAML source-level
// literal type spelling: literal_string -> "value", literal_int -> 42,
// literal_bool -> true/false. The string form uses strconv.Quote so
// embedded quotes, backslashes, and non-ASCII characters land
// escape-clean.
func literalSpelling(lit *FuzzLiteral) (string, error) {
	switch lit.Kind {
	case LiteralString:
		return strconv.Quote(lit.String), nil
	case LiteralInt:
		return strconv.FormatInt(lit.Int, 10), nil
	case LiteralBool:
		if lit.Bool {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("unknown literal kind %q", lit.Kind)
	}
}

