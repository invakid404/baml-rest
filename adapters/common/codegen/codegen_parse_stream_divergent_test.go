package codegen

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common"
)

// widgetDynamicShape carries a DynamicProperties field, so
// hasDynamicPropertiesForType reports it dynamic; widgetPlainShape has
// no such field and is non-dynamic. The parse / parse-stream wrappers
// key their DynamicProperties-unwrap emission off these shapes.
type widgetDynamicShape struct {
	Value             string
	DynamicProperties any
}

type widgetPlainShape struct {
	Value string
}

// TestEmitParseMethods_StreamUnwrapTracksStreamShape locks the fix for
// the CodeRabbit finding: the parse-STREAM unwrap helper (name + call)
// must be gated on the ACTUAL ParseStream return shape, not the sync
// (final) return shape. Stream and final shapes are emitted
// independently and can diverge; the pre-fix code derived the
// stream-unwrap decision from syncFuncType.Out(0) (the final type),
// which could (a) emit/call an unwrap helper for a non-dynamic stream
// type or (b) skip unwrapping when only the stream type is dynamic.
//
// No live M4a fixture exercises a divergent shape (every current dynamic
// method has coinciding stream/final types), so this synthesises the
// four stream×final dynamic/plain combinations and pins the emitted
// helper declarations and call sites for each.
func TestEmitParseMethods_StreamUnwrapTracksStreamShape(t *testing.T) {
	// Only Out(0) of each func matters to the gating; the params are
	// irrelevant, so zero-arg funcs returning the value shape (the
	// generated helper takes *Shape, matching a value-typed return) keep
	// the fixtures minimal.
	finalDynamic := func() (widgetDynamicShape, error) { return widgetDynamicShape{}, nil }
	finalPlain := func() (widgetPlainShape, error) { return widgetPlainShape{}, nil }
	streamDynamic := func() (widgetDynamicShape, error) { return widgetDynamicShape{}, nil }
	streamPlain := func() (widgetPlainShape, error) { return widgetPlainShape{}, nil }

	// Method name "Widget" renders to the deterministic helper names
	// below via the same strcase pipeline the emitter uses.
	const (
		streamHelperDecl = "func unwrapDynamicWidgetOutputStream("
		streamUnwrapCall = "unwrapDynamicWidgetOutputStream(&result)"
		finalHelperDecl  = "func unwrapDynamicWidgetOutputFinal("
		finalUnwrapCall  = "unwrapDynamicWidgetOutputFinal(&result)"
	)

	cases := []struct {
		name             string
		finalFn          any
		streamFn         any
		wantStreamUnwrap bool
		wantFinalUnwrap  bool
	}{
		{
			// Coinciding dynamic shapes — the M4a status quo. Both
			// unwraps emitted; regenerated output for real methods is
			// unchanged by the fix because this branch is unaffected.
			name: "both dynamic", finalFn: finalDynamic, streamFn: streamDynamic,
			wantStreamUnwrap: true, wantFinalUnwrap: true,
		},
		{
			// Coinciding plain shapes — neither side unwraps.
			name: "both plain", finalFn: finalPlain, streamFn: streamPlain,
			wantStreamUnwrap: false, wantFinalUnwrap: false,
		},
		{
			// Divergent, bug (b): only the STREAM type is dynamic. Pre-fix
			// isDynamic (final) is false, so the stream envelope was left
			// un-unwrapped. The fix must emit + call the stream helper
			// while leaving the final path untouched.
			name: "stream dynamic, final plain", finalFn: finalPlain, streamFn: streamDynamic,
			wantStreamUnwrap: true, wantFinalUnwrap: false,
		},
		{
			// Divergent, bug (a): only the FINAL type is dynamic. Pre-fix
			// isDynamic (final) is true, so a stream unwrap helper was
			// emitted for the NON-dynamic stream type (whose value has no
			// DynamicProperties field — invalid generated code) and called
			// in the stream wrapper. The fix must suppress the stream
			// helper entirely while keeping the final unwrap.
			name: "final dynamic, stream plain", finalFn: finalDynamic, streamFn: streamPlain,
			wantStreamUnwrap: false, wantFinalUnwrap: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &generator{
				pkgs: PackageConfig{
					OutputPkgName:      "generated",
					GeneratedClientPkg: "example.com/x/baml_client",
					InterfacesPkg:      "github.com/invakid404/baml-rest/bamlutils",
				},
				intro: Introspection{
					ParseMethods:     map[string]struct{}{"Widget": {}},
					SyncFuncs:        map[string]any{"Widget": tc.finalFn},
					ParseStreamFuncs: map[string]any{"Widget": tc.streamFn},
				},
				out:                  common.MakeFile(),
				selfUtilsPkg:         "example.com/x/generated/utils",
				emittedUnwrapHelpers: map[string]bool{},
			}
			g.emitParseMethods()
			rendered := g.out.GoString()

			// Stream-unwrap decision must track the STREAM shape.
			if got := strings.Contains(rendered, streamHelperDecl); got != tc.wantStreamUnwrap {
				t.Errorf("stream unwrap helper declared = %v, want %v (gating must follow the ParseStream return shape)\n%s", got, tc.wantStreamUnwrap, rendered)
			}
			if got := strings.Contains(rendered, streamUnwrapCall); got != tc.wantStreamUnwrap {
				t.Errorf("stream unwrap called in parse-stream wrapper = %v, want %v\n%s", got, tc.wantStreamUnwrap, rendered)
			}

			// Final-unwrap decision must be independent and track the
			// sync (final) shape — unchanged by the fix.
			if got := strings.Contains(rendered, finalHelperDecl); got != tc.wantFinalUnwrap {
				t.Errorf("final unwrap helper declared = %v, want %v\n%s", got, tc.wantFinalUnwrap, rendered)
			}
			if got := strings.Contains(rendered, finalUnwrapCall); got != tc.wantFinalUnwrap {
				t.Errorf("final unwrap called in parse wrapper = %v, want %v\n%s", got, tc.wantFinalUnwrap, rendered)
			}
		})
	}
}
