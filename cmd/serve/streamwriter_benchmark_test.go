package main

import (
	"bufio"
	stdjson "encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/apierror"
)

func BenchmarkNDJSONPublishDataParallel(b *testing.B) {
	publisher := &NDJSONStreamWriterPublisher{
		w:      bufio.NewWriterSize(io.Discard, 4096),
		cancel: func() {},
	}
	data := []byte(`{"message":"benchmark payload","count":123,"ok":true}`)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := publisher.PublishData(data, "raw-payload", ""); err != nil {
				panic(err)
			}
		}
	})
}

func BenchmarkSSEPublishDataParallel(b *testing.B) {
	data := []byte(`{"message":"benchmark payload","count":123,"ok":true}`)

	for _, tc := range []struct {
		name     string
		needsRaw bool
		raw      string
	}{
		{name: "no_raw"},
		{name: "with_raw", needsRaw: true, raw: "raw-payload"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			publisher := &SSEStreamWriterPublisher{
				w:        bufio.NewWriterSize(io.Discard, 4096),
				cancel:   func() {},
				needsRaw: tc.needsRaw,
			}

			b.ReportAllocs()
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := publisher.PublishData(data, tc.raw, ""); err != nil {
						panic(err)
					}
				}
			})
		})
	}
}

// BenchmarkSSEFrame exercises the SSE frame builder in isolation so we
// can measure the bytebufferpool win without bufio.Writer / Flush noise.
func BenchmarkSSEFrame(b *testing.B) {
	cases := []struct {
		name      string
		eventType string
		payload   string
	}{
		{name: "small_no_event", eventType: "", payload: `{"v":1}`},
		{name: "small_final", eventType: sseEventFinal, payload: `{"v":1}`},
		{name: "small_multiline", eventType: sseEventFinal, payload: "line1\nline2\nline3"},
		{name: "large_16KiB", eventType: sseEventFinal, payload: strings.Repeat("x", 16*1024)},
		{name: "large_multiline_64KiB", eventType: sseEventFinal, payload: strings.Repeat(strings.Repeat("x", 1024)+"\n", 64)},
	}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf := streamFrameBufferPool.Get()
				buf.Reset()
				appendSSEEventFrame(buf, tc.eventType, tc.payload)
				streamFrameBufferPool.Put(buf)
			}
		})
	}
}

// BenchmarkNDJSONEncode measures encodeNDJSONEvent in isolation across
// representative event shapes (data, raw reasoning, metadata, error).
func BenchmarkNDJSONEncode(b *testing.B) {
	small := &NDJSONEvent{
		Type: NDJSONEventData,
		Data: stdjson.RawMessage(`{"v":1}`),
	}
	rawReasoning := &NDJSONEvent{
		Type:      NDJSONEventData,
		Data:      stdjson.RawMessage(`{"v":1}`),
		Raw:       strings.Repeat("r", 1024),
		Reasoning: strings.Repeat("e", 1024),
	}
	metadata := &NDJSONEvent{
		Type: NDJSONEventMetadata,
		Data: stdjson.RawMessage(`{"client":"primary","attempt":1}`),
	}
	errEvent := &NDJSONEvent{
		Type:    NDJSONEventError,
		Error:   "upstream temporarily unavailable",
		Code:    apierror.Code("worker_unavailable"),
		Details: stdjson.RawMessage(`{"retryable":true,"attempts":3}`),
	}

	cases := []struct {
		name  string
		event *NDJSONEvent
	}{
		{name: "data_small", event: small},
		{name: "data_with_raw_reasoning", event: rawReasoning},
		{name: "metadata", event: metadata},
		{name: "error_with_details", event: errEvent},
	}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf := streamFrameBufferPool.Get()
				buf.Reset()
				if err := encodeNDJSONEvent(buf, tc.event); err != nil {
					b.Fatal(err)
				}
				streamFrameBufferPool.Put(buf)
			}
		})
	}
}
