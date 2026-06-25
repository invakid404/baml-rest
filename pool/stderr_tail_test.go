package pool

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// newTestRing builds a ring buffer with explicit small bounds so the
// eviction behaviour can be exercised without writing thousands of
// lines or megabytes of data.
func newTestRing(maxLines, maxBytes int) *stderrRingBuffer {
	return &stderrRingBuffer{maxLines: maxLines, maxBytes: maxBytes}
}

func TestStderrRingBuffer_KeepsLastNLines(t *testing.T) {
	r := newTestRing(3, 1<<20)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(r, "line-%d\n", i)
	}

	got, n := r.snapshot()
	if want := "line-7\nline-8\nline-9"; got != want {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
	if n != 3 {
		t.Fatalf("line count = %d, want 3", n)
	}
}

func TestStderrRingBuffer_ByteCapBounded(t *testing.T) {
	// maxBytes well below total written: the buffer must never retain
	// more than maxBytes (plus at most one straddling line).
	r := newTestRing(1000, 64)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(r, "0123456789-%03d\n", i) // 15 bytes/line
	}

	r.mu.Lock()
	curBytes := r.curBytes
	lineSum := 0
	for _, l := range r.lines {
		lineSum += len(l)
	}
	r.mu.Unlock()

	if curBytes != lineSum {
		t.Fatalf("curBytes accounting drifted: tracked %d, actual %d", curBytes, lineSum)
	}
	// At least one line is always retained even past the cap, so the
	// bound is maxBytes + one line's worth, never unbounded.
	if curBytes > 64+15 {
		t.Fatalf("retained %d bytes, exceeds bound (maxBytes=64 + one line)", curBytes)
	}
	// The freshest line must always survive.
	if got, _ := r.snapshot(); !strings.Contains(got, "0123456789-199") {
		t.Fatalf("most recent line dropped; snapshot=%q", got)
	}
}

// assertWithinCaps fails if the buffer's snapshot exceeds either cap.
// It checks the snapshot-visible totals (lines including the partial,
// and content bytes excluding the join separators), which is exactly
// what the host surfaces.
func assertWithinCaps(t *testing.T, r *stderrRingBuffer) {
	t.Helper()
	snap, n := r.snapshot()
	if n > r.maxLines {
		t.Fatalf("snapshot has %d lines, exceeds maxLines=%d", n, r.maxLines)
	}
	// Content bytes = joined length minus the (n-1) "\n" separators.
	contentBytes := len(snap)
	if n > 1 {
		contentBytes -= n - 1
	}
	if contentBytes > r.maxBytes {
		t.Fatalf("snapshot has %d content bytes, exceeds maxBytes=%d", contentBytes, r.maxBytes)
	}
}

// TestStderrRingBuffer_StrictCapsUnderAdversarialInput proves the
// "last N lines / M bytes" contract holds for the two pathological
// shapes that previously broke it: (i) a single line larger than the
// byte cap, and (ii) more than the line cap of short lines followed by
// an unterminated partial (which snapshot includes as an extra line).
func TestStderrRingBuffer_StrictCapsUnderAdversarialInput(t *testing.T) {
	const (
		maxLines = 100
		maxBytes = 64 * 1024
	)

	t.Run("single oversized line", func(t *testing.T) {
		r := newTestRing(maxLines, maxBytes)
		// One newline-terminated line ~3x the byte cap.
		big := strings.Repeat("A", 3*maxBytes)
		r.Write([]byte(big + "\n"))
		assertWithinCaps(t, r)

		// The TAIL is what's kept: the freshest bytes survive.
		snap, _ := r.snapshot()
		if !strings.HasSuffix(big, snap) {
			t.Fatalf("retained content is not the tail of the input line")
		}
	})

	t.Run("over line-cap then trailing partial", func(t *testing.T) {
		r := newTestRing(maxLines, maxBytes)
		for i := 0; i < 150; i++ {
			fmt.Fprintf(r, "short-line-%d\n", i)
		}
		// Unterminated partial on top of an already-full ring — this is
		// the case that used to yield maxLines+1 lines / ~2x bytes.
		r.Write([]byte("panic: still being written"))
		assertWithinCaps(t, r)

		snap, _ := r.snapshot()
		if !strings.HasSuffix(snap, "panic: still being written") {
			t.Fatalf("partial line not surfaced as final snapshot line; got:\n%s", snap)
		}
	})

	t.Run("oversized partial alone", func(t *testing.T) {
		r := newTestRing(maxLines, maxBytes)
		// A partial larger than the byte cap, never newline-terminated.
		r.Write([]byte(strings.Repeat("B", 5*maxBytes)))
		assertWithinCaps(t, r)
	})

	t.Run("mixed: oversized line then more lines then partial", func(t *testing.T) {
		r := newTestRing(maxLines, maxBytes)
		r.Write([]byte(strings.Repeat("C", 2*maxBytes) + "\n"))
		for i := 0; i < 200; i++ {
			fmt.Fprintf(r, "follow-%d\n", i)
		}
		r.Write([]byte("trailing partial no newline"))
		assertWithinCaps(t, r)
	})
}

func TestStderrRingBuffer_ReassemblesChunkedLine(t *testing.T) {
	r := newTestRing(10, 1<<20)
	// A single logical line split across several Write calls, the last
	// chunk carrying the newline.
	r.Write([]byte("panic: "))
	r.Write([]byte("RangeAny called using nil "))
	r.Write([]byte("*OrderedMap pointer\n"))

	got, n := r.snapshot()
	if want := "panic: RangeAny called using nil *OrderedMap pointer"; got != want {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
	if n != 1 {
		t.Fatalf("line count = %d, want 1", n)
	}
}

func TestStderrRingBuffer_IncludesUnterminatedPartial(t *testing.T) {
	// A worker that dies mid-line never emits the trailing newline; the
	// partial must still surface (that's often the panic header itself).
	r := newTestRing(10, 1<<20)
	r.Write([]byte("first\n"))
	r.Write([]byte("panic: boom (no newline)"))

	got, n := r.snapshot()
	if want := "first\npanic: boom (no newline)"; got != want {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
	if n != 2 {
		t.Fatalf("line count = %d, want 2", n)
	}
}

func TestStderrRingBuffer_EmptySnapshot(t *testing.T) {
	r := newStderrRingBuffer()
	if got, n := r.snapshot(); got != "" || n != 0 {
		t.Fatalf("empty snapshot = (%q, %d), want (\"\", 0)", got, n)
	}
}

// TestStderrRingBuffer_ConcurrentWrites stresses the mutex under -race:
// go-plugin drives Stderr and SyncStderr from independent goroutines, so
// concurrent Write calls must be safe and the buffer must stay bounded.
func TestStderrRingBuffer_ConcurrentWrites(t *testing.T) {
	r := newTestRing(50, 1<<20)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				fmt.Fprintf(r, "w%d-line-%d\n", w, i)
			}
		}(w)
	}
	wg.Wait()

	_, n := r.snapshot()
	if n > 50 {
		t.Fatalf("retained %d lines, exceeds maxLines=50", n)
	}
}

func TestWithWorkerStderr_AppendsTailAndPreservesSentinel(t *testing.T) {
	const marker = "panic: RangeAny called using nil *OrderedMap pointer"

	h := &workerHandle{id: 7, stderrTail: newStderrRingBuffer()}
	fmt.Fprintf(h.stderrTail, "goroutine 1 [running]:\n%s\n", marker)

	// Mirror the production terminal-exhaustion wrap.
	base := fmt.Errorf("%w: %w", ErrPoolRetriesExhausted,
		errors.New("rpc error: code = Unavailable desc = error reading from server: EOF"))
	got := withWorkerStderr(base, h)

	if !strings.Contains(got.Error(), marker) {
		t.Fatalf("surfaced error missing stderr tail marker; got:\n%s", got.Error())
	}
	if !strings.Contains(got.Error(), "worker 7 stderr") {
		t.Fatalf("surfaced error missing worker-id delimiter; got:\n%s", got.Error())
	}
	// The sentinel chain must survive the wrap so HTTP classification
	// still sees worker_unavailable.
	if !errors.Is(got, ErrPoolRetriesExhausted) {
		t.Fatalf("withWorkerStderr broke errors.Is(ErrPoolRetriesExhausted)")
	}
}

func TestWithWorkerStderr_Noop(t *testing.T) {
	base := fmt.Errorf("%w", ErrPoolRetriesExhausted)

	cases := map[string]*workerHandle{
		"nil handle": nil,
		"nil tail":   {id: 1},
		"empty tail": {id: 2, stderrTail: newStderrRingBuffer()},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			if got := withWorkerStderr(base, h); got != base {
				t.Fatalf("expected unchanged error, got: %v", got)
			}
		})
	}

	// nil error stays nil regardless of handle.
	if got := withWorkerStderr(nil, &workerHandle{id: 3, stderrTail: newStderrRingBuffer()}); got != nil {
		t.Fatalf("withWorkerStderr(nil, ...) = %v, want nil", got)
	}
}
