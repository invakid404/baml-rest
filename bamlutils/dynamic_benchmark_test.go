package bamlutils

import (
	"strings"
	"testing"
)

func benchClientRegistry() *ClientRegistry {
	primary := "TestClient"
	provider := "anthropic"
	return &ClientRegistry{
		Primary: &primary,
		Clients: []*ClientProperty{{
			Name:     primary,
			Provider: provider,
			Options:  map[string]any{"model": "m", "api_key": "k"},
		}},
	}
}

func benchOutputSchema() *DynamicOutputSchema {
	return &DynamicOutputSchema{
		Properties: MustOrderedMap(OrderedKV("name", &DynamicProperty{Type: "string"})),
	}
}

func benchTextMessages(n int) []DynamicMessage {
	body := strings.Repeat("a", 256)
	msgs := make([]DynamicMessage, n)
	for i := 0; i < n; i++ {
		text := body
		msgs[i] = DynamicMessage{Role: "user", TextContent: &text}
	}
	return msgs
}

func benchMultimodalMessages(n int) []DynamicMessage {
	url := "https://example.com/image.png"
	mediaType := "image/png"
	text := "describe"
	cacheType := "ephemeral"
	msgs := make([]DynamicMessage, n)
	for i := 0; i < n; i++ {
		t := text
		msgs[i] = DynamicMessage{
			Role: "user",
			PartsContent: []DynamicContentPart{
				{Type: "text", Text: &t},
				{Type: "image", Image: &MediaInput{URL: &url, MediaType: &mediaType}},
				{Type: "pdf", PDF: &MediaInput{URL: &url, MediaType: &mediaType}},
				{Type: "output_format"},
			},
			Metadata: &MessageMetadata{CacheControl: &CacheControl{Type: cacheType}},
		}
	}
	return msgs
}

func runWorkerInputBench(b *testing.B, in *DynamicInput) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := in.ToWorkerInput()
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkDynamicInputToWorkerInput(b *testing.B) {
	registry := benchClientRegistry()
	schema := benchOutputSchema()

	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "single_text_message", n: 1},
		{name: "10_text_messages", n: 10},
		{name: "100_text_messages", n: 100},
	} {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			in := &DynamicInput{
				Messages:       benchTextMessages(tc.n),
				ClientRegistry: registry,
				OutputSchema:   schema,
			}
			runWorkerInputBench(b, in)
		})
	}

	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "single_multimodal_message", n: 1},
		{name: "10_multimodal_messages", n: 10},
		{name: "100_multimodal_messages", n: 100},
	} {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			in := &DynamicInput{
				Messages:       benchMultimodalMessages(tc.n),
				ClientRegistry: registry,
				OutputSchema:   schema,
			}
			runWorkerInputBench(b, in)
		})
	}
}
