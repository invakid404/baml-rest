//go:build !llmhttp_debug

package llmhttp

// Production build of the borrowed-response guard. All three hooks are empty
// and inline to nothing, so a normal build pays ZERO cost for the borrow
// lifecycle instrumentation — no finalizer, no poison loop, no extra field
// touches. The debug variant (build tag llmhttp_debug, borrow_guard_debug.go)
// supplies real implementations that catch use-after-release and missed-release
// bugs in the consumer-managed borrow.
//
// See Response's lifetime contract: a borrowed Response must be Release()d and
// its body view must not be retained past that point. These hooks make
// violations fail loudly under the debug tag without burdening production.

// armBorrowGuard is called when a borrowed Response is handed to a consumer; in
// debug builds it installs a finalizer that warns if the Response is GC'd
// without Release. No-op here.
func armBorrowGuard(_ *Response) {}

// disarmBorrowGuard is called from Release; in debug builds it clears the
// missed-release finalizer. No-op here.
func disarmBorrowGuard(_ *Response) {}

// poisonReleasedBody is called from Release just before pooled storage is
// returned; in debug builds it overwrites the bytes so a retained
// BodyString/BodyBytes view reads garbage rather than silently-valid stale
// data. No-op here.
func poisonReleasedBody(_ []byte) {}
