//go:build integration && nanollm_integration

package provideroracle

// Stock-BAML capture helpers + version guards for the TPBP-A Anthropic oracle.
//
// The DIAGNOSTIC oracle is the real stock BAML v0.223.0 provider request, built
// in-memory through the public Go API (baml.CreateRuntime + runtime.BuildRequest)
// with NO socket and NO checked-in generated source: each row hands BAML a tiny
// `main.baml` string with fixed FAKE creds / model / base / prompt and reads the
// pre-send Request (method, URL, header map, exact Body.Text()). A version guard
// pins the loaded CFFI runtime AND this module's go.mod require to stock
// v0.223.0, so a green result can never be attributed to a different runtime.

import (
	"context"
	"os"
	"runtime/debug"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
	"golang.org/x/mod/modfile"

	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

const (
	// moduleGoModPath is this test-only module's go.mod (it — not root — pins the
	// stock BAML + nanollm the oracle binary links), relative to this package dir.
	moduleGoModPath = "../go.mod"

	bamlModulePath         = "github.com/boundaryml/baml"
	wantBAMLModuleVersion  = "v0.223.0"
	wantBAMLRuntimeVersion = "0.223.0"

	nanollmModulePath        = "github.com/viktordanov/nanollm-ffi/go"
	wantNanollmModuleVersion = "v0.4.3"
	// wantNanollmRuntimeVersion is the EXACT embedded Rust crate version the loaded
	// engine must report — pinned in-binary just like the BAML runtime guard, so a
	// mismatched native engine cannot green-light the oracle.
	wantNanollmRuntimeVersion = "0.4.3"
)

// bamlCapture is a captured stock-BAML Anthropic request: the runtime-neutral
// snapshot (method/URL/normalized headers/body) plus the exact body bytes and the
// raw header map (retained for the one-sided baml-original-url divergence proof).
type bamlCapture struct {
	snap    testutil.Snapshot
	body    []byte
	headers map[string]string
}

// captureBAMLAnthropic builds the in-memory BAML project from src, drives
// BuildRequest for fn with kwargs (adding the stock `stream:false` arg the
// generated client always passes), and returns the pre-send capture. It fails
// closed on an empty body, a missing method/URL, or duplicate/multi-value
// headers. It NEVER sends. Per the secret-free failure contract it reports only a
// fixed STAGE token (and the fixed function name) on error — never the raw
// CFFI/runtime error, which could echo the generated source, the prompt, the
// fake credential, or the body bytes.
func captureBAMLAnthropic(t *testing.T, src, fn string, kwargs map[string]any) bamlCapture {
	t.Helper()

	rt, err := baml.CreateRuntime(".", map[string]string{"main.baml": src}, map[string]string{})
	if err != nil {
		t.Fatal("BAML capture failed at stage=create_runtime")
	}

	kw := make(map[string]any, len(kwargs)+1)
	for k, v := range kwargs {
		kw[k] = v
	}
	kw["stream"] = false // the generated Request.<fn> always encodes this
	args := baml.BamlFunctionArguments{Kwargs: kw, Env: map[string]string{}}
	encoded, err := args.Encode()
	if err != nil {
		t.Fatal("BAML capture failed at stage=encode_args")
	}

	req, err := rt.BuildRequest(context.Background(), fn, encoded)
	if err != nil {
		t.Fatalf("BAML capture failed at stage=build_request (fn=%s)", fn)
	}
	method, err := req.Method()
	if err != nil {
		t.Fatal("BAML capture failed at stage=method")
	}
	url, err := req.Url()
	if err != nil {
		t.Fatal("BAML capture failed at stage=url")
	}
	hdrs, err := req.Headers()
	if err != nil {
		t.Fatal("BAML capture failed at stage=headers")
	}
	bodyObj, err := req.Body()
	if err != nil {
		t.Fatal("BAML capture failed at stage=body")
	}
	bodyText, err := bodyObj.Text()
	if err != nil {
		t.Fatal("BAML capture failed at stage=body_text")
	}
	if bodyText == "" {
		t.Fatal("BAML body is empty — the request build failed before producing a body")
	}
	if method == "" || url == "" || len(hdrs) == 0 {
		t.Fatalf("incomplete BAML capture: method_empty=%v url_empty=%v headers=%d", method == "", url == "", len(hdrs))
	}

	body := []byte(bodyText)
	snap, err := testutil.NewSnapshot(method, url, testutil.PairsFromStringMap(hdrs), body)
	if err != nil {
		t.Fatal("BAML request has a duplicate/multi-value header (stage=snapshot)")
	}
	return bamlCapture{snap: snap, body: body, headers: hdrs}
}

// --- version guards ---

// TestBAMLVersionPinned requires the LOADED CFFI runtime AND this module's go.mod
// require to be exactly stock BAML v0.223.0 (not replaced), so a green oracle can
// never be attributed to a different runtime.
func TestBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != wantBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports %q, want exactly %q", got, wantBAMLRuntimeVersion)
	}
	version, replaced, found := moduleRequire(t, moduleGoModPath, bamlModulePath)
	if !found {
		t.Fatalf("module go.mod does not pin %s", bamlModulePath)
	}
	if replaced {
		t.Fatalf("module go.mod replaces %s; the oracle must link the stock module", bamlModulePath)
	}
	if version != wantBAMLModuleVersion {
		t.Fatalf("module go.mod pins %s %s, want exactly %s", bamlModulePath, version, wantBAMLModuleVersion)
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep == nil || dep.Path != bamlModulePath {
				continue
			}
			if dep.Replace != nil {
				t.Fatalf("build info reports %s replaced (%s %s)", bamlModulePath, dep.Replace.Path, dep.Replace.Version)
			}
			if dep.Version != wantBAMLModuleVersion {
				t.Fatalf("build info reports %s %s, want %s", bamlModulePath, dep.Version, wantBAMLModuleVersion)
			}
		}
	}
}

// TestNanollmVersionPinned requires this module's go.mod require for the public
// nanollm-ffi module to be exactly v0.4.3 (not replaced) and the loaded engine to
// report a non-empty crate version — the native leg's runtime pin.
func TestNanollmVersionPinned(t *testing.T) {
	version, replaced, found := moduleRequire(t, moduleGoModPath, nanollmModulePath)
	if !found {
		t.Fatalf("module go.mod does not pin %s", nanollmModulePath)
	}
	if replaced {
		t.Fatalf("module go.mod replaces %s; the oracle must link the stock nanollm-ffi module", nanollmModulePath)
	}
	if version != wantNanollmModuleVersion {
		t.Fatalf("module go.mod pins %s %s, want exactly %s", nanollmModulePath, version, wantNanollmModuleVersion)
	}
	if got := nanollm.Version(); got != wantNanollmRuntimeVersion {
		t.Fatalf("loaded nanollm engine reports crate version %q, want exactly %q", got, wantNanollmRuntimeVersion)
	}
}

// moduleRequire parses a go.mod with the canonical modfile parser and returns the
// EXACT require version for modulePath, whether it is replaced, and whether the
// require was found (exact string match — a prerelease pin fails the == check).
func moduleRequire(t *testing.T, goModPath, modulePath string) (version string, replaced, found bool) {
	t.Helper()
	// goModPath is a fixed manifest path (non-secret); still report only the fixed
	// path + stage token, never the raw I/O/parse error, to keep every failure path
	// in this package uniformly free of formatted engine/decode errors.
	data, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read %s failed (stage=read_gomod)", goModPath)
	}
	mf, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		t.Fatalf("parse %s failed (stage=parse_gomod)", goModPath)
	}
	for _, r := range mf.Require {
		if r.Mod.Path == modulePath {
			version = r.Mod.Version
			found = true
		}
	}
	for _, rep := range mf.Replace {
		if rep.Old.Path == modulePath {
			replaced = true
		}
	}
	return version, replaced, found
}
