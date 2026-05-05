package pool

import (
	"testing"
)

// TestConvertSharedStateSeeds_NegativeCastSemantics pins the
// host-side seed-conversion contract: negative `start` values are
// reinterpreted as unsigned, matching BAML's
// `(start as usize) % strategy.len()` cast on 64-bit. The
// SharedStateStore counter wire format is uint64, so the bit pattern
// must be stored verbatim — not clamped to zero — for the centralised
// host counter to dispatch the same child BAML's per-worker runtime
// would. A clamp-to-zero regression here is the original host-side
// bug; it would not be caught by the in-process Coordinator's tests
// because pool.New consumes the seeds via SharedStateStore, a
// separate code path.
func TestConvertSharedStateSeeds_NegativeCastSemantics(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want uint64
	}{
		// Bit-identical reinterpretation. uint64(int(-1)) on 64-bit
		// is 0xFFFFFFFFFFFFFFFF — the same value Rust's
		// `(-1i32 as usize)` produces, so the subsequent
		// `% childCount` modulo agrees across the language seam.
		{"-1 cast to uint64 max", -1, 0xFFFFFFFFFFFFFFFF},
		{"-7 cast to (uint64 max)-6", -7, 0xFFFFFFFFFFFFFFF9},
		// Non-negative values pass through unchanged. These guard
		// against an over-eager fix that touched the positive arm
		// of the conversion.
		{"0 stays 0", 0, 0},
		{"1 stays 1", 1, 1},
		{"42 stays 42", 42, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seeds := convertSharedStateSeeds(map[string]int{"k": tc.in})
			got, ok := seeds["k"]
			if !ok {
				t.Fatalf("missing key in converted seeds; got %v", seeds)
			}
			if got != tc.want {
				t.Errorf("convertSharedStateSeeds(%d) = %#x, want %#x (cast-and-store, not clamp-to-zero)",
					tc.in, got, tc.want)
			}
		})
	}
}

// TestConvertSharedStateSeeds_MapIsSnapshot pins the snapshot
// contract: the helper returns a fresh map so post-construction
// mutations of the caller's map don't bleed into the running
// SharedStateStore. A regression that returned the input map
// directly (or a shared backing array) would silently let the
// caller race the worker pool.
func TestConvertSharedStateSeeds_MapIsSnapshot(t *testing.T) {
	in := map[string]int{"k": 5}
	seeds := convertSharedStateSeeds(in)

	in["k"] = 99
	in["new"] = 7

	if seeds["k"] != 5 {
		t.Errorf("post-conversion mutation of input changed snapshot: got %d, want 5", seeds["k"])
	}
	if _, leaked := seeds["new"]; leaked {
		t.Errorf("post-conversion key insertion leaked into snapshot: got %v", seeds)
	}
}

// TestConvertSharedStateSeeds_NilAndEmpty pins the boundary cases.
// The helper accepts nil and empty inputs and returns an empty
// (non-nil) map; callers can range over the result unconditionally.
func TestConvertSharedStateSeeds_NilAndEmpty(t *testing.T) {
	for _, in := range []map[string]int{nil, {}} {
		seeds := convertSharedStateSeeds(in)
		if seeds == nil {
			t.Errorf("expected non-nil result for input %v, got nil", in)
		}
		if len(seeds) != 0 {
			t.Errorf("expected empty result for input %v, got %v", in, seeds)
		}
	}
}
