package nativeprompt

import "github.com/invakid404/baml-rest/bamlutils"

// corpusCase is one differential fixture. The same native []Message + schema
// drive BOTH the native renderer and (in the integration harness) the BAML
// oracle, so there is no parallel hand-built expectation that could drift.
type corpusCase struct {
	name     string
	messages []Message
	// schema is the dynamic output schema; ctx.output_format renders it. Every
	// case carries one because a dynamic request requires an output schema, even
	// when the prompt never reaches ctx.output_format.
	schema *bamlutils.DynamicOutputSchema
	// clientOptions are extra client_registry client options the oracle needs,
	// e.g. allowed_role_metadata to make BAML forward cache_control to the wire.
	clientOptions map[string]any
}

func strptr(s string) *string { return &s }

// simpleSchema is a flat {answer: string} schema.
func simpleSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

// richSchema is a two-field schema so a rendered ctx.output_format block is
// non-trivial (exercises multiple fields + a primitive prefix line).
func richSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("count", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}
}

// corpusCases is the seeded parity corpus: text-only, output_format-only,
// text + {output_format} replacement, role metadata incl. cache_control, and an
// image media part.
func corpusCases() []corpusCase {
	return []corpusCase{
		{
			name: "text_only",
			messages: []Message{
				{Role: "system", Content: strptr("You are a helpful assistant.")},
				{Role: "user", Content: strptr("What is 2+2?")},
			},
			schema: simpleSchema(),
		},
		{
			name: "output_format_only",
			messages: []Message{
				{Role: "system", Parts: []ContentPart{{OutputFormat: true}}},
				{Role: "user", Content: strptr("Produce the structured output.")},
			},
			schema: richSchema(),
		},
		{
			name: "text_replace_output_format",
			messages: []Message{
				{Role: "system", Content: strptr("You are precise.")},
				{Role: "user", Content: strptr("Answer strictly using {output_format} and nothing else.")},
			},
			schema: richSchema(),
		},
		{
			name: "role_metadata_cache_control",
			messages: []Message{
				{Role: "system", Content: strptr("You are a helpful assistant.")},
				{
					Role:     "user",
					Parts:    []ContentPart{{Text: strptr("Say ok.")}},
					Metadata: &Metadata{CacheControl: &CacheControl{Type: "ephemeral"}},
				},
				{Role: "assistant", Content: strptr("I will answer carefully.")},
				{Role: "user", Content: strptr("Now answer.")},
			},
			schema:        simpleSchema(),
			clientOptions: map[string]any{"allowed_role_metadata": []any{"cache_control"}},
		},
		{
			name: "image_media",
			messages: []Message{
				{
					Role: "user",
					Parts: []ContentPart{
						{Text: strptr("Describe this image:")},
						{Image: &Media{Kind: MediaImage, URL: "https://example.com/cat.png"}},
					},
				},
			},
			schema: simpleSchema(),
		},
	}
}
