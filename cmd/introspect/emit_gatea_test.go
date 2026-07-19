package main

// Gate A — pure-Go representation proof for the de-BAML Phase 8A static prompt
// descriptor emitter (emitStaticPromptDescriptors + promptEmitter). It exercises
// every exported arm of the descriptor model with a synthetic corpus and proves:
//
//   - deterministic emission: emitting the same content from two differently
//     ordered maps yields byte-identical Go source, and the method keys of BOTH
//     the descriptor-factory map and the decline map are emitted in sorted order,
//     with the two partitions disjoint;
//   - the emitted package COMPILES;
//   - fidelity: each factory's returned value equals the original synthetic
//     descriptor, verified against an INDEPENDENT, LOSSLESS presence+value
//     snapshot — a reflection walk (no serialization) that authoritatively records
//     nil-vs-present-empty slice/pointer distinctions and exact scalar bytes.
//     encoding/json is deliberately NOT used for presence: its `omitempty` drops
//     an explicitly-present empty slice and remarshals it as nil;
//   - freshness + mutation isolation: calling a factory twice yields equal
//     values, and recursively mutating one returned value does not poison a later
//     call.
//
// The compile/fidelity/freshness/mutation legs run a generated harness in a
// subprocess `go test` against the emitted package; they are skipped under
// -short (they invoke the Go toolchain).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// gateAInterfacesPkg is the bamlutils import root the synthetic emission resolves
// promptdescriptor / bamlparser / schemadescriptor against.
const gateAInterfacesPkg = "github.com/invakid404/baml-rest/bamlutils"

func sp(s string) *string { return &s }

// syntheticWeirdString packs Unicode, quotes, backslashes, newline/tab/CR, NUL,
// ESC, a BOM, and a backtick into one valid-UTF-8 string so the string-literal
// emission (jen.Lit) is stressed byte-for-byte.
const syntheticWeirdString = "A\"q\"\\b\n\tαβ☕\x00\x1b\r\ufeff`end{{ x }}"

// buildSyntheticCorpus returns a descriptor set exercising every exported arm of
// the model plus nil-vs-empty edge cases, and THREE declines (disjoint from the
// descriptors, deliberately NOT in sorted order) so the partition and
// decline-ordering proofs are non-vacuous.
func buildSyntheticCorpus() (map[string]promptdescriptor.Function, map[string]string) {
	// A recursive bamlparser.TypeExpr reaching every kind, with a field `@` and a
	// block `@@` attribute (Attribute.Block true and false), Args covering every
	// bamlparser.Value shape, spans, and the flag fields.
	primArg := &bamlparser.TypeExpr{
		Kind: bamlparser.KindList, Dims: 2,
		Elem: &bamlparser.TypeExpr{
			Kind: bamlparser.KindMap,
			Key:  &bamlparser.TypeExpr{Kind: bamlparser.KindPrimitive, Primitive: "string"},
			Value: &bamlparser.TypeExpr{
				Kind:     bamlparser.KindUnion,
				Nullable: true,
				Variants: []*bamlparser.TypeExpr{
					{Kind: bamlparser.KindPrimitive, Primitive: "int"},
					{Kind: bamlparser.KindMedia, Media: "image"},
					{Kind: bamlparser.KindLiteral, LiteralKind: "string", LiteralValue: syntheticWeirdString},
					{Kind: bamlparser.KindTuple, Items: []*bamlparser.TypeExpr{
						{Kind: bamlparser.KindGroup, Parenthesized: true, Inner: &bamlparser.TypeExpr{Kind: bamlparser.KindPrimitive, Primitive: "bool"}},
						{Kind: bamlparser.KindUnsupported, Reason: "synthetic unsupported arm"},
					}},
					{
						Kind: bamlparser.KindNameRef, Name: "a::b", Namespaced: true,
						Attributes: []*bamlparser.Attribute{
							{
								// Field attribute (`@check(...)`): Block == false, every Value shape.
								Name: "check", Block: false, HasParens: true, Parenthesized: true,
								RawArgs: `x, {{ this|length > 0 }}`,
								Args: []*bamlparser.Value{
									{Literal: sp("lit")},
									{EnvRef: sp("HOME")},
									{Ident: sp("ident")},
									{Number: sp("42")},
									{Raw: sp("raw#body")},
									{List: []*bamlparser.Value{{Literal: sp("a")}, {Number: sp("1")}}},
								},
								Span: bamlparser.Span{Start: 1, End: 9, Line: 2, Col: 3},
							},
							{
								// Block attribute (`@@dynamic`): exercises Attribute.Block == true.
								Name: "dynamic", Block: true, HasParens: false,
								Span: bamlparser.Span{Start: 30, End: 40, Line: 5, Col: 1},
							},
						},
						Span: bamlparser.Span{Start: 10, End: 20, Line: 3, Col: 4},
					},
					{Kind: bamlparser.KindNameRef, Name: "a.b", Path: true},
				},
			},
		},
		Span: bamlparser.Span{Start: 100, End: 200, Line: 10, Col: 5},
	}

	// A schemadescriptor.Bundle reaching every Type kind, enum/class/alias defs,
	// class-level constraints, constraint/streaming metadata, media, and every
	// literal kind.
	bundle := schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "AllArms",
		Stream:  false,
		Target: schemadescriptor.Type{
			Kind: schemadescriptor.TypeUnion,
			Meta: schemadescriptor.TypeMeta{
				Constraints: []schemadescriptor.Constraint{
					{Level: schemadescriptor.ConstraintCheck, Expression: "this|length > 0", Label: sp("nonempty")},
					{Level: schemadescriptor.ConstraintAssert, Expression: "true"},
				},
				Stream: schemadescriptor.StreamingBehavior{Needed: true, Done: true, State: true},
			},
			Union: &schemadescriptor.UnionType{
				Nullable: true,
				Variants: []schemadescriptor.Type{
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveFloat},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveBool},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveNull},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveMedia, Media: schemadescriptor.MediaImage},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveMedia, Media: schemadescriptor.MediaAudio},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveMedia, Media: schemadescriptor.MediaPDF},
					{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveMedia, Media: schemadescriptor.MediaVideo},
					{Kind: schemadescriptor.TypeTop},
					{Kind: schemadescriptor.TypeEnum, Name: "Color", Dynamic: true},
					{Kind: schemadescriptor.TypeClass, Name: "Person", Mode: schemadescriptor.NonStreaming},
					{Kind: schemadescriptor.TypeLiteral, Literal: &schemadescriptor.LiteralValue{Kind: schemadescriptor.LiteralString, String: syntheticWeirdString}},
					{Kind: schemadescriptor.TypeLiteral, Literal: &schemadescriptor.LiteralValue{Kind: schemadescriptor.LiteralInt, Int: -9223372036854775808}},
					{Kind: schemadescriptor.TypeLiteral, Literal: &schemadescriptor.LiteralValue{Kind: schemadescriptor.LiteralBool, Bool: true}},
					{Kind: schemadescriptor.TypeList, Elem: &schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
					{Kind: schemadescriptor.TypeMap,
						Key:   &schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString},
						Value: &schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}},
					{Kind: schemadescriptor.TypeRecursiveAlias, Name: "JSON", Mode: schemadescriptor.Streaming},
					{Kind: schemadescriptor.TypeTuple, Items: []schemadescriptor.Type{{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}}},
					{Kind: schemadescriptor.TypeArrow, Arrow: &schemadescriptor.ArrowType{
						Params: []schemadescriptor.Type{{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}},
						Return: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveBool},
					}},
				},
			},
		},
		Enums: []schemadescriptor.EnumDef{{
			Name:        schemadescriptor.Name{Name: "Color", Alias: sp("Colour")},
			Values:      []schemadescriptor.EnumValue{{Name: schemadescriptor.Name{Name: "RED"}, Description: sp("the red one")}, {Name: schemadescriptor.Name{Name: "GREEN"}}},
			Constraints: []schemadescriptor.Constraint{{Level: schemadescriptor.ConstraintCheck, Expression: "e"}},
		}},
		Classes: []schemadescriptor.ClassDef{{
			Name:        schemadescriptor.Name{Name: "Person"},
			Description: sp("a person"),
			Mode:        schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "name", Alias: sp("full_name")}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}, Description: sp("the name"), StreamingNeeded: true},
				{Name: schemadescriptor.Name{Name: "age"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}},
			},
			// Class-level constraints (previously untested).
			Constraints: []schemadescriptor.Constraint{{Level: schemadescriptor.ConstraintAssert, Expression: "this.age >= 0", Label: sp("age_nonneg")}},
			Stream:      schemadescriptor.StreamingBehavior{Needed: true},
		}},
		RecursiveClasses:           []string{"Person"},
		StructuralRecursiveAliases: []schemadescriptor.RecursiveAliasDef{{Name: "JSON", Target: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}}},
	}

	allArms := promptdescriptor.Function{
		Version:  promptdescriptor.Version,
		Method:   "AllArms",
		Prompt:   syntheticWeirdString,
		Args:     []promptdescriptor.Argument{{Name: "topic", Type: primArg}, {Name: "bare"}},
		Client:   "SynthClient",
		Provider: "openai",
		Return:   bundle,
		Macros: []promptdescriptor.TemplateString{{
			Name:       "macro",
			Args:       []promptdescriptor.Argument{{Name: "m", Type: &bamlparser.TypeExpr{Kind: bamlparser.KindPrimitive, Primitive: "string"}}},
			Body:       "hello {{ m }}" + syntheticWeirdString,
			SourcePath: "macros.baml",
		}},
		ClientConfig: promptdescriptor.ClientConfig{
			Present:            true,
			Name:               "SynthClient",
			Provider:           "openai",
			Model:              promptdescriptor.ClientModel{Value: "gpt-4o", Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: true},
			RequestBodyPresent: true,
			RequestBody: []promptdescriptor.RequestBodyEntry{
				{Key: "top_p", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionNumber, Number: "0.9"}},
				{Key: "flag", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionBool, Bool: true}},
				{Key: "ident", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionIdent, String: "auto"}},
				{Key: "secret", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionEnv, String: "OPENAI_KEY"}},
				{Key: "stop", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionList, List: []promptdescriptor.OptionValue{
					{Kind: promptdescriptor.OptionString, String: "a"},
					{Kind: promptdescriptor.OptionString, String: "b"},
				}}},
				{Key: "meta", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionObject, Object: []promptdescriptor.RequestBodyEntry{
					{Key: "tag", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: "x"}},
					{Key: "rank", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionNumber, Number: "1"}},
				}}},
			},
			TransportOptions:     []promptdescriptor.ClientOption{{Key: "base_url", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: "https://synth.invalid/v1"}}},
			BodyAffectingOptions: []promptdescriptor.ClientOption{{Key: "temperature", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionNumber, Number: "0.2"}}},
		},
	}

	// Streaming Return bundle (Bundle.Stream == true, previously untested).
	streamVariant := promptdescriptor.Function{
		Version:  promptdescriptor.Version,
		Method:   "StreamVariant",
		Prompt:   "p",
		Client:   "SynthClient",
		Provider: "openai",
		Return: schemadescriptor.Bundle{
			Version: schemadescriptor.Version,
			Method:  "StreamVariant",
			Stream:  true,
			Target:  schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString},
		},
		ClientConfig: promptdescriptor.ClientConfig{Present: true, Name: "SynthClient", Provider: "openai", Model: promptdescriptor.ClientModel{Value: "gpt-4o", Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: true}},
	}

	// Regular (non-raw) literal model: ModelProvenanceLiteral with RawString ==
	// false and an escape-bearing value retained verbatim (previously untested).
	regularModel := promptdescriptor.Function{
		Version:  promptdescriptor.Version,
		Method:   "RegularModel",
		Prompt:   "p",
		Client:   "RegClient",
		Provider: "openai",
		Return:   schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "RegularModel", Target: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
		ClientConfig: promptdescriptor.ClientConfig{
			Present: true, Name: "RegClient", Provider: "openai",
			Model: promptdescriptor.ClientModel{Value: `gpt\t4`, Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: false},
		},
	}

	// Edge cases: nil-vs-empty distinctions and the ModelProvenanceEnv model. The
	// empty-but-present slices here are on fields the emitter preserves verbatim
	// (direct `if != nil`, NOT collapsed by an enclosing IsZero gate), so they must
	// round-trip as []T{} — distinct from nil.
	emptyEdges := promptdescriptor.Function{
		Version:  promptdescriptor.Version,
		Method:   "EmptyEdges",
		Prompt:   "",                            // empty string
		Args:     []promptdescriptor.Argument{}, // empty-but-present slice
		Client:   "EnvClient",
		Provider: "openai",
		Return: schemadescriptor.Bundle{
			Version: schemadescriptor.Version,
			Method:  "EmptyEdges",
			Target: schemadescriptor.Type{
				Kind: schemadescriptor.TypeTuple,
				// Present-empty Constraints INSIDE the IsZero-gated TypeMeta: exercises
				// the schemaTypeDict Meta guard, which must NOT collapse this
				// present-empty slice to nil (TypeMeta.IsZero() reports true for a
				// non-nil empty Constraints). Regression guard for that emitter fix.
				Meta:  schemadescriptor.TypeMeta{Constraints: []schemadescriptor.Constraint{}},
				Items: []schemadescriptor.Type{}, // empty-but-present (omitempty) slice
			},
			Enums:                      []schemadescriptor.EnumDef{},           // empty-but-present (omitempty)
			Classes:                    []schemadescriptor.ClassDef{},          // empty-but-present (omitempty)
			RecursiveClasses:           []string{},                             // empty-but-present (omitempty)
			StructuralRecursiveAliases: []schemadescriptor.RecursiveAliasDef{}, // empty-but-present (omitempty)
		},
		Macros: nil, // nil slice distinct from empty
		ClientConfig: promptdescriptor.ClientConfig{
			Present:              true,
			Name:                 "EnvClient",
			Provider:             "openai",
			Model:                promptdescriptor.ClientModel{Provenance: promptdescriptor.ModelProvenanceEnv, EnvVar: "MODEL_NAME"},
			RequestBodyPresent:   true,
			RequestBody:          []promptdescriptor.RequestBodyEntry{}, // empty-but-present request_body {}
			TransportOptions:     []promptdescriptor.ClientOption{},     // empty-but-present
			BodyAffectingOptions: []promptdescriptor.ClientOption{},     // empty-but-present
		},
	}

	dynModel := promptdescriptor.Function{
		Version:      promptdescriptor.Version,
		Method:       "DynModel",
		Prompt:       "p",
		Client:       "DynClient",
		Provider:     "openai",
		Return:       schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "DynModel", Target: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
		ClientConfig: promptdescriptor.ClientConfig{Present: true, Name: "DynClient", Provider: "openai", Model: promptdescriptor.ClientModel{Provenance: promptdescriptor.ModelProvenanceDynamic}},
	}

	descriptors := map[string]promptdescriptor.Function{
		"AllArms":       allArms,
		"EmptyEdges":    emptyEdges,
		"DynModel":      dynModel,
		"StreamVariant": streamVariant,
		"RegularModel":  regularModel,
	}
	// Three declines, deliberately NOT in sorted order and disjoint from the
	// descriptors, so the decline-ordering and disjointness proofs are non-vacuous.
	declines := map[string]string{
		"ZDeclinedMethod": "synthetic decline reason z: reachable at-at-dynamic",
		"ADeclinedMethod": "synthetic decline reason a: return bundle unavailable",
		"MDeclinedMethod": "synthetic decline reason m: unresolvable macro-arg type graph",
	}
	return descriptors, declines
}

// emitCorpus renders the emitted introspected package for a corpus into a string.
func emitCorpus(t *testing.T, descriptors map[string]promptdescriptor.Function, declines map[string]string) string {
	t.Helper()
	out := jen.NewFile("introspected")
	emitStaticPromptDescriptors(out, &config{InterfacesPkg: gateAInterfacesPkg}, descriptors, declines)
	var b strings.Builder
	if err := out.Render(&b); err != nil {
		t.Fatalf("render emitted package: %v", err)
	}
	return b.String()
}

func TestGateADeterministicEmission(t *testing.T) {
	descriptors, declines := buildSyntheticCorpus()

	first := emitCorpus(t, descriptors, declines)

	// Rebuild the maps as fresh, independently-iterated copies; Go map iteration
	// order is randomized per range, so a differing iteration order is exercised.
	// Emission must be byte-identical because keys are sorted and jen renders
	// struct-literal fields in sorted order.
	second := emitCorpus(t, copyFnMap(descriptors), copyStrMap(declines))

	if first != second {
		t.Fatalf("emission is not deterministic: two renders of the same corpus differ (lengths %d vs %d)", len(first), len(second))
	}

	// Method keys emitted in sorted order in the descriptor factory map.
	assertSortedKeys(t, first, `"([A-Za-z0-9_]+)": func\(\) promptdescriptor\.Function`, "StaticPromptDescriptors")

	// Decline keys emitted in sorted order (non-vacuous: 3 declines inserted
	// unsorted). Scope the key scan to the StaticPromptDeclines literal so a
	// prompt/reason substring can never be mistaken for a key.
	assertSortedKeys(t, declineLiteral(t, first), `"([A-Za-z0-9_]+)":`, "StaticPromptDeclines")

	// Disjoint partition: no decline method appears in the descriptor factory map.
	for method := range declines {
		if strings.Contains(first, `"`+method+`": func() promptdescriptor.Function`) {
			t.Errorf("decline %q leaked into the descriptor factory map", method)
		}
	}
}

// declineLiteral returns the body between the braces of the emitted
// `StaticPromptDeclines = map[string]string{ ... }` literal. Decline reasons
// carry no `{`/`}`, so the first `}` after the opener closes the map.
func declineLiteral(t *testing.T, src string) string {
	t.Helper()
	const open = "StaticPromptDeclines = map[string]string{"
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("StaticPromptDeclines literal not found in emitted output")
	}
	rest := src[i+len(open):]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatalf("StaticPromptDeclines literal is unterminated")
	}
	return rest[:j]
}

func assertSortedKeys(t *testing.T, src, pattern, label string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var got []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		got = append(got, m[1])
	}
	if len(got) == 0 {
		t.Fatalf("%s: no keys matched %q", label, pattern)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("%s: keys not strictly sorted in source: %v", label, got)
		}
	}
}

func copyFnMap(m map[string]promptdescriptor.Function) map[string]promptdescriptor.Function {
	out := make(map[string]promptdescriptor.Function, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestGateACompileFreshnessMutation emits the synthetic corpus, then compiles it
// and runs a harness that checks fidelity (vs an independent lossless presence
// snapshot), freshness, and mutation isolation.
func TestGateACompileFreshnessMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess compile/freshness/mutation harness in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	descriptors, _ := buildSyntheticCorpus()

	repoRoot := gateARepoRoot(t)

	// Emit into an OS temp dir OUTSIDE the repo tree (t.TempDir auto-removes it).
	// Writing under the repo — even under a `testdata` dir — is unsafe: `go build
	// ./...` skips testdata for PACKAGE selection, but cmd/embed enumerates the
	// FILESYSTEM and would embed a leftover throwaway package, rewriting the root
	// //go:embed directive (embed.go MUST stay byte-identical to base). A temp dir
	// keeps `go test ./...` from leaving anything under any embedded path.
	//
	// The emitted package imports bamlutils packages, so synthesize a minimal
	// module here with a replace to the local bamlutils and reuse the repo go.sum
	// (a superset of bamlutils's transitive-dep hashes). GOPROXY=off + GOSUMDB=off
	// keep the build fully offline from the populated module cache.
	dir := t.TempDir()

	out := jen.NewFile("introspected")
	emitStaticPromptDescriptors(out, &config{InterfacesPkg: gateAInterfacesPkg}, descriptors, nil)
	if err := out.Save(filepath.Join(dir, "introspected.go")); err != nil {
		t.Fatalf("save emitted package: %v", err)
	}

	// Independent LOSSLESS oracle: a reflection presence+value snapshot of the
	// ORIGINAL corpus (computed WITHOUT jen and WITHOUT serialization, so it
	// authoritatively records nil-vs-present-empty slices/pointers and exact
	// scalar bytes). The harness recomputes the identical snapshot over the emitted
	// factory output and must match. presenceSnapshot MUST stay behavior-identical
	// to the copy embedded in gateAHarness; any drift causes a visible mismatch,
	// never a silent pass.
	if err := os.WriteFile(filepath.Join(dir, "presence.txt"), []byte(presenceSnapshot(descriptors)), 0o644); err != nil {
		t.Fatalf("write presence.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness_test.go"), []byte(gateAHarness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	// Synthesize the module context for the throwaway package.
	bamlutilsAbs := filepath.Join(repoRoot, "bamlutils")
	goMod := fmt.Sprintf("module gateaharness\n\ngo %s\n\nrequire %s v0.0.0\n\nreplace %s => %s\n",
		gateAGoVersion(t, repoRoot), gateAInterfacesPkg, gateAInterfacesPkg, filepath.ToSlash(bamlutilsAbs))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("write go.sum: %v", err)
		}
	}

	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOWORK=off",        // temp module, not the repo workspace
		"GOFLAGS=-mod=mod",  // let go add bamlutils's indirect requires from cache
		"GOPROXY=off",       // cache-only: no network
		"GOSUMDB=off",       // no checksum-DB lookup
		"GOTOOLCHAIN=local", // never download a toolchain
	)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("emitted-package harness failed: %v\n%s", err, outBytes)
	}
}

// gateAGoVersion returns the `go` directive version from the repo root go.mod so
// the synthesized temp module matches the toolchain.
func gateAGoVersion(t *testing.T, repoRoot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read root go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "go ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "go "))
		}
	}
	return "1.26"
}

func gateARepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = <repo>/cmd/introspect/emit_gatea_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// presenceSnapshot / gateASnap: the parent copy of the lossless oracle. Keep
// byte-identical in behavior to the copy embedded in gateAHarness.
func presenceSnapshot(v any) string {
	var b strings.Builder
	gateASnap(&b, "", reflect.ValueOf(v))
	return b.String()
}

func gateASnap(b *strings.Builder, path string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			fmt.Fprintf(b, "%s=iface:nil\n", path)
			return
		}
		gateASnap(b, path, v.Elem())
	case reflect.Map:
		keys := make([]string, 0, v.Len())
		km := make(map[string]reflect.Value, v.Len())
		for _, k := range v.MapKeys() {
			ks := fmt.Sprint(k.Interface())
			keys = append(keys, ks)
			km[ks] = k
		}
		sort.Strings(keys)
		fmt.Fprintf(b, "%s=map:len=%d\n", path, len(keys))
		for _, ks := range keys {
			gateASnap(b, path+"["+ks+"]", v.MapIndex(km[ks]))
		}
	case reflect.Ptr:
		if v.IsNil() {
			fmt.Fprintf(b, "%s=ptr:nil\n", path)
			return
		}
		fmt.Fprintf(b, "%s=ptr:set\n", path)
		gateASnap(b, path+"*", v.Elem())
	case reflect.Slice:
		if v.IsNil() {
			fmt.Fprintf(b, "%s=slice:nil\n", path)
			return
		}
		fmt.Fprintf(b, "%s=slice:len=%d\n", path, v.Len())
		for i := 0; i < v.Len(); i++ {
			gateASnap(b, fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
	case reflect.Struct:
		tp := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if tp.Field(i).PkgPath != "" {
				continue // skip unexported (e.g. bamlparser.Attribute scratch)
			}
			gateASnap(b, path+"."+tp.Field(i).Name, v.Field(i))
		}
	case reflect.String:
		fmt.Fprintf(b, "%s=str:%s\n", path, strconv.Quote(v.String()))
	case reflect.Bool:
		fmt.Fprintf(b, "%s=bool:%v\n", path, v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fmt.Fprintf(b, "%s=int:%d\n", path, v.Int())
	default:
		fmt.Fprintf(b, "%s=other:%v\n", path, v.Interface())
	}
}

// gateAHarness is the generated-package test. It lives in the emitted package
// (package introspected) so it references the emitted symbols directly. It never
// prints raw prompt/client-literal VALUES on mismatch (only field paths + the
// structural marker; string values are length-redacted), matching the Phase 8A
// security posture. Its presenceSnapshot/gateASnap MUST match the parent copy.
const gateAHarness = `package introspected

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

func factoryMap() map[string]promptdescriptor.Function {
	m := make(map[string]promptdescriptor.Function, len(StaticPromptDescriptors))
	for k, f := range StaticPromptDescriptors {
		m[k] = f()
	}
	return m
}

func TestEmittedFidelity(t *testing.T) {
	want, err := os.ReadFile("presence.txt")
	if err != nil {
		t.Fatalf("read presence.txt: %v", err)
	}
	got := presenceSnapshot(factoryMap())
	if string(want) != got {
		t.Errorf("emitted presence/value snapshot != independent oracle: %s", firstSnapshotDiff(string(want), got))
	}
}

func TestFreshnessAndMutationIsolation(t *testing.T) {
	for method, factory := range StaticPromptDescriptors {
		a := factory()
		b := factory()
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("two factory calls for %q differ", method)
		}
		deepMutate(reflect.ValueOf(&a).Elem())
		c := factory()
		if !reflect.DeepEqual(b, c) {
			t.Fatalf("mutating one factory result poisoned a later call for %q", method)
		}
	}
}

func TestPartition(t *testing.T) {
	for method := range StaticPromptDescriptors {
		if _, dup := StaticPromptDeclines[method]; dup {
			t.Errorf("method %q in both descriptor and decline maps", method)
		}
		if fn, ok := StaticPromptDescriptor(method); !ok || fn.Method == "" {
			t.Errorf("accessor missed present method %q", method)
		}
	}
	if _, ok := StaticPromptDescriptor("definitely-absent"); ok {
		t.Error("accessor manufactured a descriptor for an absent method")
	}
}

func deepMutate(v reflect.Value) {
	switch v.Kind() {
	case reflect.Ptr:
		if !v.IsNil() {
			deepMutate(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.CanSet() {
				deepMutate(f)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			deepMutate(v.Index(i))
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(v.String() + "~MUT")
		}
	case reflect.Bool:
		if v.CanSet() {
			v.SetBool(!v.Bool())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.CanSet() {
			v.SetInt(v.Int() + 1)
		}
	}
}

// firstSnapshotDiff reports the first differing snapshot line by field path +
// structural marker, redacting any string VALUE to its length.
func firstSnapshotDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	n := len(wl)
	if len(gl) < n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			return fmt.Sprintf("line %d: want %q got %q", i+1, redactSnapLine(wl[i]), redactSnapLine(gl[i]))
		}
	}
	if len(wl) != len(gl) {
		return fmt.Sprintf("snapshot line count differs: want %d got %d", len(wl), len(gl))
	}
	return "no line diff"
}

func redactSnapLine(line string) string {
	if i := strings.Index(line, "=str:"); i >= 0 {
		return line[:i] + fmt.Sprintf("=str:len=%d", len(line)-(i+len("=str:")))
	}
	return line
}

func presenceSnapshot(v any) string {
	var b strings.Builder
	gateASnap(&b, "", reflect.ValueOf(v))
	return b.String()
}

func gateASnap(b *strings.Builder, path string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			fmt.Fprintf(b, "%s=iface:nil\n", path)
			return
		}
		gateASnap(b, path, v.Elem())
	case reflect.Map:
		keys := make([]string, 0, v.Len())
		km := make(map[string]reflect.Value, v.Len())
		for _, k := range v.MapKeys() {
			ks := fmt.Sprint(k.Interface())
			keys = append(keys, ks)
			km[ks] = k
		}
		sort.Strings(keys)
		fmt.Fprintf(b, "%s=map:len=%d\n", path, len(keys))
		for _, ks := range keys {
			gateASnap(b, path+"["+ks+"]", v.MapIndex(km[ks]))
		}
	case reflect.Ptr:
		if v.IsNil() {
			fmt.Fprintf(b, "%s=ptr:nil\n", path)
			return
		}
		fmt.Fprintf(b, "%s=ptr:set\n", path)
		gateASnap(b, path+"*", v.Elem())
	case reflect.Slice:
		if v.IsNil() {
			fmt.Fprintf(b, "%s=slice:nil\n", path)
			return
		}
		fmt.Fprintf(b, "%s=slice:len=%d\n", path, v.Len())
		for i := 0; i < v.Len(); i++ {
			gateASnap(b, fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
	case reflect.Struct:
		tp := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if tp.Field(i).PkgPath != "" {
				continue
			}
			gateASnap(b, path+"."+tp.Field(i).Name, v.Field(i))
		}
	case reflect.String:
		fmt.Fprintf(b, "%s=str:%s\n", path, strconv.Quote(v.String()))
	case reflect.Bool:
		fmt.Fprintf(b, "%s=bool:%v\n", path, v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fmt.Fprintf(b, "%s=int:%d\n", path, v.Int())
	default:
		fmt.Fprintf(b, "%s=other:%v\n", path, v.Interface())
	}
}
`
