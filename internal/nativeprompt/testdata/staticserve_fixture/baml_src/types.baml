// Simple two-field class return for the bare ctx.output_format corpus. Two
// scalar fields keep the rendered output-format block small and exact (field
// order, newlines, and the "Answer in JSON..." prefix) so it can be compared
// byte-for-byte between BAML's renderer and the native
// internal/schema/outputformat renderer.
//
// NOTE on the project-wide surface: no @@dynamic, no @skip, and no
// template_string anywhere in this project — the claimed corpus must stay inside
// the narrow static surface, and a project template_string would attach to every
// function and make the whole claimed set decline (FeatureTemplateString). A
// project @@dynamic enum would likewise decline V3 for EVERY function.
//
// The project DOES declare an enum (`Color`), media-free input classes, and — as
// the de-BAML Slice 7.1b media-unreachability witnesses — a `MediaBundle` class
// carrying media. See the sections at the end of this file. All of them are
// INPUT types: no function returns them, so the output-format half of the corpus
// above is unaffected, and the media-bearing signatures are exactly the routes
// proven to reach no native admission.
class StaticAnswer {
  answer string
  confidence int
}

// De-BAML Phase 2 (recursive classes). The NARROW served recursive family: a
// SELF-recursive class (Node) and a MUTUALLY-recursive SCC (A <-> B). Each has a
// required scalar `value` field plus EXACTLY ONE direct nullable-class edge
// (`next Node?` / `b B?` / `a A?`) — the only recursive edge shape the native
// static-final lane admits. The required `value` field keeps every class at >=2
// fields, so none is a one-field implied-recursion shape (Loop{next Loop?}),
// which stays a tracked #583 decline (its pair-guard regression is separate).
// BAML v0.223's Go generator lowers `Node?` to a `*Node` pointer, so these
// generate legal mutually-recursive Go carriers (the recursive-return decoder
// compile proof).
class Node {
  value string
  next Node?
}

class A {
  value string
  b B?
}

class B {
  value string
  a A?
}

// The ONE-FIELD implied-recursion shape (a recursive class with NO scalar value
// field). It is the pair-guard regression witness, NOT a served schema: the
// admission fingerprint (isProvenRecursiveStaticReturn) DECLINES it pre-claim, so
// native falls back to BAML. Its differential proves stock BAML v0.223 treats a
// self-referential (implied-key) input as a circular-reference parse error, matching
// native's Class::coerce pair-guard.
class Loop {
  next Loop?
}

// A recursively-@stream.*-annotated variant of the Node shape (only for the SERVE
// fixture). @stream.done on the value field is PRESERVED by static lowering onto this
// final non-streaming descriptor, so the exact recursive fingerprint DECLINES it and
// the route falls back to BAML with ZERO native sockets (the stream-annotation
// zero-socket ROUTE regression). It is NOT in the differential oracle. (Named NodeAnn,
// NOT NodeStream: a `<method>Stream` name collides with the generated stream-parse
// helper of the un-annotated Node method.)
class NodeAnn {
  value string @stream.done
  next NodeAnn?
}

// De-BAML Phase 3a (recursive ALIASES) — the SERVED structural-recursive alias.
// The EXACT five-arm direct JSON family the native static-final lane serves
// byte-exact vs BAML v0.223: int | string | bool | JSON[] | map<string, JSON>.
// One canonical `JSON` alias, list element = map value = the same bare `JSON`,
// a bare string map key, exactly the five ordered non-null arms, NO float/null
// arm. Its recursive list/map descendants are part of the served family.
type JSON = int | string | bool | JSON[] | map<string, JSON>

// De-BAML Phase 3c (recursive ALIASES) — the SECOND SERVED structural-recursive
// alias. JsonValue adds a `float` arm plus an explicit `null` arm (so the whole
// alias is nullable) and a distinct name; it is admitted by its OWN canonical
// name-pinned fingerprint (IsProvenJsonValueRecursiveAliasStaticFamily), NOT by a
// widening of the `JSON` one. Its lowered form is a NULLABLE union with exactly the
// six ordered stored variants int, float, bool, string, JsonValue[],
// map<string, JsonValue>. Its differential proves the two deltas versus `JSON`: the
// int-vs-float scored split (1 -> int, 1.0/1.5 -> float, byte-exact Go float
// formatting) and the first-class typed null (`JsonValue` null -> nil alias pointer
// / public `null`, where the served `JSON` null -> [] the list fallback).
type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>

// De-BAML Phase 3c — the RESIDUAL DECLINE witness that takes over the
// declined-oracle role now that JsonValue is served. It is the NEAREST possible
// negative: the IDENTICAL arm SET and nullability as JsonValue, with only the arm
// ORDER changed (float before int) and the name changed. Stock BAML v0.223 accepts
// and generates it, but the served fingerprint pins BOTH the canonical name AND the
// ordered variant list, so it declines PRE-TRANSPORT with zero native sockets and
// BAML serves it. That keeps "an alias indistinguishable from the served family
// except for arm order still declines" a concrete, generated-oracle-backed proof
// rather than an assertion about the distant non-alias decline rows.
//
// (An arm-COUNT witness — JsonValue plus an extra `map<string, string>` arm — was
// tried first, per the scope's preference for a wider sibling. Stock v0.223 accepts
// it, but the fixture's ordered-map client hack does not cover a non-recursive map
// arm inside a structural-alias union, so the fixture would not compile. The scope's
// stated fallback, a renamed/reordered alias, is used instead; the arm-COUNT,
// arm-KIND, nullability and metadata rejections are pinned separately by the
// synthetic-bundle fingerprint tests in internal/debaml.)
type JsonValueReordered = float | int | bool | string | null | JsonValueReordered[] | map<string, JsonValueReordered>

// De-BAML Slice 7.1b (resolved static values). The enum + nested input classes
// that make every V3 fact OBSERVABLE in a differential:
//
//   - CANONICAL identity vs DISPLAY: RED/GREEN carry deliberately non-canonical
//     aliases, so a renderer that confused the two is caught immediately;
//     BLUE is unaliased, so the alias-or-canonical display rule is exercised
//     both ways.
//   - CLASS FIELD ORDER + ALIASES: Swatch/Palette declare aliased and unaliased
//     fields in a fixed source order, so a Go-declaration-order or
//     alphabetical-order renderer diverges.
//   - LIST ORDER and NESTING: Palette carries Color[] and a nested Swatch, and
//     StaticRenderPalettes takes Palette[], so item order and the
//     class-inside-list / list-inside-class debug spellings are all covered.
//
// These are INPUT types only; no function returns them, so the output-format
// half of the corpus is unchanged.
enum Color {
  RED @alias("rouge")
  GREEN @alias("vert")
  BLUE
}

class Swatch {
  color Color
  label string @alias("etiquette")
}

class Palette {
  primary Color @alias("principale")
  shades Color[]
  swatch Swatch
  name string
}

// De-BAML Slice 7.1b — the MEDIA unreachability witness (scope section 6, "Media
// stays closed"). A class whose field is media, so the live proof covers not only
// a direct media argument and a media list but a media value reached THROUGH a
// class — the shape a naive input walker would be most likely to miss.
//
// Media is not a V3 input value node, so no function reaching this class gets a
// V3 descriptor, and the generated static seam therefore installs nothing. It is
// declared here (rather than only in a unit fixture) so that fact is proven at
// the LIVE admission boundary, not just at descriptor construction.
class MediaBundle {
  img image
  caption string
}

// De-BAML Slice 7.2b-2 — the TWO NARROW CONSTRAINT-BEARING return types.
//
// These are the exact two concrete generated fixture return types the 7.2b scope
// admits as the first production-admission fingerprint: a flat class with exactly
// `answer string` then `confidence int`, and EXACTLY ONE direct constraint on
// `confidence` — one @check or one @assert, never both and never more.
//
// They exist HERE, in a real generated project, so the generated Go carrier is the
// one BAML's own generator emits rather than a hand-written look-alike: the checked
// field lowers to the generated `Checked[int64]` alias (which the fixture transform
// re-points at bamlutils.Checked, the carrier with deterministic bytes), while the
// assert-only field stays an ordinary int64 because `as_check()` excludes an assert
// from the CFFI check list.
//
// Slice 7.2b-3 turns admission ON for exactly these two: the generated /call for
// each OPENS one native socket and native serves the result. They sit on the SERVED
// side of the static serving partition (staticServingCorpusConstraintServed).
class StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this > 0 }})
}

class StaticAssertAnswer {
  answer string
  confidence int @assert(positive, {{ this > 0 }})
}

// De-BAML Slice 7.2b-3 — the LIVE RETURN-SCHEMA SIBLINGS of the two admitted return
// types.
//
// Each differs from StaticCheckedAnswer in exactly one property — these three do; the
// type-category group below is one-/two-property and says which is which — and each
// must still decline INSIDE admission — descriptored, projector-emitted,
// seam-installed, and refused BEFORE a socket, with BAML supplying the result. They exist in the real
// generated project (rather than only as hand-built Bundles in a unit test) so the
// cutover's narrowness is a fact about the shipping route, not about a fixture.
//
// StaticTwoCheckAnswer     — a SECOND @check. Two entries is a collection the scope
//                            leaves declined even though the carrier is deterministic.
// StaticReorderedAnswer    — the SAME two fields in the OTHER order.
// OtherCheckedAnswer       — byte-for-byte StaticCheckedAnswer's shape under a
//                            DIFFERENT class name: the fixture-identity pin, and the
//                            only sibling whose delta is invisible in the type graph.
class StaticTwoCheckAnswer {
  answer string
  confidence int @check(positive, {{ this > 0 }}) @check(big, {{ this > 5 }})
}

class StaticReorderedAnswer {
  confidence int @check(positive, {{ this > 0 }})
  answer string
}

class OtherCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this > 0 }})
}

// The TYPE-CATEGORY siblings — the families the scope leaves declined (floats, lists,
// unions, optionals). Each keeps the two fields, their order and the label.
//
// Only the FLOAT row isolates the type axis alone: it keeps `this > 0` unchanged, so its
// live zero-socket result is attributable to the type and nothing else. The list, union
// and optional rows necessarily change the PREDICATE too, because `this > 0` is not
// well-typed for a list, a union or a null — a BAML project that tried it would not
// compile. They are therefore type+predicate pairs, and they are honest about it: what
// they prove live is that the family is refused pre-socket, not which of the two
// properties refused it. Independent type-versus-predicate rejection is proven at the
// descriptor/Bundle level by internal/debaml's single-axis sibling corpus, which varies
// one axis at a time because it never has to compile.
//
// A `map<string, int>` sibling is DELIBERATELY absent, and for a generated-client
// reason rather than a gate one: BAML v0.223's Go generator lowers a class-field map to
// `baml.OrderedMap[T]`, a symbol this fixture's client does not carry (the project is
// generated with --skip-baml-module-patch, exactly as the arm-COUNT alias witness note
// in this file records for the same hack). The map family is covered at the
// descriptor/bundle level instead — internal/debaml's sibling corpus and nativeserve's
// admission sibling table both carry one — so the gate is proven; only the LIVE socket
// row is unavailable.
class StaticFloatCheckedAnswer {
  answer string
  confidence float @check(positive, {{ this > 0 }})
}

class StaticListCheckedAnswer {
  answer string
  confidence int[] @check(positive, {{ this|length > 0 }})
}

class StaticUnionCheckedAnswer {
  answer string
  confidence int | string @check(positive, {{ this != null }})
}

class StaticOptionalCheckedAnswer {
  answer string
  confidence int? @check(positive, {{ this != null }})
}

// The METADATA / PREDICATE / LABEL siblings — the same `int` field with exactly one
// property changed.
class StaticAliasedCheckedAnswer {
  answer string
  confidence int @alias("score") @check(positive, {{ this > 0 }})
}

class StaticGtePredicateAnswer {
  answer string
  confidence int @check(positive, {{ this >= 0 }})
}

class StaticNonAsciiLabelAnswer {
  answer string
  confidence int @check(positivé, {{ this > 0 }})
}
