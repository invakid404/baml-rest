package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
)

// captureLog implements the func(format, args...) shape that
// emitFallbackRoundRobinDeferredWarnings expects, recording each
// formatted line for assertion. Buffer-backed so per-case test setup
// can read back the emitted warnings.
type captureLog struct {
	lines []string
}

func (c *captureLog) Logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// TestEmitFallbackRoundRobinDeferredWarnings_NestedFallbackContaining-
// RoundRobin pins the multi-level DFS contract: a fallback whose
// nested-fallback child reaches a round-robin client through any
// depth of fallback hops must emit a warning. The shape
// `fallback[fallback[fallback[rr[A,B], C], D], E]` is the canonical
// regression — the single-level loop only inspected the immediate
// grandchild and would miss the RR three levels deep.
//
// Companion sub-test: when the same nested branch contains multiple
// fallback paths that converge on the same RR client, the warning
// emits exactly once per (parent, child) pair (visited-set
// dedup-by-construction).
//
// Negative sub-test: a flat `fallback[fallback[A, B], C]` with no RR
// anywhere must emit no warning at all.
func TestEmitFallbackRoundRobinDeferredWarnings_NestedFallbackContainingRoundRobin(t *testing.T) {
	// countLinesMentioning returns the count of captured warning lines
	// that mention every supplied substring. Used to verify per-
	// (parent, child) warning emission while staying agnostic to the
	// order in which the emitter sorts parents.
	countLinesMentioning := func(lines []string, substrs ...string) int {
		count := 0
	OUTER:
		for _, line := range lines {
			for _, s := range substrs {
				if !strings.Contains(line, s) {
					continue OUTER
				}
			}
			count++
		}
		return count
	}

	t.Run("rr-three-levels-deep-warns-for-outermost-pair", func(t *testing.T) {
		// Outer = baml-fallback whose first child is a nested
		// fallback. The nested fallback's first child is itself a
		// fallback. That deepest fallback's first child is the RR
		// wrapper. Without DFS, the (Outer, Mid) pair would inspect
		// only Mid's immediate children, see no baml-roundrobin
		// grandchild, and miss the warning — even though the chain
		// reaches RR two levels below Mid.
		//
		// Each baml-fallback in the descent is also enumerated as a
		// parent in its own right, so the emitter logs one warning
		// per fallback-with-deferred-child pair. This test pins the
		// load-bearing assertion: the (Outer, Mid) pair MUST emit
		// (it's the case the single-level loop missed) and the
		// warning names InnerRR as the reached descendant.
		cfg := &bamlConfig{
			clientProvider: map[string]string{
				"Outer":   "baml-fallback",
				"Mid":     "baml-fallback",
				"Inner":   "baml-fallback",
				"InnerRR": "baml-roundrobin",
				"A":       "openai",
				"B":       "openai",
				"C":       "openai",
				"D":       "openai",
				"E":       "openai",
			},
			fallbackChains: map[string][]string{
				"Outer":   {"Mid", "E"},
				"Mid":     {"Inner", "D"},
				"Inner":   {"InnerRR", "C"},
				"InnerRR": {"A", "B"},
			},
		}
		var cap captureLog
		emitFallbackRoundRobinDeferredWarnings(cfg, cap.Logf)

		// The (Outer, Mid) pair is the regression case — the single-
		// level loop walked only `cfg.fallbackChains[Mid]` (which is
		// [Inner, D]) and never recursed into Inner to find InnerRR.
		// The DFS must reach InnerRR through Mid → Inner → InnerRR
		// and emit a warning naming Outer/Mid/InnerRR.
		if n := countLinesMentioning(cap.lines, `"Outer"`, `"Mid"`, `"InnerRR"`, "nested fallback"); n != 1 {
			t.Errorf("expected exactly 1 warning for (Outer, Mid) naming InnerRR via DFS; got %d.\nall lines:\n%s",
				n, strings.Join(cap.lines, "\n"))
		}
		// Sanity-check the intermediate pair too — (Mid, Inner) is
		// also a valid deferred shape (Mid is itself a fallback whose
		// nested child Inner reaches InnerRR). The single-level loop
		// would already catch this one; DFS preserves it.
		if n := countLinesMentioning(cap.lines, `"Mid"`, `"Inner"`, `"InnerRR"`); n != 1 {
			t.Errorf("expected exactly 1 warning for (Mid, Inner) naming InnerRR; got %d.\nall lines:\n%s",
				n, strings.Join(cap.lines, "\n"))
		}
		// Negative pin: (Inner, InnerRR) is the centralised
		// composition (immediate RR child with non-strategy leaves)
		// and must NOT warn — that's shape 1's centralised case.
		if n := countLinesMentioning(cap.lines, `"Inner"`, `"InnerRR"`, "round-robin child"); n != 0 {
			t.Errorf("centralised composition (Inner has immediate RR child with non-strategy leaves) must NOT emit a shape-1 warning; got %d.\nall lines:\n%s",
				n, strings.Join(cap.lines, "\n"))
		}
	})

	t.Run("converging-paths-emit-single-warning-per-pair", func(t *testing.T) {
		// Outer's nested-fallback child Mid has two paths to the
		// same RR client (Mid → Inner1 → RR and Mid → Inner2 → RR).
		// The DFS visited-set must terminate on the first match so
		// the (Outer, Mid) pair emits exactly ONE warning even
		// though the converging chain offers two routes to the same
		// RR descendant.
		//
		// (Mid, Inner1) and (Mid, Inner2) are independent pairs and
		// emit their own warnings — this test does not pin their
		// count, only the (Outer, Mid) single-walk-dedup invariant.
		cfg := &bamlConfig{
			clientProvider: map[string]string{
				"Outer":  "baml-fallback",
				"Mid":    "baml-fallback",
				"Inner1": "baml-fallback",
				"Inner2": "baml-fallback",
				"RR":     "baml-roundrobin",
				"A":      "openai",
				"B":      "openai",
				"C":      "openai",
			},
			fallbackChains: map[string][]string{
				"Outer":  {"Mid", "C"},
				"Mid":    {"Inner1", "Inner2"},
				"Inner1": {"RR"},
				"Inner2": {"RR"},
				"RR":     {"A", "B"},
			},
		}
		var cap captureLog
		emitFallbackRoundRobinDeferredWarnings(cfg, cap.Logf)

		// Pair (Outer, Mid) must appear exactly once regardless of
		// which converging path the DFS took first. A regression
		// that re-walked Mid's children after the first match would
		// emit two warnings for (Outer, Mid) — one per path.
		if n := countLinesMentioning(cap.lines, `"Outer"`, `"Mid"`, `"RR"`); n != 1 {
			t.Errorf("expected exactly 1 warning for (Outer, Mid) even with converging paths to RR; got %d.\nall lines:\n%s",
				n, strings.Join(cap.lines, "\n"))
		}
	})

	t.Run("non-strategy-leaves-emit-no-warning", func(t *testing.T) {
		// Outer = fallback whose nested-fallback child reaches only
		// non-strategy leaves. No RR anywhere → no warning. Confirms
		// the DFS doesn't false-positive on plain nested fallbacks
		// without an RR descendant.
		cfg := &bamlConfig{
			clientProvider: map[string]string{
				"Outer": "baml-fallback",
				"Mid":   "baml-fallback",
				"Inner": "baml-fallback",
				"A":     "openai",
				"B":     "openai",
				"C":     "openai",
				"D":     "openai",
			},
			fallbackChains: map[string][]string{
				"Outer": {"Mid", "D"},
				"Mid":   {"Inner", "C"},
				"Inner": {"A", "B"},
			},
		}
		var cap captureLog
		emitFallbackRoundRobinDeferredWarnings(cfg, cap.Logf)
		if len(cap.lines) != 0 {
			t.Errorf("non-strategy-only nested fallback must emit no warning, got %d:\n%s",
				len(cap.lines), strings.Join(cap.lines, "\n"))
		}
	})

	t.Run("immediate-rr-with-non-strategy-leaves-centralised-no-warning", func(t *testing.T) {
		// The plain `fallback[rr[A,B], C]` shape is centralised via
		// BuildRequest — no warning. Regression guard against a
		// future change that re-broadens the shape-1 emitter.
		cfg := &bamlConfig{
			clientProvider: map[string]string{
				"Outer":   "baml-fallback",
				"InnerRR": "baml-roundrobin",
				"A":       "openai",
				"B":       "openai",
				"C":       "openai",
			},
			fallbackChains: map[string][]string{
				"Outer":   {"InnerRR", "C"},
				"InnerRR": {"A", "B"},
			},
		}
		var cap captureLog
		emitFallbackRoundRobinDeferredWarnings(cfg, cap.Logf)
		if len(cap.lines) != 0 {
			t.Errorf("centralised composition must emit no warning, got %d:\n%s",
				len(cap.lines), strings.Join(cap.lines, "\n"))
		}
	})

	t.Run("immediate-rr-with-strategy-leaf-still-warns", func(t *testing.T) {
		// Shape 1 deferred path: RR's chain contains a strategy
		// leaf. Recursive strategy planning inside a fallback child
		// isn't supported, so the warning still emits.
		cfg := &bamlConfig{
			clientProvider: map[string]string{
				"Outer":     "baml-fallback",
				"InnerRR":   "baml-roundrobin",
				"InnerFall": "baml-fallback",
				"A":         "openai",
				"B":         "openai",
				"C":         "openai",
			},
			fallbackChains: map[string][]string{
				"Outer":     {"InnerRR", "C"},
				"InnerRR":   {"InnerFall", "B"},
				"InnerFall": {"A"},
			},
		}
		var cap captureLog
		emitFallbackRoundRobinDeferredWarnings(cfg, cap.Logf)
		if len(cap.lines) != 1 {
			t.Fatalf("RR child with a strategy leaf must still emit a deferred-shape warning, got %d:\n%s",
				len(cap.lines), strings.Join(cap.lines, "\n"))
		}
		if !strings.Contains(cap.lines[0], "round-robin child") {
			t.Errorf("warning must classify as shape 1 (round-robin child …); got: %s", cap.lines[0])
		}
	})

	t.Run("cycle-in-fallback-chains-does-not-loop", func(t *testing.T) {
		// Pathological config: Outer's nested fallback child cycles
		// back to itself via Mid. The DFS visited-set must terminate
		// the walk. No RR exists, so no warning expected; the test
		// passes when emit returns instead of hanging.
		cfg := &bamlConfig{
			clientProvider: map[string]string{
				"Outer": "baml-fallback",
				"Mid":   "baml-fallback",
				"A":     "openai",
			},
			fallbackChains: map[string][]string{
				"Outer": {"Mid"},
				"Mid":   {"Mid", "A"},
			},
		}
		var cap captureLog
		emitFallbackRoundRobinDeferredWarnings(cfg, cap.Logf)
		if len(cap.lines) != 0 {
			t.Errorf("cycle without RR descendants must emit no warning, got %d:\n%s",
				len(cap.lines), strings.Join(cap.lines, "\n"))
		}
	})
}

func TestMethodsDict(t *testing.T) {
	tests := []struct {
		name     string
		methods  []map[string]any
		wantKeys []string
		wantArgs map[string][]string
	}{
		{
			name:     "empty methods",
			methods:  []map[string]any{},
			wantKeys: []string{},
			wantArgs: map[string][]string{},
		},
		{
			name: "single method no args",
			methods: []map[string]any{
				{"name": "GetUser", "args": []string{}},
			},
			wantKeys: []string{"GetUser"},
			wantArgs: map[string][]string{"GetUser": {}},
		},
		{
			name: "single method with args",
			methods: []map[string]any{
				{"name": "CreateUser", "args": []string{"name", "email"}},
			},
			wantKeys: []string{"CreateUser"},
			wantArgs: map[string][]string{"CreateUser": {"name", "email"}},
		},
		{
			name: "multiple methods",
			methods: []map[string]any{
				{"name": "GetUser", "args": []string{"id"}},
				{"name": "CreateUser", "args": []string{"name", "email"}},
				{"name": "DeleteUser", "args": []string{"id"}},
			},
			wantKeys: []string{"GetUser", "CreateUser", "DeleteUser"},
			wantArgs: map[string][]string{
				"GetUser":    {"id"},
				"CreateUser": {"name", "email"},
				"DeleteUser": {"id"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := methodsDict(tt.methods)

			// Check count
			if len(code) != len(tt.wantKeys) {
				t.Errorf("methodsDict() returned %d entries, want %d", len(code), len(tt.wantKeys))
			}

			// Render and verify output
			f := jen.NewFile("test")
			f.Var().Id("Methods").Op("=").Map(jen.String()).Index().String().Values(code...)
			output := f.GoString()

			// Check each expected key and args
			for _, key := range tt.wantKeys {
				if !strings.Contains(output, `"`+key+`"`) {
					t.Errorf("methodsDict() missing key %q", key)
				}
			}

			for key, args := range tt.wantArgs {
				for _, arg := range args {
					// The arg should appear as a string literal
					if !strings.Contains(output, `"`+arg+`"`) {
						t.Errorf("methodsDict() missing arg %q for key %q", arg, key)
					}
				}
			}
		})
	}
}

func TestMethodsDict_OutputFormat(t *testing.T) {
	methods := []map[string]any{
		{"name": "TestMethod", "args": []string{"arg1", "arg2"}},
	}

	code := methodsDict(methods)
	f := jen.NewFile("test")
	f.Var().Id("Methods").Op("=").Map(jen.String()).Index().String().Values(code...)
	output := f.GoString()

	// Should produce valid Go map syntax
	// Expected pattern: "TestMethod": []string{"arg1", "arg2"}
	if !strings.Contains(output, `"TestMethod": []string{"arg1", "arg2"}`) {
		t.Errorf("methodsDict() output format incorrect.\nGot:\n%s", output)
	}
}
