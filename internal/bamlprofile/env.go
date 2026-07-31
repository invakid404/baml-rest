package bamlprofile

import (
	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/pycompat"
	"github.com/invakid404/minijinja-go/v2/syntax"
	"github.com/invakid404/minijinja-go/v2/value"
)

// Config carries the per-render inputs the get_env-equivalent environment needs
// that are not fixed engine configuration.
//
// It is intentionally small for this slice. It is the typed boundary where the
// deferred host value model plugs in later (enum globals, class/map/media
// lowering, the enum ValueCmp for #597); those are layered onto the returned
// Environment or passed as render context by later slices, not stubbed here.
type Config struct {
	// OutputFormat is the pre-rendered ctx.output_format block that BAML installs
	// before rendering (it is the schema description a prompt reaches via
	// {{ ctx.output_format }}). It may be empty when the template never reaches
	// ctx.output_format, or when the function returns a schemaless type such as
	// string. Computing its full BAML v0.223 surface from a return type is a
	// later-slice concern; this slice takes it pre-rendered, exactly as
	// internal/nativeprompt/env.go does today.
	OutputFormat string
}

// New constructs a fork *minijinja.Environment configured exactly like BAML
// v0.223's get_env(), for the parts that do not need the host value model yet.
//
// Authority (byte-for-byte): BAML v0.223
// engine/baml-lib/baml-core/src/ir/jinja_helpers.rs:7-36
//
//	let mut env = minijinja::Environment::new();
//	env.set_formatter(|out, state, value| {          // top-level none -> "null"
//	    let value = if value.is_none() { &Value::from("null") } else { value };
//	    minijinja::escape_formatter(out, state, value)
//	});
//	env.set_debug(true);
//	env.set_trim_blocks(true);
//	env.set_lstrip_blocks(true);
//	env.add_filter("regex_match", regex_match);
//	env.add_filter("sum", sum_filter);               // OVERRIDES the builtin sum
//	env.set_unknown_method_callback(minijinja_contrib::pycompat::unknown_method_callback);
//
// minijinja::Environment::new() (fork NewEnvironment) already installs the
// BAML-exact builtin filter/test/function registry, so New only layers get_env's
// deltas on top of it.
//
// get_env() does NOT set an auto-escape mode: BAML renders prompts through
// render_str, whose template name has no html/xml extension, so the engine
// default is already AutoEscapeNone. We force AutoEscapeNone so the choice does
// not depend on a template name, matching internal/nativeprompt/env.go. That is
// observable-equivalent for the prompt/constraint surface (BAML never renders an
// html-named template) and is verified by the differential.
//
// get_env() also does NOT add `ctx` or `_`; BAML injects those as render
// context. New adds them as globals, matching internal/nativeprompt/env.go's
// proven behavior (a global is available in every render; the observable
// result is the same). Because ctx carries a per-render OutputFormat, New builds
// a fresh environment per render, exactly as nativeprompt.buildEnv does.
func New(cfg Config) *minijinja.Environment {
	env := minijinja.NewEnvironment()

	// BAML's custom formatter: a top-level none renders as the literal "null"
	// (not minijinja's default empty string); everything else renders through the
	// engine's escaping path. Under AutoEscapeNone the escape func is the
	// identity, so this is none -> "null", else the value's display string.
	//
	// Nested none inside a host class/list is handled by those host values'
	// display impls in BAML; that is part of the deferred host value model. Until
	// it lands there are no host render values here, so this top-level rule is the
	// whole formatter. When the host model lands it gains ObjectWithString
	// dispatch here (see doc.go).
	env.SetFormatter(func(_ *minijinja.State, val value.Value, escape func(string) string) string {
		if val.IsNone() {
			return escape("null")
		}
		return escape(val.String())
	})

	// env.set_debug(true).
	env.SetDebug(true)

	// env.set_trim_blocks(true); env.set_lstrip_blocks(true). Start from the
	// engine defaults so no unrelated whitespace knob is disturbed, then set the
	// two BAML sets, exactly as the fork's own oracle harness does.
	ws := syntax.DefaultWhitespace()
	ws.TrimBlocks = true
	ws.LstripBlocks = true
	env.SetWhitespace(ws)

	// See the doc comment: BAML relies on the engine default (none) for its
	// html-less prompt templates; we make it explicit.
	env.SetAutoEscapeFunc(func(string) minijinja.AutoEscape { return minijinja.AutoEscapeNone })

	// get_env additions/overrides.
	env.AddFilter("regex_match", regexMatchFilter)
	env.AddFilter("sum", sumFilter) // overrides the engine builtin sum
	env.SetUnknownMethodCallback(pycompat.UnknownMethodCallback)

	// Simple globals matching internal/nativeprompt/env.go.
	env.AddGlobal("_", value.FromObject(roleHelper{}))
	env.AddGlobal("ctx", value.FromObject(ctxObject{outputFormat: cfg.OutputFormat}))

	return env
}
