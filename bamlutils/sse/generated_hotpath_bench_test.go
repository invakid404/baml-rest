package sse

import "testing"

type benchChunk string

var benchSSEChunksSink []SSEChunk
var benchExtractResultSink ExtractResult

func (c benchChunk) Text() (string, error) { return string(c), nil }
func (c benchChunk) JSON() (any, error)    { return nil, nil }

func makeBenchChunks(n int) []benchChunk {
	chunks := make([]benchChunk, n)
	for i := range chunks {
		chunks[i] = benchChunk(`{"choices":[{"delta":{"content":"x"}}]}`)
	}
	return chunks
}

func BenchmarkGeneratedChunkInterfaceCopy(b *testing.B) {
	chunks := makeBenchChunks(64)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sseChunks := make([]SSEChunk, len(chunks))
		for j, chunk := range chunks {
			sseChunks[j] = chunk
		}
		benchSSEChunksSink = sseChunks
	}
}

func BenchmarkGeneratedChunkInterfaceCopyAndExtract(b *testing.B) {
	chunks := makeBenchChunks(64)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		extractor := NewIncrementalExtractor(false)
		sseChunks := make([]SSEChunk, len(chunks))
		for j, chunk := range chunks {
			sseChunks[j] = chunk
		}
		benchExtractResultSink = extractor.Extract(1, "openai", sseChunks)
	}
}

func BenchmarkExtractPreboxedChunks(b *testing.B) {
	chunks := makeBenchChunks(64)
	sseChunks := make([]SSEChunk, len(chunks))
	for i, chunk := range chunks {
		sseChunks[i] = chunk
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		extractor := NewIncrementalExtractor(false)
		benchExtractResultSink = extractor.Extract(1, "openai", sseChunks)
	}
}

// BenchmarkExtractFromConcreteChunks benchmarks the generic ExtractFrom path
// that the generated adapter actually uses — no interface-slice boxing at all.
func BenchmarkExtractFromConcreteChunks(b *testing.B) {
	chunks := makeBenchChunks(64)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		extractor := NewIncrementalExtractor(false)
		benchExtractResultSink = ExtractFrom(extractor, 1, "openai", chunks)
	}
}

// BenchmarkExtractFromSteadyState measures the cost of a single incremental
// tick in steady state. The extractor is pre-warmed with half the chunks during
// setup so the benchmarked call only processes the new chunk window, matching
// the real per-tick path in generated adapters. Each b.N iteration is one tick.
func BenchmarkExtractFromSteadyState(b *testing.B) {
	const totalChunks = 1024
	const warmupChunks = totalChunks / 2
	const chunksPerTick = 4

	allChunks := makeBenchChunks(totalChunks)

	// Pre-warm: advance the extractor to a mid-stream position.
	extractor := NewIncrementalExtractor(false)
	for end := chunksPerTick; end <= warmupChunks; end += chunksPerTick {
		ExtractFrom(extractor, 1, "openai", allChunks[:end])
	}
	cursor := warmupChunks

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		next := cursor + chunksPerTick
		if next > totalChunks {
			// Wrap around: reset extractor and re-warm so we stay in steady state
			b.StopTimer()
			extractor = NewIncrementalExtractor(false)
			for end := chunksPerTick; end <= warmupChunks; end += chunksPerTick {
				ExtractFrom(extractor, 1, "openai", allChunks[:end])
			}
			cursor = warmupChunks
			next = cursor + chunksPerTick
			b.StartTimer()
		}
		benchExtractResultSink = ExtractFrom(extractor, 1, "openai", allChunks[:next])
		cursor = next
	}
}
