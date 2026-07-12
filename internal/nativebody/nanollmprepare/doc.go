// Package nanollmprepare holds the de-BAML Phase 4b gated, no-send nanollm
// OpenAI Prepare validation. The actual proof lives in the doubly build-tagged
// nanollm_integration_test.go (`nanollm_prebuilt && nanollm_integration`); this
// file exists only so the package always has a non-test source file (so tooling
// never sees an empty package when the tags are off).
//
// This package is a SEPARATE, test-only module (see go.mod) that is NOT part of
// the workspace, exactly so the private nanollm dependency stays out of the
// root/default module graph. Nothing in the default build imports it.
package nanollmprepare
