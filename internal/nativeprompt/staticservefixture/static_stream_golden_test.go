package staticservefixture

// De-BAML Phase 3b — static-STREAM codegen GOLDEN guard.
//
// It asserts, over the checked-in generated static-serve fixture adapter, the exact
// placement contract the transport-seam brief requires:
//
//   - the native static-stream install (installNativeStaticStream) is emitted ONLY in a
//     method's StreamRequest builder (the `…BuildRequest` funcs), NEVER in the unary
//     `…BuildCallRequest` bridge — so the true-unary /call path carries no stream hook;
//   - every install is gated behind deBAMLStaticStreamServe(adapter) (flag-off identity:
//     no descriptor lookup / no install when no serve callback is wired);
//   - the method-independent helper (installNativeStaticStream + deBAMLStaticStreamServe)
//     and the adapter getter/setter (NativeStaticStreamServeComparator) are emitted.
//
// It is a pure go/parser assertion over the committed artifacts (no CGO, no regen), so it
// runs in the default lane and pins the emission against accidental drift.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureAdapterGo      = "../testdata/staticserve_fixture/generated/adapter.go"
	fixtureAdapterImplGo  = "../testdata/staticserve_fixture/generated/adapter/adapter.go"
	fixtureDeBAMLStaticGo = "../testdata/staticserve_fixture/generated/debaml_static.go"
)

// funcRefsIdent reports whether fn's body references an identifier named target.
func funcRefsIdent(fn *ast.FuncDecl, target string) bool {
	found := false
	if fn.Body == nil {
		return false
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == target {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestStaticStreamInstall_OnlyInStreamBuilder pins that the native static-stream install
// appears only in the StreamRequest builders, never the unary BuildCallRequest bridge, and
// is always gated by deBAMLStaticStreamServe.
func TestStaticStreamInstall_OnlyInStreamBuilder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fixtureAdapterGo, nil, 0)
	if err != nil {
		t.Fatalf("parse generated adapter %s: %v", fixtureAdapterGo, err)
	}

	installs := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		if !funcRefsIdent(fn, "installNativeStaticStream") {
			continue
		}
		installs++
		name := fn.Name.Name
		// MUST be a StreamRequest builder (…BuildRequest), NEVER the unary bridge.
		if strings.Contains(name, "BuildCallRequest") {
			t.Errorf("installNativeStaticStream emitted in UNARY bridge %q — the native stream seam must never be installed on the unary /call path", name)
		}
		if !strings.HasSuffix(name, "BuildRequest") {
			t.Errorf("installNativeStaticStream emitted in unexpected func %q (want a …BuildRequest stream builder)", name)
		}
		// MUST be gated behind the flag-off-identity serve getter.
		if !funcRefsIdent(fn, "deBAMLStaticStreamServe") {
			t.Errorf("installNativeStaticStream in %q is NOT gated by deBAMLStaticStreamServe (flag-off identity broken)", name)
		}
	}
	if installs == 0 {
		t.Fatal("expected installNativeStaticStream in at least one StreamRequest builder; found none (the fixture has static serve methods)")
	}
	t.Logf("static-stream install present in %d StreamRequest builders, none on the unary bridge", installs)
}

// TestStaticStreamHelperEmitted pins that the method-independent helper + the adapter
// getter/setter are emitted.
func TestStaticStreamHelperEmitted(t *testing.T) {
	helper := readFileOrFatal(t, fixtureDeBAMLStaticGo)
	for _, want := range []string{
		"func installNativeStaticStream(",
		"func deBAMLStaticStreamServe(",
		"NativeStaticStreamServeComparator()",
		"DeBAMLParseRequest{StaticStreamDescriptor:",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("generated debaml_static.go is missing %q", want)
		}
	}

	impl := readFileOrFatal(t, fixtureAdapterImplGo)
	for _, want := range []string{
		"func (b *BamlAdapter) SetNativeStaticStreamServeComparator(",
		"func (b *BamlAdapter) NativeStaticStreamServeComparator()",
	} {
		if !strings.Contains(impl, want) {
			t.Errorf("generated adapter/adapter.go is missing the getter/setter %q", want)
		}
	}
}

func readFileOrFatal(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
