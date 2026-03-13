package main

import (
	"bufio"
	"io"
	"testing"
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
			if err := publisher.PublishData(data, "raw-payload"); err != nil {
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
					if err := publisher.PublishData(data, tc.raw); err != nil {
						panic(err)
					}
				}
			})
		})
	}
}
