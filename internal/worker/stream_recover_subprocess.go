//go:build subprocess

package worker

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// recoverBridgePanic is a no-op in subprocess builds. The worker runs
// in a separate OS process; a panic terminates that process and the
// pool's restart loop replaces it. Adding a deferred recover() here
// would silently absorb panics that operators today rely on as a hard
// failure signal.
func recoverBridgePanic(_ context.Context, _ chan<- *workerplugin.StreamResult, _ bamlutils.Logger) {
}
