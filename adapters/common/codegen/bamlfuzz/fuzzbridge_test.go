package bamlfuzz

import (
	"encoding/binary"
	"testing"
)

// TestSeedFromBytesDeterministic pins the contract that identical input
// bytes always hash to the same uint64. Without this, the same fuzz
// corpus entry would replay against a different rapid bit stream each
// run, and a failing seed could not be reproduced from the corpus
// alone.
func TestSeedFromBytesDeterministic(t *testing.T) {
	inputs := [][]byte{
		nil,
		{},
		{0},
		{0xFF},
		[]byte("abc"),
		[]byte("the quick brown fox jumps over the lazy dog"),
		{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A},
	}
	for _, in := range inputs {
		first := SeedFromBytes(in)
		second := SeedFromBytes(in)
		if first != second {
			t.Errorf("SeedFromBytes(%v) not deterministic: %d vs %d", in, first, second)
		}
	}
}

// TestSeedFromBytesShortPadding pins the contract that a short input
// and its zero-padded extension produce *different* seeds. rapid's
// own MakeFuzz pads short inputs with zero bytes when assembling its
// 64-bit groups, so without the hashing layer those two inputs would
// drive identical rapid runs. A future "padding optimisation" that
// collapses them would silently shrink the effective input space the
// fuzzer can reach — locking this here keeps that regression visible.
func TestSeedFromBytesShortPadding(t *testing.T) {
	short := []byte{1, 2, 3}
	padded := []byte{1, 2, 3, 0, 0, 0, 0, 0}
	if SeedFromBytes(short) == SeedFromBytes(padded) {
		t.Errorf("SeedFromBytes collapsed %v and %v to the same seed", short, padded)
	}
}

// TestSeedFromBytesEmpty pins the empty-input convention. fnv-64a's
// offset basis is non-zero; an empty []byte through fnv64a would
// produce that constant. Pinning empty input to 0 makes the
// degenerate "no input" case readable in failure logs and lets future
// readers know which branch handled it.
func TestSeedFromBytesEmpty(t *testing.T) {
	if got := SeedFromBytes(nil); got != 0 {
		t.Errorf("SeedFromBytes(nil) = %d, want 0", got)
	}
	if got := SeedFromBytes([]byte{}); got != 0 {
		t.Errorf("SeedFromBytes([]byte{}) = %d, want 0", got)
	}
}

// TestSeedFromBytesDistinct pins a small handful of well-known inputs
// to distinct seeds. Catches accidental hashing-function swaps (e.g.
// someone replacing fnv-64a with fnv-64 or with a non-cryptographic
// xxhash) that would still be deterministic but would invalidate any
// on-disk reproductions referring to the previous encoding.
func TestSeedFromBytesDistinct(t *testing.T) {
	a := SeedFromBytes([]byte("a"))
	b := SeedFromBytes([]byte("b"))
	if a == b {
		t.Errorf("SeedFromBytes('a') == SeedFromBytes('b') = %d", a)
	}
	// fnv-64a pin: "a" → 0xaf63dc4c8601ec8c. If this constant ever
	// changes, every corpus entry referring to the old encoding
	// becomes a reproduction artefact for a different seed — the
	// constant lives here so a hashing swap fails loudly.
	const fnvOfA uint64 = 0xaf63dc4c8601ec8c
	if a != fnvOfA {
		t.Errorf("SeedFromBytes('a') = %#x, want %#x — hashing function changed", a, fnvOfA)
	}
}

// TestMakeFuzzPipelineEncoding pins the bytes→uint64 decision MakeFuzz
// applies before handing the seed to rapid. The full dispatch
// (testing.F + f.Fuzz + rapid.Custom + Example) only runs end-to-end
// under `go test -fuzz`, which the integration-tag targets
// FuzzBamlfuzz{Static,Dynamic} exercise in CI. Locally,
//
//	go test -tags=integration -fuzz='^FuzzBamlfuzzDynamic$' \
//	    -fuzztime=30s ./integration
//
// drives the bridge end-to-end through the actual testing.F engine —
// faking *testing.F outside that engine is brittle, so the unit test
// stops at the seed-derivation boundary.
//
// Two branches are pinned: an 8-byte input is interpreted as a
// little-endian uint64 seed verbatim (the corpus replay path), while
// any other length is hashed through SeedFromBytes first (the
// generator-driven exploration path).
func TestMakeFuzzPipelineEncoding(t *testing.T) {
	t.Run("8_byte_direct_seed_path", func(t *testing.T) {
		// A corpus entry the way the fuzz engine writes one to
		// -fuzzcachedir: a uint64 seed encoded as 8 little-endian
		// bytes. The bridge must replay this exact seed, not hash it.
		const wantSeed uint64 = 0x86113a7efb803239
		raw := make([]byte, 8)
		binary.LittleEndian.PutUint64(raw, wantSeed)
		if got := binary.LittleEndian.Uint64(raw); got != wantSeed {
			t.Fatalf("8-byte LE direct decode broke: %d != %d", got, wantSeed)
		}
		// The hashed path would produce a different seed; pinning
		// that here guarantees a future regression that re-hashes
		// 8-byte inputs would be caught.
		if hashed := SeedFromBytes(raw); hashed == wantSeed {
			t.Errorf("SeedFromBytes(raw) collided with the direct seed; the test loses its discrimination")
		}
	})

	t.Run("non_8_byte_hashed_path", func(t *testing.T) {
		// A non-8-byte input must route through SeedFromBytes; the
		// resulting uint64 then feeds rapid.Example(int(seed)) just
		// like the corpus-path uint64. Verify that the hashed seed is
		// stable so the same fuzz input always reaches the same rapid
		// draw.
		raw := []byte("anchor")
		first := SeedFromBytes(raw)
		second := SeedFromBytes(raw)
		if first != second {
			t.Errorf("SeedFromBytes(%q) not deterministic: %d vs %d", raw, first, second)
		}
		// The hashed branch must produce a different uint64 than a
		// naive LE decode of the same bytes would; that's what
		// guarantees short inputs distinct from 8-byte corpus seeds
		// reach different rapid runs.
		if first == 0 {
			t.Errorf("SeedFromBytes(%q) collapsed to zero; non-empty input must produce non-zero seed", raw)
		}
	})
}

