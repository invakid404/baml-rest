// Parity oracle functions. EVERY prompt is ONLY `{{ ctx.output_format }}` so
// the captured first-message text is exactly BAML's rendered output-format
// block and nothing else — the ground truth the native renderer must match
// byte-for-byte. The native side parses these SAME .baml bytes, builds the
// static descriptor for each function's output type, and renders it.
//
// Every function here MUST be SUPPORTED by the native builder (the corpus is
// all-supported by design); declines are covered by the unit tests in
// internal/nativeschema, not by this oracle.
//
// RETURN TYPES are restricted to class / class[] / primitive because BAML's Go
// generator emits invalid code for a function returning a bare map<...> type.
// Maps, unions, optionals, and literal-unions are exercised as class FIELDS
// (MapHolder / UnionHolder); the rendered output-format is identical either way.

function ParitySimpleOutput(input: string) -> SimpleOutput {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityPerson(input: string) -> Person {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityPersonWithAddress(input: string) -> PersonWithAddress {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityPeople(input: string) -> Person[] {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityCategorized(input: string) -> CategorizedItem {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityInt(input: string) -> int {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityLiterals(input: string) -> LiteralBag {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityMaps(input: string) -> MapHolder {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityEnumKeyMap(input: string) -> EnumKeyMapHolder {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityUnions(input: string) -> UnionHolder {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityStyled(input: string) -> Styled {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityProfile(input: string) -> Profile {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

// --- slice-4: attributes + constraints. Each proves a metadata construct
// (doc comment / constraint / stream flag) does NOT change ctx.output_format.

function ParityDocComments(input: string) -> DocCommented {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityDocVsDesc(input: string) -> DocVsDesc {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityConstraints(input: string) -> Constrained {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityStream(input: string) -> Streamed {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityMixed(input: string) -> Mixed {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

// --- slice-5: recursion. Recursive class, mutual class recursion, and a
// structural recursive alias (through list + map) — each must render
// byte-identically to BAML v0.223.

function ParityTree(input: string) -> RecursiveTreeNode {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityMutual(input: string) -> MutualA {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

function ParityJson(input: string) -> JsonHolder {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}

// Two DISJOINT structural recursive aliases reached from one holder — pins
// multi-entry StructuralRecursiveAliases hoist order (reverse-of-reference).
function ParityDualAlias(input: string) -> DualAliasHolder {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}
