//go:build debamlworkerfixture && !debamlworkerstaticfixture

package main

import (
	"github.com/invakid404/baml-rest/dynclient/workerruntime"
	"github.com/invakid404/baml-rest/worker"
)

// De-BAML serving cutover S3a — the BOOTED-ARTIFACT BUILD FIXTURE.
//
// # What this is for
//
// The cutover's central claims are about what a native-capable artifact does on the
// DEPLOYED `/call` route: with the umbrella flag ON it makes ZERO native claims for
// every configuration the shipped policy does not enroll (S3a), and serves the ONE
// it does enroll natively, with one upstream request and both retained BAML oracles
// running (S3b). Proving either requires sending a request to a booted
// artifact — and an artifact built from a checkout cannot receive one: baml-rest's
// root `adapter.go` is the "overwritten during build" stub, so `Methods` is empty
// until the CONTAINER build generates a client from the deployment's BAML project.
//
// So this file, under an opt-in build tag, gives THIS ENTRYPOINT — the shipped
// serve-profile worker, not a lookalike — a real `Baml_Rest_Dynamic` method table,
// by linking dynclient's committed generated dynamic client. Everything else about
// the binary is identical to the shipped one by construction: the same main(), the
// same flag-first branch, the same serve-profile options, the same native factories,
// the same attestation stamp. One build tag swaps one thing: which BAML methods
// exist.
//
// # Why it is safe
//
// It confers NO authority. It selects the method table, not what may be claimed
// natively — which remains the immutable cohort enrollment's answer, and that is
// empty. A fixture binary is subject to exactly the same admission predicate as the
// shipped one, which is the entire point: the proof is only worth something if the
// thing under test is the shipped predicate.
//
// It cannot reach a released build. Every file it depends on carries this tag
// (dynclient/workerruntime), the tag is set by exactly one script
// (scripts/build-s3a-fixture-artifact.sh), and cmd/build never sets it.
func fixtureRuntime() worker.Runtime { return workerruntime.New() }
