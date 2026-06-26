//go:build integration

package integration

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// TestNativeOutputFormatRendererParity is the #530 differential gate: the
// native Go renderer (internal/schema/outputformat) must reproduce BAML's real
// ctx.output_format byte-for-byte for the dynamic-schema surface.
//
// For each fixture a single *bamlutils.DynamicOutputSchema value drives BOTH
// sides — there is no parallel hand-built fixture that could silently drift:
//
//  1. It is attached to a dynclient.Request whose first (system) message
//     contains ONLY an `{type:"output_format"}` content part, so the upstream
//     prompt's first message text is exactly the rendered output-format block
//     and nothing else.
//  2. dynclient.DynamicCall renders the request through the real BAML runtime
//     and the mock LLM captures the outbound provider body; extractFirstMessageText
//     recovers BAML's ground-truth string. (dynclient defaults to
//     preserve_schema_order = true, so BAML renders properties in OrderedMap
//     insertion order.)
//  3. The same value is lowered with schema.FromDynamicOutputSchema — which
//     consumes the OrderedMap insertion order, matching step 2 — and rendered
//     with outputformat.Render(bundle, outputformat.Options{}) (BAML
//     RenderOptions::default, matching the template's bare {{ ctx.output_format }}).
//
// The byte-exact comparison is the parity criterion; prompt-cache stability
// depends on the exact bytes.
//
// Scope of the dynamic-path fixtures (deliberately narrower than the renderer's
// full capability):
//
//   - Definition order. BAML emits enum/class DEFINITIONS in sorted-name order
//     even though it preserves property/field/value insertion order (dynclient
//     defaults to preserve_schema_order = true). sortDefinitions reorders the
//     enum/class maps to match, so the lowered Bundle's definition order equals
//     BAML's; properties and values keep insertion order on both sides.
//   - Descriptions/aliases. At this BAML version the dynamic TypeBuilder
//     propagates enum-value descriptions and aliases but NOT class-level or
//     field-level descriptions or field aliases. schema.FromDynamicOutputSchema
//     still lowers the latter, so a fixture carrying them would make the Bundle
//     richer than BAML's effective schema and the byte comparison would
//     (correctly) diverge — a fidelity gap in the dynamic lowering, not in the
//     renderer. These fixtures therefore exercise only what the dynamic path
//     represents faithfully; class/field descriptions, field aliases, recursion,
//     class hoisting, and the option knobs are pinned byte-exact by the captured
//     BAML goldens in the outputformat package's unit tests instead.
func TestNativeOutputFormatRendererParity(t *testing.T) {
	dynclientCallGate(t)

	for _, tc := range rendererParityCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			scenarioID := "test-native-renderer-" + tc.name
			opts := setupNonStreamingScenario(t, scenarioID, tc.mockContent)

			// Sort definitions so the lowered Bundle's enum/class order matches
			// BAML's sorted-definition emission; BAML sorts internally regardless
			// of input order, so sending the sorted form keeps both sides aligned.
			outputSchema := sortDefinitions(tc.schema())

			req := dynclient.Request{
				Messages: []dynclient.Message{
					{
						Role: "system",
						PartsContent: []dynclient.ContentPart{
							{Type: "output_format"},
						},
					},
					{Role: "user", TextContent: strPtr("Produce the structured output.")},
				},
				ClientRegistry: dynRegistry(opts.ClientRegistry),
				OutputSchema:   outputSchema,
			}

			client := newDynclient(t)
			// The provider request is built and sent before the LLM response is
			// parsed, so it is captured regardless of whether response parsing
			// against the schema succeeds. A render failure (which would mean no
			// request was sent) surfaces below as a GetLastRequest error.
			if _, err := client.DynamicCall(ctx, req); err != nil {
				t.Logf("DynamicCall returned (response parsing may fail; request still captured): %v", err)
			}

			capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
			if err != nil {
				t.Fatalf("GetLastRequest: %v", err)
			}
			bamlRendered, err := extractFirstMessageText(capturedBody)
			if err != nil {
				t.Fatalf("extractFirstMessageText: %v\ncaptured: %s", err, string(capturedBody))
			}

			bundle, err := schema.FromDynamicOutputSchema(outputSchema, schema.BuildOptions{})
			if err != nil {
				t.Fatalf("FromDynamicOutputSchema: %v", err)
			}
			got, err := outputformat.Render(bundle, outputformat.Options{})
			if err != nil {
				t.Fatalf("outputformat.Render: %v", err)
			}

			if got != bamlRendered {
				t.Errorf("native renderer diverged from BAML ground truth\n--- native ---\n%q\n--- BAML ---\n%q\n\n--- native (raw) ---\n%s\n--- BAML (raw) ---\n%s",
					got, bamlRendered, got, bamlRendered)
			}
		})
	}
}

// rendererParityCase is one differential fixture: a dynamic schema and a mock
// LLM response. The mock content only needs to keep the call from erroring
// before the request is captured; the test asserts on the request, not the
// response.
type rendererParityCase struct {
	name        string
	schema      func() *bamlutils.DynamicOutputSchema
	mockContent string
}

// --- small ordered-map builders --------------------------------------------

func dProps(entries ...bamlutils.OrderedEntry[*bamlutils.DynamicProperty]) bamlutils.OrderedMap[*bamlutils.DynamicProperty] {
	return bamlutils.MustOrderedMap(entries...)
}
func dClasses(entries ...bamlutils.OrderedEntry[*bamlutils.DynamicClass]) bamlutils.OrderedMap[*bamlutils.DynamicClass] {
	return bamlutils.MustOrderedMap(entries...)
}
func dEnums(entries ...bamlutils.OrderedEntry[*bamlutils.DynamicEnum]) bamlutils.OrderedMap[*bamlutils.DynamicEnum] {
	return bamlutils.MustOrderedMap(entries...)
}
func dProp(key string, p *bamlutils.DynamicProperty) bamlutils.OrderedEntry[*bamlutils.DynamicProperty] {
	return bamlutils.OrderedKV(key, p)
}
func dClass(key string, c *bamlutils.DynamicClass) bamlutils.OrderedEntry[*bamlutils.DynamicClass] {
	return bamlutils.OrderedKV(key, c)
}
func dEnum(key string, e *bamlutils.DynamicEnum) bamlutils.OrderedEntry[*bamlutils.DynamicEnum] {
	return bamlutils.OrderedKV(key, e)
}

// sortDefinitions returns a copy of s with the Enums and Classes maps reordered
// by ascending key, mirroring BAML's sorted-definition emission. Properties and
// every nested map (class properties, enum values) are left untouched so their
// insertion order is preserved, matching BAML's preserve_schema_order behaviour.
func sortDefinitions(s *bamlutils.DynamicOutputSchema) *bamlutils.DynamicOutputSchema {
	out := &bamlutils.DynamicOutputSchema{Properties: s.Properties}

	if s.Enums.Len() > 0 {
		keys := s.Enums.Keys()
		sort.Strings(keys)
		entries := make([]bamlutils.OrderedEntry[*bamlutils.DynamicEnum], 0, len(keys))
		for _, k := range keys {
			v, _ := s.Enums.Get(k)
			entries = append(entries, bamlutils.OrderedKV(k, v))
		}
		out.Enums = bamlutils.MustOrderedMap(entries...)
	}

	if s.Classes.Len() > 0 {
		keys := s.Classes.Keys()
		sort.Strings(keys)
		entries := make([]bamlutils.OrderedEntry[*bamlutils.DynamicClass], 0, len(keys))
		for _, k := range keys {
			v, _ := s.Classes.Get(k)
			entries = append(entries, bamlutils.OrderedKV(k, v))
		}
		out.Classes = bamlutils.MustOrderedMap(entries...)
	}

	return out
}

func rendererParityCases() []rendererParityCase {
	return []rendererParityCase{
		{
			name: "flat_primitives",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("answer", &bamlutils.DynamicProperty{Type: "string"}),
						dProp("count", &bamlutils.DynamicProperty{Type: "int"}),
						dProp("score", &bamlutils.DynamicProperty{Type: "float"}),
						dProp("ok", &bamlutils.DynamicProperty{Type: "bool"}),
					),
				}
			},
			mockContent: `{"answer":"hello","count":3,"score":1.5,"ok":true}`,
		},
		{
			// Literal-typed fields plus a bare null, exercising LiteralValue
			// rendering on the dynamic path. Field order is preserved.
			name: "literal_fields",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("kind", &bamlutils.DynamicProperty{Type: "literal_string", Value: "fixed"}),
						dProp("version", &bamlutils.DynamicProperty{Type: "literal_int", Value: 2}),
						dProp("enabled", &bamlutils.DynamicProperty{Type: "literal_bool", Value: true}),
						dProp("nothing", &bamlutils.DynamicProperty{Type: "null"}),
					),
				}
			},
			mockContent: `{"kind":"fixed","version":2,"enabled":true,"nothing":null}`,
		},
		{
			// Class references. The dynamic TypeBuilder does not propagate
			// class/field descriptions, so the fixture carries none; class
			// definitions appear in sorted order (one class here), fields in
			// insertion order.
			name: "nested_class_ref",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("name", &bamlutils.DynamicProperty{Type: "string"}),
						dProp("address", &bamlutils.DynamicProperty{Ref: "Address"}),
						dProp("billing", &bamlutils.DynamicProperty{Ref: "Address"}),
					),
					Classes: dClasses(
						dClass("Address", &bamlutils.DynamicClass{
							Properties: dProps(
								dProp("street", &bamlutils.DynamicProperty{Type: "string"}),
								dProp("city", &bamlutils.DynamicProperty{Type: "string"}),
							),
						}),
					),
				}
			},
			mockContent: `{"name":"n","address":{"street":"s","city":"c"},"billing":{"street":"s","city":"c"}}`,
		},
		{
			name: "lists_and_optional",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}),
						dProp("people", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Ref: "Person"}}),
						dProp("nickname", &bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "string"}}),
					),
					Classes: dClasses(
						dClass("Person", &bamlutils.DynamicClass{
							Properties: dProps(
								dProp("first", &bamlutils.DynamicProperty{Type: "string"}),
								dProp("age", &bamlutils.DynamicProperty{Type: "int"}),
							),
						}),
					),
				}
			},
			mockContent: `{"tags":["a"],"people":[{"first":"f","age":1}],"nickname":null}`,
		},
		{
			name: "enums_inline_and_hoisted",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("status", &bamlutils.DynamicProperty{Ref: "Status"}),
						dProp("priority", &bamlutils.DynamicProperty{Ref: "Priority"}),
						dProp("category", &bamlutils.DynamicProperty{Ref: "Category"}),
					),
					Enums: dEnums(
						// <= 6 values, no descriptions: rendered inline.
						dEnum("Status", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "PENDING"}, {Name: "ACTIVE"}, {Name: "ARCHIVED"}},
						}),
						// > 6 values: hoisted.
						dEnum("Priority", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "P0"}, {Name: "P1"}, {Name: "P2"}, {Name: "P3"},
								{Name: "P4"}, {Name: "P5"}, {Name: "P6"},
							},
						}),
						// value description forces hoisting; also exercises aliases.
						dEnum("Category", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "BUG", Description: "A defect", Alias: "bug"},
								{Name: "FEATURE"},
							},
						}),
					),
				}
			},
			mockContent: `{"status":"ACTIVE","priority":"P1","category":"BUG"}`,
		},
		{
			name: "maps_unions_literals",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("attributes", &bamlutils.DynamicProperty{
							Type:   "map",
							Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
							Values: &bamlutils.DynamicTypeSpec{Type: "string"},
						}),
						dProp("either", &bamlutils.DynamicProperty{
							Type: "union",
							OneOf: []*bamlutils.DynamicTypeSpec{
								{Type: "string"},
								{Type: "int"},
								{Ref: "Address"},
							},
						}),
						dProp("kind", &bamlutils.DynamicProperty{Type: "literal_string", Value: "fixed"}),
					),
					Classes: dClasses(
						dClass("Address", &bamlutils.DynamicClass{
							Properties: dProps(
								dProp("city", &bamlutils.DynamicProperty{Type: "string"}),
							),
						}),
					),
				}
			},
			mockContent: `{"attributes":{"k":"v"},"either":"x","kind":"fixed"}`,
		},
	}
}
