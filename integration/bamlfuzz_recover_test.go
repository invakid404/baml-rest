//go:build integration

package integration

import "runtime/debug"

// callWithRecover invokes fn and converts any panic into a returned
// (panicked, panicValue, stack) triple. The bool sentinel handles
// panic(nil) correctly: panicked is set true before fn runs and
// cleared only on normal return, so recover() is called
// unconditionally when fn panics.
func callWithRecover(fn func()) (panicked bool, recovered any, stack []byte) {
	panicked = true
	defer func() {
		if panicked {
			recovered = recover()
			stack = debug.Stack()
		}
	}()
	fn()
	panicked = false
	return
}
