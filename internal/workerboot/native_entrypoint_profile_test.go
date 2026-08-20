package workerboot

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// De-BAML serving cutover S2 — the ENTRYPOINT PROFILE guard.
//
// S2 promotes the native-capable worker to the standard artifact. Three facts have
// to stay true for that promotion to be safe, and none is observable from the root
// module's own behaviour:
//
//  1. BAML_REST_USE_DEBAML=false on a NATIVE-CAPABLE artifact suppresses ALL native
//     capability init, factories, Prepare and sockets. The flag-off branch must
//     hand workerboot a static build-capability advertisement and NOTHING that
//     could reach the engine.
//  2. That flag-off branch must STILL advertise the static build capability. The
//     artifact is stamped `native_capable` by the build; a flag-off branch that
//     passed a zero Options would make workerboot derive `baml_only`, contradict
//     the stamp and refuse to serve — turning the global kill switch into an
//     outage. A cold review found exactly that on cmd/worker-shadow, which the
//     first version of this guard did not read.
//  3. The BAML-only artifact is genuinely BAML-only — a zero Options literal — so
//     the rollback lane can never acquire a native lane by accident.
//
// These live in main packages this package cannot import (two of them in the
// out-of-go.work nanollmprepare module), and each is a single literal whose
// deletion or extension leaves every behavioural test green. So this guard reads
// the entrypoints as SOURCE and checks the literals structurally.
//
// It is deliberately in the ROOT module, next to workerboot: it needs no cgo, no
// nanollm and no build of the isolated module, so it runs in the ordinary unit
// lane on every change.
//
// FAIL-CLOSED, TWICE.
//
//   - The set of "native injection" Options fields is derived by REFLECTION, not
//     from a hand-written list: every func-typed or interface-typed field is
//     native injection, because those are the only shapes through which an engine
//     can enter this process. A future factory joins the guarded set the moment it
//     is declared, whatever it is named.
//   - The set of NATIVE-CAPABLE ENTRYPOINTS is cross-checked against the
//     filesystem AND against cmd/build/build.sh. The P0 above was a guard that
//     knew about one of two shippable entrypoints; a new sibling now trips this
//     test until someone classifies it, and an entrypoint build.sh can ship but
//     this guard does not know about trips it too.

// nativeWorkerModuleCmdDir is the isolated module's entrypoint directory.
var nativeWorkerModuleCmdDir = filepath.Join("..", "nativebody", "nanollmprepare", "cmd")

// bamlOnlyWorkerMainPath is the root module's BAML-only rollback entrypoint.
var bamlOnlyWorkerMainPath = filepath.Join("..", "..", "cmd", "worker", "main.go")

// buildScriptPath is the artifact-selection script, read here to cross-check which
// entrypoints can actually ship.
var buildScriptPath = filepath.Join("..", "..", "cmd", "build", "build.sh")

// nativeCapableEntrypoint describes one shippable native-capable worker main.
type nativeCapableEntrypoint struct {
	// dir is the entrypoint's directory name under the isolated module's cmd/.
	dir string
	// flagOffFunc builds the FLAG-OFF Options; it must inject nothing native.
	flagOffFunc string
	// flagOnFunc builds the FLAG-ON Options.
	flagOnFunc string
	// requiredInjections are native injection fields the FLAG-ON literal must set.
	// They keep the flag-off proofs non-vacuous: they are what proves this binary
	// really has a native lane to suppress.
	requiredInjections []string
}

// nativeCapableEntrypoints is every entrypoint cmd/build/build.sh can ship under
// the `native_capable` artifact profile. Written out here INDEPENDENTLY of the
// filesystem and of build.sh, so the guard has its own idea of what "all of them"
// means; both of the other two are then required to agree with it.
func nativeCapableEntrypoints() []nativeCapableEntrypoint {
	return []nativeCapableEntrypoint{
		{
			dir:                "worker",
			flagOffFunc:        "flagOffProfileOptions",
			flagOnFunc:         "serveProfileOptions",
			requiredInjections: []string{"NativeCapability", "NativeInit", "NativeServeFactory"},
		},
		{
			dir:                "worker-shadow",
			flagOffFunc:        "flagOffProfileOptions",
			flagOnFunc:         "shadowProfileOptions",
			requiredInjections: []string{"NativeCapability", "NativeInit", "NativeShadowFactory"},
		},
	}
}

func (e nativeCapableEntrypoint) mainPath() string {
	return filepath.Join(nativeWorkerModuleCmdDir, e.dir, "main.go")
}

// nonWorkerEntrypoints names isolated-module commands that are NOT deployable
// workers, with the reason each is exempt from the flag-off contract.
//
// This is the guard's only escape hatch and it is itself CHECKED: an exempt
// command must not call workerboot.Run. A real worker cannot hide behind the
// exemption by being listed here, and an exemption that names a command which has
// since become a worker fails.
var nonWorkerEntrypoints = map[string]string{
	"gen-staticserve-fixture": "a build-time code generator that emits the checked-in static-serve fixture adapters; " +
		"it boots no worker and cmd/build/build.sh cannot ship it as an artifact",
}

// nativeInjectionFields returns the Options field names through which a native
// engine can enter a worker: every func-typed field (the factories and
// NativeInit) and every interface-typed field (NativeCapability). The remaining
// fields are plain data — the static build-capability advertisement — which by
// construction cannot execute anything.
func nativeInjectionFields(t *testing.T) map[string]bool {
	t.Helper()
	fields := map[string]bool{}
	typ := reflect.TypeOf(Options{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		// Runtime is the BAML METHOD TABLE this worker dispatches through, not native
		// wiring: it decides which BAML methods exist, never what may be claimed
		// natively (that is the immutable cohort enrollment's answer). It is nil on
		// every shipped entrypoint and non-nil only under the `debamlworkerfixture`
		// build tag, which exists so the booted-artifact proof has a method to send a
		// request to; the entrypoint's own
		// TestShippedEntrypointsInstallNoRuntimeOverride pins that nil.
		//
		// The exemption is NAMED rather than shape-derived, so a future interface
		// field still trips the fail-closed classification below.
		if f.Name == "Runtime" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Func, reflect.Interface:
			fields[f.Name] = true
		case reflect.Bool, reflect.String:
			// Static advertisement only; safe in the flag-off literal.
		default:
			// Fail closed: a field shape this guard has not reasoned about could
			// carry an engine (a struct with methods, a pointer, a channel), and
			// silently treating it as inert is exactly the gap that would let a
			// future native lane slip into the flag-off branch.
			t.Fatalf("workerboot.Options field %s has unclassified kind %s; extend the S2 entrypoint guard before adding it", f.Name, f.Type.Kind())
		}
	}
	if len(fields) == 0 {
		t.Fatal("no native injection fields found on workerboot.Options; the guard would be vacuous")
	}
	return fields
}

// parseEntrypoint parses one worker main.go.
func parseEntrypoint(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

// findFunc returns the named top-level function declaration.
func findFunc(t *testing.T, file *ast.File, path, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("%s: function %q not found", path, name)
	return nil
}

// optionsLiteralFields returns the Options field names set by the (single)
// workerboot.Options composite literal inside fn. It fails when the literal is
// missing or when a key is not a plain identifier — an Options literal that this
// guard cannot read is an Options literal it cannot vouch for.
func optionsLiteralFields(t *testing.T, fn *ast.FuncDecl, path string) []string {
	t.Helper()
	var found []string
	literals := 0
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Options" {
			return true
		}
		literals++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("%s: %s contains a non-keyed Options element; the S2 guard cannot verify it", path, fn.Name.Name)
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				t.Fatalf("%s: %s contains a non-identifier Options key; the S2 guard cannot verify it", path, fn.Name.Name)
			}
			found = append(found, key.Name)
		}
		return true
	})
	if literals != 1 {
		t.Fatalf("%s: %s builds %d workerboot.Options literals, want exactly 1", path, fn.Name.Name, literals)
	}
	sort.Strings(found)
	return found
}

// TestNativeCapableEntrypointSetIsComplete is the fail-closed half that the first
// version of this guard was missing: it cross-checks the declared entrypoint list
// against the isolated module's cmd/ directory AND against the packages
// cmd/build/build.sh can ship under the native_capable profile.
//
// A new sibling entrypoint, or a build.sh that learns to ship one, fails here
// until it is classified — rather than silently escaping the flag-off contract the
// way cmd/worker-shadow did.
func TestNativeCapableEntrypointSetIsComplete(t *testing.T) {
	declared := map[string]bool{}
	for _, e := range nativeCapableEntrypoints() {
		declared[e.dir] = true
	}

	entries, err := os.ReadDir(nativeWorkerModuleCmdDir)
	if err != nil {
		t.Fatalf("read %s: %v", nativeWorkerModuleCmdDir, err)
	}
	onDisk := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(nativeWorkerModuleCmdDir, entry.Name(), "main.go")); err != nil {
			continue
		}
		onDisk[entry.Name()] = true
	}
	for dir := range onDisk {
		if declared[dir] {
			continue
		}
		reason, exempt := nonWorkerEntrypoints[dir]
		if !exempt {
			t.Errorf("isolated module entrypoint cmd/%s is not classified by the S2 entrypoint guard; every native-capable main must satisfy the flag-off contract (or be listed as a non-worker with a reason)", dir)
			continue
		}
		// The exemption is checked, not trusted: a command that boots a worker is
		// a deployable artifact whatever the list says.
		path := filepath.Join(nativeWorkerModuleCmdDir, dir, "main.go")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read exempt entrypoint %s: %v", path, err)
			continue
		}
		if strings.Contains(string(src), "workerboot.Run(") {
			t.Errorf("cmd/%s is exempted as %q but calls workerboot.Run; it is a deployable worker and must satisfy the flag-off contract", dir, reason)
		}
	}
	for dir := range nonWorkerEntrypoints {
		if !onDisk[dir] {
			t.Errorf("the S2 entrypoint guard exempts cmd/%s, which no longer exists; drop the stale exemption", dir)
		}
	}
	for dir := range declared {
		if !onDisk[dir] {
			t.Errorf("the S2 entrypoint guard declares cmd/%s, which no longer exists; the guard is reading nothing", dir)
		}
	}

	// build.sh names the shippable packages directly. Whatever it can ship under
	// the native_capable profile must be in the declared set.
	script, err := os.ReadFile(buildScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", buildScriptPath, err)
	}
	text := string(script)
	for dir := range onDisk {
		named := strings.Contains(text, `NATIVE_WORKER_PKG="./cmd/`+dir+`/"`)
		if named && !declared[dir] {
			t.Errorf("build.sh can ship cmd/%s as a native_capable artifact but the guard does not classify it", dir)
		}
		if declared[dir] && !named {
			t.Errorf("the guard classifies cmd/%s but build.sh no longer names it; artifact selection and this guard have drifted", dir)
		}
	}
}

// TestFlagOffNativeArtifactInjectsZeroNativeCapability is the flag-off
// zero-native assertion, for EVERY shippable native-capable artifact. The
// flag-off branch must supply only the static build-capability advertisement: no
// capability, no init, no factory. Adding ANY native injection field to that
// literal — the mutation that would leak a native init past the kill switch —
// fails here.
func TestFlagOffNativeArtifactInjectsZeroNativeCapability(t *testing.T) {
	injection := nativeInjectionFields(t)
	for _, e := range nativeCapableEntrypoints() {
		t.Run(e.dir, func(t *testing.T) {
			path := e.mainPath()
			file := parseEntrypoint(t, path)
			fields := optionsLiteralFields(t, findFunc(t, file, path, e.flagOffFunc), path)
			for _, name := range fields {
				if injection[name] {
					t.Errorf("%s: flag-off options set native injection field %s; BAML_REST_USE_DEBAML=false must install no capability, no runtime init, no factory and open no native socket", path, name)
				}
			}
		})
	}
}

// TestFlagOffNativeArtifactStillAdvertisesBuildCapability is the P0 regression
// guard. A native-capable artifact is STAMPED native_capable by the build, and
// workerboot derives the running profile from the capability advertisement. A
// flag-off branch that advertises nothing derives baml_only, contradicts its own
// stamp, and exits before serving any BAML — turning the global kill switch into
// an outage. This is what cmd/worker-shadow did.
//
// The assertion is deliberately separate from the zero-native one above: those two
// pull in opposite directions (advertise MORE vs inject NOTHING), and collapsing
// them into a single test is how the advertisement half got lost for one of the
// two entrypoints.
func TestFlagOffNativeArtifactStillAdvertisesBuildCapability(t *testing.T) {
	for _, e := range nativeCapableEntrypoints() {
		t.Run(e.dir, func(t *testing.T) {
			path := e.mainPath()
			file := parseEntrypoint(t, path)
			fields := optionsLiteralFields(t, findFunc(t, file, path, e.flagOffFunc), path)
			for _, required := range []string{"NativeBuildCapable", "NativeEngineName"} {
				found := false
				for _, name := range fields {
					if name == required {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: flag-off options do not set %s; the artifact is stamped native_capable, so workerboot would derive baml_only, reject the stamp and refuse to serve — BAML_REST_USE_DEBAML=false must stay a total BAML revert, not an outage", path, required)
				}
			}
		})
	}
}

// TestFlagIsResolvedBeforeAnyNativeWiring pins the ORDER, which is the other half
// of "flag-off = zero native": the umbrella flag must be resolved as the FIRST
// thing main does, and the flag-off branch must return immediately. A native
// construction hoisted above the check — a capability probe, a runtime init —
// would execute engine code before the kill switch was consulted, and the
// literal-level guards above would not see it.
func TestFlagIsResolvedBeforeAnyNativeWiring(t *testing.T) {
	for _, e := range nativeCapableEntrypoints() {
		t.Run(e.dir, func(t *testing.T) {
			path := e.mainPath()
			file := parseEntrypoint(t, path)
			mainFn := findFunc(t, file, path, "main")

			if len(mainFn.Body.List) == 0 {
				t.Fatalf("%s: main() is empty", path)
			}
			ifStmt, ok := mainFn.Body.List[0].(*ast.IfStmt)
			if !ok {
				t.Fatalf("%s: main()'s first statement is %T, want the umbrella-flag check", path, mainFn.Body.List[0])
			}
			cond := exprSource(t, ifStmt.Cond)
			if !strings.Contains(cond, "DeBAMLConfigFromEnv") || !strings.Contains(cond, "Enabled") {
				t.Fatalf("%s: main()'s first statement is not the umbrella-flag check: %s", path, cond)
			}
			if ifStmt.Init != nil {
				t.Fatalf("%s: main()'s flag check carries an init statement; nothing may run before the kill switch is consulted", path)
			}

			// The flag-off branch must hand over the flag-off literal and RETURN —
			// no fallthrough into the native wiring below it.
			body := ifStmt.Body.List
			if len(body) != 2 {
				t.Fatalf("%s: flag-off branch has %d statements, want exactly a workerboot.Run(%s()) and a return", path, len(body), e.flagOffFunc)
			}
			runCall := exprSource(t, body[0])
			if !strings.Contains(runCall, "workerboot.Run("+e.flagOffFunc+"())") {
				t.Fatalf("%s: flag-off branch does not run the flag-off profile: %s", path, runCall)
			}
			if _, ok := body[1].(*ast.ReturnStmt); !ok {
				t.Fatalf("%s: flag-off branch does not return; it would fall through into the native wiring", path)
			}
		})
	}
}

// TestFlagOnProfileStillWiresNativeCapability keeps the flag-off guards honest.
// They assert an ABSENCE, and an absence is trivially satisfied by a file that no
// longer wires anything. This asserts the presence of the native wiring in the
// FLAG-ON branch, so the flag-off proofs are proofs about a worker that really
// does have a native lane to suppress.
func TestFlagOnProfileStillWiresNativeCapability(t *testing.T) {
	injection := nativeInjectionFields(t)
	for _, e := range nativeCapableEntrypoints() {
		t.Run(e.dir, func(t *testing.T) {
			path := e.mainPath()
			file := parseEntrypoint(t, path)
			fields := optionsLiteralFields(t, findFunc(t, file, path, e.flagOnFunc), path)
			var injected []string
			for _, name := range fields {
				if injection[name] {
					injected = append(injected, name)
				}
			}
			for _, required := range e.requiredInjections {
				found := false
				for _, name := range injected {
					if name == required {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: %s no longer injects %s; the native-capable artifact would have no native lane, making the flag-off proofs vacuous (injected: %v)", path, e.flagOnFunc, required, injected)
				}
			}
		})
	}
}

// TestBAMLOnlyRollbackArtifactInjectsNothing pins the rollback artifact. S2 keeps
// the BAML-only worker as the explicit rollback lane, and the whole value of that
// lane is that it CANNOT serve natively under any flag, policy or enrollment. A
// zero Options literal is what makes that a build-level fact rather than a
// runtime promise, so any field appearing here fails.
func TestBAMLOnlyRollbackArtifactInjectsNothing(t *testing.T) {
	file := parseEntrypoint(t, bamlOnlyWorkerMainPath)
	fields := optionsLiteralFields(t, findFunc(t, file, bamlOnlyWorkerMainPath, "main"), bamlOnlyWorkerMainPath)
	if len(fields) != 0 {
		t.Errorf("the BAML-only rollback worker passes Options fields %v; it must pass a ZERO Options literal so it can never acquire a native lane", fields)
	}
}

// exprSource renders a node back to source for readable assertions and failure
// messages.
func exprSource(t *testing.T, n ast.Node) string {
	t.Helper()
	var b strings.Builder
	if err := printNode(&b, n); err != nil {
		t.Fatalf("render node: %v", err)
	}
	return b.String()
}
