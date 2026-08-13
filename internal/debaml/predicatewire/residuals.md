# de-BAML Slice 7.2c-1 — residual ledger

Every form the 7.2c scope defers, with its disposition and the authority behind it.

This file is proof material, not documentation: `TestResidualLedgerCoversEveryDeferral`
in this package parses the table below, requires every residual CAPTURED from stock here
to have a row, requires every row to be `DECLINED`, and requires every authority that
names a test — in this package or in a sibling one — to name a test that really exists
there.

**Nothing in this table is admitted by 7.2c-1.** The only served predicate is still
`this > I` on the two name-pinned families, which
`TestPredicateWireAdmissionIsUnchanged` re-asserts through the production gates.

## What "authority" means per row

Three kinds appear, and they are not interchangeable:

- `predicatewire:<Test>` — measured by THIS package against the stock v0.223.0 CFFI, in
  this PR. The strongest kind: the deferral rests on an observation.
- `<package>:<Test>` — already measured elsewhere in the repo against the same stock
  CFFI. Cited rather than duplicated.
- `scope:<section>` — a decision recorded in `/tmp/shared/slice72c-scope.md` §, with no
  new measurement in this PR. These are the rows a later slice must measure before it can
  move them, and they are marked so that is visible rather than implied.

## Why the type/shape candidates are cited and not re-captured here

Every type/shape variant changes the GENERATED GO SHAPE of the pinned class
(`confidence` becomes `Checked[float64]`, `Checked[string]`, a bare value, or the struct
gains/reorders a field). `baml_go`'s type map is PROCESS-GLOBAL — one entry per class
name across every runtime — so a shape variant cannot share `StaticCheckedAnswer` with
the six operator captures in this package. The only way to capture it here would be to
rename the class, which is exactly the move the scope forbids: it would confound
"declined for its shape" with "declined for its name".

Their stock behaviour is already captured under their own names by the 49
constraint-bearing rows of `internal/debaml`'s serving oracle, which this table cites per
row. This bound is deliberate, and `TestResidualLedgerCoversEveryDeferral` logs it so a
partial matrix cannot read as a complete one.

## The ledger

| id | form | disposition | authority |
| --- | --- | --- | --- |
| two_checks | two unique `@check` on `confidence` | DECLINED | predicatewire:TestTwoCheckWireOrderIsUnstable |
| three_checks | three unique `@check` on `confidence` | DECLINED | predicatewire:TestTwoCheckWireOrderIsUnstable |
| duplicate_labels | two `@check` sharing one label | DECLINED | predicatewire:TestDuplicateLabelsFoldLastWriteWins |
| check_then_assert | one `@check` then one `@assert` | DECLINED | predicatewire:TestMixedCheckAndAssertInBothDeclarationOrders |
| assert_then_check | one `@assert` then one `@check` | DECLINED | predicatewire:TestMixedCheckAndAssertInBothDeclarationOrders |
| two_asserts | two failing `@assert` on the pinned assert family | DECLINED | predicatewire:TestTwoAssertsOnThePinnedFamilyRecordCauseOrder |
| noncanonical_literal | `+5`, `007`, `1_000`, `5.0`, i64 overflow | DECLINED | predicatewire:TestStockNonCanonicalLiteralDispositions |
| padding_two_spaces | two ASCII spaces each side of the predicate | DECLINED | predicatewire:TestStockPaddingIsStrippedForEveryOperator |
| operator_ge | `this >= I` | DECLINED | predicatewire:TestPredicateWireAdmissionIsUnchanged |
| operator_lt | `this < I` | DECLINED | predicatewire:TestPredicateWireAdmissionIsUnchanged |
| operator_le | `this <= I` | DECLINED | predicatewire:TestPredicateWireAdmissionIsUnchanged |
| operator_eq | `this == I` | DECLINED | predicatewire:TestPredicateWireAdmissionIsUnchanged |
| operator_ne | `this != I` | DECLINED | predicatewire:TestPredicateWireAdmissionIsUnchanged |
| i64_beyond_exact | an admitted-family VALUE at or past 2^53 | DECLINED | predicatewire:TestDirectIntBoundaryMatrix |
| i64_long_literal | a canonical literal longer than 15 digits — including `9007199254740991`, which is 2^53-1 and therefore below the guard's own 2^53 threshold | DECLINED | predicatewire:TestDirectIntBoundaryMatrix |
| i64_negative_literal | a negative canonical literal, which the generic guard reads as arithmetic | DECLINED | predicatewire:TestDirectIntBoundaryMatrix |
| compound_predicate | `and`, `or`, `not`, `&&`, ternaries, membership, concatenation | DECLINED | scope:Verified broadening decisions 2 |
| filters_and_arithmetic | filters, arithmetic, parentheses, alternate literal syntax | DECLINED | scope:Verified broadening decisions 2 |
| type_float | `confidence float` | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_string | a constrained `string` field | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_bool | a constrained `bool` field | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_enum | a constrained enum field | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_nullable | a constrained nullable field | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_list | a constrained list, and a constrained list ELEMENT | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_map | a constrained map key or value | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_union | a constrained union | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_nested_class | a constrained field on a nested class | DECLINED | debaml:TestServingOracleBoundaryLock |
| type_media | a constrained media value | DECLINED | scope:Verified broadening decisions 5 |
| target_constraint | `@check`/`@assert` on the return type itself | DECLINED | debaml:TestServingOracleBoundaryLock |
| class_constraint | a `@@check`/`@@assert` class-level constraint | DECLINED | debaml:TestServingOracleBoundaryLock |
| toplevel_constrained | a bare constrained scalar target (all six operators, both levels) | DECLINED | predicatewire:TestTopLevelOperatorFormsAreDeclined |
| shape_third_field | a third field beside `answer` and `confidence` | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| shape_reordered_fields | `confidence` before `answer` | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| shape_constraint_on_answer | the constraint on `answer` instead of `confidence` | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| shape_two_constrained_fields | constraints on both fields | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| shape_class_name | either pinned class renamed | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| shape_extra_definitions | a second class, or an enum, in the bundle | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| meta_alias | an `@alias` on a field | DECLINED | checkedwire:TestStockAliasIngressHasCanonicalOutput |
| meta_description | a `@description` on a field | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| meta_stream_dynamic | stream or dynamic metadata | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| label_non_ascii | a non-ASCII constraint label or expression | DECLINED | checkedwire:asymmetries.md |
| label_empty_present | a present-but-empty label | DECLINED | debaml:TestStaticCheckedGatesShareOneFingerprint |
| route_static_stream | the static STREAM lane | DECLINED | debaml:TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed |
| route_dynamic_final | the dynamic final lane | DECLINED | debaml:TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed |
| route_direct_parse | `ParseStaticBundle` without the static-unary claim | DECLINED | predicatewire:TestPredicateWireAdmissionIsUnchanged |
| route_call_with_raw | `CallWithRaw` | DECLINED | debaml:TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed |

## The two findings a later slice must act on

1. **2+ `@check` is blocked by measurement, not by effort.** `sonic.Marshal` of stock's
   two-key result produced BOTH orderings across 200 observations, and the three-key
   result produced three. The native carrier is deterministic, so at least N-1 of the
   distinct strings stock emits are unreachable for ANY single native ordering — byte-exact
   parity is not available at any key count above one, whichever permutation native picks.
   (That the two sides agree on CONTENT is proven separately, by strictly decoding both and
   comparing order-insensitively; which sampled permutation native happens to equal is an
   observation about Go map iteration, not a stock contract.) Moving this row needs a newly
   approved stock ordering contract, not mapper work.

2. **The direct-i64 totality gap is much wider than the 2^53 guard.** Of the 222 rows in
   the boundary matrix, stock answers every one exactly; native's current generic profile
   answers 36 and refuses 186. The refusals split three INDEPENDENT ways, each with its
   own ledger row above:

   - `i64_beyond_exact` — a VALUE at or past 2^53. Note this is the GUARD's threshold,
     not where float64 loses exactness: every integer up to and including 2^53 round-trips
     exactly, and 2^53+1 is the first that does not, so the guard sits one step early on
     purpose;
   - `i64_long_literal` — a LITERAL longer than 15 digits. This one is not a restatement of
     the first: `9007199254740991` is 2^53-1 — below the guard's own 2^53 threshold, and
     exactly representable in float64 either way — and is still refused purely for its
     sixteen digits. Over non-negative literals the measured answering frontier therefore
     crosses at 10^15, **below** 2^53;
   - `i64_negative_literal` — **every** negative literal, whatever its magnitude, because
     `-` is an arithmetic byte and `this ...` never parses as the closed numeric
     sublanguage. The scope did not call this one out at all.

   7.2c-2 must close all three for the direct grammar, not only the first. The magnitude
   frontier applies only on the non-negative side; the sign clause is absolute and is
   reported separately rather than folded into a single number.
