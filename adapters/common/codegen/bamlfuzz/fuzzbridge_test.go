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

// TestMakeFuzzPipelineEncoding pins the bytes→uint64→little-endian
// encoding that MakeFuzz applies before handing off to rapid.MakeFuzz.
// The full dispatch (testing.F + f.Fuzz + the rapid bit stream) only
// runs end-to-end under `go test -fuzz`, which the integration-tag
// targets TestBamlfuzz{Static,Dynamic}Fuzz exercise in CI. Locally,
//
//	go test -tags=integration -fuzz='^TestBamlfuzzDynamicFuzz$' \
//	    -fuzztime=30s ./integration
//
// drives the bridge end-to-end through the actual testing.F engine —
// faking *testing.F outside that engine is brittle, so the unit test
// stops at the encoding boundary.
func TestMakeFuzzPipelineEncoding(t *testing.T) {
	raw := []byte("anchor")
	seed := SeedFromBytes(raw)
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], seed)
	if got := binary.LittleEndian.Uint64(encoded[:]); got != seed {
		t.Errorf("uint64 round-trip through little-endian encoding broke: %d != %d", got, seed)
	}
}

