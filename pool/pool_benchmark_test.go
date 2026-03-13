package pool

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkGetWorkerParallel(b *testing.B) {
	for _, size := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("pool_%d", size), func(b *testing.B) {
			p := newTestPool(b, size, goodFactory)
			b.Cleanup(func() { _ = p.Close() })
			b.ReportAllocs()
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					h, err := p.getWorker()
					if err != nil || h == nil {
						panic(fmt.Sprintf("getWorker failed: %v", err))
					}
				}
			})
		})
	}
}

func BenchmarkGetWorkerForRetryParallel(b *testing.B) {
	ctx := context.Background()
	for _, size := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("pool_%d", size), func(b *testing.B) {
			p := newTestPool(b, size, goodFactory)
			b.Cleanup(func() { _ = p.Close() })
			b.ReportAllocs()
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					h, err := p.getWorkerForRetry(ctx, nil)
					if err != nil || h == nil {
						panic(fmt.Sprintf("getWorkerForRetry failed: %v", err))
					}
				}
			})
		})
	}
}
