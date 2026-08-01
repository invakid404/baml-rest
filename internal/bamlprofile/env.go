package bamlprofile

import (
	"fmt"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/pycompat"
	"github.com/invakid404/minijinja-go/v2/syntax"
	"github.com/invakid404/minijinja-go/v2/value"
)

// Config carries the per-render inputs the get_env-equivalent environment needs
// that are not fixed engine configuration.
//
// It is the typed boundary where the host value model plugs in: Enums installs
// BAML's per-enum namespace globals; class/enum/list host VALUES enter through
// the render context, built with EnumMember / ClassValue / ListValue. Aliases
// and ordered fields are RESOLVED inputs — this leaf does not parse BAML source
// or import BAML IR (that descriptor-adapter is a later slice).
type Config struct {
	// OutputFormat is the pre-rendered ctx.output_format block that BAML installs
	// before rendering (it is the schema description a prompt reaches via
	// {{ ctx.output_format }}). It may be empty when the template never reaches
	// ctx.output_format, or when the function returns a schemaless type such as
	// string. Computing its full BAML v0.223 surface from a return type is a
	// later-slice concern; this slice takes it pre-rendered, exactly as
	// internal/nativeprompt/env.go does today.
	OutputFormat string

	// Enums are the resolved enum declarations. New installs one namespace global
	// per enum (Color -> Color.RED), reproducing BAML's render-time per-enum
	// global injection (jinja-runtime/src/lib.rs:316-327,642-658). BAML injects a
	// global for EVERY enum declared in the project, not only referenced ones, so
	// a caller passes the full resolved set. Order is irrelevant (globals are
	// keyed by name); it is a slice, not a map, to keep it an explicit ordered
	// input rather than implying map-iteration semantics.
	Enums []EnumDef
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
//
// New returns an error only when Config.Enums is malformed (an empty enum or
// variant name, a duplicate variant, or two EnumDefs with the same Name). That
// is a resolved-metadata contract violation, so it fails loud here rather than
// installing a global that renders or compares wrong.
func New(cfg Config) (*minijinja.Environment, error) {
	env := minijinja.NewEnvironment()

	// BAML's custom formatter: a top-level none renders as the literal "null"
	// (not minijinja's default empty string); everything else renders through the
	// engine's escaping path. Under AutoEscapeNone the escape func is the
	// identity, so this is none -> "null", else the value's display string.
	//
	// Nested none inside a host class/list is handled by the host debug walker
	// (debugValue in class.go); this top-level rule is the none -> "null" that
	// BAML's jinja_helpers formatter applies before rendering.
	//
	// Object rendering is NOT this formatter's business. The fork's Value.String
	// dispatches value.ObjectWithString itself (v2.16.0-baml.4, PATCHES #104),
	// which is BAML's escape_formatter reaching an object's own render/Display —
	// and, being at the fork's primitive, it composes through native containers,
	// |join, filter coercions and `~` as well, which a formatter-only branch here
	// never could. The top-level none -> "null" rule is the only get_env delta.
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

	// The get_env simple globals, matching internal/nativeprompt/env.go. Declared
	// ONCE so the reserved-name preflight below derives its set from this same
	// list: a future third get_env global is then automatically protected against
	// an enum-name collision, with no second place to keep in sync.
	simpleGlobals := []struct {
		name  string
		value value.Value
	}{
		{"_", value.FromObject(roleHelper{})},
		{"ctx", value.FromObject(ctxObject{outputFormat: cfg.OutputFormat})},
	}
	for _, g := range simpleGlobals {
		env.AddGlobal(g.name, g.value)
	}

	// Per-enum namespace globals: Color -> {RED: <member>, ...}, keyed by
	// canonical variant name, non-enumerable — BAML's render-time injection
	// (lib.rs:316-327). A malformed enum descriptor fails loud.
	//
	// PREFLIGHT, then install: every EnumDef is validated and every name checked
	// for a collision BEFORE the first AddGlobal. Environment.AddGlobal is a plain
	// map assignment, so a colliding name silently REPLACES whatever was there —
	// a malformed resolved-metadata set rendering as a valid-but-wrong template.
	// Preflighting also keeps the failure atomic: a rejected Config never yields a
	// half-populated environment.
	//
	// The reserved names are DERIVED from the simpleGlobals installed just above,
	// so this check tracks that list rather than restating it. Without it an
	// EnumDef named "ctx" or "_" would clobber a get_env global (those go in
	// FIRST, so the enum would win) and `ctx.output_format` would go silently
	// empty. Neither name can occur in real resolved metadata anyway: stock BAML
	// v0.223 REJECTS `enum ctx {...}` and `enum _ {...}` at CreateRuntime, so a
	// caller supplying one is supplying something BAML could never have produced.
	// Rejecting it is the conservative, fail-loud decline.
	namespaces := make([]*enumType, 0, len(cfg.Enums))
	reserved := make(map[string]struct{}, len(simpleGlobals))
	for _, g := range simpleGlobals {
		reserved[g.name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(cfg.Enums))
	for _, def := range cfg.Enums {
		t, err := newEnumType(def)
		if err != nil {
			return nil, err
		}
		if _, bad := reserved[def.Name]; bad {
			return nil, fmt.Errorf("bamlprofile: enum definition %q collides with the get_env global of the same name", def.Name)
		}
		if _, dup := seen[def.Name]; dup {
			return nil, fmt.Errorf("bamlprofile: duplicate enum definition %q", def.Name)
		}
		seen[def.Name] = struct{}{}
		namespaces = append(namespaces, t)
	}
	for _, ns := range namespaces {
		env.AddGlobal(ns.name, value.FromObject(ns))
	}

	return env, nil
}
