// Claimed static corpus for the Phase 3 slice-2 differential oracle. Every
// function here stays strictly inside the narrow static surface the native
// renderer claims (literal text + direct primitive interpolation, fixed
// text-only role blocks, and bare ctx.output_format). The oracle drives each
// function's generated Request.<Function> method and compares BAML's rendered
// provider body byte-for-byte with native RenderStatic.

// Literal text + direct string interpolation, rendered as a Completion (no
// roles). Exercises raw-string dedent/trim and KindCompletion.
function StaticCompletion(topic: string) -> string {
  client StaticOracleClient
  prompt #"
    Write a short, plain note about {{ topic }}.
  "#
}

// Exact string/int/float/bool binding and MiniJinja scalar display in a single
// Completion body. The oracle drives several value rows (ASCII/Unicode strings,
// int extremes, finite floats, both bools) so the byte-exact scalar formatting
// of native minijinja-go is proven against BAML's minijinja fork.
function StaticPrimitiveArgs(text: string, count: int, ratio: float, flag: bool) -> string {
  client StaticOracleClient
  prompt #"
    text={{ text }}
    count={{ count }}
    ratio={{ ratio }}
    flag={{ flag }}
  "#
}

// Text-only chat with fixed system -> user -> assistant roles. Uses the
// positional _.role("...") form and the _.chat(role="...") alias/kwarg form,
// each message carrying one literal line plus one primitive interpolation.
function StaticRoleChat(topic: string, count: int) -> string {
  client StaticOracleClient
  prompt #"
    {{ _.role("system") }}
    You are a concise assistant.
    {{ _.role("user") }}
    Tell me about {{ topic }}.
    {{ _.chat(role="assistant") }}
    Here are {{ count }} facts.
  "#
}

// Bare ctx.output_format inside a chat message, alongside a primitive
// interpolation. Rendered from the function's retained StaticAnswer return
// descriptor with default native output-format options.
function StaticOutputFormat(topic: string) -> StaticAnswer {
  client StaticOracleClient
  prompt #"
    {{ _.role("user") }}
    Summarize {{ topic }} for me.
    {{ ctx.output_format }}
  "#
}

// Bare ctx.output_format inside a Completion (no roles), proving the completion
// `prompt` body preserves the rendered output-format block exactly.
function StaticCompletionOutputFormat(topic: string) -> StaticAnswer {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }}.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 2 (recursive classes) — SELF-recursive return. Bare
// ctx.output_format Completion whose return descriptor is the self-recursive
// Node{value string; next Node?}. Proves the native static-final lane renders,
// serves, and byte-exactly reproduces BAML v0.223's Parse for a self-recursive
// return, and that DecodeStaticFinal[Node] round-trips BAML's *Node pointer tree.
function StaticRecursiveNode(topic: string) -> Node {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as a linked list of notes.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 2 (recursive classes) — MUTUALLY-recursive return rooted at A.
// The return descriptor is A, whose `b B?` edge points into the A<->B SCC.
// Proves the target-direction (A-rooted) generated decoder instantiation.
function StaticRecursiveA(topic: string) -> A {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as an alternating A/B chain.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 2 (recursive classes) — MUTUALLY-recursive return rooted at B.
// The return descriptor is B, whose `a A?` edge points into the A<->B SCC.
// Proves the OTHER target-direction (B-rooted) generated decoder instantiation
// for the same mutual SCC.
function StaticRecursiveB(topic: string) -> B {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as an alternating B/A chain.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 2 (recursive classes) — the ONE-FIELD implied-recursion witness. The
// return descriptor is Loop{next Loop?}, which the served fingerprint DECLINES. Used
// ONLY by the pair-guard differential: stock BAML v0.223 Parse.StaticRecursiveLoop on
// a self-referential input errors (circular reference), matching native.
function StaticRecursiveLoop(topic: string) -> Loop {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as a self-referential loop.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 2 (recursive classes) — the @stream.*-annotated zero-socket ROUTE
// regression. The return descriptor is NodeAnn{value string @stream.done; next
// NodeAnn?}; static lowering preserves the @stream.done annotation, so the served
// fingerprint DECLINES it pre-transport and the generated /call opens ZERO native
// sockets (BAML serves). Serve-fixture only; not in the differential oracle.
function StaticRecursiveNodeAnn(topic: string) -> NodeAnn {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as an annotated linked list.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 3a (recursive ALIASES) — the SERVED direct five-arm JSON return.
// Bare ctx.output_format Completion whose return descriptor is the structural
// recursive alias `JSON`. Proves the native static-final lane renders, serves, and
// byte-exactly reproduces BAML v0.223's Parse for a recursive-alias return
// (map sorted-public / insertion-internal, broad-union scoring/hint, null -> []).
function StaticRecursiveAliasJSON(topic: string) -> JSON {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as arbitrary JSON.
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 3c — the SECOND SERVED recursive-alias return, FINAL LANE ONLY. Bare
// ctx.output_format Completion whose return descriptor is the nullable structural
// recursive alias `JsonValue`. Proves the native static lane renders, serves, and
// byte-exactly reproduces BAML v0.223's Parse for the float+null family (int-vs-float
// scoring, Go float formatting, first-class typed null).
//
// STREAMING IS NOT ADMITTED for this family: /stream declines pre-transport and BAML
// serves it, so this method appears in the served corpus of the FINAL cutover manifest
// and on the DECLINED side of the stream manifest. The stream gate admits by descriptor
// shape pre-socket and a claimed stream has no route back to BAML, so a family whose
// parse can still decline on a VALUE must not claim a stream socket — see
// internal/debaml/static_stream_serve.go for the full reasoning and the residual ledger
// that gates reopening it.
function StaticRecursiveAliasJsonValue(topic: string) -> JsonValue {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as arbitrary JSON (wide).
    {{ ctx.output_format }}
  "#
}

// De-BAML Phase 3c — the RESIDUAL DECLINED-oracle return. Its alias has the SAME arm
// set and nullability as the served JsonValue with only the arm ORDER (and the name)
// changed, so the name-and-order-pinned fingerprint DECLINES it pre-transport: native
// opens ZERO sockets and BAML serves. It is the nearest generated negative witness for
// the Phase-3c fingerprint.
function StaticRecursiveAliasJsonValueReordered(topic: string) -> JsonValueReordered {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }} as arbitrary JSON (reordered arms).
    {{ ctx.output_format }}
  "#
}

// ---------------------------------------------------------------------------
// De-BAML Slice 7.1b — resolved static VALUES and the #597 enum fence.
//
// Every function below returns `string` so the corpus stays about VALUE
// rendering: no ctx.output_format, no schema, just the exact bytes the enum /
// class / list host values and the admitted enum expressions produce. The
// oracle drives each one through the generated Request.<Function> with typed
// Go values and compares BAML's provider body byte-for-byte with the native
// descriptor -> projector -> binder -> RenderStatic leg.
// ---------------------------------------------------------------------------

// #597 row 1: canonical-name equality. Stock answers `true`.
function StaticEnumCanonicalEq() -> string {
  client StaticOracleClient
  prompt #"
    {{ Color.RED == 'RED' }}
  "#
}

// #597 row 2: DISPLAY-ALIAS equality. Stock answers `false` — and native
// DELIBERATELY DECLINES it. This is the required negative control: 7.1b claims
// canonical identity only, so the row is called through the stock client and
// asserted as a native admission decline plus a live BAML fallback.
function StaticEnumDisplayAliasEq() -> string {
  client StaticOracleClient
  prompt #"
    {{ Color.RED == 'rouge' }}
  "#
}

// #597 row 3: same-member equality. Stock answers `true`.
function StaticEnumSameMemberEq() -> string {
  client StaticOracleClient
  prompt #"
    {{ Color.RED == Color.RED }}
  "#
}

// #597 row 4: one-element membership, canonical string in a member list.
function StaticEnumCanonicalInMemberList() -> string {
  client StaticOracleClient
  prompt #"
    {{ 'RED' in [Color.RED] }}
  "#
}

// #597 row 5: different-member equality. Stock answers `false`.
function StaticEnumDifferentMemberEq() -> string {
  client StaticOracleClient
  prompt #"
    {{ Color.RED == Color.BLUE }}
  "#
}

// Reverse-form witnesses. Only forms the stock fixture proves are admitted; no
// symmetry is inferred beyond these.
function StaticEnumReverseCanonicalEq() -> string {
  client StaticOracleClient
  prompt #"
    {{ 'RED' == Color.RED }}
  "#
}

function StaticEnumMemberInCanonicalList() -> string {
  client StaticOracleClient
  prompt #"
    {{ Color.RED in ['RED'] }}
  "#
}

// Enum ARGUMENT vs member token, both operand orders.
function StaticEnumArgMemberEq(color: Color) -> string {
  client StaticOracleClient
  prompt #"
    {{ color == Color.RED }}
  "#
}

function StaticEnumMemberArgEq(color: Color) -> string {
  client StaticOracleClient
  prompt #"
    {{ Color.RED == color }}
  "#
}

// Enum ARGUMENT vs canonical string, both operand orders.
function StaticEnumArgCanonicalEq(color: Color) -> string {
  client StaticOracleClient
  prompt #"
    {{ color == 'RED' }}
  "#
}

function StaticEnumCanonicalArgEq(color: Color) -> string {
  client StaticOracleClient
  prompt #"
    {{ 'RED' == color }}
  "#
}

// ADMITTED direct host-value renders. These are the rows that prove BINDING
// rather than leaf-engine behavior: the bytes exist only because the descriptor
// carried resolved aliases/element order and the binder built real host values.
// Each is compared byte-for-byte against stock BAML.
//
// (The CLASS renders further below are NOT in this group — they are deliberate
// decline witnesses; see their own header.)
function StaticRenderEnum(color: Color) -> string {
  client StaticOracleClient
  prompt #"
    {{ color }}
  "#
}

function StaticRenderList(colors: Color[]) -> string {
  client StaticOracleClient
  prompt #"
    {{ colors }}
  "#
}

// A SCALAR list, so the admitted "bare list render" row is differentially
// covered for a scalar element type too, not only for enum elements.
function StaticRenderStrings(tags: string[]) -> string {
  client StaticOracleClient
  prompt #"
    {{ tags }}
  "#
}

// ---------------------------------------------------------------------------
// DELIBERATE DECLINE WITNESSES — direct CLASS rendering.
//
// These two are NOT admitted, and that is the point of keeping them here. Stock
// BAML v0.223's generated Go client encodes a class argument as
// `fields := map[string]any{...}` and hands that map to EncodeClass, so the
// field order the direct debug-map render prints follows GO MAP ITERATION and
// differs between runs of the SAME call
// (internal/nativeprompt/staticoracle's TestStockClassRenderOrderIsNonDeterministic
// measures exactly that against this fixture). There is therefore no byte-exact
// answer for native to reproduce, and admitting the shape would make the native
// serve path's plan byte-compare fail RANDOMLY.
//
// They stay in the corpus so the decline keeps its EVIDENCE: the oracle still
// builds each one through the generated stock Request.<Function>, then asserts
// native's pre-claim decline. Section 4 of the 7.1b scope lists a bare class
// render as admitted; this is the one documented under-approximation, tracked as
// #583 and reopened only if BAML's Go client gains an order-preserving class
// encoding.
//
// Their CONTENT is still deliberately rich (an aliased enum field, a list of
// enums, a nested class with its own aliased field, a plain scalar, and a
// list-of-classes) so that if the shape is ever admitted, the witnesses already
// exercise every nested arm.
// ---------------------------------------------------------------------------
function StaticRenderPalette(palette: Palette) -> string {
  client StaticOracleClient
  prompt #"
    {{ palette }}
  "#
}

function StaticRenderPalettes(palettes: Palette[]) -> string {
  client StaticOracleClient
  prompt #"
    {{ palettes }}
  "#
}

// De-BAML Slice 7.1b — the STATIC STREAM witnesses for value binding.
//
// The unary rows above all return `string`, whose stream the native static stream
// route does not claim, so they could only ever prove the stream route DECLINES.
// These two return the SERVED recursive-alias family `JSON` — the one shape the
// native static stream lane admits — while their PROMPTS differ only in whether
// the admitted grammar covers them. That isolates the value/grammar gate as the
// single variable on the stream route:
//
//   - StaticStreamRenderEnum binds an enum ARGUMENT and renders it, so the stream
//     is claimed natively;
//   - StaticStreamAliasEq is the display-alias equality negative neighbour, so the
//     stream declines PRE-TRANSPORT and BAML serves it.
//
// Serve-fixture only: the stock differential oracle owns the rendered bytes.
function StaticStreamRenderEnum(color: Color) -> JSON {
  client StaticOracleClient
  prompt #"
    {{ color }}
  "#
}

function StaticStreamAliasEq(color: Color) -> JSON {
  client StaticOracleClient
  prompt #"
    {{ color == 'rouge' }}
  "#
}

// ---------------------------------------------------------------------------
// De-BAML Slice 7.1b — MEDIA unreachability witnesses (scope section 6).
//
// Media has no V3 input value node, so BuildPromptDescriptors emits NO descriptor
// for these three functions and the generated static seam has nothing to install:
// no projector runs, no native callback fires, no native socket opens, and BAML
// serves them exactly as it always has — with the umbrella flag ON.
//
// They exist so that is proven at the LIVE unary /call and /stream admission
// boundaries rather than only at descriptor construction: a future wiring change
// that admitted a descriptor for a media-bearing signature would light these up
// immediately. Direct media, a media LIST, and media reached THROUGH a class are
// all covered because they take different paths through the generated adapter's
// argument conversion.
// ---------------------------------------------------------------------------

function StaticMediaImage(img: image) -> string {
  client StaticOracleClient
  prompt #"
    Describe {{ img }}
  "#
}

function StaticMediaImageList(imgs: image[]) -> string {
  client StaticOracleClient
  prompt #"
    Describe {{ imgs }}
  "#
}

function StaticMediaInClass(bundle: MediaBundle) -> string {
  client StaticOracleClient
  prompt #"
    Describe {{ bundle }}
  "#
}
