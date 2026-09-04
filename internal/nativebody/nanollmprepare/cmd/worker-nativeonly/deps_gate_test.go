//go:build nanollm_integration

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenDeps are the import paths the packaged native-only command MUST NOT
// reach — the deletion-substrate invariant. Matched as substrings, except the
// root generated package which is the module path exactly.
var forbiddenDeps = []string{
	"baml_client",
	"github.com/boundaryml/baml",
	"github.com/invakid404/baml-rest/dynclient",
	"dynclient/baml-patched",
	"language_client_go",
	"github.com/invakid404/baml-rest/internal/rootruntime",
	"github.com/invakid404/baml-rest/introspected",
	"github.com/invakid404/baml-rest/internal/workerboot",
	// ExecBridge-U1c: the standard-only oracle composite is BAML-AWARE (it consumes the
	// neutral BAML closures) and is imported ONLY by cmd/worker; the native-only command
	// must never reach it.
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/standardspineoracle",
	// M3e-A: the legacy/standard static-stream serve lane is BAML-plan-compare-bound
	// (it builds BAML's StreamRequest plan before its claim), so the native-only command
	// must reach neither it nor the generated static seam that installs it.
	"github.com/invakid404/baml-rest/nativeserve/canary",
}

// positiveDeps must be present so an empty/wrong go-list output cannot pass this
// gate by absence.
var positiveDeps = []string{
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/cmd/worker-nativeonly",
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativeonlyboot",
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated",
	"github.com/invakid404/baml-rest/nativeserve/spine",
	"github.com/invakid404/baml-rest/worker",
	"github.com/invakid404/baml-rest/workerplugin",
	"github.com/viktordanov/nanollm-ffi/go",
	// M3e-A: the STREAM path's own machinery must be PRESENT, so a dead or wrongly
	// generated graph cannot make the forbidden-list check pass by absence. These are
	// exactly the packages the spine stream executor links: the pre-socket admission
	// gate, the one-shot exact stream transport, the shared BAML-free delta cadence,
	// and the root-owned native static-stream parsers.
	"github.com/invakid404/baml-rest/nativeserve/admission",
	"github.com/invakid404/baml-rest/nativeserve/execute",
	"github.com/invakid404/baml-rest/bamlutils/buildrequest",
	"github.com/invakid404/baml-rest/internal/debaml",
}

// TestNativeOnlyWorkerHasNoBAML is the whole packaged-command dependency gate. It
// runs `GOWORK=off go list -deps` against the EXACT command package and tags built
// into cmd/serve/worker, and fails on any BAML/CFFI/dynclient/rootruntime/
// introspected/workerboot/root-baml_rest dependency. TestMain has already generated
// the deployment registry, so the debamlnativespinegenerated build sees the real
// aggregate. This is the acceptance gate; the container build runs the same check.
//
// M3e-A made this artifact STREAM-capable, so the positive list now names the stream
// path's own packages too: a graph that reached none of them would satisfy the
// forbidden list vacuously, which is exactly the false-green this gate must not allow.
func TestNativeOnlyWorkerHasNoBAML(t *testing.T) {
	_, moduleRoot := repoPaths()
	cmd := exec.Command("go", "list", "-deps", "-tags="+nativeOnlyBuildTags, "./cmd/worker-nativeonly")
	cmd.Dir = moduleRoot
	cmd.Env = append(cmd.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		t.Fatal("go list -deps returned no dependencies (wrong package/tags?)")
	}
	deps := strings.Split(trimmed, "\n")
	depSet := make(map[string]bool, len(deps))
	for _, d := range deps {
		d = strings.TrimSpace(d)
		depSet[d] = true
		// The root generated baml_rest package is the module path EXACTLY; matching
		// it as a substring would reject every first-party dependency.
		if d == "github.com/invakid404/baml-rest" {
			t.Errorf("native-only command reaches the root generated baml_rest package %q", d)
		}
		for _, bad := range forbiddenDeps {
			if strings.Contains(d, bad) {
				t.Errorf("native-only command depends on %q (matches forbidden %q)", d, bad)
			}
		}
	}
	for _, want := range positiveDeps {
		if !depSet[want] {
			t.Errorf("native-only command is missing expected dependency %q (an empty/wrong list must not pass by absence)", want)
		}
	}
}
