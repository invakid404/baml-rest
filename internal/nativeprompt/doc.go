// Package nativeprompt is a de-BAML spike: a native Go reimplementation of
// BAML's dynamic prompt template renderer, built on the first-party
// minijinja-Go port (github.com/mitsuhiko/minijinja/minijinja-go/v2).
//
// It renders exactly one template — the generated dynamic function
// Baml_Rest_Dynamic (cmd/build/dynamic.baml) — reproducing BAML v0.223's
// jinja-runtime behaviour for that template's feature surface:
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
// This package is TEST-ONLY plumbing for the front-end de-BAML arc. It is NOT
// wired into production request building — the served request path stays BAML.
// A companion build-only differential harness (see the //go:build integration
// oracle test) proves this renderer byte-exact against BAML's real runtime
// across a seeded corpus; a fail-closed [Supports] predicate declines any
// prompt shape or media kind the spike does not prove.
//
// Version pinning: minijinja-Go is pinned to v2.16.0, the exact minijinja
// version BAML v0.223 depends on (BoundaryML's fork is one commit — "add
// value_cmp on top of custom_cmp" — over vanilla minijinja 2.16.0). That is
// the tightest achievable version alignment and the reason parity is expected
// to hold. The one place the BoundaryML fork bites — enum value_cmp, where
// BAML's fork routes the == operator through custom comparison and minijinja-Go
// does not — is documented and proven with a fixture in valuecmp_test.go; it
// does not affect the dynamic template, which performs no enum comparison.
package nativeprompt
