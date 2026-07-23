// Simple two-field class return for the bare ctx.output_format corpus. Two
// scalar fields keep the rendered output-format block small and exact (field
// order, newlines, and the "Answer in JSON..." prefix) so it can be compared
// byte-for-byte between BAML's renderer and the native
// internal/schema/outputformat renderer.
//
// NOTE: no enum, no @@dynamic, no @skip, no media, no template_string anywhere
// in this project — the claimed corpus must stay inside the narrow static
// surface, and a project template_string would attach to every function and
// make the whole claimed set decline (FeatureTemplateString).
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
