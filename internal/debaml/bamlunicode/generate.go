package bamlunicode

// Regenerate the committed *_gen.go tables and the provenance manifest from the
// pinned, SHA-256-verified UCD inputs under testdata/ucd. The generator never
// downloads: the raw UCD text lives in the generator cache (testdata/ucd, kept
// out of the production embed bundle via .embedignore) and is hash-checked
// before use. Run `go run ./internal/debaml/bamlunicode/cmd/gen -check` to
// verify the committed outputs are byte-for-byte fresh.
//
//go:generate go run ./cmd/gen
