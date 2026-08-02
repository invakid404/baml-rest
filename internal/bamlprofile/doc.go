// Package bamlprofile is baml-rest's leaf BAML profile over the pinned pure-Go
// minijinja fork github.com/invakid404/minijinja-go/v2@v2.16.0-baml.6.
//
// The fork is a generic, BAML-exact minijinja ENGINE. It deliberately does not
// carry BAML's environment configuration: a consumer still owns BAML's
// get_env() setup, host value model, custom globals, and the prompt/constraint
// lowering split (fork oracle/corpus.go documents this boundary). This package
// is that consumer. It supplies the BAML-rest-owned profile over the fork so
// that internal/nativeprompt and internal/debaml can, in later slices, render
// prompts and evaluate constraints byte-for-byte the way BAML v0.223 does,
// WITHOUT patching, wrapping, or calling BAML at runtime.
//
// # Dependency direction
//
//	fork minijinja-go  <-  internal/bamlprofile  <-  nativeprompt / debaml  <-  admission and serving
//
// This package imports the fork and Go stdlib only. It MUST NOT import
// internal/nativeprompt, internal/debaml, any serving code, nanollm, or the
// stock BAML runtime/CFFI. The shipped path is pure-Go, CGO-free, and
// host-zero-nanollm. Stock BAML v0.223 exists in this package only as a
// test-only differential oracle behind the `integration` build tag
// (see ./profileoracle), mirroring internal/nativeprompt/staticoracle.
//
// # What this slice builds
//
// PR-1 — the get_env engine configuration:
//   - trim_blocks + lstrip_blocks, autoescape off, set_debug(true);
//   - the none -> "null" top-level formatter;
//   - the fork's BAML-exact builtin filter/test/function registry PLUS BAML's
//     get_env additions/overrides: regex_match and BAML's sum (see filters.go);
//   - the pycompat unknown-method callback;
//   - the simple globals `_` (role/chat helper) and `ctx` (output_format),
//     matching internal/nativeprompt/env.go.
//
// Authority: jinja_helpers.rs get_env() in BAML v0.223
// (engine/baml-lib/baml-core/src/ir/jinja_helpers.rs:7-36).
//
// PR-2 — the enum & class host value model (enum.go, class.go, list.go):
//   - per-enum namespace globals installed from Config.Enums (Environment.
//     AddGlobal), keyed by canonical variant name, non-enumerable;
//   - enum-member objects with separate canonical / alias / enum-name fields:
//     display is alias-or-canonical, `.value` is canonical only, and
//     ObjectWithValueCmp is BAML's exact enum comparator — closing the #597
//     enum-`==` fence at the profile level (both operand orders + membership);
//   - class objects (canonical attribute/iteration/index access, alias only in
//     display), host list objects, and a hand-written Rust debug renderer for
//     direct class/list rendering (nested none -> null, aliased keys, four-space
//     nesting, trailing commas), in both the alternate `{:#?}` and the
//     non-alternate spelling BAML's two Display impls select.
//
// The host objects render through value.ObjectWithString and signal their
// non-enumerability by being ObjectReprMap objects that implement no
// value.MapObject/MapGetter. Both are GENERIC fork seams (v2.16.0-baml.4
// PATCHES #102-#105, extended in -baml.6 PATCHES #106-#108 to pycompat
// str.join/keys/values/items/get, dictsort, and the pprint/debug renderers):
// the fork's Value.String/Value.Repr and the alternate-debug renderers dispatch
// ObjectWithString, its equality/ordering/iteration/join branch on the ok
// boolean of Value.MapKeys, and its map API reaches a host map through
// MapKeys/GetItem rather than AsMap. So this package adds no per-filter,
// per-container, or per-operator special case, and no comparator answer BAML's
// own comparator would not give.
//
// Authority: baml_value_to_jinja_value.rs and lib.rs in BAML v0.223 (cited at
// each type). Byte-exactness is proven by the stock-CFFI differential in
// ./profileoracle.
//
// PR-3 — the constraint lowerer & typed façade (constraints.go, project.go):
//   - EvaluateConstraints over resolved Constraint/ConstraintRequest values,
//     reproducing run_user_checks + evaluate_predicate: a bare get_env()
//     environment, the stored expression wrapped verbatim as `{{ <expr> }}`,
//     exactly one bound name (`this`), and the rendered-TEXT "true"/"false"
//     classifier — a non-boolean rendering is an ERROR, never a failed predicate;
//   - the batch aborting on the first evaluator error with NO partial report,
//     mirroring `collect::<Result<Vec<_>>>()`, and a false ASSERT surfacing as
//     ConstraintReport.AssertFailed while a false CHECK is retained as a result;
//   - the CONSTRAINT-side serde projection of `this` (project.go), which is a
//     different lowering from PR-2's prompt host model: an enum is its CANONICAL
//     string (no alias), a class is an ordered map of CANONICAL keys (no alias
//     key, no `{map:#?}` render), a list is a plain sequence, and every
//     unsupported shape fails closed;
//   - the environment split: newGetEnvBase carries get_env's engine
//     configuration, New adds the prompt globals, newConstraintEnvironment does
//     not — so `_`, `ctx` and the enum namespaces are undefined in a predicate,
//     as they are in stock BAML. No fork change was needed for any of it.
//
// Authority: jinja_helpers.rs:67-93, jsonish coercer mod.rs:322-338 and
// field_type.rs:180-294, baml_value.rs:41-57 (cited at each declaration). Parity
// is proven by the stock-CFFI CallFunctionParse differential in ./profileoracle,
// which runs BAML's real coercer rather than merely rendering a prompt.
//
// Two stock behaviors PR-3 MEASURED and did not reproduce, both owned by the
// serving slice and ledgered on #583:
//
//   - stock BAML evaluates NO constraints on a bare `string` return type —
//     jsonish::from_str short-circuits before coercion (jsonish/src/lib.rs:
//     233-237). EvaluateConstraints has no return type and evaluates whatever it
//     is given, so Slice 7.2 must reproduce the skip; evaluating there would
//     REJECT responses stock accepts. Pinned by
//     TestStockSkipsConstraintsOnBareStringReturn.
//   - duplicate check LABELS collapse in the response representation (last
//     wins), while PR-3 returns both results in declared order. The stable
//     policy is 7.2's; the collapse is measured by
//     TestConstraintDuplicateLabelCollapse.
//
// # What is DEFERRED / DECLINED (explicitly NOT built here)
//
//   - Media host values (Image/Audio/Pdf/Video, URL/Base64/File): BAML's serde
//     marker must be parsed by prompt lowering into a provider media body, which
//     this unwired leaf has no path for. There is no media constructor, so a
//     media value cannot enter a render context at all. Tracked on #602.
//   - BAML's render-layer format(type=json|yaml|toon) host-serialization
//     override: the get_env-level `format` is the fork's printf filter, so a
//     class|format(type=...) ERRORS rather than silently emitting a
//     serialization (proven in host_test.go). Tracked on #602.
//   - The prompt lowerer, and the descriptor/parser layer that would supply
//     resolved enum/class aliases, ordered fields and BARE constraint
//     expressions in production (PR-2 and PR-3 consume them as explicit typed
//     inputs). In particular this leaf does NOT parse BAML source, strip
//     `{{ ... }}` from a constraint expression, typecheck a Jinja expression, or
//     import BAML IR/descriptors — a bracket-wrapped expression is REJECTED, not
//     normalized.
//   - Constraint ingress for media values and BamlValue::Map: PR-2 provides no
//     host constructor for either, so projectConstraintThis declines them rather
//     than guessing a projection. Tracked on #602/#572.
//   - The serving side of constraints: translating descriptors into resolved
//     Constraints, running them during native response parsing, the public
//     `Checked<T>`/JSON envelope and its stable ordering, assert-rejection
//     message wording and first-five presentation, duplicate-label policy,
//     streaming/partial semantics, and fallback routing. All Slice 7.2.
package bamlprofile
