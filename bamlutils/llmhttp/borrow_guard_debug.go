//go:build llmhttp_debug

package llmhttp

import (
	"runtime"
	"sync/atomic"
)

// Debug build of the borrowed-response guard (build with -tags llmhttp_debug).
// It instruments the consumer-managed borrow to make two otherwise-silent
// classes of bug fail loudly:
//
//   - Use-after-release: a consumer retains a BodyString/BodyBytes view past
//     Release. poisonReleasedBody overwrites the pooled storage on Release, so
//     the stale view reads poisonByte garbage instead of plausibly-valid data
//     (or, worse, another request's body once the buffer is reused).
//
//   - Missed release: a borrowed Response is dropped without Release, leaking
//     the pooled fasthttp response / drain buffer until GC. armBorrowGuard sets
//     a finalizer that increments missedReleaseCount (and warns on stderr) when
//     such a Response is collected while still holding pooled storage.
//
// All of this is gated behind the build tag, so production builds (which use
// borrow_guard.go) pay nothing — see that file.

// poisonByte is written over released borrowed storage. 0xde is recognizable in
// a hex dump ("deadbeef"-ish) and is not a valid leading JSON byte, so a stale
// read that reaches the extractor fails fast.
const poisonByte = 0xde

// missedReleaseCount counts borrowed Responses that were garbage-collected
// without Release. It climbs alongside the stderr warning in any debug build;
// tests read it to assert missed-release detection deterministically. Atomic
// because the finalizer runs on its own goroutine.
var missedReleaseCount atomic.Int64

// liveBorrows is the number of borrowed Responses currently armed — handed out
// by ExecuteBorrowed and not yet Released. armBorrowGuard increments it,
// disarmBorrowGuard (from Release) decrements it. It is a deterministic,
// synchronously-updated gauge of outstanding borrows, which lets tests assert
// the orchestrator's per-attempt release contract (each attempt holds exactly
// one borrow at a time; a regression that accumulated borrows across attempts
// would push the gauge above one).
var liveBorrows atomic.Int64

// LiveBorrowsForTest returns the current outstanding-borrow count. Debug-build-
// only seam (no such symbol exists in production builds) used by orchestrator
// tests to verify borrowed responses are released before the next attempt.
func LiveBorrowsForTest() int64 { return liveBorrows.Load() }

// reportMissedRelease records and warns about a missed release. Called from the
// finalizer installed by armBorrowGuard.
func reportMissedRelease() {
	missedReleaseCount.Add(1)
	println("llmhttp: BUG: borrowed Response garbage-collected without Release (missed release); pooled storage leaked")
}

// armBorrowGuard installs a finalizer that reports a missed release. It is
// called when a borrowed Response is constructed (the fasthttp lanes). Owned
// responses are never armed. Release calls disarmBorrowGuard to clear it.
func armBorrowGuard(r *Response) {
	if r == nil {
		return
	}
	liveBorrows.Add(1)
	runtime.SetFinalizer(r, func(rr *Response) {
		// If the consumer released, the finalizer was cleared and this never
		// runs; the released/borrowed guard is belt-and-suspenders against a
		// resurrected or partially-torn-down value.
		if rr != nil && !rr.released && rr.borrowed() {
			reportMissedRelease()
		}
	})
}

// disarmBorrowGuard clears the missed-release finalizer. Called from Release so
// a correctly-released Response never trips reportMissedRelease.
func disarmBorrowGuard(r *Response) {
	if r == nil {
		return
	}
	liveBorrows.Add(-1)
	runtime.SetFinalizer(r, nil)
}

// poisonReleasedBody overwrites released borrowed storage so a retained body
// view reads garbage. Called from Release before the storage returns to its
// pool.
func poisonReleasedBody(b []byte) {
	for i := range b {
		b[i] = poisonByte
	}
}
