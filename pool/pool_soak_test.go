package pool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDispatchRestartConcurrentWaitersRepeated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping restart soak test in short mode")
	}

	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	const (
		cycles  = 12
		waiters = 12
	)

	for cycle := 0; cycle < cycles; cycle++ {
		p.mu.RLock()
		failed := p.workers[0]
		p.mu.RUnlock()

		failed.healthy.Store(false)

		blockedFactory := make(chan struct{})
		p.newWorker = func(id int) (*workerHandle, error) {
			<-blockedFactory
			return newMockHandle(id, newMockWorker()), nil
		}

		p.dispatchRestart(failed)

		errCh := make(chan error, waiters)
		var wg sync.WaitGroup
		for i := 0; i < waiters; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h, err := p.getWorkerForRetry(context.Background(), nil)
				if err != nil {
					errCh <- fmt.Errorf("cycle %d: getWorkerForRetry: %w", cycle, err)
					return
				}
				if h == failed {
					errCh <- fmt.Errorf("cycle %d: waiter received failed handle", cycle)
					return
				}
				if !h.healthy.Load() {
					errCh <- fmt.Errorf("cycle %d: waiter received unhealthy replacement", cycle)
				}
			}()
		}

		waitDone := make(chan struct{})
		go func() {
			defer close(waitDone)
			wg.Wait()
		}()

		select {
		case <-waitDone:
			t.Fatalf("cycle %d: waiters returned before replacement was released", cycle)
		case <-time.After(25 * time.Millisecond):
		}

		close(blockedFactory)

		requireCompleteWithin(t, 2*time.Second, func() {
			wg.Wait()
		})

		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
	}
}
