# Recorded stock-v0.223.0 asymmetries — Slice 7.2b

Three places where stock BAML v0.223.0 does something native cannot reproduce
byte-for-byte, or cannot yet prove it reproduces. Each is **measured** against the
real CFFI by this package and each stays an **explicit decline**: the node goes to
BAML. None of them is "fixed" — stock's behaviour is the contract, and where a native
claim cannot be proven equal to it the safe answer is to decline.

Every row here is a `#583` parity-affecting residual. The tests that measure it are
named per row; `TestAsymmetriesLedgerCoversEveryRow` fails if this file and the row
table in `asymmetry_test.go` drift apart, and `TestAsymmetriesRemainDeclined` fails if
any row stops declining at the native entry points.

This slice (7.2b-1) changes **no** admission gate. The declines recorded below are the
current behaviour of `debaml.SupportsNativeFinalBundle` and `debaml.ParseStaticBundle`,
asserted here beside the stock measurement that explains why they must stay.

---

### bare-string-return-skips-its-constraints

- **Stock behaviour:** a bare top-level `string` return skips both `@check` and
  `@assert`. The predicate is never evaluated, so even an assert that must fail
  produces the raw assistant text and no error.
- **Measured by:** `TestStockBareStringReturnSkipsConstraints`, over the
  `BareStringAssertSkipped` fixture — `string @assert(never, {{ this == "definitely-not-this" }})`
  with the raw text `hello` returns exactly `"hello"`.
- **Discriminating control:** the same predicate shape on an `int` target DOES fail
  (`AssertFailLabelled`), so the skip belongs to the bare-string position rather than
  to the predicate.
- **7.2b disposition:** every constraint-bearing bare-string return stays **declined**.
  Existing unconstrained bare strings are unchanged.
- **Why not "fix" it:** evaluating a constraint stock skipped would make native reject
  a response BAML accepts. That is a behaviour change dressed as a bug fix. Declining
  is safe over-decline.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583

---

### duplicate-check-labels-fold-last-write-wins

- **Stock behaviour:** the raw CFFI check list preserves both declarations of a
  repeated `@check` label, in declaration order. `baml_go`'s
  `serde.decodeCheckedValue` then writes them into a `map[string]Check` in list order,
  so exactly **one** survives in the public Go value and it is the **last**.
- **Measured by:** `TestStockDuplicateLabelFoldIsLastWriteWins`, over the
  `DuplicateLabels` fixture — `int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 1 }})`
  yields one entry whose expression is `this > 1`, the SECOND declaration. The first
  declaration's expression is asserted absent, so a first-write-wins fold would fail
  the row.
- **7.2b disposition:** a candidate node carrying duplicate `@check` labels stays
  **declined**. `bamlutils.NewChecked` refuses to build the carrier as the last line of
  that rule; the same test shows that feeding it only what SURVIVED the fold does
  reproduce stock's bytes, so the refusal is about the declaration and not about the
  carrier's expressiveness.
- **Why not a list-shaped `checks`:** a public array would diverge from stock's own
  public shape, which is a map. The carrier keeps the map and carries declaration
  order out of band precisely so it can be deterministic WITHOUT changing the shape.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583

---

### alias-ingress-has-canonical-output

- **Stock behaviour:** an `@alias` is accepted on ingress and stock emits the
  **canonical** field name. The assistant text carries the alias; the wire carries
  `qty`, with its checked value and status.
- **NOT claimed — predicate sequencing.** This fixture cannot distinguish whether the
  predicate ran *before* or *after* the field-name rewrite: `this > 0` has the same
  result on either side of it, so a scalar row witnesses the combined output and
  nothing about the order. Establishing sequencing would need a predicate whose result
  differs across the rewrite; that is deliberately not built here, and the wording of
  the row, the test and the fixture prose is narrowed to match what is measured.
- **Measured by:** `TestStockAliasIngressHasCanonicalOutput`, over the
  `AliasIngress` fixture — `class CW_AliasedChecked { qty int @alias("amount") @check(positive, {{ this > 0 }}) }`
  given `{"amount": 7}` yields `{"qty":{"value":7,"checks":{...}}}`. The wire is
  asserted NOT to carry the alias.
- **7.2b disposition:** an alias anywhere in the candidate return graph — target,
  class, field, enum, or constraint-bearing node — stays **declined**. The narrower
  claim does not widen anything: the decline is unaffected either way, and
  `TestAsymmetriesRemainDeclined` keeps it enforced.
- **Why:** #662's collector has the state evidence, but the first admission cut has no
  alias parity proof. Alias ingress and canonical output need independent parity proof
  before a constraint-bearing aliased node can be claimed, and predicate sequencing is
  recorded here as **unmeasured** rather than inferred from a row that cannot show it.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583

---

## Unmeasured hazard: the non-ASCII cause-truncation boundary

`validate_asserts` truncates a cause at 100 **bytes** — Rust's `String::len()` — and
`String::truncate` **panics** when the index does not fall on a UTF-8 character
boundary. A panic inside the CFFI is not recoverable by the Go caller: it takes the
process down rather than failing a test, which is the same failure mode
`is divisibleby(0)` has.

So this package drives the 100/101-byte boundary with **ASCII only**
(`TestStockAssertErrorCauseTruncation`), where byte length and character count agree.
A cause whose 100th byte falls INSIDE a multi-byte character is deliberately **not**
driven, and its behaviour is recorded as unmeasured rather than guessed at.

The consequence for admission is already covered by the first fingerprint, which
allows only ASCII labels and expressions. Widening beyond ASCII needs this boundary
measured first — in a subprocess, so an abort is attributable — and until then a
non-ASCII expression or label stays declined.

- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583
