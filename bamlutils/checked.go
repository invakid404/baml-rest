package bamlutils

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/bytedance/sonic"
)

// De-BAML Slice 7.2b-1 — the public `Checked[T]` carrier.
//
// This is the stock-BAML-v0.223.0-compatible constraint carrier the native serving
// path will emit for a `@check`-bearing node. It is added AHEAD of any admission
// change: nothing in this repo claims a constraint-bearing schema yet (every
// constraint bundle still declines to BAML through internal/debaml's gate), and no
// out-of-`go.work` module consumes these types. What this file establishes is the
// SHAPE and the BYTES, so the stock byte oracle
// (internal/debaml/checkedwire) has something to compare against.
//
// # The stock contract being mirrored
//
// github.com/boundaryml/baml@v0.223.0
// engine/language_client_go/baml_go/shared/checked.go declares exactly:
//
//	type Check struct {
//	    Name       string `json:"name"`
//	    Expression string `json:"expression"`
//	    Status     string `json:"status"`
//	}
//
//	type Checked[T any] struct {
//	    Value  T                `json:"value"`
//	    Checks map[string]Check `json:"checks"`
//	}
//
// The CFFI encoder emits an ORDERED list of checks (constraint declaration /
// evaluation order); `serde.decodeCheckedValue` folds that list into the
// `map[string]Check` above, so the stock Go value has neither multiplicity nor
// declared ordering — duplicate labels are LAST-WRITE-WINS and the map's iteration
// order is Go's. Both facts are measured against the real CFFI by
// internal/debaml/checkedwire; they are the reason this carrier keeps the map (so
// the public shape stays stock's) while carrying the declaration order out of band
// (so the BYTES are deterministic).
//
// # Why the custom marshal exists
//
// The worker serializes a final parse result with [sonic.Marshal], and sonic's
// default config does not sort map keys. Marshalling stock's plain struct therefore
// emits `checks` in Go map iteration order, which differs run to run. That is
// acceptable for stock (nothing promises an order) but is not acceptable for a
// native claim: two runs of the same request would produce different bytes. So
// [Checked.MarshalJSON] writes `value` then `checks`, and writes the check keys in
// the order the constructor recorded. This is the deliberately-endorsed
// deterministic refinement of a value stock can already represent — NOT a new public
// shape: `checks` stays a JSON object, [Checked.Checks] stays a map, and the ordered
// slice is not exported.
type Checked[T any] struct {
	// Value is the checked value. It is emitted FIRST, matching stock's field order.
	Value T `json:"value"`
	// Checks is stock's public shape: label -> check result. The map key is always
	// the contained [Check.Name]; a carrier where the two disagree is malformed and
	// fails to serialize rather than emitting a contradictory entry.
	Checks map[string]Check `json:"checks"`

	// declaredCheckOrder is the constraint declaration order of the keys of Checks.
	// It is unexported ON PURPOSE (see the type comment): the public shape must stay
	// stock's. A nil slice means "no trusted order" — the state a hand-built or
	// JSON-decoded carrier is in — and selects the documented lexicographic fallback
	// in [Checked.MarshalJSON]. It is never Go map iteration order.
	declaredCheckOrder []string
}

// Check is one constraint result, with stock v0.223.0's JSON field names. The field
// ORDER here is material: [Checked.MarshalJSON] writes `name`, `expression`, then
// `status`, which is the order stock's struct definition produces.
type Check struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Status     string `json:"status"`
}

// The two [Check.Status] values stock v0.223.0 emits: a predicate that evaluated
// true is "succeeded", one that evaluated false is "failed" — and a false @check
// still emits its value (unlike a false @assert, which emits no value at all).
//
// These are the stock vocabulary, not a validated enum: [NewChecked] deliberately
// does NOT reject an unrecognised status, because a status this repo has not seen is
// evidence about stock rather than a malformed carrier, and silently rejecting it
// would hide it.
const (
	CheckSucceeded = "succeeded"
	CheckFailed    = "failed"
)

// ErrCheckedMalformed is returned by [NewChecked] and by [Checked.MarshalJSON] for a
// carrier that cannot be serialized without either contradicting itself or picking an
// arbitrary order.
//
// It is deliberately a FAILURE rather than a best-effort emission: the alternatives —
// dropping a duplicate label, or emitting a map entry whose key disagrees with the
// `name` inside it — both produce bytes that claim something the carrier does not
// actually hold.
var ErrCheckedMalformed = errors.New("bamlutils: malformed Checked carrier")

// NewChecked builds a carrier from ORDERED check results — the constructor the
// native production mapping (Slice 7.2b-2) will use, where `ordered` is the
// constraint declaration/evaluation order the coercion produced.
//
// It rejects, rather than normalising:
//
//   - an empty check label. Stock cannot produce one: BAML's grammar requires an
//     identifier for `@check(<label>, …)`. An empty label would still serialize
//     deterministically, so this is a constructor-only rule (see [Checked.MarshalJSON],
//     whose failure set is exactly the non-deterministic and self-contradictory ones).
//   - a DUPLICATE label. Stock's map fold makes duplicates last-write-wins, so a
//     native claim over a duplicate-label node cannot be proven byte-equal to stock;
//     the node must decline before it is ever mapped. Refusing here is the last line
//     of that rule, not the first.
//
// The returned carrier's map keys are the check names by construction, and the
// declaration order is retained for [Checked.MarshalJSON]. `ordered` is not aliased:
// the caller may reuse or mutate its slice afterwards.
func NewChecked[T any](value T, ordered []Check) (Checked[T], error) {
	checks := make(map[string]Check, len(ordered))
	order := make([]string, 0, len(ordered))
	for i, c := range ordered {
		if c.Name == "" {
			return Checked[T]{}, fmt.Errorf("%w: check %d has an empty label", ErrCheckedMalformed, i)
		}
		if _, dup := checks[c.Name]; dup {
			return Checked[T]{}, fmt.Errorf("%w: duplicate check label %q at index %d; stock v0.223.0 folds "+
				"duplicate labels into one map entry (last write wins), so a duplicate cannot be reproduced "+
				"byte-for-byte and the node must decline before it is mapped", ErrCheckedMalformed, c.Name, i)
		}
		checks[c.Name] = c
		order = append(order, c.Name)
	}
	return Checked[T]{Value: value, Checks: checks, declaredCheckOrder: order}, nil
}

// MarshalJSON writes `value` then `checks`, with the check keys in the recorded
// declaration order and each check object's fields in stock's `name`, `expression`,
// `status` order.
//
// # Determinism
//
// The key order is NEVER Go map iteration order. It is either the order [NewChecked]
// recorded, or — for a carrier built by hand or decoded from JSON, which has no
// trusted order — the ONE documented fallback: lexicographic by label. Both are
// stable across runs.
//
// # Failure rather than a contradictory emission
//
// A carrier fails to serialize when it cannot be written without claiming something
// it does not hold:
//
//   - a map entry whose key differs from the [Check.Name] inside it (which of the two
//     is the label?); or
//   - a recorded order that does not describe the map exactly — a repeated label, a
//     label with no entry, or a length mismatch, all of which arise from mutating the
//     exported map after construction and would silently drop or duplicate an entry.
//
// # Encoder note
//
// The nested value is encoded with [sonic.Marshal], the worker's serializer, so the
// bytes this produces under sonic are the bytes stock's plain struct produces under
// sonic. encoding/json honours this method too, but then applies its own HTML escaping
// to the result, rewriting `<`, `>` and `&` to their six-byte \u00XX escapes. That is
// encoding/json's framing of ANY json.Marshaler rather than a property of this carrier,
// and it means a check EXPRESSION containing `<` differs between the two encoders.
// sonic is the wire authority; internal/debaml/checkedwire pins both forms.
func (c Checked[T]) MarshalJSON() ([]byte, error) {
	order, err := c.marshalOrder()
	if err != nil {
		return nil, err
	}
	value, err := sonic.Marshal(c.Value)
	if err != nil {
		return nil, fmt.Errorf("bamlutils: marshal Checked value: %w", err)
	}

	var b bytes.Buffer
	b.WriteString(`{"value":`)
	b.Write(value)
	b.WriteString(`,"checks":`)
	// A nil map is `null`, exactly as Go's own map encoding renders it, so a carrier
	// with NO check map does not present as one carrying an empty map. [NewChecked]
	// always allocates, so a constructed carrier with zero checks emits `{}`.
	if c.Checks == nil {
		b.WriteString("null}")
		return b.Bytes(), nil
	}
	b.WriteByte('{')
	for i, name := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		check := c.Checks[name]
		if err := writeJSONString(&b, name); err != nil {
			return nil, err
		}
		b.WriteString(`:{"name":`)
		if err := writeJSONString(&b, check.Name); err != nil {
			return nil, err
		}
		b.WriteString(`,"expression":`)
		if err := writeJSONString(&b, check.Expression); err != nil {
			return nil, err
		}
		b.WriteString(`,"status":`)
		if err := writeJSONString(&b, check.Status); err != nil {
			return nil, err
		}
		b.WriteByte('}')
	}
	b.WriteString("}}")
	return b.Bytes(), nil
}

// UnmarshalJSON REPLACES the carrier with the JSON wire state.
//
// # Why the carrier needs its own unmarshaller
//
// Without one, both decoders write straight into the receiver's exported fields, and a
// carrier that is DECODED INTO TWICE — or decoded into after [NewChecked] built it —
// keeps state the wire never mentioned. encoding/json and sonic both reuse a non-nil
// `Checks` map rather than allocating, so a key absent from the new document survives;
// and neither can touch [Checked.declaredCheckOrder], so a constructor's order outlives
// the value it described. The result is a carrier whose recorded order no longer
// matches its map — which [Checked.MarshalJSON] then correctly refuses to serialize.
// Replacement, not merge, is the contract: after a successful decode the carrier holds
// exactly what the document said, including the zero value when `value` was absent.
//
// # No trusted order survives a decode
//
// The wire shape carries a `checks` OBJECT, not the ordered CFFI list [NewChecked]
// records its order from, so a decoded carrier cannot recover one. The private order is
// therefore CLEARED on success and a later [Checked.MarshalJSON] takes the documented
// lexicographic fallback. This is what makes the round trip deterministic rather than
// dependent on how the carrier was previously built.
//
// # Atomic, and strict
//
// The document is decoded into FRESH fields and the receiver is written only after that
// succeeds, so a failure — including encoding/json's LATE type errors, which are
// reported after the rest of the object has already been consumed — leaves the carrier
// exactly as it was rather than half-replaced.
//
// The inner decode is STRICT (unknown field rejected, exactly one JSON value required),
// mirroring [decodeStaticStrict]. That is load-bearing rather than defensive: a
// json.Unmarshaler takes over its whole subtree, so a lenient implementation here would
// silently disable the `DisallowUnknownFields` that [DecodeStaticFinal] applies — a
// field stock cannot produce would be dropped instead of rejected, in the one decoder
// the generated static path uses.
func (c *Checked[T]) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errors.New("bamlutils: unmarshal Checked: nil receiver")
	}
	// FRESH fields: never the receiver's, so neither a stale map entry nor a partially
	// decoded document can reach it.
	var decoded struct {
		Value  T                `json:"value"`
		Checks map[string]Check `json:"checks"`
	}
	dec := stdjson.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Errorf("bamlutils: unmarshal Checked: %w", err)
	}
	if err := dec.Decode(new(stdjson.RawMessage)); err != io.EOF {
		if err == nil {
			return errors.New("bamlutils: unmarshal Checked: unexpected trailing JSON value")
		}
		return fmt.Errorf("bamlutils: unmarshal Checked: trailing content: %w", err)
	}

	c.Value = decoded.Value
	c.Checks = decoded.Checks
	c.declaredCheckOrder = nil
	return nil
}

// marshalOrder resolves the key order to emit and validates the carrier, returning
// [ErrCheckedMalformed] for the states described on [Checked.MarshalJSON].
func (c Checked[T]) marshalOrder() ([]string, error) {
	order := c.declaredCheckOrder
	if order == nil {
		// No trusted order: the documented deterministic fallback. Sorting the keys is
		// the whole point — ranging over the map would make the bytes depend on Go's
		// randomised iteration order.
		keys := make([]string, 0, len(c.Checks))
		for k := range c.Checks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		order = keys
	} else if len(order) != len(c.Checks) {
		return nil, fmt.Errorf("%w: the recorded declaration order has %d label(s) but Checks has %d entry/entries; "+
			"the map was mutated after construction", ErrCheckedMalformed, len(order), len(c.Checks))
	}
	// Equal lengths plus no repeat plus every label present makes `order` exactly the
	// key set, so no entry can be dropped or emitted twice.
	seen := make(map[string]struct{}, len(order))
	for _, name := range order {
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: the recorded declaration order repeats the label %q", ErrCheckedMalformed, name)
		}
		seen[name] = struct{}{}
		check, ok := c.Checks[name]
		if !ok {
			return nil, fmt.Errorf("%w: the recorded declaration order names %q, which Checks has no entry for; "+
				"the map was mutated after construction", ErrCheckedMalformed, name)
		}
		if check.Name != name {
			return nil, fmt.Errorf("%w: Checks[%q] carries the name %q; the map key and the check name must agree, "+
				"and emitting either one would claim a label the carrier does not hold",
				ErrCheckedMalformed, name, check.Name)
		}
	}
	return order, nil
}

// writeJSONString appends s as a JSON string using the SAME encoder the value goes
// through, so a label or expression is escaped exactly as sonic would escape it in
// stock's plain-struct rendering.
func writeJSONString(b *bytes.Buffer, s string) error {
	encoded, err := sonic.Marshal(s)
	if err != nil {
		return fmt.Errorf("bamlutils: marshal Checked string %q: %w", s, err)
	}
	b.Write(encoded)
	return nil
}
