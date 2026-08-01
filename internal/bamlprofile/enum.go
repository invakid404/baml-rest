package bamlprofile

import (
	"fmt"
	"strings"

	"github.com/invakid404/minijinja-go/v2/value"
)

// This file is the BAML v0.223 enum host value model: the per-enum namespace
// global BAML injects at render time, and the enum-member object it lowers a
// BamlValue::Enum to. Together with the ObjectWithValueCmp comparator here, they
// reproduce BAML's enum equality/ordering/membership — the behavior the #597
// enum-`==` fence documented as diverging on the old external engine.
//
// Authority (byte-for-byte): BAML v0.223
//   - enum member lowering + object impl + value_cmp:
//     engine/baml-lib/jinja-runtime/src/baml_value_to_jinja_value.rs:43-58,173-256
//   - enum namespace type + per-enum global injection:
//     engine/baml-lib/jinja-runtime/src/baml_value_to_jinja_value.rs:148-171
//     and engine/baml-lib/jinja-runtime/src/lib.rs:316-327,642-658
//
// The enum member deliberately keeps THREE separate fields — canonical variant
// name, optional resolved alias, and enum name — because BAML does:
//   - display / serialization is alias-or-canonical (rs:183-196);
//   - `.value` is the canonical name ONLY (rs:205-225: no `.name`/`.alias`);
//   - comparison identity is canonical then Option<alias>, and enum_name is
//     NOT part of it (rs:235-253) — hence cross-enum members with equal
//     canonical+alias compare EQUAL.
// Collapsing those into one string would lose one of these behaviors.

// EnumValue is one resolved enum variant: its canonical variant name (the
// comparison identity, e.g. "RED") and an optional resolved alias (its display
// value, e.g. "rouge"). Alias is nil when the variant has no @alias. Aliases are
// RESOLVED inputs; this leaf does not parse BAML source or evaluate alias
// expressions (that IR/EvaluationContext work is a later descriptor-adapter
// slice, per the scope).
type EnumValue struct {
	Canonical string
	Alias     *string
}

// EnumDef is one resolved enum declaration: its name and its variants in
// declared order. It is the profile analogue of an IR enum walked by
// render_prompt (lib.rs:642-658); New installs one namespace global per EnumDef.
type EnumDef struct {
	Name   string
	Values []EnumValue
}

// enumMember is BAML's MinijinjaBamlEnumValue: a lowered enum value. The same Go
// type backs BOTH an enum member reached through a namespace global (Color.RED)
// and one passed as a function argument, exactly as BAML uses one Rust type for
// both — which is what makes cross-enum members downcast to the same type and so
// compare by canonical+alias while ignoring enum_name.
type enumMember struct {
	canonical string  // variant name; the comparison identity and `.value`
	alias     *string // resolved alias, or nil; the display/serialization value when present
	enumName  string  // retained (BAML stores it) but NEVER exposed and NEVER compared
}

// newEnumMember builds a validated enum member. canonical and enumName must be
// non-empty; a malformed descriptor fails loud here rather than rendering a
// degraded value later.
func newEnumMember(enumName, canonical string, alias *string) (*enumMember, error) {
	if enumName == "" {
		return nil, fmt.Errorf("bamlprofile: enum member has empty enum name (canonical %q)", canonical)
	}
	if canonical == "" {
		return nil, fmt.Errorf("bamlprofile: enum %q has an enum member with empty canonical name", enumName)
	}
	// Own the alias, like ListValue owns its items and ClassValue its field keys:
	// the caller's *string must not stay live inside a constructed member, or
	// writing through it afterwards would silently change the member's display AND
	// its Option<alias> comparison identity. The POINTER-ness is load-bearing and
	// kept — nil (no @alias) and non-nil "" (@alias("")) are different values — so
	// this copies the pointee rather than collapsing to a string.
	var owned *string
	if alias != nil {
		a := *alias
		owned = &a
	}
	return &enumMember{canonical: canonical, alias: owned, enumName: enumName}, nil
}

// EnumMember constructs a host enum-member value (BAML's BamlValue::Enum
// lowering) for use as a render argument. alias is the resolved alias or nil. It
// fails loud on malformed metadata rather than emitting a degraded value.
func EnumMember(enumName, canonical string, alias *string) (value.Value, error) {
	m, err := newEnumMember(enumName, canonical, alias)
	if err != nil {
		return value.Undefined(), err
	}
	return value.FromObject(m), nil
}

// display is the enum member's rendered/serialized string: the alias when
// present, otherwise the canonical variant name (rs:183-196).
func (e *enumMember) display() string {
	if e.alias != nil {
		return *e.alias
	}
	return e.canonical
}

// GetAttr exposes ONLY `.value`, returning the canonical variant name — exactly
// the single key BAML's get_value answers (rs:205-225, with its explicit "do not
// add more fields here"). `.name`, `.alias`, and the stored enum name are absent,
// so `Color.RED.name is undefined` is true.
func (e *enumMember) GetAttr(name string) value.Value {
	if name == "value" {
		return value.FromString(e.canonical)
	}
	return value.Undefined()
}

// ObjectRepr is Map, matching BAML's MinijinjaBamlEnumValue::repr (rs:207-209).
//
// LOAD-BEARING NEGATIVE CAPABILITY: the member is a Map-repr object that
// deliberately implements NEITHER value.MapObject NOR value.MapGetter. That
// omission is not an oversight — it is the fork-level spelling of BAML's
// `Enumerator::NonEnumerable` with an unknown length (rs:211-217). A member is
// NOT an empty map: an ordinary `{}` has a known, empty pair iterator, whereas
// BAML's `try_iter_pairs()` for this object is absent entirely.
//
// The fork encodes exactly that distinction in the boolean of
// Value.MapKeys() ([]string, bool): (nil, false) here versus ([], true) for a
// known-empty map. Equality, ordering, membership, iteration, `is iterable` and
// the debug/pprint walkers all branch on it (fork v2.16.0-baml.4, PATCHES #102,
// #103, #105). So `Color.RED == Color` and `Color.RED == {}` are false while
// BAML's asymmetric `{} == Color.RED` stays true, and ordering two of them
// faults instead of inventing a result.
//
// Adding a Keys()/Map() method here — even one returning nothing — would turn
// the member into a known-empty map and silently reintroduce every one of those
// divergences. TestEnumHostObjectsAreNonEnumerableMaps guards it.
func (e *enumMember) ObjectRepr() value.ObjectRepr { return value.ObjectReprMap }

// ObjectString is the member's render: alias-or-canonical, bare (rs:181-186,
// 227-229). The fork's Value.String and Value.Repr both dispatch
// value.ObjectWithString (PATCHES #104), which is the fork analogue of BAML's
// object Display/Debug being the same call — so this text composes through
// native containers, |join, |upper, `~`, string coercion and the output
// formatter with no per-consumer help from this package.
func (e *enumMember) ObjectString() string { return e.display() }

// ValueCmp is BAML's MinijinjaBamlEnumValue::value_cmp + custom_cmp
// (rs:227-253), the comparator that closes the #597 fence. It is reached from
// BOTH operand positions by the fork's Value.Equal/Value.Compare, so reverse
// equality, `!=`, sequence equality and membership all inherit it.
//
//   - Against a string: compare the CANONICAL name (not the alias),
//     case-sensitively — preserving pre-0.206 enum-as-string behavior. So
//     `Color.RED == 'RED'` is true and `Color.RED == 'rouge'` is false.
//   - Against another enum member (same Go type, any enum): compare canonical,
//     then Option<alias> with nil<non-nil then lexical. enum_name is NOT
//     compared, so equal-canonical+equal-alias members of different enums are
//     equal.
//   - Against anything else (int, float, bool, none, an unrelated object):
//     DECLINE with (0, false). Declining is not "not equal"; it lets the fork
//     apply its ordinary rules. The profile never invents equality or order for
//     a type BAML's comparator refuses.
func (e *enumMember) ValueCmp(other value.Value) (int, bool) {
	if s, ok := other.AsString(); ok {
		return strings.Compare(e.canonical, s), true
	}
	if obj, ok := other.AsObject(); ok {
		if o, ok := obj.(*enumMember); ok {
			if c := strings.Compare(e.canonical, o.canonical); c != 0 {
				return c, true
			}
			return compareOptionalAlias(e.alias, o.alias), true
		}
		// A different object type: BAML's custom_cmp downcast_ref fails and
		// returns None. Decline.
		return 0, false
	}
	return 0, false
}

// compareOptionalAlias is Rust's Option<String>::cmp: None < Some, and two Somes
// compare lexically. It is the alias tie-breaker of the enum comparator
// (rs:249-252, `self.alias.cmp(&other.alias)`).
func compareOptionalAlias(a, b *string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return strings.Compare(*a, *b)
	}
}

// enumType is BAML's MinijinjaBamlEnumType: the per-enum namespace global. It
// maps each CANONICAL variant name to its member (Color.RED -> member), and is
// non-enumerable — you reach members by name, you do not iterate the namespace
// (rs:148-171). New installs one per EnumDef via Environment.AddGlobal.
type enumType struct {
	name    string
	members map[string]*enumMember
}

// newEnumType builds a validated namespace from a resolved EnumDef. It fails
// loud on an empty enum name or a duplicate canonical variant rather than
// silently dropping or overwriting a variant.
func newEnumType(def EnumDef) (*enumType, error) {
	if def.Name == "" {
		return nil, fmt.Errorf("bamlprofile: enum definition has an empty name")
	}
	members := make(map[string]*enumMember, len(def.Values))
	for _, v := range def.Values {
		m, err := newEnumMember(def.Name, v.Canonical, v.Alias)
		if err != nil {
			return nil, err
		}
		if _, dup := members[v.Canonical]; dup {
			return nil, fmt.Errorf("bamlprofile: enum %q has duplicate variant %q", def.Name, v.Canonical)
		}
		members[v.Canonical] = m
	}
	return &enumType{name: def.Name, members: members}, nil
}

// GetAttr looks a variant up by its canonical name and returns its member
// (rs:158-164). An unknown name is undefined.
func (t *enumType) GetAttr(name string) value.Value {
	if m, ok := t.members[name]; ok {
		return value.FromObject(m)
	}
	return value.Undefined()
}

// ObjectRepr is Map (rs:154-156). Like enumMember, the namespace deliberately
// implements NEITHER value.MapObject NOR value.MapGetter: that is BAML's
// `Enumerator::NonEnumerable` with an unknown length (rs:165-171), and it makes
// the fork's Value.MapKeys report ok == false rather than a known-empty map. See
// enumMember.ObjectRepr for why that negative capability is load-bearing; it is
// what keeps `Color == Color.RED`, `{% for x in Color %}`, and `Color.RED < Color`
// behaving as stock BAML does. TestEnumHostObjectsAreNonEnumerableMaps guards it.
func (t *enumType) ObjectRepr() value.ObjectRepr { return value.ObjectReprMap }

// ObjectString renders the namespace as `{}`.
//
// MinijinjaBamlEnumType declares NO Display/render of its own (rs:148-171,
// unlike the member at rs:181-186), so stock MiniJinja falls back to the default
// `Object::render` for an ObjectRepr::Map: a debug_map built from the object's
// pairs. Its enumerator is NonEnumerable, so there are no pairs and the result is
// the empty map. Verified on stock BAML v0.223 CFFI in every position —
// `{{ Color }}`, `{{ [Color] }}`, `{{ Color|string }}`, `{{ Color ~ "!" }}`,
// `{{ Color|upper }}`, and as a native map value — all `{}`.
//
// This must be spelled out HERE, not inherited. The fork's Value.String/Repr
// dispatch value.ObjectWithString and otherwise fall through to Go's `%v`; they
// deliberately do NOT synthesize an engine-style default render for an object
// implementing neither ObjectWithString nor fmt.Stringer (v2.16.0-baml.4,
// "Known, not fixed"). Without this method the namespace rendered as
// `&{Color map[RED:0x14000a2c1e0 ...]}` — Go struct formatting, carrying heap
// ADDRESSES, so the output also differed between runs.
func (t *enumType) ObjectString() string { return "{}" }
