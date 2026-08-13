// GENERATED at test time from the Slice 7.2b-1 checked-wire fixture table
// (fixtures_test.go) — do not edit.
//
// Regenerate with:
//   CHECKED_WIRE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml/checkedwire -run TestCheckedWireProjectDrift

client<llm> CheckedWireClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://checked-wire.invalid/v1"
  }
}

class CW_StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this > 0 }})
}

class CW_StaticAssertAnswer {
  answer string
  confidence int @assert(positive, {{ this > 0 }})
}

class CW_RequiredAssert {
  v int @assert(gt, {{ this > 100 }})
}

class CW_AliasedChecked {
  qty int @alias("amount") @check(positive, {{ this > 0 }})
}

// WIRE: one checked int whose predicate holds. Stock emits status "succeeded".
function CW_CheckPass(topic: string) -> int @check(gt, {{ this > 0 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: the same shape with a FALSE check. The value is still emitted; only the status differs.
function CW_CheckFail(topic: string) -> int @check(gt, {{ this > 100 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: the nested class placement — the carrier sits on the `confidence` field.
function CW_NestedCheck(topic: string) -> CW_StaticCheckedAnswer {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: the SAME nested class with a FALSE check. The value is still emitted, nested, with status "failed".
function CW_NestedCheckFail(topic: string) -> CW_StaticCheckedAnswer {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: the same nested class given `answer` TWICE. Stock keeps the FIRST occurrence, and the check still runs on the single `confidence`.
function CW_NestedCheckDuplicateKey(topic: string) -> CW_StaticCheckedAnswer {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: the ASSERT twin of the nested class, predicate HOLDING — no wrapper, no check entry, an ordinary int field.
function CW_NestedAssertPass(topic: string) -> CW_StaticAssertAnswer {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: the same declaration with the predicate FALSE — no value at all, and the required-field wrapper chain around the two-field class.
function CW_NestedAssertFail(topic: string) -> CW_StaticAssertAnswer {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: an expression carrying `<`, which encoding/json HTML-escapes and sonic does not.
function CW_CheckLessThan(topic: string) -> int @check(lt, {{ this < 100 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// @assert IS NOT A CHECK: a PASSING assert creates no check entry, so the value stays a bare int.
function CW_AssertPassNoCheck(topic: string) -> int @assert(ok, {{ this > 0 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// @assert IS NOT A CHECK: a node carrying both keeps only the CHECK in the wrapper.
function CW_CheckPlusPassingAssert(topic: string) -> int @check(c, {{ this > 0 }}) @assert(a, {{ this > 0 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: one LABELLED failing assert — the `Failed: <label> <expr>` cause form.
function CW_AssertFailLabelled(topic: string) -> int @assert(gt, {{ this > 100 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: one UNLABELLED failing assert — the same cause form with the label part empty.
function CW_AssertFailUnlabelled(topic: string) -> int @assert({{ this > 100 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: the same failure on a REQUIRED class field, which adds stock's wrapper chain.
function CW_AssertFailRequiredField(topic: string) -> CW_RequiredAssert {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: exactly MAX_CAUSES (5) failures — the top reason must carry no truncation count.
function CW_AssertFailFive(topic: string) -> int @assert(a1, {{ this > 101 }}) @assert(a2, {{ this > 102 }}) @assert(a3, {{ this > 103 }}) @assert(a4, {{ this > 104 }}) @assert(a5, {{ this > 105 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: six failures — the top reason gains `{total} (truncated to first 5)`.
function CW_AssertFailSix(topic: string) -> int @assert(a1, {{ this > 101 }}) @assert(a2, {{ this > 102 }}) @assert(a3, {{ this > 103 }}) @assert(a4, {{ this > 104 }}) @assert(a5, {{ this > 105 }}) @assert(a6, {{ this > 106 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: a cause of exactly 100 bytes — at the limit, so NOT truncated.
function CW_AssertFailCause100(topic: string) -> int @assert(t, {{ this > 100 and "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" != "" }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: a cause of exactly 101 bytes — one over, so truncated to 100 plus `...`.
function CW_AssertFailCause101(topic: string) -> int @assert(t, {{ this > 100 and "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" != "" }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// The same expression as a @check, so the expression text BAML RETAINED is readable and the cause length above is measured rather than assumed.
function CW_CauseExpr100Probe(topic: string) -> int @check(t, {{ this > 100 and "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" != "" }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// As above, for the one-byte-over expression.
function CW_CauseExpr101Probe(topic: string) -> int @check(t, {{ this > 100 and "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" != "" }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: `{{this > 0}}` — no padding at all.
function CW_ExprPadNone(topic: string) -> int @check(pad0, {{this > 0}}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: `{{ this > 0 }}` — the ordinary one-space form every fixture writes.
function CW_ExprPadOne(topic: string) -> int @check(pad1, {{ this > 0 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: `{{  this > 0  }}` — two spaces each side, the padding the fingerprint DECLINES.
function CW_ExprPadTwo(topic: string) -> int @check(pad2, {{  this > 0  }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ERROR BYTES: an expression that fails to EVALUATE. Stock reports `Failed to evaluate constraints:` and must NOT turn it into a failed check.
function CW_EvaluatorError(topic: string) -> int @check(ev, {{ this|urlencode }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ASYMMETRY: two @check attributes under ONE label. The raw CFFI list keeps both; baml_go's map fold keeps one.
function CW_DuplicateLabels(topic: string) -> int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 1 }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ASYMMETRY: a bare top-level `string` return SKIPS its constraints — even a false assert.
function CW_BareStringAssertSkipped(topic: string) -> string @assert(never, {{ this == "definitely-not-this" }}) {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// ASYMMETRY: alias ingress produces CANONICAL output — `amount` arrives and stock emits `qty`. This scalar predicate is NOT a sequencing witness.
function CW_AliasIngress(topic: string) -> CW_AliasedChecked {
  client CheckedWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
