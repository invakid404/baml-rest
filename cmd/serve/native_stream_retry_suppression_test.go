package main

import (
	"go/ast"
	"go/parser"
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

// suppressionAssignmentOperands returns the identifier/selector operands of the
// `cfg.DisableStreamInfrastructureRetries = ...` assignment in the given file, in order.
func suppressionAssignmentOperands(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var operands []string
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
		ast.Inspect(assign.Rhs[0], func(rn ast.Node) bool {
			switch v := rn.(type) {
			case *ast.SelectorExpr:
				operands = append(operands, exprText(v))
				return false
			case *ast.Ident:
				operands = append(operands, v.Name)
				return false
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatalf("%s does not assign DisableStreamInfrastructureRetries; the arming rule is missing", path)
	}
	return operands
}

// exprText renders a (possibly nested) selector as dotted text.
func exprText(sel *ast.SelectorExpr) string {
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	case *ast.SelectorExpr:
		return exprText(x) + "." + sel.Sel.Name
	default:
		return sel.Sel.Name
	}
}

// TestStreamRetrySuppressionIsArmedByCapabilityAndFlag asserts BOTH worker-mode boot paths
// arm suppression from exactly the two conjuncts, in that order, and from nothing else.
func TestStreamRetrySuppressionIsArmedByCapabilityAndFlag(t *testing.T) {
	want := []string{"nativeStreamServeCapable", "runtimeCfg.DeBAML.Enabled"}
	for _, path := range []string{"worker_mode_subprocess.go", "worker_mode_inprocess.go"} {
		got := suppressionAssignmentOperands(t, path)
		if len(got) != len(want) {
			t.Errorf("%s arms suppression from %v, want exactly %v — a dropped conjunct would replay a claimed native stream, an extra one would narrow the guarantee", path, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s conjunct %d = %q, want %q", path, i, got[i], want[i])
			}
		}
	}
}
