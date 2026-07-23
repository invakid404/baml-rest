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
	var v T
	dec := stdjson.NewDecoder(bytes.NewReader(canonicalJSON))
	// Reject a field the concrete return type does not declare — BAML's CFFI Decode
	// panics on an unexpected class field, so a strict rejection (rather than a silent
	// drop) is the faithful analogue for the admitted class shapes.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("bamlutils: decode static final: %w", err)
	}
	// Require a SINGLE JSON value: a trailing second value / non-whitespace is
	// malformed input, never a silently-accepted first value.
	if err := dec.Decode(new(stdjson.RawMessage)); err != io.EOF {
		if err == nil {
			return v, fmt.Errorf("bamlutils: decode static final: unexpected trailing JSON value")
		}
		return v, fmt.Errorf("bamlutils: decode static final: trailing content: %w", err)
	}
	return v, nil
}
