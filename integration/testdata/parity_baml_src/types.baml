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

// === slice-4: attributes + constraints ===================================
//
// The output_format is the FINAL (non-streaming) schema. Constraints
// (@check/@assert) are metadata and stream flags (@stream.*) affect streaming
// mode, not the final block — so NONE of the constructs below may change the
// rendered ctx.output_format. Each doc-comment / constraint / stream fixture
// must render byte-identically to the same shape without them, which is exactly
// what the native builder produces (it captures the metadata into the descriptor
// but the renderer ignores it). This is the load-bearing proof for slice 4.

// --- /// doc comments are CAPTURED AS A NO-OP -----------------------------
//
// BAML lowers `///` to a docstring its output_format renderer NEVER reads (only
// @description renders — confirmed by THIS oracle). A doc-commented class /
// field / enum / enum-value must therefore render with NO `//` description
// lines. If BAML rendered doc comments, DocStatus would gain descriptions and
// HOIST (changing the whole block), and DocCommented would grow `//` lines —
// either would break byte-parity, so this fixture is the empirical proof.

/// A documented enum (its enum-level doc comment must NOT render).
enum DocStatus {
    /// the active value (this doc comment must NOT render)
    ACTIVE
    INACTIVE
}

/// A documented output class.
/// Second doc line — still must NOT render.
class DocCommented {
    /// the identifier field (must NOT render)
    id string
    /// how many items (must NOT render)
    count int
    status DocStatus
}

// --- doc comment vs @description precedence -------------------------------
//
// With BOTH a `///` and an @description on one node, @description is the SOLE
// rendered source; the doc comment is invisible. Only "the real title" may
// appear as a `//` line for `title`.

class DocVsDesc {
    /// doc comment on title (must NOT render)
    title string @description("the real title")
    body string
}

// --- @assert / @check constraints (repeated, field + block level) ---------
//
// Opaque constraints, never rendered. Byte-identical to a constraint-free class.

class Constrained {
    score int @check(positive, {{ this > 0 }}) @assert(bounded, {{ this < 100 }})
    label string @assert({{ this|length > 0 }})
    note string?
    @@assert(consistent, {{ this.score < 100 }})
}

// --- @stream.* flags (field-level reassociated to type + class level) -----
//
// Streaming flags affect STREAMING mode, not the FINAL non-streaming
// output_format this oracle renders — so byte-identical to a stream-free class.

class Streamed {
    ready bool @stream.done
    payload string @stream.not_null @stream.with_state
    tail int
    @@stream.done
}

// --- mixed @alias + @description + @check + @stream on one field ----------
//
// Only @alias (renders as the field NAME) and @description (renders as a `//`
// line) affect output_format; the @check and @stream.done do not.

class Mixed {
    full_name string @alias("name") @description("the full name") @check(nonempty, {{ this|length > 0 }}) @stream.done
    status Category @description("the status category")
    @@assert(ok, {{ this.full_name|length > 0 }})
}

// === slice-5: recursion ==================================================
//
// Recursive classes + structural recursive aliases. The native builder detects
// the cycles (an SCC over class->field->class edges for classes; alias cycles
// that recurse THROUGH a list/map for structural aliases), emits recursive
// classes normally but lists them in Bundle.RecursiveClasses (so the renderer
// HOISTS them the way BAML does), and emits structural recursive aliases as
// hoisted `Name = <target>` definitions referenced by a recursive-alias node.
// Each must render byte-identically to BAML v0.223 (that is what this oracle
// proves). A direct/degenerate alias cycle (NOT through a list/map) is invalid
// and DECLINES — covered by the internal/nativeschema unit tests, not here (the
// oracle corpus is all-supported).

// --- recursive class via a list<Self> field ------------------------------
//
// RecursiveTreeNode.children is RecursiveTreeList? = RecursiveTreeNode[]?, so
// the class references itself through a list => a recursive class, always
// hoisted as a top-level definition. RecursiveTreeList is a NON-recursive alias
// (it points at a class, not another alias), so it inlines to RecursiveTreeNode[]
// and is not emitted as a structural alias.
class RecursiveTreeNode {
    value string
    children RecursiveTreeList?
}
type RecursiveTreeList = RecursiveTreeNode[]

// --- mutual class recursion (A -> B -> A) --------------------------------
//
// MutualA and MutualB share one SCC. BAML's recursive_classes carries a cycle
// in min-declaration-id-first (Tarjan) order, so both hoist as [MutualA, MutualB]
// (MutualA is declared first). A declaration-order-only build would still get
// this two-class case right, but the SCC detection is what makes it recursive.
class MutualA {
    label string
    b MutualB?
}
class MutualB {
    a MutualA
}

// --- structural recursive alias (JSON value) -----------------------------
//
// JsonValue recurses through BOTH a list (JsonValue[]) and a map
// (map<string, JsonValue>), so it is a STRUCTURAL recursive alias (valid), NOT
// an invalid direct cycle. It is emitted as a hoisted `JsonValue = ...`
// definition and referenced by name. Held as a class FIELD because a bare-alias
// return type is outside the codegen-safe class/class[]/primitive set.
type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>
class JsonHolder {
    data JsonValue
}

// --- two DISJOINT structural recursive aliases in one schema --------------
//
// RecStrList and RecStrMap are independent single-alias structural cycles (one
// recurses through a list, the other through a map), both reached from one
// holder. This pins MULTI-entry StructuralRecursiveAliases ordering against the
// oracle: the holder's fields are [list_field, map_field] in declaration order,
// so BAML's LIFO reachability pops the LAST field first and hoists the alias
// definitions as [RecStrMap, RecStrList] — reverse of reference. A single-alias
// fixture cannot prove this cross-entry order (the disjoint-SCC edge).
type RecStrList = RecStrList[]
type RecStrMap = map<string, RecStrMap>
class DualAliasHolder {
    list_field RecStrList
    map_field RecStrMap
}
