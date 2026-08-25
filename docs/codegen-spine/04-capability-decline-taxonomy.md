# Native capability / decline taxonomy (D3, D7)

Stable, machine-readable feature and decline codes. **Governing principle:** a
descriptor may exist for a method whose serving feature is not admitted, but
**native codegen must never infer support from mere descriptor presence.**
Admission selects a whole method + mode-capability **before** any claim/socket; a
typed pre-socket decline may fall back to generated BAML during transition
(D7), a post-send failure never does. Machine-checked subset:
`internal/codegenspine/manifest.json` (`capabilities`, `declines`) — every
`required_capability_coverage` entry is exercised by a real `.baml` fixture.

## 1. This taxonomy reuses the live codes — it does not invent a parallel set

The native lane already carries a rich, bounded decline vocabulary. This freeze
**references** it rather than duplicating it, so the codes stay singly-sourced.

| Source of record | What it enumerates | Path |
|---|---|---|
| Admission `Stage` + `Reason` | ~100 bounded decline reasons (the primary set) | `nativeserve/admission/decline.go` |
| Prompt-lane `Feature` keys | prompt-render decline features | `internal/nativeprompt/support.go:36-59` |
| Body-lane `Feature` keys | OpenAI-chat-body decline features | `internal/nativebody/support.go:57-122` |
| `VerificationPolicy` | `strict_openai` / `trusted_provider` | `nativeserve/admission/clientmap.go:19-45` |
| Parse/constraint sentinels | `ErrDeBAMLParseUnsupported`, `ErrConstraintUnsupported` | `bamlutils/interfaces.go:1058`, `internal/debaml/constraint_profile.go:92` |

Umbrella decline sentinel: `nativeserve/admission.ErrDeclined`
(`decline.go:308`). **Freeze rule:** the *bounded enums* (admission `Stage`/
`Reason`, the `Feature` key constants, `VerificationPolicy`, `MediaKind`) are
stable codes safe to reuse verbatim. The free-form `fmt.Errorf` decline text in
`internal/nativeschema` (e.g. `"unsupported type"`) and the debaml
`unsupported(...)` message strings are **diagnostic text, not codes** — reuse the
*sentinel identities*, never the message strings.

## 2. Capability codes (the M0 freeze)

Each capability has a stable `code`, a `kind`, and a `proven_status` recording
what the native lane admits **today** (grounded citations in the manifest):

| Code | Kind | Status | Grounding |
|---|---|---|---|
| `static_method` | mode | proven | `bamlutils.StreamingMethod`; `nativeschema.BuildStaticSchemas` |
| `final_call` | mode | proven | `StreamModeCall`; native unary `/call` cohort |
| `call_with_raw` | mode | transitional | `with_raw_unproven` decline (`decline.go:123`) |
| `stream` | mode | transitional | `streaming_unproven` (`decline.go:124`); static streaming not emitted (nativeschema D12) |
| `stream_with_raw` | mode | transitional | `StreamModeStreamWithRaw` |
| `final_parse` | mode | transitional | native-first dynamic `/parse` final w/ oracle; `direct_parse_unproven` (`decline.go:299`) |
| `media_image` | media | proven | only image proven (`nativeprompt/support.go:199-224`) |
| `media_audio` | media | declined | beyond proven corpus; `media_part` (`decline.go:214`) |
| `media_pdf` | media | declined | `MediaPDF`; gate `Kind != MediaImage` |
| `media_video` | media | declined | `MediaVideo` |
| `dynamic_types` | dynamic | transitional | runtime `TypeBuilder.DynamicTypes` overlay — a request-time construct (`interfaces.go:693`), **not** the schema `@@dynamic` marker |
| `schema_dynamic_class` | dynamic | declined | the `.baml` `@@dynamic` class/enum marker; native schema building fail-closed declines it (`nativeschema/build.go:68-69`) |
| `baml_snippets` | dynamic | declined | `TypeBuilder.BamlSnippets`; D4 — parse or reject, never silently ignore |
| `checks` | checks | transitional | `Constraint level=check`; `ErrConstraintUnsupported` out-of-profile |
| `asserts` | checks | transitional | `Constraint level=assert`; `parse_error` on failure |
| `strategy_fallback` | strategy | declined | `fallback_chain` (`decline.go:132`) |
| `strategy_round_robin` | strategy | declined | `round_robin` (`decline.go:133`) |
| `single_leaf_client` | strategy | proven | `not_single_leaf` gates to one leaf (`decline.go:131`) |
| `provider_openai` | provider | proven | `nativebody.ProviderOpenAI`; `PolicyStrictOpenAI` |

`proven_status` is a **closed vocabulary** — `{proven, transitional, declined}`
(test-enforced) — because a later milestone will machine-branch on it: the
scope's build-fails-if-a-retained-endpoint-is-missing rule reads exactly this
field. `transitional` = native may serve it only behind the compare oracle;
`declined` = native must stay on generated BAML and emit a typed pre-claim
decline.

## 3. Feature gating that is already fail-closed (must stay so)

- **Provider:** only `openai` is proven (`nativebody.ProviderOpenAI`,
  `support.go:126`); the only accepted OpenAI client options are the
  `transportTrio` `{model, base_url, api_key}` (`clientmap.go:129`). Everything
  else declines (`headers_option`, `tools_option`, `response_format_option`,
  `request_body_option`, `unproven_client_option`).
- **Roles:** proven set `{system, user, assistant}` (`support.go:133`); others →
  `role_unsupported`.
- **Filters/macros:** only `replace` reproduced (`allowedFilters`,
  `nativeprompt/support.go:69`); `macro|import|include|extends|from|call` blocked.
- **Media:** only `image` proven natively; `audio/pdf/video` decline
  (`media_part`). Note the three `MediaKind` declarations agree on the string
  values `image/audio/pdf/video` but are distinct Go types
  (`bamlutils/media.go:9` int-iota, `schemadescriptor/descriptor.go:179` string,
  `internal/nativeprompt/input.go:10` string) — the native carrier work must not
  assume one shared type.
- **Dynamic (`@@dynamic`) and static streaming** stay declined at the schema
  builder (`nativeschema/build.go` D12 / `build.go:68-69`).

## 4. Decline discipline (D3, D7)

- **Whole-project build strictness (D3):** a native *build* fails on invalid/
  unreadable retained source or a missing retained method. Per-method capability
  declines are allowed **only for transition builds** and must appear in the
  capability manifest — never silently drop a retained method. (Contrast today's
  permissive introspection walk, which skips unreadable/unparseable files,
  `cmd/introspect/main.go:1465-1481`; the native executable must be stricter.)
- **Rollout granularity (D7):** select by whole method + mode-capability before
  claim; no per-type or mid-attempt mixing. A typed pre-socket decline may fall
  back to generated BAML during transition; a post-send failure never does.
- **Representative frozen reasons** (subset of `decline.go`, for the manifest):
  `worker_not_capable`, `flag_disabled`, `not_single_leaf`, `fallback_chain`,
  `round_robin`, `provider_not_openai`, `model_not_literal`, `tools_option`,
  `response_format_option`, `request_body_option`, `media_part`,
  `role_unsupported`, `stream_schema_unsupported`, `output_schema_unbounded`,
  `with_raw_unproven`, `streaming_unproven`, `direct_parse_unproven`.

## 5. Fixture grounding (representative capability corpus)

The manifest binds each required capability category to a real `.baml` file and a
genuine feature signal (parsed with the production `bamlparser`, asserted present
in source):

| Capability | Fixture | Signal |
|---|---|---|
| `static_method`, `final_call` | `integration/testdata/baml_src/functions.baml` | `^function …`, `… ->` |
| `media_image`, `media_audio` | same | `: image`, `: audio` |
| `strategy_fallback`, `strategy_round_robin` | `integration/testdata/baml_src/clients.baml` | `provider baml-fallback`, `provider round-robin` |
| `schema_dynamic_class` (declined `@@dynamic` marker) | `integration/testdata/baml_src/types.baml` | `@@dynamic` |
| `stream`, `checks`, `asserts` | `integration/testdata/parity_baml_src/types.baml` | `@stream.`, `@check(`, `@assert(` |

`dynamic_types` (the transitional runtime **TypeBuilder** overlay) is *not*
groundable from `.baml` source — it is a request-time input, not schema syntax —
so `manifest_test.go` grounds it separately by unmarshalling an actual
`{"type_builder":{"dynamic_types":…}}` payload into `bamlutils.BamlOptions` and
asserting `TypeBuilder.DynamicTypes` is populated
(`TestDynamicTypesCapabilityGroundedInLiveInput`). This keeps the schema-side
`@@dynamic` decline and the runtime TypeBuilder overlay as two distinct,
separately-grounded capabilities.

This is the fixture set M0 establishes; M3+ extends it with JSON-roundtrip
goldens for carrier parity, and M4 wires the "missing retained endpoint fails the
build" rule against `proven_status`.

## 6. Pre-decline sub-codes are STRUCTURAL, not string-derived (M1)

A *pre-decline* is a function that never became a `promptdescriptor.Function`
because an upstream pass (`nativeschema.BuildStaticSchemas` /
`BuildPromptDescriptors`) declined it first. Those passes return a free-form,
human-readable reason string. Early M1 rounds reverse-engineered the precise
sub-code (`media_part`, `schema_dynamic_class`, `checks`, `asserts`,
`unsupported_input_shape`, `unsupported_output_shape`) by scanning that string for
feature words. That was fragile: the same reason prose mentions `@check`,
`@assert`, and `@@dynamic` in explanatory help even when an unrelated attribute is
the cause, quotes user-controlled names, and varies by phrasing — so every new
upstream wording risked regressing a code.

**ADR (M1, fix #6/#7): the feature-from-string parser is dropped entirely; the
sub-code comes from a STRUCTURAL verdict the WINNING producer carries.**
`nativeschema` stamps each decline with a typed `PreDeclineFeature` on the SAME
path that produced it (`BuildPromptDescriptorsWithFeatures`), alongside a
byte-identical reason string. `internal/nativespine.preDeclineCode` is a pure
lookup on that verdict — it reads no reason text, so no help prose and no
user-controlled declaration name (a class, field, or **macro** name) can influence
a code.

- **Precise feature — only where the producer knows it exactly.**
  `schema_dynamic_class` is stamped from the eligibility scan / project-enum /
  macro-argument verdict (`scanDynamic`), including the project-wide `@@dynamic`
  enum that poisons every function. This is carried from the branch that won, so it
  never disagrees with the decline.
- **Context — everywhere else.** Every other decline is named by its reliable
  input-vs-return CONTEXT (`FeatureInputShape` / `FeatureOutputShape`), taken from
  which side of the signature the decline fired on (a return-bundle decline is
  output; an input-value-graph, macro, or eligibility decline is input). The
  context is carried structurally, **not** read from the reason prefix.

**M1 boundary (dark slice) — fix #7.** The static-SCHEMA builder
(`BuildStaticSchemas` / `ValidateOutput`) is *not* instrumented to carry which node
caused its decline. A round-6 attempt to recover the feature by an INDEPENDENT
second walk of the raw return/argument graph was removed: it did not share the
producer's precedence and could name an incidental feature over the real cause —
an earlier `@check` that lowered fine beating a later `media` field, a union
treated as featureless so `image?` missed media, and a single-`@` `@dynamic` field
mis-read as schema-dynamic. So a schema-builder pre-decline now resolves to its
context code (`unsupported_output_shape` / `unsupported_input_shape`), never a
guessed `media_part` / `checks`. **M2** replaces this with full producer
instrumentation (the schema builder carrying the causative `PreDeclineFeature` from
the failing node), restoring precise media/checks sub-codes for these pre-declines.

**Catalogue unchanged (19 codes).** `checks`, `asserts`, and the media codes stay
emittable via the *admitted* classifier path (`classifyOutputSchema`): a VALID
`@check`/`@assert` lowers into the return bundle, the function is admitted, and the
classifier declines it with the precise code. Only the *malformed*/pre-declined
variants fall to context. `DeclineCodes()` == `codegen_admission_declines` is
therefore untouched. The full reason string is always preserved in `Decline.Detail`.
