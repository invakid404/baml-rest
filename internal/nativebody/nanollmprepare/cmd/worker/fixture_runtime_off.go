//go:build !debamlworkerfixture

package main

import "github.com/invakid404/baml-rest/worker"

// fixtureRuntime is NIL in every shipped build of this entrypoint, which is what
// selects the root generated package — the deployment's own BAML project, written
// into it by the container build. This file is the default; the tagged twin exists
// only for the build fixture described in fixture_runtime.go.
func fixtureRuntime() worker.Runtime { return nil }
