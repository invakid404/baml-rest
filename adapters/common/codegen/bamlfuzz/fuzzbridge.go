package bamlfuzz

import (
	"encoding/binary"
	"hash/fnv"
	"testing"

	"pgregory.net/rapid"
)

// MakeFuzz adapts rapid's bit-stream-driven generator model onto Go's
// testing.F []byte-driven fuzz model.
//
// Each fuzz invocation becomes a single uint64 seed that drives a
// PRNG-backed rapid bit stream; body then draws as many bits as it
// needs without the input []byte capping draw count. Two input shapes
// are recognised:
//
//   - Exactly 8 bytes: interpreted as a little-endian uint64 seed
//     verbatim. The nightly workflow's persisted -fuzzcachedir corpus
//     stores its entries this way, so a restored corpus entry replays
//     the exact rapid stream the engine recorded.
//   - Anything else (including the empty input): hashed with fnv-64a
//     into a single uint64 (see SeedFromBytes). The hashing layer is
//     what lets short inputs like {1, 2, 3} and their zero-padded
//     extension {1, 2, 3, 0, 0, 0, 0, 0} drive distinct rapid runs;
//     without it both prefixes would collapse to the same seed.
//
// The seed feeds rapid via `rapid.Custom(...).Example(int(seed))`,
// matching the canonical seeded-draw mechanism integration/buildRapidCase
// already uses. Unlike rapid.MakeFuzz — which converts the raw bytes
// into a fixed buffer and panics with `invalidData("overrun")` once
// draws exhaust it — Example installs a randomBitStream seeded from
// the uint64, so the body gets unlimited draws; a multi-Draw property
// then runs to completion where the buffer-backed path would skip on
// the second draw.
//
// body runs once per fuzz invocation with the per-call *testing.T and
// a rapid *rapid.T configured from the derived seed. rapid handles its
// own Cleanup paths inside Example; the wrapping testing.T's Cleanup
// runs after body returns.
func MakeFuzz(f *testing.F, body func(*testing.T, *rapid.T)) {
	f.Helper()
	f.Fuzz(func(t *testing.T, raw []byte) {
		var seed uint64
		if len(raw) == 8 {
			seed = binary.LittleEndian.Uint64(raw)
		} else {
			seed = SeedFromBytes(raw)
		}
		rapid.Custom(func(rt *rapid.T) struct{} {
			body(t, rt)
			return struct{}{}
		}).Example(int(seed))
	})
}

// SeedFromBytes derives a deterministic uint64 from raw fuzz input via
// fnv-64a. Empty input pins to 0 (the fnv-64a offset basis would yield
// a non-zero seed for a zero-byte hash; pinning empty input to 0
// matches the "no input → degenerate seed" intuition and keeps the
// helper trivially reproducible across Go releases).
//
// Exported so the unit tests can pin the encoding without going
// through MakeFuzz, and so future tooling (e.g. corpus minimisation
// scripts) can compute the same seed without re-deriving the hashing
// rule from the source.
func SeedFromBytes(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}
