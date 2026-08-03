package nativeprompt

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeschema"
)

// This file is the SOURCE-OWNED half of the Slice 7.1b admission proof.
//
// valuecmp_test.go drives the binder and the grammar gate with hand-built
// descriptors, which is the right shape for near-neighbour coverage but leaves
// one gap: a hand-built descriptor can be wrong in the same direction as the
// code under test. Here the descriptors come from the CHECKED-IN .baml source
// of the stock differential fixture, through the production builder:
//
//	testdata/static_oracle/baml_src
//	  -> bamlparser.ParseBytes
//	  -> nativeschema.BuildStaticSchemas + BuildPromptDescriptors
//	  -> promptdescriptor.Function (V3)
//	  -> SupportsStatic / RenderStatic
//
// so the enum names, canonical members, aliases, class field order and list
// element types are the ones the SOURCE states — the same source the stock BAML
// client in internal/nativeprompt/staticoracle is generated from, and the same
// one its byte-exact differential runs against. This suite is pure Go (no CGO,
// no BAML runtime): it proves the native leg reaches the right answers; the
// integration oracle proves those answers are BAML's.

// staticOracleSrcDir is the checked-in .baml project shared with the stock
// differential oracle.
const staticOracleSrcDir = "testdata/static_oracle/baml_src"

// sourceOracleDescriptors builds the fixture's descriptors through the exact
// production build order, asserting zero declines so a regression that turns a
// claimed function into a decline fails here rather than silently shrinking the
// proof.
func sourceOracleDescriptors(t *testing.T) map[string]promptdescriptor.Function {
	t.Helper()
	ents, err := os.ReadDir(staticOracleSrcDir)
	if err != nil {
		t.Fatalf("read %s: %v", staticOracleSrcDir, err)
	}
	var paths []string
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".baml" {
			paths = append(paths, filepath.Join(staticOracleSrcDir, e.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatalf("no .baml files under %s", staticOracleSrcDir)
	}

	var files []nativeschema.SourceFile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		f, perr := bamlparser.ParseBytes(p, data)
		if perr != nil {
			t.Fatalf("ParseBytes %s: %v", p, perr)
		}
		files = append(files, nativeschema.SourceFile{File: f, Path: p})
	}

	schemas, schemaDeclines := nativeschema.BuildStaticSchemas(files)
	descriptors, promptDeclines := nativeschema.BuildPromptDescriptors(
		files, schemas, schemaDeclines,
		map[string]string{"StaticOracleClient": "openai"},
		nativeschema.BuildClientConfigs(files),
	)
	if len(schemaDeclines) != 0 {
		t.Fatalf("unexpected BuildStaticSchemas declines: %v", schemaDeclines)
	}
	if len(promptDeclines) != 0 {
		t.Fatalf("unexpected BuildPromptDescriptors declines: %v", promptDeclines)
	}
	return descriptors
}

// renderSourceFunction renders one fixture function as a completion and returns
// its exact bytes.
func renderSourceFunction(t *testing.T, descriptors map[string]promptdescriptor.Function,
	method string, values []promptdescriptor.ArgumentValue) string {
	t.Helper()
	fn, ok := descriptors[method]
	if !ok {
		t.Fatalf("no descriptor built for %q", method)
	}
	if err := SupportsStatic(fn, values); err != nil {
		t.Fatalf("%s: SupportsStatic declined: %v", method, err)
	}
	rp, err := RenderStatic(fn, values)
	if err != nil {
		t.Fatalf("%s: RenderStatic: %v", method, err)
	}
	if rp.Kind != KindCompletion {
		t.Fatalf("%s: kind = %q, want a completion", method, rp.Kind)
	}
	return rp.Completion
}

// TestSourceOwnedEnumFenceMatchesBAML drives the #597 rows and the admitted
// host-value renders from the SOURCE-BUILT descriptors. The expected strings are
// stock BAML v0.223's answers, and the stock differential
// (internal/nativeprompt/staticoracle) proves them against the real runtime on
// the SAME source; this suite proves the native leg produces them from source
// metadata alone.
func TestSourceOwnedEnumFenceMatchesBAML(t *testing.T) {
	descriptors := sourceOracleDescriptors(t)

	color := func(canonical string) []promptdescriptor.ArgumentValue {
		return vals(argV("color", enumV("Color", canonical)))
	}

	cases := []struct {
		method string
		values []promptdescriptor.ArgumentValue
		want   string
	}{
		// The four historical #597 rows this slice claims, plus the reverse forms.
		// (The fifth, StaticEnumDisplayAliasEq, is declined — see
		// TestSourceOwnedDeliberateDeclines.)
		{"StaticEnumCanonicalEq", nil, "true"},
		{"StaticEnumSameMemberEq", nil, "true"},
		{"StaticEnumCanonicalInMemberList", nil, "true"},
		{"StaticEnumDifferentMemberEq", nil, "false"},
		{"StaticEnumReverseCanonicalEq", nil, "true"},
		{"StaticEnumMemberInCanonicalList", nil, "true"},

		// Enum ARGUMENT comparisons, both operand orders and both answers.
		{"StaticEnumArgMemberEq", color("RED"), "true"},
		{"StaticEnumArgMemberEq", color("BLUE"), "false"},
		{"StaticEnumMemberArgEq", color("RED"), "true"},
		{"StaticEnumArgCanonicalEq", color("RED"), "true"},
		{"StaticEnumArgCanonicalEq", color("GREEN"), "false"},
		{"StaticEnumCanonicalArgEq", color("RED"), "true"},

		// Direct host-value renders: the ALIAS for an aliased member, the
		// CANONICAL name for an unaliased one.
		{"StaticRenderEnum", color("RED"), "rouge"},
		{"StaticRenderEnum", color("GREEN"), "vert"},
		{"StaticRenderEnum", color("BLUE"), "BLUE"},

		// Lists render in INPUT order (not canonical order, not sorted).
		{"StaticRenderList", vals(argV("colors", listV(
			enumV("Color", "BLUE"), enumV("Color", "RED"), enumV("Color", "BLUE")))), "[BLUE, rouge, BLUE]"},
		{"StaticRenderList", vals(argV("colors", listV())), "[]"},
		{"StaticRenderStrings", vals(argV("tags", listV(strV("b"), strV("a")))), `["b", "a"]`},
	}

	for _, c := range cases {
		c := c
		t.Run(c.method+"/"+c.want, func(t *testing.T) {
			if got := renderSourceFunction(t, descriptors, c.method, c.values); got != c.want {
				t.Errorf("%s => %q, want %q (stock BAML v0.223)", c.method, got, c.want)
			}
		})
	}
}

// TestSourceOwnedDeliberateDeclines pins the parity declines on the SAME
// source-built descriptors: the display-alias equality row (whose stock answer
// is `false` and which this slice refuses to claim) and the class renders
// (whose stock field order is not reproducible — see
// internal/nativeprompt/staticoracle's TestStockClassRenderOrderIsNonDeterministic).
func TestSourceOwnedDeliberateDeclines(t *testing.T) {
	descriptors := sourceOracleDescriptors(t)

	palette := vals(argV("palette", classV("Palette",
		fieldV("primary", enumV("Color", "GREEN")),
		fieldV("shades", listV(enumV("Color", "BLUE"))),
		fieldV("swatch", classV("Swatch",
			fieldV("color", enumV("Color", "RED")),
			fieldV("label", strV("hi")))),
		fieldV("name", strV("spring")))))

	cases := []struct {
		method string
		values []promptdescriptor.ArgumentValue
		want   string
	}{
		{"StaticEnumDisplayAliasEq", nil, FeatureEnumComparison},
		{"StaticRenderPalette", palette, FeatureEnumClassValue},
		{"StaticRenderPalettes", vals(argV("palettes", listV(palette[0].Value))), FeatureEnumClassValue},
	}
	for _, c := range cases {
		c := c
		t.Run(c.method, func(t *testing.T) {
			fn, ok := descriptors[c.method]
			if !ok {
				t.Fatalf("no descriptor built for %q", c.method)
			}
			assertStaticDecline(t, fn, c.values, c.want)
		})
	}
}

// TestSourceOwnedUniverseIsResolvedFromSource proves the descriptors really
// carry the SOURCE facts the rows above depend on, so a green suite cannot come
// from an empty universe that happens to render the same bytes.
func TestSourceOwnedUniverseIsResolvedFromSource(t *testing.T) {
	descriptors := sourceOracleDescriptors(t)

	// EVERY function carries the whole project enum set, including the ones that
	// take no enum — BAML installs the namespace globals wholesale.
	for method, fn := range descriptors {
		if len(fn.InputValues.ProjectEnums) != 1 || fn.InputValues.ProjectEnums[0].Name != "Color" {
			t.Fatalf("%s: project enums = %+v, want exactly the source's Color", method, fn.InputValues.ProjectEnums)
		}
	}

	// The Palette closure is transitive and in source DECLARATION order, with
	// resolved field aliases and element types.
	fn := descriptors["StaticRenderPalette"]
	if len(fn.InputValues.Classes) != 2 ||
		fn.InputValues.Classes[0].Name != "Swatch" || fn.InputValues.Classes[1].Name != "Palette" {
		t.Fatalf("class closure = %+v, want [Swatch Palette] in declaration order", fn.InputValues.Classes)
	}
	palette := fn.InputValues.Classes[1]
	wantFields := []string{"primary", "shades", "swatch", "name"}
	for i, want := range wantFields {
		if palette.Fields[i].Canonical != want {
			t.Errorf("Palette field %d = %q, want %q", i, palette.Fields[i].Canonical, want)
		}
	}
	if palette.Fields[0].Alias == nil || *palette.Fields[0].Alias != "principale" {
		t.Errorf("Palette.primary alias = %v, want \"principale\"", palette.Fields[0].Alias)
	}
	if palette.Fields[1].Type.Kind != promptdescriptor.ValueList ||
		palette.Fields[1].Type.Elem == nil ||
		palette.Fields[1].Type.Elem.EnumName != "Color" {
		t.Errorf("Palette.shades type = %+v, want a list of enum Color", palette.Fields[1].Type)
	}
}
