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
// Two input shapes are recognised:
//
//   - Exactly 8 bytes: treated as a little-endian uint64 seed and
//     forwarded to rapid verbatim. The seed corpus the integration
//     fuzz targets ship via f.Add encodes `dynamicSeedFor` /
//     `staticSeedFor` outputs this way, so corpus entries replay the
//     exact rapid bit stream the rapid oracle subtree already covers.
//   - Anything else (including the empty input): hashed with fnv-64a
//     into a single uint64 (see SeedFromBytes) and then forwarded to
//     rapid as 8 little-endian bytes. The hashing layer is what lets
//     short inputs like {1, 2, 3} and their zero-padded extension
//     {1, 2, 3, 0, 0, 0, 0, 0} drive distinct rapid runs —
//     rapid.MakeFuzz's own bytes→uint64 routine pads with zeros, so
//     an unhashed path for non-8-byte inputs would collapse those two
//     into the same bit stream.
//
// body runs once per fuzz invocation with the per-call *testing.T and
// a rapid *rapid.T configured from the derived seed. rapid handles
// its own Cleanup paths inside its checkOnce; the wrapping
// testing.T's Cleanup runs after body returns.
func MakeFuzz(f *testing.F, body func(*testing.T, *rapid.T)) {
	f.Helper()
	f.Fuzz(func(t *testing.T, raw []byte) {
		var seed uint64
		if len(raw) == 8 {
			seed = binary.LittleEndian.Uint64(raw)
		} else {
			seed = SeedFromBytes(raw)
		}
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
