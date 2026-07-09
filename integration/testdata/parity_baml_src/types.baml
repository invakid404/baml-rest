// Parity corpus types. Declaration order is deliberately chosen to DIFFER
// from output-format reachability order for the multi-definition cases, so the
// byte-parity oracle proves the native builder emits BAML's reverse-of-
// reference DFS order rather than declaration order.

// --- basic classes -------------------------------------------------------

class SimpleOutput {
    message string
}

class Address {
    street string
    city string
    zip string
}

class Person {
    name string
    age int
    email string?
    tags string[]
}

class PersonWithAddress {
    name string
    address Address
}

// --- enum (inline: <=6 values, no description => rendered inline) ---------

enum Category {
    TECH
    BUSINESS
    OTHER
}

class CategorizedItem {
    name string
    category Category
}

// --- union of classes ----------------------------------------------------

class SuccessResult {
    data string
}

class ErrorResult {
    error string
    code int
}

// --- literal-typed fields ------------------------------------------------

class LiteralBag {
    kind "circle" | "square"
    version 2
    enabled true
}

// --- container/union/optional shapes, held as FIELDS ---------------------
//
// BAML's Go generator emits invalid code for a function whose RETURN type is a
// bare map<...> (it returns `nil` for a value-type OrderedMap), so the shared
// corpus — and this one — only ever RETURN class/class[]/primitive types and
// exercise maps/unions/optionals/literal-unions as class FIELDS instead. The
// output-format rendering of the field type is identical either way, so this
// preserves full render coverage while staying codegen-safe.

// Map KEY shapes: string primitive, string-literal union, and (below) enum.
// BAML's Go generator emits a Decode that PANICS on a literal-union map key
// (`interface conversion: serde.OrderedMap[...] not map[Union...]int64`), and
// (empirically) on an enum map key too — a generated-DECODE codegen bug, not an
// output-format RENDER issue. The oracle container runs in SUBPROCESS mode so
// that panic is isolated to the worker process (which the pool restarts) and
// never crashes the server: the outbound provider request — carrying the
// rendered ctx.output_format we byte-compare — is captured BEFORE the response
// is decoded, so every key shape is still oracle-verified.
class MapHolder {
    string_map map<string, int>
    enum_map map<string, Category>
    literal_key_map map<"a" | "b", int>
}

// Enum-KEY map, with BOTH the key enum and the value enum hoisted (each via a
// value description). BAML's reachability pushes the map KEY then the VALUE, so
// LIFO pops the value first: the hoisted block order must be
// [MapValueEnum, MapKeyEnum] — the value definition before the key definition.
// This pins BAML's key-then-value map traversal on reachable key AND value
// definitions.
enum MapKeyEnum {
    KEY_A @description("the A key")
    KEY_B
}

enum MapValueEnum {
    VAL_X @description("the X value")
    VAL_Y
}

class EnumKeyMapHolder {
    keyed map<MapKeyEnum, MapValueEnum>
}

class UnionHolder {
    class_union SuccessResult | ErrorResult
    prim_union int | string
    nullable_union int | string | null
    literal_union "active" | "inactive"
    maybe_person Person?
}

// --- reachability ordering: two HOISTED enums (each hoisted via a value
// description) plus a class field. ColorEnum is DECLARED before SizeEnum, but
// Styled references color(ColorEnum) then size(SizeEnum), so BAML's LIFO
// reachability pops the last field first and emits [SizeEnum, ColorEnum] —
// the OPPOSITE of declaration order. A declaration-order build would diverge.

enum ColorEnum {
    RED @description("the color red")
    GREEN
    BLUE
}

enum SizeEnum {
    SMALL @description("a small size")
    LARGE
}

class Styled {
    color ColorEnum
    size SizeEnum
    label string
}

// --- aliases + descriptions (field aliases/descriptions, enum-value alias +
// description). StatusEnum hoists because ACTIVE carries a description; its
// hoisted header and value line must render the alias + description.

enum StatusEnum {
    PENDING
    ACTIVE @alias("active_status") @description("currently active")
    ARCHIVED
}

class Profile {
    full_name string @alias("name") @description("the person's full name")
    status StatusEnum
    note string?
}
