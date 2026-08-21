//go:build debamlworkerstaticfixture

package staticworkerruntime

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	"github.com/invakid404/baml-rest/worker"
)

// Runtime presents the staticserve fixture's generated STATIC BAML client as the
// neutral worker.Runtime a worker entrypoint installs. Its method table is the
// fixture project's own functions — real schema-defined static methods, each with
// the generated de-BAML static `/call` seam — and it knows no dynamic method,
// which is exactly the surface the serving cutover does NOT enroll.
//
// See the package doc for why this exists and why it is tag-gated.
type Runtime struct{}

// InitRuntime loads the fixture project's BAML runtime exactly once.
func (Runtime) InitRuntime() { fixture.InitBamlRuntime() }

// Method looks up a streaming BAML method by name, preserving the (value, ok)
// shape the worker handler relies on for its "method %q not found" path.
func (Runtime) Method(name string) (bamlutils.StreamingMethod, bool) {
	method, ok := fixture.Methods[name]
	return method, ok
}

// ParseMethod looks up a parse-only BAML method by name. Same (value, ok) contract.
func (Runtime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	method, ok := fixture.ParseMethods[name]
	return method, ok
}

// MakeAdapter delegates to the fixture's generated MakeAdapter — the same
// indirection dynclient's generated runtime uses.
func (Runtime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return fixture.MakeAdapter(ctx)
}

// New returns the runtime value a worker entrypoint installs.
func New() worker.Runtime { return Runtime{} }
