// Package nanollmprepare holds the de-BAML Phase 4b gated, no-send nanollm
// OpenAI Prepare validation. The actual proof lives in the build-tagged
// nanollm_integration_test.go (`nanollm_integration`); this file exists only so
// the package always has a non-test source file (so tooling never sees an empty
// package when the tag is off).
//
// This module is no longer no-send-only: its sibling package
// [github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/execute]
// hosts Slice 2's gated, test-only FUNCTIONAL-SEND suite, which drives nanollm's
// real Do/DoStream over loopback against a go-mocklm subprocess. That package is
// likewise `//go:build nanollm_integration`-gated and imports no BAML runtime;
// see its execute/doc.go for the loopback-only / no-BAML invariants. This Prepare
// package and its P5 differential legs (static/, dynamic/) still perform NO send.
//
// This package is a SEPARATE, test-only module (see go.mod) that is NOT part of
// the workspace, exactly so the public github.com/viktordanov/nanollm-ffi/go
// dependency — a cgo package that embeds and links a prebuilt Rust FFI archive —
// stays out of the root/default module graph. Nothing in the default build
// imports it.
package nanollmprepare
