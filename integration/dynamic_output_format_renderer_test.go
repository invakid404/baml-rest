//go:build integration

package integration

import (
	"context"
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
// depends on the exact bytes. The fixtures are fed to BOTH sides in their
// ORIGINAL order — no normalization — so the harness reflects production
// exactly and cannot mask an ordering divergence.
//
// Scope of the dynamic-path fixtures (deliberately narrower than the renderer's
// full capability), and the known dynamic-path fidelity gap they steer around —
// in the Bundle-construction layer, NOT the renderer (which is pinned byte-exact
// against BAML's own static render tests by the outputformat package's 40+
// goldens):
//
//   - Multi-definition order is now COVERED (#534). BAML does NOT name-sort
//     definitions: it discovers reachable enums/classes from the target via a
//     LIFO stack (baml-runtime relevant_data_models), so the rendered definition
//     order is reverse-of-reference (DFS) order. schema.FromDynamicOutputSchema
//     reproduces that traversal, so two-plus hoisted definitions now match
//     byte-exact — e.g. fields referencing enums First then Second render the
//     Second block before the First block on BOTH sides. The two_hoisted_enums_*,
//     hoisted_enums_containers, and mixed_reachable_graph fixtures below pin this:
//     enums are declared in the OPPOSITE order to their reference order to prove
//     declared (OrderedMap) order does not drive the output. (Non-recursive class
//     definition order under hoist_classes=true is not observable through the
//     production bare {{ ctx.output_format }} content part, which never hoists
//     non-recursive classes; that order is pinned by the native-render HoistAll
//     unit fixtures in internal/schema and the renderer's static goldens. A real
//     BAML hoist_classes=true oracle for the dynamic path is a follow-up — see
//     the note in internal/schema/order.go and the #534 PR.)
//   - Descriptions/aliases. At this BAML version the dynamic TypeBuilder
//     propagates enum-value descriptions and aliases but NOT class-level or
//     field-level descriptions or field aliases. schema.FromDynamicOutputSchema
//     still lowers the latter, so a fixture carrying them would make the Bundle
//     richer than BAML's effective schema. These fixtures therefore carry no
//     class/field descriptions or field aliases; that rendering, plus recursion,
//     class hoisting, and the option knobs, is pinned byte-exact by the captured
//     BAML goldens in the outputformat package instead.
func TestNativeOutputFormatRendererParity(t *testing.T) {
	dynclientCallGate(t)

	for _, tc := range rendererParityCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			scenarioID := "test-native-renderer-" + tc.name
			opts := setupNonStreamingScenario(t, scenarioID, tc.mockContent)

			// Original fixture order, exactly as production sends it. No
			// normalization: BAML's TypeBuilder receives this order and the
			// lowered Bundle is built from the same schema, so any definition
			// order divergence is surfaced rather than masked.
			outputSchema := tc.schema()

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
			// One inline enum (<= 6 values, no descriptions) and one hoisted
			// enum (a value description forces hoisting; also exercises an enum
			// value alias).
			name: "enum_inline_and_hoisted",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("status", &bamlutils.DynamicProperty{Ref: "Status"}),
						dProp("label", &bamlutils.DynamicProperty{Ref: "Category"}),
					),
					Enums: dEnums(
						dEnum("Status", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "PENDING"}, {Name: "ACTIVE"}, {Name: "ARCHIVED"}},
						}),
						dEnum("Category", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "BUG", Description: "A defect", Alias: "bug"},
								{Name: "FEATURE"},
							},
						}),
					),
				}
			},
			mockContent: `{"status":"ACTIVE","label":"BUG"}`,
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
		{
			// Two hoisted enums referenced top-level as First then Second.
			// BAML's LIFO traversal pops the last-referenced (Second) first,
			// so the Second definition block renders BEFORE the First block.
			// The enums are DECLARED in First, Second order to prove declared
			// (OrderedMap) order does not drive the output: a declared-order
			// build would emit First-before-Second and diverge. Each enum is
			// hoisted via a value-level description (descriptions on enum
			// VALUES propagate through the dynamic TypeBuilder).
			name: "two_hoisted_enums_top_level",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("first", &bamlutils.DynamicProperty{Ref: "First"}),
						dProp("second", &bamlutils.DynamicProperty{Ref: "Second"}),
					),
					Enums: dEnums(
						dEnum("First", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "F1", Description: "first one"},
								{Name: "F2"},
							},
						}),
						dEnum("Second", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "S1", Description: "second one"},
								{Name: "S2"},
							},
						}),
					),
				}
			},
			mockContent: `{"first":"F1","second":"S1"}`,
		},
		{
			// A reached class drives enum order: the synthetic target reaches
			// Envelope, whose fields reference First then Second. The enum
			// blocks must order [Second, First] because Envelope's fields are
			// queued in field order and popped last-first. Enums declared
			// First, Second to defeat declared-order parity.
			name: "two_hoisted_enums_nested_class",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("env", &bamlutils.DynamicProperty{Ref: "Envelope"}),
					),
					Classes: dClasses(
						dClass("Envelope", &bamlutils.DynamicClass{
							Properties: dProps(
								dProp("a", &bamlutils.DynamicProperty{Ref: "First"}),
								dProp("b", &bamlutils.DynamicProperty{Ref: "Second"}),
							),
						}),
					),
					Enums: dEnums(
						dEnum("First", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "F1", Description: "first one"},
								{Name: "F2"},
							},
						}),
						dEnum("Second", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{
								{Name: "S1", Description: "second one"},
								{Name: "S2"},
							},
						}),
					),
				}
			},
			mockContent: `{"env":{"a":"F1","b":"S1"}}`,
		},
		{
			// Container placements exercising the push-order details that
			// drive traversal: a list element, a map (key pushed THEN value,
			// so the value enum is processed first), and a union (members
			// pushed in iter_include_null order, last member popped first).
			// All four enums hoist (value descriptions); the comparison pins
			// that the native traversal reproduces BAML's per-container stack
			// discipline byte-exact. Map key is a plain string to stay inside
			// BAML's map-key rules.
			name: "hoisted_enums_containers",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("tags", &bamlutils.DynamicProperty{
							Type:  "list",
							Items: &bamlutils.DynamicTypeSpec{Ref: "Alpha"},
						}),
						dProp("attrs", &bamlutils.DynamicProperty{
							Type:   "map",
							Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
							Values: &bamlutils.DynamicTypeSpec{Ref: "Beta"},
						}),
						dProp("choice", &bamlutils.DynamicProperty{
							Type: "union",
							OneOf: []*bamlutils.DynamicTypeSpec{
								{Ref: "Gamma"},
								{Ref: "Delta"},
							},
						}),
					),
					Enums: dEnums(
						dEnum("Alpha", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "A1", Description: "a"}, {Name: "A2"}},
						}),
						dEnum("Beta", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "B1", Description: "b"}, {Name: "B2"}},
						}),
						dEnum("Gamma", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "G1", Description: "g"}, {Name: "G2"}},
						}),
						dEnum("Delta", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "D1", Description: "d"}, {Name: "D2"}},
						}),
					),
				}
			},
			mockContent: `{"tags":["A1"],"attrs":{"k":"B1"},"choice":"G1"}`,
		},
		{
			// Mixed reachable graph: hoisted enums interleaved with a class
			// reference. The class (Wrapper) renders inline under bare
			// ctx.output_format, but its fields still feed the traversal, so
			// the enum block order reflects the full DFS. Proves BAML emits
			// ALL enum definitions before any (here inline) class content and
			// that class refs influence enum discovery order.
			name: "mixed_reachable_graph",
			schema: func() *bamlutils.DynamicOutputSchema {
				return &bamlutils.DynamicOutputSchema{
					Properties: dProps(
						dProp("status", &bamlutils.DynamicProperty{Ref: "First"}),
						dProp("payload", &bamlutils.DynamicProperty{Ref: "Wrapper"}),
						dProp("tail", &bamlutils.DynamicProperty{Ref: "Third"}),
					),
					Classes: dClasses(
						dClass("Wrapper", &bamlutils.DynamicClass{
							Properties: dProps(
								dProp("inner", &bamlutils.DynamicProperty{Ref: "Second"}),
								dProp("note", &bamlutils.DynamicProperty{Type: "string"}),
							),
						}),
					),
					Enums: dEnums(
						dEnum("First", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "F1", Description: "f"}, {Name: "F2"}},
						}),
						dEnum("Second", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "S1", Description: "s"}, {Name: "S2"}},
						}),
						dEnum("Third", &bamlutils.DynamicEnum{
							Values: []*bamlutils.DynamicEnumValue{{Name: "T1", Description: "t"}, {Name: "T2"}},
						}),
					),
				}
			},
			mockContent: `{"status":"F1","payload":{"inner":"S1","note":"n"},"tail":"T1"}`,
		},
	}
}
