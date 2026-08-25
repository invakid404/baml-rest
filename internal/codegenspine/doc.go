// Package codegenspine carries the M0 "contract freeze" artifacts for the
// rank-1 native codegen spine (see docs/codegen-spine/). It is a passive,
// dependency-light package: a checked-in machine-readable manifest
// (manifest.json) plus tests that validate that manifest against the LIVE
// contract types in the tree (never against an aspirational shape) and a
// source-guard that proves this pin/tar-INDEPENDENT slice touched none of the
// collision paths.
//
// M0 deliberately ships NO descriptor package, NO codegen, and NO runtime
// change. Everything here is documentation-adjacent freeze data plus the test
// harness that keeps it honest. The design decisions the manifest encodes are
// recorded, with citations, under docs/codegen-spine/.
//
// The package is named codegenspine so a later milestone (M1+) can grow a real
// projectdescriptor consumer beside it, but M0 exposes only the manifest
// loader.
package codegenspine
