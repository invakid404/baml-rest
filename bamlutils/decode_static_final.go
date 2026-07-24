package bamlutils

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"io"
)

// De-BAML Slice 8C — per-method static final decoder (the neutral, generic core).
//
// DecodeStaticFinal maps a native static SAP / same-response BAML flattened
// CANONICAL JSON into a generated static method's concrete return type T. It is the
// shared core the generated per-method `DecodeNativeStaticFinal` wrappers call: the
// generator emits a thin `func DecodeNativeStaticFinal_<Method>(b []byte) (Return,
// error) { return bamlutils.DecodeStaticFinal[Return](b) }` for each method whose
// return shape is in the INITIAL admitted set, so the per-method decoder is a
// type-instantiation of this one proven core rather than a re-derived unmarshal.
//
// It is deliberately NOT a bare json.Unmarshal (scope §8.1 — a naive unmarshal is
// not BAML-equivalent for aliases/unions/ordered-maps/field-aliases/optionals/
// enums/recursive types, and it silently drops unknown fields and tolerates trailing
// garbage). It uses a STRICT decode — DisallowUnknownFields (BAML's CFFI Decode
// panics on an unexpected field; strict rejection is the closest faithful analogue)
// and a single-value / EOF requirement (a trailing second value is malformed, never
// a silently-accepted first value). Its BAML-equivalence is PROVEN, per admitted
// return shape, by a v0.223 differential fixture (the mapper differential): the
// admitted set is exactly the shapes for which this strict typed decode reproduces
// BAML's CFFI Decode byte-for-byte — a PRIMITIVE SCALAR (string/int/float/bool)
// return, and a FLAT CLASS return whose fields are all non-nullable primitive
// scalars with no @alias/constraints. The admitted set is NARROWED (review P2.1) to
// precisely the instantiations the differential covers over the static_oracle client:
// a top-level `string` (StaticCompletion) and a flat class of `string`|`int` fields
// (StaticAnswer{answer:string, confidence:int}). A top-level int/float/bool scalar, a
// float/bool class field, and every richer shape have no v0.223 differential method,
// so they decline PRE-CLAIM at admittedStaticReturnShape and never reach here. It is
// NOT a generic decoder standing in for arbitrary shapes: the proof obligation is per
// admitted return type, enforced by the narrowed gate + the per-shape differential.
//
// De-BAML Phase 2 (recursive classes) EXTENDS the proven set with three recursive
// pointer-carrier shapes: the self-recursive Node{Value string; Next *Node} and the
// mutual A{Value string; B *B} <-> B{Value string; A *A} SCC. BAML's Go generator
// lowers `T?` to a `*T` pointer, so these are legal recursive Go structs that
// encoding/json decodes cleanly (the alias-only Go ICE is not implicated — classes go
// through pointer indirection). Their strict typed decode reproduces BAML's CFFI
// Decode byte-for-byte once the static absent-optional normalizer makes an omitted
// terminal marshal as `"next":null` (matching BAML's nil-pointer marshal). These
// shapes are admitted ONLY through the isProvenRecursiveStaticReturn fingerprint +
// the static-recursion manifest (24 positive rows across depths 0/1/2/N and both
// terminal encodings + the pair-guard row); every other recursive/alias shape stays
// declined PRE-CLAIM.
//
// SENSITIVE: canonicalJSON is parsed provider output and T carries the model's full
// structured response; the caller treats both like the response body and never logs
// them.
func DecodeStaticFinal[T any](canonicalJSON []byte) (T, error) {
	// disallowUnknownFields=true: reject a field the concrete return type does not
	// declare — BAML's CFFI Decode panics on an unexpected class field, so a strict
	// rejection (rather than a silent drop) is the faithful analogue for the admitted
	// class shapes.
	return decodeStaticStrict[T](canonicalJSON, "bamlutils: decode static final", true)
}

// DecodeStaticAliasFinal is the de-BAML Phase 3a (recursive ALIASES) NARROW per-method
// materializer/decoder for the served structural-recursive-alias union return (the
// direct five-arm JSON alias), kept SEPARATE from [DecodeStaticFinal] so the generic
// decoder's proof set (scalars / flat classes / recursive-class pointer carriers) is
// NOT silently widened to aliases/unions/ordered-maps.
//
// Unlike the generic decoder it does NOT set DisallowUnknownFields: the generated
// alias carrier (BAML's types.JSON — a Union5.../Union6... value union) is NOT a struct
// with a fixed field set but a tagged union whose own UnmarshalJSON dispatches the
// arms, so a struct-field allowlist is meaningless for it (and DisallowUnknownFields is
// silently ignored for a type with a custom UnmarshalJSON anyway). It IS still a STRICT
// single-value decode (a trailing second value is malformed). Its BAML-equivalence is
// proven per admitted alias by the v0.223 recursive-alias differential: the native
// static SAP emits the SORTED-public canonical bytes (json.Marshal of the equivalent
// Go value — sorted map keys + HTML escaping), byte-identical to the generated static
// callback (Parse.<Method> then json.Marshal on types.JSON), and this decode maps those
// canonical bytes back to the concrete generated union via its generated UnmarshalJSON.
//
// SENSITIVE: canonicalJSON is parsed provider output and T carries the model's full
// structured response; the caller treats both like the response body and never logs
// them.
func DecodeStaticAliasFinal[T any](canonicalJSON []byte) (T, error) {
	// disallowUnknownFields=false: the generated alias carrier is a tagged UNION with
	// its own UnmarshalJSON (not a fixed-field struct), so a field allowlist is
	// meaningless for it (and DisallowUnknownFields is ignored for a custom-Unmarshal
	// type anyway) — see the doc comment above.
	return decodeStaticStrict[T](canonicalJSON, "bamlutils: decode static alias final", false)
}

// DecodeStaticAliasStream is the de-BAML Phase 3b (recursive-alias STREAMING) NARROW
// per-method decoder for the served alias's PARTIAL and final stream carrier. It is kept
// SEPARATE from both [DecodeStaticFinal] (the generic proof set) and [DecodeStaticAliasFinal]
// (the value-union FINAL carrier) so neither is widened by the stream lane.
//
// It is instantiated with the generated STREAM carrier stream_types.JSON — a POINTER union
// (*Union5…), distinct from the value union types.JSON the final decoder uses. The native
// static-stream SAP (debaml.ParseStaticStreamPartial / …Final) emits the SORTED-public
// canonical bytes (json.Marshal of the equivalent Go value — sorted map keys + HTML
// escaping), byte-identical to stock BAML v0.223's ParseStream.<Method> then json.Marshal;
// this decode maps those bytes back to the concrete generated pointer union via its
// generated UnmarshalJSON. Its byte/event-exactness is proven per admitted alias by the
// strict per-prefix + SSE-replay differentials.
//
// TYPED-NIL: the generated result wrapper distinguishes a typed-nil stream_types.JSON (a
// present-but-null partial) from an untyped nil (no event). For the served non-nullable
// JSON alias every emitted partial is a concrete value, so a typed-nil never arises; a
// `null` input would still decode to a typed-nil *Union5 here (never a decode error), which
// the caller may forward as a present partial. It is a STRICT single-value decode (a
// trailing second value is malformed).
//
// SENSITIVE: canonicalJSON is parsed provider output and T carries the model's full
// structured response; the caller treats both like the response body and never logs them.
func DecodeStaticAliasStream[T any](canonicalJSON []byte) (T, error) {
	// disallowUnknownFields=false: the generated STREAM carrier is a POINTER union with
	// its own UnmarshalJSON (not a fixed-field struct), same rationale as the alias
	// final decoder above.
	return decodeStaticStrict[T](canonicalJSON, "bamlutils: decode static alias stream", false)
}

// decodeStaticStrict is the shared strict-JSON-decode + trailing-EOF core that the three
// exported per-lane static decoders ([DecodeStaticFinal], [DecodeStaticAliasFinal],
// [DecodeStaticAliasStream]) delegate to; it is the SINGLE place the strict-decode contract
// lives, so the three lanes cannot drift.
//
// It performs ONE strict decode of a SINGLE JSON value into T and then requires EOF: a
// trailing second value / non-whitespace is malformed input, never a silently-accepted
// first value.
//
// errPrefix is prepended to every error so each exported decoder keeps its OWN narrow error
// strings byte-for-byte (`<prefix>: <cause>`, `<prefix>: unexpected trailing JSON value`,
// `<prefix>: trailing content: <cause>`). disallowUnknownFields gates
// DisallowUnknownFields: only [DecodeStaticFinal]'s class-shape proof set rejects an
// unexpected field; the alias/union carriers dispatch their own UnmarshalJSON and cannot
// meaningfully allowlist struct fields (and DisallowUnknownFields is ignored for a
// custom-Unmarshal type regardless), so they pass false.
//
// SENSITIVE: canonicalJSON is parsed provider output and T carries the model's full
// structured response; the caller treats both like the response body and never logs them.
func decodeStaticStrict[T any](canonicalJSON []byte, errPrefix string, disallowUnknownFields bool) (T, error) {
	var v T
	dec := stdjson.NewDecoder(bytes.NewReader(canonicalJSON))
	if disallowUnknownFields {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("%s: %w", errPrefix, err)
	}
	if err := dec.Decode(new(stdjson.RawMessage)); err != io.EOF {
		if err == nil {
			return v, fmt.Errorf("%s: unexpected trailing JSON value", errPrefix)
		}
		return v, fmt.Errorf("%s: trailing content: %w", errPrefix, err)
	}
	return v, nil
}
