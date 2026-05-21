package sseclient

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func buildSSEStream(eventCount int, payload string) []byte {
	var buf bytes.Buffer
	for i := 0; i < eventCount; i++ {
		buf.WriteString("data: ")
		buf.WriteString(payload)
		buf.WriteString("\n\n")
	}
	return buf.Bytes()
}

func buildMultilineSSEStream(eventCount int, linesPerEvent int, linePayload string) []byte {
	var buf bytes.Buffer
	for i := 0; i < eventCount; i++ {
		for j := 0; j < linesPerEvent; j++ {
			buf.WriteString("data: ")
			buf.WriteString(linePayload)
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func runStreamBench(b *testing.B, body []byte) {
	b.SetParallelism(16)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			events, errc := Stream(context.Background(), bytes.NewReader(body))
			for range events {
			}
			if err := <-errc; err != nil {
				b.Fatalf("stream error: %v", err)
			}
		}
	})
}

func BenchmarkStreamParallel(b *testing.B) {
	smallPayload := strings.Repeat("x", 128)
	mediumPayload := strings.Repeat("x", 4*1024)
	largePayload := strings.Repeat("x", 80*1024)

	b.Run("small_128B", func(b *testing.B) {
		body := buildSSEStream(1000, smallPayload)
		runStreamBench(b, body)
	})
	b.Run("medium_4KiB", func(b *testing.B) {
		body := buildSSEStream(1000, mediumPayload)
		runStreamBench(b, body)
	})
	b.Run("large_80KiB", func(b *testing.B) {
		body := buildSSEStream(100, largePayload)
		runStreamBench(b, body)
	})
	b.Run("multiline_4KiB", func(b *testing.B) {
		linePayload := strings.Repeat("x", 1024)
		body := buildMultilineSSEStream(1000, 4, linePayload)
		runStreamBench(b, body)
	})
}
