package utils

import "testing"

var unwrapSink any

func benchmarkDynamicValue() any {
	return map[string]any{
		"id":   "123",
		"name": "example",
		"meta": map[string]any{
			"tags": []any{"a", "b", "c"},
			"nested": map[string]any{
				"ok":    true,
				"count": 42,
				"list": []any{
					map[string]any{"k": "v1"},
					map[string]any{"k": "v2"},
				},
			},
		},
	}
}

func BenchmarkUnwrapDynamicValue_PlainNestedValue(b *testing.B) {
	value := benchmarkDynamicValue()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		unwrapSink = UnwrapDynamicValue(value)
	}
}
