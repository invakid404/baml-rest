//go:build !nativeworkerartifact

package main

// hostEmbeddedWorkerNativeCapable reports whether THIS serve artifact embeds a
// NATIVE-CAPABLE worker — the de-BAML serving-cutover S2 artifact profile of the
// deployable unit (host binary + embedded worker), as opposed to what that
// worker is permitted to serve (S1 cohort policy) or told to do
// (BAML_REST_USE_DEBAML).
//
// It is an explicit COMPILE/DEPLOYMENT fact, exactly like nativeStreamServeCapable
// next to it, and it is deliberately a SEPARATE tag: nativestreamserve marks the
// narrower "the embedded worker installs the native STREAM serve factory" case
// (NATIVE_WORKER only), whereas the artifact profile covers every build that
// embeds an isolated-module, natively-linked worker — which includes the
// SHADOW_WORKER profile, whose worker links the native engine but wires no
// stream serve factory. Deriving the artifact profile from the stream tag would
// therefore mislabel a shadow artifact as BAML-only.
//
// This is the DEFAULT build: BAML-only. cmd/build/build.sh sets the
// `nativeworkerartifact` tag exactly when it builds the worker from the isolated
// nanollmprepare module, selecting the true variant in
// artifact_profile_native.go. The coupling is not merely assumed: build.sh
// stamps the artifact profile into the binary with -ldflags, and
// artifactprofile.Attest fails the process at startup if the stamp and this
// constant ever disagree.
const hostEmbeddedWorkerNativeCapable = false
