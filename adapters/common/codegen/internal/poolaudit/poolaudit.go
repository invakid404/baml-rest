// Package poolaudit is the test-only counterpart to the slice-pool
// helpers emitted by codegen. When the generator is run with
// EmitPoolAuditHooks=true (test mode only), the rendered get/put
// helpers call into this package so the lifecycle harness can observe
// pool checkouts, releases, and zero-on-Put behavior.
//
// Production codegen output never imports this package — the option
// defaults to false and the package itself lives under `internal/` so
// dynclient cannot depend on it even transitively.
package poolaudit

import (
	"fmt"
	"reflect"
	"sync"
)

// Counters is the snapshot returned by Snapshot. The maps are copies
// of the package-level state; callers may mutate them freely.
type Counters struct {
	Checkouts map[string]int
	Releases  map[string]int
}

// Violation records a single zero-on-Put failure: the slice element at
// Index was not zero by the time the put helper was about to return
// its slice to the pool.
type Violation struct {
	TypeName string
	Index    int
	Detail   string
}

var (
	mu         sync.Mutex
	checkouts  = map[string]int{}
	releases   = map[string]int{}
	violations []Violation
)

// Reset clears all observed state. Tests call this in setup. Safe to
// call concurrently with no other access — the mutex serializes.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	checkouts = map[string]int{}
	releases = map[string]int{}
	violations = nil
}

// OnCheckout is emitted at the head of getXSlice in audit mode. The
// rendered call passes the unwrapped BAML type name as a string
// literal so the audit can group counts per pooled element type.
func OnCheckout(typeName string) {
	mu.Lock()
	checkouts[typeName]++
	mu.Unlock()
}

// OnRelease is emitted at the tail of putXSlice in audit mode, after
// the pool.Put call. Pairs 1:1 with OnCheckout for a healthy lifecycle.
func OnRelease(typeName string) {
	mu.Lock()
	releases[typeName]++
	mu.Unlock()
}

// CheckZeroPrePut is emitted in putXSlice in audit mode immediately
// AFTER the generator's existing zero loop completes and BEFORE the
// pool.Put call. It reflects over the slice header and records a
// violation for every element that is not equal to the zero value of
// the element type — i.e. it asserts "the zero loop is still present
// and executed every live slot".
//
// `used` arrives as `any` because the audit package is type-erased;
// reflection extracts the element type and the zero comparison runs
// element-wise via reflect.DeepEqual against reflect.Zero. The
// generator emits this with the same `used` expression the zero loop
// iterated, so an omitted-loop seed surfaces here as one violation
// per non-zero element.
func CheckZeroPrePut(typeName string, used any) {
	if used == nil {
		return
	}
	rv := reflect.ValueOf(used)
	if rv.Kind() != reflect.Slice {
		mu.Lock()
		violations = append(violations, Violation{
			TypeName: typeName,
			Index:    -1,
			Detail:   fmt.Sprintf("expected slice, got %s", rv.Kind()),
		})
		mu.Unlock()
		return
	}
	elemType := rv.Type().Elem()
	zero := reflect.Zero(elemType).Interface()
	var found []Violation
	for i := 0; i < rv.Len(); i++ {
		got := rv.Index(i).Interface()
		if !reflect.DeepEqual(got, zero) {
			found = append(found, Violation{
				TypeName: typeName,
				Index:    i,
				Detail:   fmt.Sprintf("non-zero element at index %d: %#v", i, got),
			})
		}
	}
	if len(found) == 0 {
		return
	}
	mu.Lock()
	violations = append(violations, found...)
	mu.Unlock()
}

// Imbalanced returns the type names whose checkout count differs from
// release count, plus the magnitude of the gap. Empty result means
// every checkout was paired with a release.
func Imbalanced() []string {
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]struct{}{}
	for name := range checkouts {
		seen[name] = struct{}{}
	}
	for name := range releases {
		seen[name] = struct{}{}
	}
	var out []string
	for name := range seen {
		c := checkouts[name]
		r := releases[name]
		if c != r {
			out = append(out, fmt.Sprintf("%s: checkouts=%d releases=%d (delta=%d)", name, c, r, c-r))
		}
	}
	return out
}

// ZeroOnPutViolations returns a copy of the recorded violations. Tests
// assert len() == 0 for the happy path; the regression seed for the
// zero-loop bug class expects a non-empty result.
func ZeroOnPutViolations() []Violation {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Violation, len(violations))
	copy(out, violations)
	return out
}

// Snapshot returns a copy of the per-type counters at this instant.
// The async-stream barrier test reads it to assert "no releases yet"
// after dispatch returns but before the adapter unblocks.
func Snapshot() Counters {
	mu.Lock()
	defer mu.Unlock()
	cs := make(map[string]int, len(checkouts))
	for k, v := range checkouts {
		cs[k] = v
	}
	rs := make(map[string]int, len(releases))
	for k, v := range releases {
		rs[k] = v
	}
	return Counters{Checkouts: cs, Releases: rs}
}
