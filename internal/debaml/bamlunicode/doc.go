// Package bamlunicode reproduces, byte-for-byte, the Unicode primitives the
// stock BoundaryML BAML 0.223.0 CFFI matcher (match_string) uses when it
// lowercases, normalizes, strips punctuation, and trims whitespace before
// comparing strings, enum values, and literals.
//
// It exists because BAML's observable matcher combines two INDEPENDENTLY
// versioned Unicode datasets (case/properties = Unicode 17.0.0 via rustc 1.93.0
// std; normalization = Unicode 16.0.0 via unicode-normalization 0.1.24). No
// single Go/x/text configuration reproduces that pair, so the tables here are
// generated from pinned UCD 17.0.0 and 16.0.0 inputs and committed. See
// versions.go for the full compatibility profile.
//
// Guarantees:
//
//   - The runtime consults ONLY these committed tables. It never reads the host
//     Go toolchain's Unicode tables (unicode.*, golang.org/x/text), never
//     branches on runtime.GOOS/GOARCH/Go version, and never touches the network.
//   - The tables are regenerated deterministically by ./cmd/gen from
//     SHA-256-pinned UCD inputs; `go run ./internal/debaml/bamlunicode/cmd/gen
//     -check` fails on any drift.
//   - Correctness is proved against an exact Rust 1.93.0 / unicode-normalization
//     0.1.24 reference (testdata/reference) for every scalar and against the
//     Unicode 16.0.0 NormalizationTest.txt conformance suite.
//
// Public API (named for BAML matcher semantics):
//
//	LowerString   -> Rust str::to_lowercase (Unicode 17.0.0, incl. Final_Sigma)
//	NFKD          -> unicode_normalization nfkd (Unicode 16.0.0)
//	IsCombiningMark -> unicode_normalization::char::is_combining_mark (Mn/Mc/Me, 16.0.0)
//	IsAlphanumeric -> char::is_alphanumeric (Alphabetic OR GC=N, Unicode 17.0.0)
//	IsWhitespace / TrimSpace -> char::is_whitespace / str::trim (Unicode 17.0.0)
//
// This package is additive and unrouted: Slice 1 of #555 proves the data and
// algorithms; Slice 2 swaps match_string onto them. It must not be used to make
// coercion claims until Slice 2 lands.
package bamlunicode
