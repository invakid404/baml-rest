package staticservefixture

// ExecBridge-U1s / M3e-B — static-STREAM ORACLE codegen GOLDEN guard.
//
// It asserts, over the checked-in generated static-serve fixture adapter, the emission
// contract the default-serve flip rests on:
//
//   - the U1s install (installNativeStaticStreamOracle) is emitted ONLY in a method's
//     StreamRequest builder, never in the unary BuildCallRequest bridge;
//   - it is gated behind deBAMLStaticStreamOracleServe(adapter), so a flag-off or non-serve
//     build performs no descriptor lookup and installs nothing;
//   - each StreamRequest builder installs the U1s lane and the LEGACY lane in the two arms
//     of ONE if/else, so exactly one of them can ever be installed — which is what the
//     orchestrator's fail-closed double-installation guard requires;
//   - the generated method supplies BOTH BAML-only oracle closures, and they name
//     ParseStream.<Method> and Parse.<Method> respectively — the per-prefix oracle must not
//     be the FINAL parser, and vice versa;
//   - the method-independent helper and the adapter getter/setter are emitted.
//
// It is a pure go/parser + text assertion over the committed artifacts (no CGO, no regen).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestStaticStreamOracleInstall_OnlyInStreamBuilder pins placement + gating.
func TestStaticStreamOracleInstall_OnlyInStreamBuilder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fixtureAdapterGo, nil, 0)
	if err != nil {
		t.Fatalf("parse generated adapter %s: %v", fixtureAdapterGo, err)
	}

	installs := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || !funcRefsIdent(fn, "installNativeStaticStreamOracle") {
			continue
		}
		installs++
		name := fn.Name.Name
		if strings.Contains(name, "BuildCallRequest") {
			t.Errorf("installNativeStaticStreamOracle emitted in the UNARY bridge %q — the stream oracle must never be installed on the /call path", name)
		}
		if !strings.HasSuffix(name, "BuildRequest") {
			t.Errorf("installNativeStaticStreamOracle emitted in unexpected func %q (want a …BuildRequest stream builder)", name)
		}
		if !funcRefsIdent(fn, "deBAMLStaticStreamOracleServe") {
			t.Errorf("installNativeStaticStreamOracle in %q is NOT gated by deBAMLStaticStreamOracleServe; flag-off identity is broken", name)
		}
		// The U1s lane SUPERSEDES the legacy one, and both live in the same builder, so
		// the generated code must be able to install exactly one.
		if !funcRefsIdent(fn, "installNativeStaticStream") {
			t.Errorf("%q installs the U1s oracle without retaining the legacy fall-back install; the legacy seam must stay reachable for its own tests", name)
		}
	}
	if installs == 0 {
		t.Fatal("expected installNativeStaticStreamOracle in at least one StreamRequest builder; found none (the fixture has static serve methods)")
	}
	t.Logf("static-stream ORACLE install present in %d StreamRequest builders, none on the unary bridge", installs)
}

// TestStaticStreamOracleInstallsAreMutuallyExclusive pins the if/else shape structurally: in
// every StreamRequest builder that carries both installs, they sit in the two arms of ONE
// if statement, so no generated path can install both seams at once.
//
// It matters because the orchestrator FAILS CLOSED on a double installation. Emitting both
// unconditionally would compile, wire cleanly, and turn every U1s stream into a terminal
// misconfiguration error at runtime — a failure this parse catches at generation time.
func TestStaticStreamOracleInstallsAreMutuallyExclusive(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fixtureAdapterGo, nil, 0)
	if err != nil {
		t.Fatalf("parse generated adapter %s: %v", fixtureAdapterGo, err)
	}
	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || !funcRefsIdent(fn, "installNativeStaticStreamOracle") {
			continue
		}
		paired := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Else == nil {
				return true
			}
			thenOracle := blockRefsIdent(ifs.Body, "installNativeStaticStreamOracle")
			elseLegacy := blockRefsIdent(ifs.Else, "installNativeStaticStream")
			if thenOracle && elseLegacy {
				paired = true
				// The oracle arm must NOT also install the legacy seam.
				if blockRefsIdent(ifs.Body, "installNativeStaticStream") {
					t.Errorf("%s: the oracle arm also installs the legacy seam; the orchestrator fails closed on both", fn.Name.Name)
				}
				return false
			}
			return true
		})
		if !paired {
			t.Errorf("%s: the U1s and legacy installs are not the two arms of one if/else; nothing stops both being installed", fn.Name.Name)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no StreamRequest builder carried the U1s install; the exclusivity guard would pass vacuously")
	}
}

// blockRefsIdent reports whether a statement subtree references an identifier by name.
func blockRefsIdent(n ast.Node, target string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == target {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestStaticStreamOracleClosuresNameTheRightBAMLParsers is the discriminating text guard on
// the two BAML-only oracle closures. Swapping them — using ParseStream for the final, or
// Parse for a prefix — would still compile and would still "have an oracle", while
// comparing against the wrong thing on every tick.
func TestStaticStreamOracleClosuresNameTheRightBAMLParsers(t *testing.T) {
	src := readFileOrFatal(t, fixtureAdapterGo)
	// The exact-U1 method the S3b booted differential drives.
	const method = "StaticRecursiveAliasJSON"
	for _, want := range []string{
		// The per-prefix oracle is ParseStream over the prefix, normalized through the
		// neutral presence helper.
		"bamlclient.ParseStream." + method + "(__pctx, __prefix, options...)",
		"bamlutils.BAMLStreamPrefixValue(__sv)",
		// The FINAL oracle is Parse (never ParseStream) over the complete text.
		"bamlclient.Parse." + method + "(__pctx, __full, options...)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the generated adapter is missing %q", want)
		}
	}
	// An ordinary ParseStream rejection must be normalized to a NO-VALUE, and a cancelled
	// context to a real error — the distinction the whole strict-cadence contract rests on.
	if !strings.Contains(src, "if __ce := __pctx.Err(); __ce != nil {") {
		t.Error("the per-prefix BAML closure does not distinguish a cancelled context from an ordinary partial rejection")
	}
}

// TestStaticStreamOracleHelperEmitted pins the method-independent helper + accessors.
func TestStaticStreamOracleHelperEmitted(t *testing.T) {
	helper := readFileOrFatal(t, fixtureDeBAMLStaticGo)
	for _, want := range []string{
		"func installNativeStaticStreamOracle(",
		"func deBAMLStaticStreamOracleServe(",
		"NativeStaticStreamOracleServeComparator()",
		"cfg.NativeOracleAttempt = func(",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("generated debaml_static.go is missing %q", want)
		}
	}
	// The U1s installer must NOT set the legacy transport-only parser seams: the oracle
	// owns the parse, and installing them would arm the outer error-swallowing cadence.
	oracleFn := helper[strings.Index(helper, "func installNativeStaticStreamOracle("):]
	if end := strings.Index(oracleFn, "\nfunc "); end > 0 {
		oracleFn = oracleFn[:end]
	}
	for _, forbidden := range []string{"cfg.NativeParseStream", "cfg.NativeParseFinal", "cfg.NativeAttempt ="} {
		if strings.Contains(oracleFn, forbidden) {
			t.Errorf("installNativeStaticStreamOracle sets %q; the oracle owns the parse and the orchestrator fails closed on a double-installed seam", forbidden)
		}
	}

	impl := readFileOrFatal(t, fixtureAdapterImplGo)
	for _, want := range []string{
		"func (b *BamlAdapter) SetNativeStaticStreamOracleServeComparator(",
		"func (b *BamlAdapter) NativeStaticStreamOracleServeComparator()",
	} {
		if !strings.Contains(impl, want) {
			t.Errorf("generated adapter/adapter.go is missing the getter/setter %q", want)
		}
	}
}
