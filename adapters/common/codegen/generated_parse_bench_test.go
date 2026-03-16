package codegen

import (
	"strings"
	"testing"
)

var parseSink int

func makeParseDeltas(n int) []string {
	deltas := make([]string, n)
	for i := range deltas {
		deltas[i] = "0123456789abcdef"
	}
	return deltas
}

func fakeLinearParse(raw string) int {
	sum := 0
	for i := 0; i < len(raw); i++ {
		sum += int(raw[i])
	}
	return sum
}

func BenchmarkGeneratedWholeRawParse(b *testing.B) {
	cases := []struct {
		name   string
		deltas []string
	}{
		{name: "tokens_64", deltas: makeParseDeltas(64)},
		{name: "tokens_256", deltas: makeParseDeltas(256)},
	}

	for _, tc := range cases {
		// Pre-compute capacity hint so builder growth does not skew the benchmark.
		capHint := 0
		for _, d := range tc.deltas {
			capHint += len(d)
		}

		b.Run(tc.name+"/current", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var full strings.Builder
				full.Grow(capHint)
				total := 0
				for _, delta := range tc.deltas {
					full.WriteString(delta)
					total += fakeLinearParse(full.String())
				}
				parseSink = total
			}
		})

		b.Run(tc.name+"/incremental", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				total := 0
				for _, delta := range tc.deltas {
					total += fakeLinearParse(delta)
				}
				parseSink = total
			}
		})
	}
}
