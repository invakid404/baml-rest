package generated

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient/internal/generated/adapter"
)

// noopMediaFactory returns nil so this benchmark exercises only the
// pool plumbing for text-only messages (no media). Multimodal coverage
// lives in `bamlutils/dynamic_benchmark_test.go` where the conversion
// runs without depending on real BAML media values.
func noopMediaFactory(_ bamlutils.MediaKind, _ *string, _ *string, _ *string) (any, error) {
	return nil, nil
}

func benchAdapter() bamlutils.Adapter {
	return &adapter.BamlAdapter{MediaFactory: noopMediaFactory}
}

func benchTextOnlyInput(n int) *BamlRestDynamicInput {
	text := "hello"
	msgs := make([]Baml_Rest_MessageMediaInput, n)
	for i := range msgs {
		t := text
		msgs[i] = Baml_Rest_MessageMediaInput{Role: "user", Content: &t}
	}
	return &BamlRestDynamicInput{Messages: msgs}
}

// runMessageConvert drives the same conversion shape the generated
// dispatch sites use: pool checkout for the outer message slice,
// ownedNested tracking, convertBaml_Rest_MessageMediaInput per message,
// release on exit. Mirrors the codegen-emitted shape so the benchmark
// measures the pool win at the layer the codegen actually emits.
func runMessageConvert(b *testing.B, adp bamlutils.Adapter, in *BamlRestDynamicInput) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messagesPtr := getbaml_Rest_MessageSlice(len(in.Messages))
		*messagesPtr = (*messagesPtr)[:len(in.Messages)]
		structMessages := *messagesPtr
		// Closure-context ownedNested: each pooled nested branch
		// inside the converter appends a release closure capturing
		// its own put<X>Slice + pointer. Drain in any order.
		var ownedNested []func()
		release := func() {
			for _, fn := range ownedNested {
				fn()
			}
			putbaml_Rest_MessageSlice(messagesPtr)
		}
		for j := range in.Messages {
			converted, err := convertBaml_Rest_MessageMediaInput(adp, &in.Messages[j], &ownedNested)
			if err != nil {
				release()
				b.Fatal(err)
			}
			structMessages[j] = converted
		}
		release()
	}
}

func BenchmarkBamlRestMessageConvert(b *testing.B) {
	adp := benchAdapter()
	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "1_text_message", n: 1},
		{name: "10_text_messages", n: 10},
		{name: "100_text_messages", n: 100},
	} {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			runMessageConvert(b, adp, benchTextOnlyInput(tc.n))
		})
	}
}
