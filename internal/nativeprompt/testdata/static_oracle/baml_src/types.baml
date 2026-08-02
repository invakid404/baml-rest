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
// The project DOES declare an enum (`Color`) and media-free input classes as of
// de-BAML Slice 7.1b — see the resolved-static-value section at the end of this
// file. Those are INPUT types: no function returns them, so the output-format
// half of the corpus above is unaffected.
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
