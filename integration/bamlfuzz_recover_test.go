//go:build integration

package integration

import "runtime/debug"

// callWithRecover invokes fn and converts any panic into a returned
// (panicValue, stack) pair. runtime.Goexit (from t.FailNow/Fatalf)
// is not recovered, so legitimate test failures propagate normally.
func callWithRecover(fn func()) (recovered any, stack []byte) {
	defer func() {
		if r := recover(); r != nil {
			recovered = r
			stack = debug.Stack()
		}
	}()
	fn()
	return nil, nil
}
