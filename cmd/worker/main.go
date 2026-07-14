// Command worker is the BAML-only subprocess worker: the default distributed
// build and the immediate-reversal target for the de-BAML cutover. It links
// patched BAML (via the root generated runtime) but NOT nanollm, and delegates
// all startup to the shared bootstrap in internal/workerboot. The BAML+nanollm
// worker is a separate main in the isolated, out-of-go.work nanollmprepare
// module; both share workerboot so their startup stays identical apart from the
// injected native capability. Keeping this binary BAML-only is what makes
// "flag-off + BAML-only worker = 100% BAML" a build-level reversal.
package main

import (
	"github.com/invakid404/baml-rest/internal/workerboot"
)

func main() {
	// Zero Options: no native capability, no native init — a pure BAML worker.
	workerboot.Run(workerboot.Options{})
}
