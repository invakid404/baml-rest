package nativespinejsonfixture

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// nilProneExecutor is a minimal neutral executor whose POINTER methods deref the receiver,
// so a typed-nil (*nilProneExecutor)(nil) boxed in the interface would panic on the first
// dispatch — exactly the footgun NewNativeRuntime must reject at CONSTRUCTION.
type nilProneExecutor struct{ marker int }

func (e *nilProneExecutor) Call(context.Context, string, any) bamlutils.NativeSpineUnaryResult {
	_ = e.marker
	return bamlutils.SucceededSpineResult(nil)
}

func (e *nilProneExecutor) Parse(context.Context, string, string) (any, error) {
	_ = e.marker
	return nil, nil
}

var _ bamlutils.NativeSpineUnaryExecutor = (*nilProneExecutor)(nil)

// TestNewNativeRuntimeRejectsNilExecutor proves the constructor rejects BOTH an untyped
// nil interface AND a TYPED-NIL executor (a nil *nilProneExecutor boxed in the interface —
// non-nil interface, nil pointer), which a plain `exec == nil` misses and which would
// panic on the first Call/Parse dispatch (CodeRabbit #1 follow-on). The typed-nil case
// FAILS on the pre-fix `exec == nil` guard (no construction-time panic).
func TestNewNativeRuntimeRejectsNilExecutor(t *testing.T) {
	assertPanics := func(name string, exec bamlutils.NativeSpineUnaryExecutor) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s: NewNativeRuntime did not panic on a nil executor", name)
			}
		}()
		_ = NewNativeRuntime(exec)
	}
	assertPanics("untyped nil", nil)
	var typedNil *nilProneExecutor
	assertPanics("typed nil *nilProneExecutor", typedNil)
}
