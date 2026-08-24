package worker

import (
	"slices"

	"github.com/invakid404/baml-rest/bamlutils"
)

// De-BAML native-first direct parse — FIELD ORDER at the worker boundary.
//
// # The fact this file encodes
//
// BAML emits a parsed class as one JSON entry per declared field, in the order the
// fields were added to the TypeBuilder. For a DYNAMIC request that order is not the
// caller's wire order: the codegen-emitted `applyDynamicTypes` populates the
// TypeBuilder through `schemaKeys(m, preserveOrder)`, which iterates the schema's
// property map in INSERTION order when `DynamicTypes.PreserveOrder` is true and in
// ALPHABETICAL key order when it is false. So a caller who does not opt into
// preserve_schema_order gets BAML fields back sorted by name, whatever order they
// declared them in.
//
// The native parser has no view of that. It lowers the carried
// bamlutils.DynamicOutputSchema verbatim and emits each class's fields in the
// lowered declaration order, which is the WIRE order. For a preserve_order=false
// request with a non-alphabetically-declared class, native's payload therefore
// carried BAML's exact content under a different key order — and the transition
// oracle, which compares BYTES, declined the agreement (`result_drift`) even though
// the two answers were the same answer.
//
// # Why the fix lives here and not in the parser
//
// "Which order does the TypeBuilder get the fields in" is a property of the
// REQUEST, not of the schema: the same schema yields two different BAML field
// orders depending on `PreserveOrder`. The worker is the one place that holds both
// halves — the schema and the flag BAML's own `applyDynamicTypes` reads — so it is
// the seam that can state the order without guessing. The native parser keeps the
// simple, self-contained rule ("emit fields in the order the schema declares
// them"), and this pass hands it a schema declared in the order BAML will actually
// use.
//
// The result is that worker.Parse needs nothing downstream to be true: given the
// same worker input BAML gets, the native leg's bytes are BAML's bytes. Callers of
// this published API that never run dynclient's absent-optional / reorder passes
// see the same agreement the /parse route does.
//
// The pass is IDEMPOTENT, which matters because the CALL path's worker input
// already carries a field-sorted schema (bamlutils.DynamicInput.ToWorkerInput sends
// the render-normalized clone): sorting a sorted schema returns the same order.

// nativeParseSchemaForOrder returns the output schema to coerce against, with every
// class's field order matching the order BAML's TypeBuilder will be populated in for
// this request.
//
// preserveOrder mirrors bamlutils.DynamicTypes.PreserveOrder: true means the caller
// opted into their declared order and the schema is used as-is; false means
// applyDynamicTypes will sort, so the returned clone is sorted the same way.
// The caller's schema is never mutated — a preserve_order=false request gets a fresh
// clone and the original stays in the decoded input for anything else that reads it.
func nativeParseSchemaForOrder(s *bamlutils.DynamicOutputSchema, preserveOrder bool) *bamlutils.DynamicOutputSchema {
	if s == nil || preserveOrder {
		return s
	}
	out := &bamlutils.DynamicOutputSchema{
		Properties: sortSchemaPropertiesByName(s.Properties),
		// Enums are carried by reference: applyDynamicTypes adds enum VALUES in
		// declaration order regardless of preserveOrder, and enum values are not
		// object keys, so no ordering decision applies to them.
		Enums: s.Enums,
	}
	if s.Classes.Len() == 0 {
		return out
	}
	classes := bamlutils.OrderedMap[*bamlutils.DynamicClass]{}
	for _, entry := range s.Classes.Entries() {
		class := entry.Value
		if class == nil {
			// A nil class body is a malformed schema the lowering step rejects with
			// the fallback sentinel. Carry it through unchanged so THAT decline is
			// what the request records, rather than a panic here.
			_ = classes.Set(entry.Key, nil)
			continue
		}
		_ = classes.Set(entry.Key, &bamlutils.DynamicClass{
			Description: class.Description,
			Alias:       class.Alias,
			Properties:  sortSchemaPropertiesByName(class.Properties),
		})
	}
	out.Classes = classes
	// The Classes map's own order is left alone: it decides only which order the
	// TypeBuilder learns the class NAMES in, never the order a parsed object's keys
	// come back in.
	return out
}

// sortSchemaPropertiesByName returns a copy of props whose entries are in ascending
// key order — the order `schemaKeys` hands applyDynamicTypes when preserve_order is
// off. Property keys are unique in a decoded schema (bamlutils rejects duplicates),
// so the rebuild never collides.
func sortSchemaPropertiesByName(props bamlutils.OrderedMap[*bamlutils.DynamicProperty]) bamlutils.OrderedMap[*bamlutils.DynamicProperty] {
	keys := props.Keys() // Keys returns a copy; sorting it does not touch props.
	if len(keys) == 0 {
		return bamlutils.OrderedMap[*bamlutils.DynamicProperty]{}
	}
	slices.Sort(keys)
	out := bamlutils.OrderedMap[*bamlutils.DynamicProperty]{}
	for _, k := range keys {
		v, _ := props.Get(k)
		_ = out.Set(k, v)
	}
	return out
}

// parsePreservesSchemaOrder reports whether this request opted into its declared
// schema order, read from the SAME field the generated applyDynamicTypes consults.
// Anything missing (no options, no type builder, no dynamic types) is the zero
// value, which is also applyDynamicTypes' behavior for an absent flag: sort.
func parsePreservesSchemaOrder(opts *bamlutils.BamlOptions) bool {
	if opts == nil || opts.TypeBuilder == nil || opts.TypeBuilder.DynamicTypes == nil {
		return false
	}
	return opts.TypeBuilder.DynamicTypes.PreserveOrder
}
