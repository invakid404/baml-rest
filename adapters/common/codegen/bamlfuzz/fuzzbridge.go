package bamlfuzz

import (
	"encoding/binary"
	"hash/fnv"
	"testing"

	"pgregory.net/rapid"
)

// MakeFuzz adapts rapid's bit-stream-driven generator model onto Go's
// testing.F []byte-driven fuzz model. The raw fuzz input is hashed with
// fnv-64a into a single uint64, which is then fed to rapid.MakeFuzz as
// 8 little-endian bytes. The hashing layer is what lets short inputs
// like {1, 2, 3} and their zero-padded extension {1, 2, 3, 0, 0, 0, 0, 0}
// drive distinct rapid runs — rapid.MakeFuzz's own bytes→uint64 routine
// pads with zeros, so the unhashed path would collapse those two inputs
// into the same bit stream.
//
// body runs once per fuzz invocation with the per-call *testing.T and a
// rapid *rapid.T configured from the derived seed. rapid handles its
// own Cleanup paths inside its checkOnce; the wrapping testing.T's
// Cleanup runs after body returns.
func MakeFuzz(f *testing.F, body func(*testing.T, *rapid.T)) {
	f.Helper()
	f.Fuzz(func(t *testing.T, raw []byte) {
		seed := SeedFromBytes(raw)
		var encoded [8]byte
		binary.LittleEndian.PutUint64(encoded[:], seed)
		rapid.MakeFuzz(func(rt *rapid.T) {
			body(t, rt)
		})(t, encoded[:])
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
