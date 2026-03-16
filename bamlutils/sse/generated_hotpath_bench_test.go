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
		extractor := NewIncrementalExtractor()
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
		extractor := NewIncrementalExtractor()
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
		extractor := NewIncrementalExtractor()
		benchExtractResultSink = ExtractFrom(extractor, 1, "openai", chunks)
	}
}

// BenchmarkExtractFromSteadyState simulates the real per-tick behavior in the
// generated adapter: one extractor is reused across many ticks, each tick
// appending a fixed window of new chunks. The chunk slice is preallocated to
// avoid measuring slice-growth overhead — only incremental extraction cost.
func BenchmarkExtractFromSteadyState(b *testing.B) {
	const totalChunks = 1024
	const chunksPerTick = 4

	// Preallocate all chunks up front
	allChunks := makeBenchChunks(totalChunks)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		extractor := NewIncrementalExtractor()
		// Simulate ticks: each tick the extractor sees a growing slice
		for end := chunksPerTick; end <= totalChunks; end += chunksPerTick {
			benchExtractResultSink = ExtractFrom(extractor, 1, "openai", allChunks[:end])
		}
	}
}
