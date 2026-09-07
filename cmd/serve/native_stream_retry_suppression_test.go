package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"
)

// native_stream_retry_suppression_test.go pins the ONE composition that decides whether the
// pool may replay a stream: DisableStreamInfrastructureRetries is armed exactly when THIS
// artifact ships a native-stream-capable worker AND the umbrella flag resolves true.
//
// ExecBridge-U1s makes this load-bearing in a way it was not before. The standard worker now
// DEFAULT-SERVES the exact stream cohort, so a replay after a claimed native stream would be
// a second irreversible provider request for a request that already has one — and the host
// cannot know, when it configures the pool, whether a given stream will claim. Suppression
// therefore stays armed for the whole native-stream-capable + flag-on artifact, including
// for streams that ultimately DECLINE to BAML. Trading a possible replay on a declined
// stream for the guarantee that a claimed one is never replayed is the deliberate direction;
// narrowing it on winner metadata (which arrives long after the pool decided) would invert it.
//
// The check is structural rather than behavioural because both call sites are one assignment
// inside a boot path that needs a real extracted worker binary. What can go wrong here is an
// EDIT — dropping a conjunct, or adding a third one keyed on something observed too late —
// and that is exactly what an AST comparison of the assignment catches.

// suppressionAssignmentText returns the RENDERED right-hand side of the
// `cfg.DisableStreamInfrastructureRetries = ...` assignment in the given file.
//
// It renders the whole expression rather than collecting operands, because an operand list
// cannot see the two edits that matter most: `&&` -> `||`, and an inserted `!`. All three of
//
//	nativeStreamServeCapable && runtimeCfg.DeBAML.Enabled
//	nativeStreamServeCapable || runtimeCfg.DeBAML.Enabled
//	nativeStreamServeCapable && !runtimeCfg.DeBAML.Enabled
//
// yield the same operands, and the third INVERTS the guarantee — it would arm suppression
// only while the kill switch is off.
func suppressionAssignmentText(t *testing.T, path string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var rendered string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DisableStreamInfrastructureRetries" {
			return true
		}
		if found {
			t.Errorf("%s assigns DisableStreamInfrastructureRetries more than once; the arming rule must have ONE source", path)
		}
		found = true
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, assign.Rhs[0]); err != nil {
			t.Fatalf("render the arming expression in %s: %v", path, err)
		}
		rendered = buf.String()
		return false
	})
	if !found {
		t.Fatalf("%s does not assign DisableStreamInfrastructureRetries; the arming rule is missing", path)
	}
	return rendered
}

// TestStreamRetrySuppressionIsArmedByCapabilityAndFlag asserts BOTH worker-mode boot paths
// arm suppression from exactly this expression — operator, operand order and negation
// included.
func TestStreamRetrySuppressionIsArmedByCapabilityAndFlag(t *testing.T) {
	const want = "nativeStreamServeCapable && runtimeCfg.DeBAML.Enabled"
	for _, path := range []string{"worker_mode_subprocess.go", "worker_mode_inprocess.go"} {
		if got := suppressionAssignmentText(t, path); got != want {
			t.Errorf("%s arms suppression from %q, want %q — a dropped conjunct replays a claimed native stream, an added one narrows the guarantee, and a flipped operator or an inserted ! inverts it",
				path, got, want)
		}
	}
}
