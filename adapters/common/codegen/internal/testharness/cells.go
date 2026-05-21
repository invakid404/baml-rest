package testharness

// NestedKind is the nested-field shape under test for a single cell —
// the same three shapes the compile matrix already crosses plus the
// mutually-recursive cycle.
type NestedKind int

const (
	// NestedValueElem is the `*[]Struct` value-element shape — the
	// closure-context pool branch happy path.
	NestedValueElem NestedKind = iota
	// NestedPtrElem is `*[]*Struct` — fallback to the legacy make
	// path; converter must NOT touch the pool.
	NestedPtrElem
	// NestedTwoPooledTypes is the multi-pool shape (Parts + Tools
	// each pool a distinct inner type from the same converter).
	NestedTwoPooledTypes
	// NestedCycle is the mutually-recursive A↔B fixture.
	NestedCycle
)

// String returns a stable lowercase tag used in cell names.
func (k NestedKind) String() string {
	switch k {
	case NestedValueElem:
		return "a_value_elem"
	case NestedPtrElem:
		return "b_ptr_elem"
	case NestedTwoPooledTypes:
		return "c_two_pooled_types"
	case NestedCycle:
		return "cycle"
	}
	return "unknown"
}

// AdapterFailureMode controls when the fake adapter returns an error
// from ConvertMedia, exercising different points in the
// dispatch/converter call graph.
type AdapterFailureMode int

const (
	// FailureSuccess is the happy path — every ConvertMedia call
	// returns a valid value. Asserts balance + zero-on-Put.
	FailureSuccess AdapterFailureMode = iota
	// FailureAtIdx0 fails on the very first ConvertMedia call; the
	// outer slice has zero filled elements but Phase A already
	// checked it out — release must still fire.
	FailureAtIdx0
	// FailureAtIdx2 fails on the third ConvertMedia call; partial
	// fill exercises the same release path with non-zero used
	// elements that still need zeroing.
	FailureAtIdx2
	// FailureInNested fails inside a nested converter (a *[]Struct
	// child) — both outer AND inner pools must release.
	FailureInNested
	// FailureAsyncLateConsume defers consumption past the dispatch
	// goroutine's first wake-up so the barrier test can assert
	// release hasn't fired yet. Only meaningful for stream-path
	// cells.
	FailureAsyncLateConsume
)

// String returns a stable lowercase tag used in cell names.
func (m AdapterFailureMode) String() string {
	switch m {
	case FailureSuccess:
		return "success"
	case FailureAtIdx0:
		return "fail_at_idx_0"
	case FailureAtIdx2:
		return "fail_at_idx_2"
	case FailureInNested:
		return "fail_in_nested"
	case FailureAsyncLateConsume:
		return "async_late_consume"
	}
	return "unknown"
}

// CellSpec is the matrix-cell descriptor consumed by both the compile-
// matrix test and the lifecycle harness. It is intentionally narrow
// — emission lives in the codegen package because it depends on
// internal generator types (methodEmitter, slicePoolTracker). What
// CellSpec captures is the test-author-visible identity of a cell:
// its name, its nested shape, the failure mode being injected, and
// whether the dispatch is expected to emit `__releaseConverted`.
//
// The same emission backend in the codegen package consumes whatever
// implementation of CellEnumerator below yields, so the matrix
// dimensions can grow (e.g. via a property-based generator) without
// touching the harness wiring.
type CellSpec struct {
	Name                   string
	NestedShape            NestedKind
	Failure                AdapterFailureMode
	ExpectReleaseConverted bool
}

// CellEnumerator yields the matrix cells under test. The compile
// matrix uses an enumerated implementation; the lifecycle harness
// extends that set with the failure-mode cross-product. Property-
// based generators can plug into the same interface to grow the
// matrix without altering the consumers.
type CellEnumerator interface {
	Cells() []CellSpec
}
