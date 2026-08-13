//go:build integration

package debaml

// The in-memory .baml PROJECT the stock leg is driven against.
//
// Unlike internal/debaml/constraintoracle and internal/debaml/guardledger, this
// oracle checks in NO generated BAML client. The project is a map[string]string
// handed to baml.CreateRuntime at test time and driven through
// BamlRuntime.CallFunctionParse — the same CFFI entry point the generated
// Parse.<Method> wrappers call, one layer down.
//
// WHY THAT MATTERS FOR THE FIXTURE PROTECTIONS. A checked-in generated client can
// go stale against the .baml it was generated from, which is why the sibling
// oracles carry a source-map freshness test. Here the .baml the runtime compiles
// is produced from the SAME schema.Bundle the native leg coerces against, in the
// same process, on every run — there is no second artifact to drift. What replaces
// the freshness check is therefore a stronger claim, not a weaker one:
//
//   - [TestServingOracleProjectDrift] byte-compares the rendered project against a
//     checked-in golden so a corpus change is visible in review, and pins its
//     SHA-256 so the golden cannot be regenerated silently;
//   - [TestServingOracleProjectIsTheOneStockDrivesAndFixtureMethodsExist] proves the
//     bytes the runtime was created from are the bytes the golden pins, and that
//     every fixture's function is present in them and reachable.
//
// SINGLE SOURCE OF TRUTH. Each fixture owns a [schema.Bundle]. The .baml is
// RENDERED from that bundle, so a fixture cannot describe one schema to stock and
// another to native. The renderer is not trusted: if it lowered a type wrongly the
// two legs would see different schemas, and the differential's canonical-value
// comparison fails loudly rather than passing on a shared mistake.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/internal/schema"
)

const (
	// soFunctionPrefix namespaces every generated function so a fixture name can
	// never collide with a BAML keyword or a declaration name.
	soFunctionPrefix = "SO_"

	// soClient is the client every function names. It is never called — the oracle
	// drives the PARSE entry point, not the LLM — but a BAML function must declare
	// one.
	soClient = "ServingOracleClient"

	// soProjectFile is the single .baml file of the in-memory project, and
	// soProjectGolden is the checked-in copy [TestServingOracleProjectDrift]
	// byte-compares it against.
	soProjectFile   = "serving_oracle.baml"
	soProjectGolden = "testdata/serving_oracle/project.baml"

	// soWriteEnv rewrites the golden instead of comparing (see the drift test).
	soWriteEnv = "BAML_SERVING_ORACLE_FIXTURE_WRITE"
)

// soPrelude is the fixed head of the project: the unroutable client.
const soPrelude = `// GENERATED at test time from the Slice 7.2a-3 serving-oracle corpus — do not edit.
//
// Every declaration below is rendered from the schema.Bundle of one corpus
// fixture (serving_oracle_corpus_test.go) by soRenderProject, so the .baml the
// stock BAML v0.223.0 CFFI compiles and the schema.Bundle the native collector
// coerces against are the same description of the same shape.
//
// Regenerate with:
//   BAML_SERVING_ORACLE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml -run TestServingOracleProjectDrift

client<llm> ` + soClient + ` {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://serving-oracle.invalid/v1"
  }
}
`

// soFunctionName is the BAML function that carries one fixture.
func soFunctionName(fixture string) string { return soFunctionPrefix + fixture }

// ---------------------------------------------------------------------------
// schema.Bundle -> .baml source
// ---------------------------------------------------------------------------

// soTypeExpr renders a type in a position where trailing attributes are legal:
// the base type followed by its own @check/@assert attributes.
func soTypeExpr(t schema.Type) string {
	out := soBaseTypeExpr(t)
	for _, c := range t.Meta.Constraints {
		out += " " + soConstraintAttr(c)
	}
	return out
}

// soInnerTypeExpr renders a type in a NESTED position (list element, map key/value,
// union variant), parenthesising exactly when the rendering is more than one type
// token.
//
// BAML v0.223 rejects `map<string, int @check(...)>` outright — a constrained
// nested type must be parenthesised — so this is a grammar requirement rather
// than a style choice; the corpus's map-value and list-element fixtures are what
// prove the parenthesised form is the one that compiles.
func soInnerTypeExpr(t schema.Type) string {
	s := soTypeExpr(t)
	if len(t.Meta.Constraints) > 0 || soIsMultiVariantUnion(t) {
		return "(" + s + ")"
	}
	return s
}

func soIsMultiVariantUnion(t schema.Type) bool {
	return t.Kind == schema.TypeUnion && t.Union != nil && len(t.Union.Variants) > 1
}

func soBaseTypeExpr(t schema.Type) string {
	switch t.Kind {
	case schema.TypePrimitive:
		switch t.Primitive {
		case schema.PrimitiveString:
			return "string"
		case schema.PrimitiveInt:
			return "int"
		case schema.PrimitiveFloat:
			return "float"
		case schema.PrimitiveBool:
			return "bool"
		case schema.PrimitiveNull:
			return "null"
		case schema.PrimitiveMedia:
			return string(t.Media)
		}
		panic("serving oracle: unrenderable primitive " + string(t.Primitive))
	case schema.TypeEnum, schema.TypeClass:
		return t.Name
	case schema.TypeLiteral:
		if t.Literal == nil {
			panic("serving oracle: literal type with no value")
		}
		switch t.Literal.Kind {
		case schema.LiteralString:
			return strconv.Quote(t.Literal.String)
		case schema.LiteralInt:
			return strconv.FormatInt(t.Literal.Int, 10)
		case schema.LiteralBool:
			return strconv.FormatBool(t.Literal.Bool)
		}
		panic("serving oracle: unrenderable literal kind " + string(t.Literal.Kind))
	case schema.TypeList:
		if t.Elem == nil {
			panic("serving oracle: list type with no element")
		}
		return soInnerTypeExpr(*t.Elem) + "[]"
	case schema.TypeMap:
		if t.Key == nil || t.Value == nil {
			panic("serving oracle: map type with no key/value")
		}
		return "map<" + soInnerTypeExpr(*t.Key) + ", " + soInnerTypeExpr(*t.Value) + ">"
	case schema.TypeUnion:
		if t.Union == nil {
			panic("serving oracle: union type with no payload")
		}
		parts := make([]string, len(t.Union.Variants))
		for i, v := range t.Union.Variants {
			parts[i] = soInnerTypeExpr(v)
		}
		joined := strings.Join(parts, " | ")
		if !t.Union.Nullable {
			return joined
		}
		if len(parts) == 1 {
			return parts[0] + "?"
		}
		return "(" + joined + ")?"
	}
	panic("serving oracle: unrenderable type kind " + string(t.Kind))
}

// soConstraintAttr renders one constraint as its BAML attribute. An unlabelled
// @check is not expressible in BAML (the grammar requires the label), so a corpus
// fixture that declares one is a bug rather than a fact about BAML; it panics
// here instead of rendering something the project would reject for an unrelated
// reason.
func soConstraintAttr(c schema.Constraint) string {
	kind := "check"
	if c.Level == schema.ConstraintAssert {
		kind = "assert"
	}
	if c.Label == nil {
		if c.Level == schema.ConstraintCheck {
			panic("serving oracle: @check without a label is not expressible in BAML")
		}
		return fmt.Sprintf("@assert({{ %s }})", c.Expression)
	}
	return fmt.Sprintf("@%s(%s, {{ %s }})", kind, *c.Label, c.Expression)
}

// soRenderClass renders one class declaration.
func soRenderClass(c schema.ClassDef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "class %s {\n", c.Name.Name)
	for _, f := range c.Fields {
		fmt.Fprintf(&b, "  %s %s", f.Name.Name, soBaseTypeExpr(f.Type))
		if f.Name.Alias != nil {
			fmt.Fprintf(&b, " @alias(%s)", strconv.Quote(*f.Name.Alias))
		}
		for _, ct := range f.Type.Meta.Constraints {
			b.WriteString(" " + soConstraintAttr(ct))
		}
		b.WriteString("\n")
	}
	for _, ct := range c.Constraints {
		fmt.Fprintf(&b, "  @@%s\n", strings.TrimPrefix(soConstraintAttr(ct), "@"))
	}
	b.WriteString("}\n")
	return b.String()
}

// soRenderEnum renders one enum declaration.
func soRenderEnum(e schema.EnumDef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "enum %s {\n", e.Name.Name)
	for _, v := range e.Values {
		fmt.Fprintf(&b, "  %s", v.Name.Name)
		if v.Name.Alias != nil {
			fmt.Fprintf(&b, " @alias(%s)", strconv.Quote(*v.Name.Alias))
		}
		b.WriteString("\n")
	}
	for _, ct := range e.Constraints {
		fmt.Fprintf(&b, "  @@%s\n", strings.TrimPrefix(soConstraintAttr(ct), "@"))
	}
	b.WriteString("}\n")
	return b.String()
}

// soRenderFunction renders the function that carries one fixture. The parameter is
// never used — Parse takes the assistant text, not the function arguments — but a
// BAML function must declare one, and the prompt must reference ctx.output_format
// or the project does not describe the return type to the model.
func soRenderFunction(fixture string, target schema.Type) string {
	return fmt.Sprintf("function %s(topic: string) -> %s {\n  client %s\n  prompt #\"{{ topic }} {{ ctx.output_format }}\"#\n}\n",
		soFunctionName(fixture), soTypeExpr(target), soClient)
}

// soDeclaration is one named class/enum declaration contributed by a fixture,
// carried with its source so a collision between two fixtures is reported with
// both renderings rather than silently resolved.
type soDeclaration struct {
	Name    string
	Source  string
	Fixture string
}

// soRenderProject renders the whole in-memory project from the corpus.
//
// Declarations are emitted in FIRST-CONTRIBUTION order, then functions in corpus
// order, so the output is a pure function of the corpus and the drift test can
// byte-compare it. Two fixtures may share a declaration name only if they render
// it identically; anything else is a corpus bug and returns an error rather than
// producing a project whose meaning depends on which fixture was rendered last.
func soRenderProject(fixtures []servingOracleFixture) (string, error) {
	var b strings.Builder
	b.WriteString(soPrelude)

	seen := map[string]soDeclaration{}
	var order []string
	add := func(d soDeclaration) error {
		prev, ok := seen[d.Name]
		if ok {
			if prev.Source != d.Source {
				return fmt.Errorf("declaration %q is rendered differently by fixtures %q and %q:\n--- %s\n%s--- %s\n%s",
					d.Name, prev.Fixture, d.Fixture, prev.Fixture, prev.Source, d.Fixture, d.Source)
			}
			return nil
		}
		seen[d.Name] = d
		order = append(order, d.Name)
		return nil
	}
	for _, f := range fixtures {
		if f.Bundle == nil {
			return "", fmt.Errorf("fixture %q has no bundle", f.Name)
		}
		for _, e := range f.Bundle.Enums {
			if err := add(soDeclaration{Name: e.Name.Name, Source: soRenderEnum(e), Fixture: f.Name}); err != nil {
				return "", err
			}
		}
		for _, c := range f.Bundle.Classes {
			if err := add(soDeclaration{Name: c.Name.Name, Source: soRenderClass(c), Fixture: f.Name}); err != nil {
				return "", err
			}
		}
	}
	for _, name := range order {
		b.WriteString("\n")
		b.WriteString(seen[name].Source)
	}
	for _, f := range fixtures {
		b.WriteString("\n")
		b.WriteString(soRenderFunction(f.Name, f.Bundle.Target))
	}
	// PROBES are rendered into the same project but are not corpus rows: they exist
	// so a named test can drive a shape the differential must NOT contain (a root
	// duplicate label, whose folded readback is deliberately lossy). Keeping them in
	// the project means the drift golden covers them too.
	for _, pr := range servingOracleProbes {
		for _, c := range pr.Bundle.Classes {
			if err := add(soDeclaration{Name: c.Name.Name, Source: soRenderClass(c), Fixture: "probe:" + pr.Name}); err != nil {
				return "", err
			}
			b.WriteString("\n")
			b.WriteString(seen[c.Name.Name].Source)
		}
		b.WriteString("\n")
		b.WriteString(soRenderFunction(pr.Name, pr.Bundle.Target))
	}
	return b.String(), nil
}

// servingOracleProbe is a project function driven only by a named test.
type servingOracleProbe struct {
	Name   string
	Doc    string
	Bundle *schema.Bundle
	Raw    string
}

// method is the generated BAML function for this probe.
func (p servingOracleProbe) method() string { return soFunctionName(p.Name) }

// soProjectFiles is the in-memory project handed to baml.CreateRuntime.
func soProjectFiles(src string) map[string]string {
	return map[string]string{soProjectFile: src}
}

// soProjectHash is the SHA-256 of the rendered project, in hex. The drift test
// pins it next to the golden so regenerating the golden without acknowledging the
// change is not possible.
func soProjectHash(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// soEnvSnapshot is the environment the runtime is created with, mirroring the
// generated client's getEnvVars. The fixture project's client is unroutable and
// no LLM call is ever made, so nothing here is read; it is passed for parity with
// the real serving path rather than for effect.
func soEnvSnapshot() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		env[k] = v
	}
	return env
}

// soSortedNames returns the fixture names, sorted, for a stable failure message.
func soSortedNames(fixtures []servingOracleFixture) []string {
	out := make([]string, 0, len(fixtures))
	for _, f := range fixtures {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}
