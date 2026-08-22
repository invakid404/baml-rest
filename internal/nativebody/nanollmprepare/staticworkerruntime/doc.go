// Package staticworkerruntime exposes the committed, generated STATIC BAML client
// of internal/nativeprompt/testdata/staticserve_fixture as a neutral
// [github.com/invakid404/baml-rest/worker.Runtime], so a SUBPROCESS worker binary
// can be built with a real STATIC (schema-defined) method table without a
// container build.
//
// # Why this exists
//
// It is the STATIC twin of dynclient/workerruntime, and it exists for a reason the
// dynamic one does not have to argue: baml-rest's root `adapter.go` is the
// "overwritten during build" stub, so an artifact built from a checkout knows NO
// methods at all — and dynclient's generated client supplies only the ONE dynamic
// method. That leaves the serving cutover's static-surface claim ("flag on, the
// fe-v1 enrollment present, a real static `/call` still declines pre-socket and
// BAML serves it unchanged") with nothing on a booted artifact to send a request
// to. A test that sends an UNKNOWN method name instead proves nothing: the worker
// rejects it before any route, factory, adapter or admission gate is reached.
//
// staticserve_fixture is a real, compilable BAML project — its own baml_src, its
// own baml_client, its own introspected package, and a generated adapter carrying
// the de-BAML STATIC serve seam (generated/debaml_static.go). It already backs the
// gated generated-route static serve proofs. This package is the one-file bridge
// that lets the isolated nanollm worker entrypoint link it as its method table.
//
// # Why the whole thing is behind a build tag
//
// This is BUILD-FIXTURE surface, not product surface. runtime.go carries
// `//go:build debamlworkerstaticfixture`, so a released consumer building this
// module normally links NOTHING from here and the package is empty. The tag is set
// by exactly one thing: scripts/build-s3b-static-fixture-artifact.sh, which builds
// the static-capable worker entrypoint for the booted-artifact static proof.
// cmd/build never sets it, and the entrypoint's own untagged guard
// (fixture_runtime_guard_test.go) still requires a shipped build to install NO
// runtime override at all.
//
// It confers no authority. It selects the method table, not what may be claimed
// natively — which remains the immutable cohort enrollment's answer, and that
// enrollment names the dynamic unary call surface ONLY. An artifact built from
// this runtime is subject to exactly the same admission predicate as the shipped
// one, which is the entire point of driving a real static route through it.
package staticworkerruntime
