// Package nativeprompt is baml-rest's native Go implementation of BAML's prompt
// template renderer: a dynamic parity renderer for the generated
// Baml_Rest_Dynamic prompt plus a deliberately narrow static candidate, both
// rendered through internal/bamlprofile — the leaf BAML profile over the pinned
// BAML-exact minijinja fork github.com/invakid404/minijinja-go/v2.
//
// The environment seam is rendercontext.go. nativeprompt owns the prompt
// language it recognizes, the marker protocol, lowering and every serving
// decision; bamlprofile owns BAML's get_env() configuration, the `_` / `ctx`
// globals, the per-enum namespaces and the enum/class/list host value model.
// Before Slice 7.1a this package built its own environment over the pre-fork
// external engine github.com/mitsuhiko/minijinja/minijinja-go/v2 and duplicated
// the `_` / `ctx` globals locally; both are gone.
//
// The dynamic renderer ([Render]/[Supports]) renders exactly one template — the
// generated dynamic function Baml_Rest_Dynamic (cmd/build/dynamic.baml) —
// reproducing BAML v0.223's jinja-runtime behaviour for that template's feature
// surface:
//
//   - trim_blocks + lstrip_blocks whitespace control;
//   - the top-level none -> "null" custom formatter;
//   - the _.role / _.chat helper (role positional or role= kwarg; all other
//     kwargs, e.g. cache_control, become message metadata; the magic-delimiter
//     emit + post-render split that reconstructs chat messages);
//   - media parts (BamlValue::Media -> a magic-delimiter + JSON marker; the
//     post-render split reconstructs a media part; text chunks are trim()'d and
//     empty chunks dropped);
//   - bare ctx.output_format wired to the native internal/schema/outputformat
//     renderer with default options;
//   - the built-in replace filter (m.content | replace("{output_format}", ...));
//   - the prompt dedent-by-minimum-leading-whitespace + trim preprocessing;
//   - the RenderedPrompt Completion-vs-Chat decision.
//
// The static lane ([RenderStatic]/[SupportsStatic]) consumes a retained
// Version-3 promptdescriptor.Function plus the ORDERED, already-typed argument
// vector a generated projector produced, and renders a deliberately narrow
// static surface through the SAME environment, dedent/trim, and lowering as the
// dynamic path:
//
//   - literal text with direct interpolation of a bound scalar, enum member, or
//     list of those (de-BAML Slice 7.1b);
//   - the exact canonical-identity enum equality forms and the two stock-proven
//     one-element membership forms;
//   - fixed text-only _.role/_.chat blocks and bare ctx.output_format.
//
// Values are bound by the V3 binder (static_bind.go) from the descriptor's
// source-resolved universe — never from Go reflection, JSON, or a raw argument
// map. [SupportsStatic] is a CLOSED allowlist over token shapes PLUS a V3 type
// gate: it accepts only the exact expression forms it proves, with operands it
// resolved, and declines everything else through the shared
// ErrUnsupported/Decline contract. Notable deliberate declines: a display alias
// is never an identity, and a direct CLASS render is refused because stock
// BAML's Go client does not print a reproducible field order for one.
//
// # Gating
//
// Both lanes are reached from nativeserve admission under the single
// BAML_REST_USE_DEBAML umbrella flag. With the flag off no native callback is
// installed and the request stays on the complete BAML path; there is no
// engine-specific flag and no runtime fallback to a second renderer. An
// unsupported template or value DECLINES before render, and the caller routes to
// BAML.
//
// # Proof
//
// Build-only differential harnesses prove the admitted surface byte-exact
// against stock BAML v0.223: the dynamic corpus through the in-process dynclient
// (see the //go:build integration oracle test here), the static corpus through a
// generated stock client (./staticoracle), and the profile leaf's host-value
// semantics through stock CFFI (internal/bamlprofile/profileoracle). The
// stock runtime is never a production dependency — the shipped path is pure Go
// and CGO-free.
//
// # Version pinning
//
// The engine is github.com/invakid404/minijinja-go/v2 v2.16.0-baml.6: minijinja
// 2.16.0 (the exact version BAML v0.223 depends on) plus BAML's value_cmp fork
// commit and the host-object seams the profile needs. Because value_cmp is
// present, `enum == "NAME"` now answers as BAML does — the #597 divergence the
// pre-fork engine could not express is closed at the ENGINE level (see
// valuecmp_test.go). It is not thereby ADMITTED: SupportsStatic still declines
// comparison/containment as FeatureEnumComparison, because admitting it needs
// the resolved static host-type seam that Slice 7.1b builds.
package nativeprompt
