//go:build debamlworkerfixture && debamlworkerstaticfixture

package main

import (
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/staticworkerruntime"
	"github.com/invakid404/baml-rest/worker"
)

// De-BAML serving cutover S3b — the STATIC booted-artifact build fixture.
//
// fixture_runtime.go gives this entrypoint dynclient's ONE dynamic method. This
// file selects the other method table the cutover has to be proved against: a real
// STATIC (schema-defined) project, so the booted artifact exposes an actual
// `/call/<Method>` route with a real generated static serve seam behind it.
//
// It is a SUB-SELECTOR of debamlworkerfixture, not a second way in: without that
// tag this file is not built and fixture_runtime_off.go still installs nil, so the
// entrypoint's untagged guard (fixture_runtime_guard_test.go) is unaffected. One
// extra tag swaps one thing: WHICH BAML methods exist. The serve-profile options,
// the native factories, the flag-first branch and the admission predicate stay the
// shipped ones — which is the only reason driving a static route through this
// binary proves anything about the shipped artifact.
func fixtureRuntime() worker.Runtime { return staticworkerruntime.New() }
