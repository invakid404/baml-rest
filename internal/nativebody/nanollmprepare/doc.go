// Package nanollmprepare holds the de-BAML Phase 4b gated, no-send nanollm
// OpenAI Prepare validation. The actual proof lives in the build-tagged
// nanollm_integration_test.go (`nanollm_integration`); this file exists only so
// the package always has a non-test source file (so tooling never sees an empty
// package when the tag is off).
//
// This module is no longer no-send-only: the reusable serve core it once hosted
// (admission/execute/parity/canary + the testutil/planassert helpers) now lives in
// the public, go-get-able module
// [github.com/invakid404/baml-rest/nativeserve] (#624), which this module imports
// via a replace directive. That relocation is what lets an in-process dynclient
// consumer link the same nanollm-backed native serve implementation the subprocess
// worker uses. Its [github.com/invakid404/baml-rest/nativeserve/execute] package
// hosts Slice 2's gated, test-only FUNCTIONAL-SEND suite, which drives nanollm's
// real Do/DoStream over loopback against a go-mocklm subprocess. That package is
// likewise `//go:build nanollm_integration`-gated and imports no BAML runtime;
// see its execute/doc.go for the loopback-only / no-BAML invariants. This Prepare
// package and the static/ P5 differential leg still perform NO send; the dynamic/
// leg's Slice-6c live differential (dynamic/live_*) drives execute.RunAttempt over
// a loopback HTTP transport, so it sends ONLY to a local capture / go-mocklm server
// (never to a real provider) for one-request-per-leg differential parity.
//
// This package is a SEPARATE, test-only module (see go.mod) that is NOT part of
// the workspace, exactly so the public github.com/viktordanov/nanollm-ffi/go
// dependency — a cgo package that embeds and links a prebuilt Rust FFI archive —
// stays out of the root/default module graph. Nothing in the default build
// imports it.
package nanollmprepare
