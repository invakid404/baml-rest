package spine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// stream_claim_order_test.go pins the ONE source-order invariant the claim discipline
// rests on, structurally rather than by comment: in the SHARED claimed-stream core
// StreamExecutor.runClaimedStream, the logical ownership marker `claimed = true` must be
// the LAST statement before the execute.RunStream call.
//
// It matters because the panic guard branches on `claimed`. Anything inserted into that
// gap — a metric, a log, a helper call, a lane's claim-time observation hook — could panic
// while the guard still reads `claimed == false`, which would turn a post-claim fault into
// a pre-socket DECLINE and invite a resend for a request that may already own a provider
// socket. Moving the marker BELOW RunStream would be the same bug, and this test catches
// both.
//
// It targets the CORE rather than StreamExecutor.Stream because ExecBridge-U1s factored the
// two stream lanes (the frozen native-only Stream and the live-oracle StreamWithOracle) onto
// one claimed-stream core: the marker moved with the code it guards. The second assertion
// below is what stops that from weakening the guard — it requires the ENTIRE file to hold
// exactly one execute.RunStream call and exactly one `claimed = true`, so neither public
// entry can grow a second, unguarded one-send path.

// streamSourcePath resolves nativeserve/spine/stream.go from this test's own location.
func streamSourcePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "stream.go")
}

// TestStreamClaimMarkerIsTheLastStatementBeforeRunStream parses the real source and
// asserts the adjacency inside the shared claimed-stream core.
func TestStreamClaimMarkerIsTheLastStatementBeforeRunStream(t *testing.T) {
	path := streamSourcePath(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	body := methodBody(t, f, "StreamExecutor", "runClaimedStream")

	marker := -1
	for i, stmt := range body.List {
		if isClaimedTrue(stmt) {
			if marker >= 0 {
				t.Fatalf("`claimed = true` is assigned more than once in runClaimedStream (statements %d and %d); the ownership boundary must be a single point", marker, i)
			}
			marker = i
		}
	}
	if marker < 0 {
		t.Fatal("runClaimedStream has no top-level `claimed = true` assignment; the logical ownership marker is missing (a physical OnClaim observer is not a substitute)")
	}
	if marker == len(body.List)-1 {
		t.Fatal("`claimed = true` is the last statement in runClaimedStream; it must be immediately followed by the execute.RunStream call")
	}
	next := body.List[marker+1]
	if !containsRunStreamCall(next) {
		t.Fatalf("the statement after `claimed = true` (line %d) does not call execute.RunStream; nothing may sit between the ownership marker and the one-send operation",
			fset.Position(next.Pos()).Line)
	}
	// And RunStream must be called exactly once in the whole method, so the adjacency
	// above cannot be satisfied by a decoy call.
	if n := countRunStreamCalls(body); n != 1 {
		t.Fatalf("runClaimedStream calls execute.RunStream %d time(s), want exactly 1", n)
	}
}

// TestTheStreamFileHasExactlyOneClaimBoundary is what keeps the guard above from being
// narrowed by refactoring rather than by intent: the WHOLE stream lane — both the frozen
// native-only Stream and the live-oracle StreamWithOracle — must funnel through the single
// guarded one-send path.
//
// Without it, an entry could grow its own execute.RunStream (with its own, unguarded claim
// bookkeeping) and the core's adjacency assertion would still pass while a second claimed
// stream existed beside it.
func TestTheStreamFileHasExactlyOneClaimBoundary(t *testing.T) {
	path := streamSourcePath(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	runStreams, markers := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		if isRunStreamCall(n) {
			runStreams++
		}
		if stmt, ok := n.(ast.Stmt); ok && isClaimedTrue(stmt) {
			markers++
		}
		return true
	})
	if runStreams != 1 {
		t.Errorf("stream.go calls execute.RunStream %d time(s), want exactly 1 — every stream lane must claim through the one shared core", runStreams)
	}
	if markers != 1 {
		t.Errorf("stream.go assigns `claimed = true` %d time(s), want exactly 1 — a second ownership marker is a second, separately-guarded claim boundary", markers)
	}
	// NON-VACUITY: both public entries and the core must still exist, or the counts above
	// would be trivially satisfied by a file that lost a lane. methodBody fails the test
	// itself when a method is absent, which is the assertion.
	for _, name := range []string{"Stream", "StreamWithOracle", "runClaimedStream"} {
		methodBody(t, f, "StreamExecutor", name)
	}
}

// methodBody returns the body of the named method on the named receiver type.
func methodBody(t *testing.T, f *ast.File, recvType, name string) *ast.BlockStmt {
	t.Helper()
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != recvType {
			continue
		}
		return fn.Body
	}
	t.Fatalf("method (*%s).%s not found; the claim-order guard would pass vacuously", recvType, name)
	return nil
}

// isClaimedTrue reports whether stmt is exactly `claimed = true`.
func isClaimedTrue(stmt ast.Stmt) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	lhs, ok := assign.Lhs[0].(*ast.Ident)
	if !ok || lhs.Name != "claimed" {
		return false
	}
	rhs, ok := assign.Rhs[0].(*ast.Ident)
	return ok && rhs.Name == "true"
}

// isRunStreamCall reports whether n is a call to execute.RunStream.
func isRunStreamCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "RunStream" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "execute"
}

func containsRunStreamCall(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if n != nil && isRunStreamCall(n) {
			found = true
			return false
		}
		return !found
	})
	return found
}

func countRunStreamCalls(body *ast.BlockStmt) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if node != nil && isRunStreamCall(node) {
			n++
		}
		return true
	})
	return n
}
