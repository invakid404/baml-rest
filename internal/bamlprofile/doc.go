// Package bamlprofile is baml-rest's leaf BAML profile over the pinned pure-Go
// minijinja fork github.com/invakid404/minijinja-go/v2@v2.16.0-baml.3.
//
// The fork is a generic, BAML-exact minijinja ENGINE. It deliberately does not
// carry BAML's environment configuration: a consumer still owns BAML's
// get_env() setup, host value model, custom globals, and the prompt/constraint
// lowering split (fork oracle/corpus.go documents this boundary). This package
// is that consumer. It supplies the BAML-rest-owned profile over the fork so
// that internal/nativeprompt and internal/debaml can, in later slices, render
// prompts and evaluate constraints byte-for-byte the way BAML v0.223 does,
// WITHOUT patching, wrapping, or calling BAML at runtime.
//
// # Dependency direction
//
//	fork minijinja-go  <-  internal/bamlprofile  <-  nativeprompt / debaml  <-  admission and serving
//
// This package imports the fork and Go stdlib only. It MUST NOT import
// internal/nativeprompt, internal/debaml, any serving code, nanollm, or the
// stock BAML runtime/CFFI. The shipped path is pure-Go, CGO-free, and
// host-zero-nanollm. Stock BAML v0.223 exists in this package only as a
// test-only differential oracle behind the `integration` build tag
// (see ./profileoracle), mirroring internal/nativeprompt/staticoracle.
//
// # What this slice (Slice 2 PR-1) builds
//
// The get_env engine configuration, sans the host value model:
//   - trim_blocks + lstrip_blocks, autoescape off, set_debug(true);
//   - the none -> "null" top-level formatter;
//   - the fork's BAML-exact builtin filter/test/function registry PLUS BAML's
//     get_env additions/overrides: regex_match and BAML's sum (see filters.go);
//   - the pycompat unknown-method callback;
//   - the simple globals `_` (role/chat helper) and `ctx` (output_format),
//     matching internal/nativeprompt/env.go.
//
// Authority: jinja_helpers.rs get_env() in BAML v0.223
// (engine/baml-lib/baml-core/src/ir/jinja_helpers.rs:7-36).
//
// # What is DEFERRED to later PRs (explicitly NOT built here)
//
//   - The host value model: enum/class/map/media host value objects, enum
//     globals, the enum ValueCmp that closes the #597 enum-`==` fence, and the
//     object-aware formatter dispatch (ObjectWithString) those need.
//   - The prompt lowerer and the constraint lowerer / predicate façade.
//
// New leaves a clear boundary for these (see env.go): the returned
// *minijinja.Environment is the get_env engine config; the host value model is
// layered on top by later slices (enum globals via AddGlobal; host values
// produced by the lowerers and passed as render context), never faked here.
package bamlprofile
