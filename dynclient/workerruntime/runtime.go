//go:build debamlworkerfixture

package workerruntime

import (
	"github.com/invakid404/baml-rest/dynclient/internal/generated"
	"github.com/invakid404/baml-rest/worker"
)

// Runtime is dynclient's generated dynamic BAML client, presented as the neutral
// worker.Runtime a worker entrypoint installs. It knows `Baml_Rest_Dynamic` and
// nothing else, which is exactly the surface the de-BAML serving cutover enrolls.
//
// See the package doc for why this exists and why it is tag-gated.
type Runtime = generated.Runtime

// New returns the runtime value a worker entrypoint installs.
func New() worker.Runtime { return Runtime{} }
