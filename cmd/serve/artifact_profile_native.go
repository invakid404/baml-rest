//go:build nativeworkerartifact

package main

// hostEmbeddedWorkerNativeCapable reports whether THIS serve artifact embeds a
// NATIVE-CAPABLE worker. See the default variant (artifact_profile_default.go)
// for the full contract.
//
// This is the de-BAML serving-cutover S2 STANDARD artifact: cmd/build/build.sh
// sets the `nativeworkerartifact` tag alongside the isolated-module worker embed
// (NATIVE_WORKER or SHADOW_WORKER), so the host knows — at compile time — that
// the worker bytes it carries link the native engine. It says nothing about what
// that worker serves: with the S1 cohort policy empty, or with
// BAML_REST_USE_DEBAML falsy, the artifact still serves 100% BAML.
const hostEmbeddedWorkerNativeCapable = true
