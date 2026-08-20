// Package workerruntime exposes dynclient's committed, generated dynamic BAML
// client as a neutral [github.com/invakid404/baml-rest/worker.Runtime], so a
// SUBPROCESS worker binary can be built with a real `Baml_Rest_Dynamic` method
// table without a container build.
//
// # Why this exists
//
// baml-rest's root package ships `adapter.go` as the "overwritten during build"
// stub: `Methods` is empty until the CONTAINER build generates a client from the
// deployment's own BAML project. That is fine for production and fatal for proof —
// a native-capable worker binary built from a checkout knows no methods, so it
// cannot be sent a `/call` request, and the de-BAML serving cutover's central claim
// ("flag on, empty policy, ZERO native claims on the deployed route") had no way to
// be exercised end to end against a booted artifact.
//
// dynclient already carries exactly the missing piece: `dynclient/internal/generated`
// is the committed, regenerated dynamic client for `cmd/build/dynamic.baml` —
// the same `Baml_Rest_Dynamic`, the same generated de-BAML seam, the same native
// serve callback wiring the container build produces. It is `internal` to dynclient,
// so this package is the one-line bridge that lets the isolated nanollm worker
// module link it.
//
// # Why the whole thing is behind a build tag
//
// This is BUILD-FIXTURE surface, not product surface. Every file that defines
// anything carries `//go:build debamlworkerfixture`, so a released consumer building
// this module normally links NOTHING from here and the package is empty. The tag is
// set by exactly one thing: scripts/build-s3a-fixture-artifact.sh, which builds the
// native-capable worker entrypoint for the booted-artifact proof.
//
// It confers no authority in any case. The runtime it exposes is the same dynamic
// client any dynclient consumer already gets from dynclient.New, presented through
// the neutral worker.Runtime interface; it carries no cohort identity, no policy,
// and no native factory.
package workerruntime
