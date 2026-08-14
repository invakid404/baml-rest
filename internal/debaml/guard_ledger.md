<!-- GENERATED from testdata/guard_ledger/ledger.json — do not edit by hand.
     Regenerate with: GUARD_LEDGER_MARKDOWN_WRITE=1 go test ./internal/debaml -run TestGuardLedgerMarkdownIsRendered -->

# de-BAML constraint-evaluator guard-removal ledger

One entry per compensation guard in the native constraint evaluator, whether or
not it moved.

The evaluator was written against the UPSTREAM pure-Go minijinja port and now
compiles against the BAML-exact fork `github.com/invakid404/minijinja-go/v2`.
The fork's PATCHES.md says what the ENGINE does; it does not say what stock BAML
v0.223 does with a given value, so it is capability evidence and never removal
authority. Every entry below therefore cites **witness rows**: generated `.baml`
methods plus exact raw model JSON, driven through the real stock CFFI by
`internal/debaml/guardledger`, which records the stock outcome envelope FIRST
and then compares the native leg against that recording.

A guard is removed only when all of its rows are **green** — stock and native
envelopes agree, either because both decided the same thing or because both
refused. A row where stock decides and native refuses is a measured **coverage
cost**, not a removal licence.

Nothing here changes admission. Every constraint-bearing bundle still declines at
`checkSupported`, so a removal widens only the internal evaluator's ANSWER
surface, which production does not reach.

## Summary

| Guard | File | Class | Disposition | Rows | Effect |
|---|---|---|---|---|---|
| `lengthGuard` | `constraint_profile.go` | P | **removed** | 15 | none (subsumed) |
| `lastMappingGuard` | `constraint_profile.go` | P | **kept-unprovable** | 5 | none (unreachable in production; no row can observe its absence) |
| `withdrawNonBAMLBuiltins` | `constraint_eval.go` | P | **kept-inert** | 5 | none (all rows agree) |
| `numericProfile` | `constraint_profile.go` | P per operator family | **kept-over-decline** | 9 | coverage-only |
| `integerResultWrappers` | `constraint_profile.go` | A | **kept-over-decline** | 5 | coverage-only |
| `splitWithdrawal` | `constraint_profile.go` | P after lifecycle rows | **kept-over-decline** | 5 | coverage-only |
| `itemsTojsonMappingGuards` | `constraint_profile.go` | P for ordered ConstraintValue maps | **kept-over-decline** | 9 | coverage-only |
| `mappingDualRender` | `constraint_eval.go`, `constraint_value.go` | A | **kept-over-decline** | 30 | coverage-only |
| `guardForeignMapping` | `constraint_profile.go` | A for non-string keys | **kept-over-decline** | 39 | coverage-only |
| `rangeWithdrawal` | `constraint_profile.go` | P for small bounded ranges only | **kept-over-decline** | 3 | coverage-only |
| `globalWithdrawals` | `constraint_profile.go` | A | **kept-over-decline** | 22 | coverage-only |
| `operatorShapeIsProven` | `constraint_operator.go` | P per grammar family | **kept-over-decline** | 16 | coverage-only |
| `displayString` | `constraint_eval.go` | P | **kept-over-decline** | 5 | coverage-only |
| `withdrawnBuiltinsTable` | `constraint_profile.go` | A | **kept-over-decline** | 6 | coverage-only |
| `installProfileGuardsTable` | `constraint_profile.go` | A | **kept-over-decline** | 6 | coverage-only |
| `checkCallParitySignatures` | `constraint_profile.go` | A | **kept-over-decline** | 15 | coverage-only |
| `divisibleByNonIntegral` | `constraint_profile.go` | A | **kept-over-decline** | 1 | coverage-only |
| `pycompatUnknownMethod` | `constraint_eval.go` | A | **kept-over-decline** | 2 | coverage-only |
| `hasMedia` | `constraint_profile.go` | U | **kept-unwitnessable** | 0 | coverage-only |
| `divisibleByZero` | `constraint_profile.go` | U | **kept-unwitnessable** | 0 | coverage-only |

## Per-callable inventory

Scope §1 treats each name in the two broad default-decline tables — and each
wrapper application over them — as its own inventory record rather than as part
of one table-level entry, so completeness is provable and the deferral attaches
to each retained decline individually.

The rows below are derived from the LIVE tables (`profileFilterBuiltins`, `profileTestBuiltins`, `withdrawnBuiltins`, `withdrawnGlobals`),
and the admitted call shape is re-derived from `provenSignatures` when this file
is rendered. A callable added to the profile without an entry here fails
`TestGuardLedgerCoversEveryCallable`, and an entry whose shape disagrees with the
live table fails it too.

Nothing in this table is removed by 7.2a-1. "declines in every shape" is a
retained over-decline and links the deferral record; a callable with a proven
signature still answers inside that shape.

| Callable | Listed by | Wrapper | Admitted call shape | Rows |
|---|---|---|---|---|
| `filter:abs` | `installProfileGuards` | guardIntegerResult | subject {number}; arity 0..0; no positional arguments; kwargs rejected | `PG_ABS` |
| `filter:attr` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:batch` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_BATCH_DICT` `FM_BATCH_LIT` |
| `filter:bool` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:capitalize` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:chain` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_CHAIN_DICT` `FM_CHAIN_LIT` |
| `filter:count` | `installProfileGuards` | guardIntegerResult | subject {string\|seq\|map\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `CNT_BOOL` `CNT_CLS` `CNT_INT` `CNT_LIST` `CNT_MAP` `CNT_NULL` `CNT_STR` |
| `filter:d` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:default` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:dictsort` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:e` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:escape` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:first` | `installProfileGuards`, `guardForeignMapping` | guardIntegerResult | subject {seq\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `CLS_LIST_ORDER` `FIRST_LIST` `FM_CHAIN_DICT` `FM_CHAIN_LIT` `FM_FIRST_DICT` `FM_FIRST_LIT` `FM_LIST_DICT` `FM_LIST_LIT` `FM_MAP_DICT` `FM_MAP_LIT` `FM_REJECTATTR_DICT` `FM_REJECTATTR_LIT` `FM_REJECT_DICT` `FM_REJECT_LIT` `FM_REVERSE_DICT` `FM_REVERSE_LIT` `FM_SELECT_DICT` `FM_SELECT_LIT` `FM_UNIQUE_DICT` `FM_UNIQUE_LIT` `ITEMS_ORDER_CLS` `ITEMS_ORDER_MAP` `MAP_LIST_ORDER` `MAP_REVERSE_ORDER` `MAP_REVERSE_ORDER_B` `MAP_STRING` `WT_SORT` |
| `filter:float` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:format` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:groupby` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_GROUPBY_DICT` `FM_GROUPBY_LIT` |
| `filter:indent` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_INDENT_DICT` `FM_INDENT_LIT` |
| `filter:int` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `N7` `N9` |
| `filter:items` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `ITEMS_CLS` `ITEMS_MAP` `ITEMS_NEST` `ITEMS_ORDER_CLS` `ITEMS_ORDER_MAP` |
| `filter:join` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_JOIN_DICT` `FM_JOIN_LIT` |
| `filter:last` | `installProfileGuards` | guardIntegerResult | subject {seq\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `LAST_CLS_KEY` `LAST_CLS_VALUE` `LAST_LIST` `LAST_MAP_KEY` `RANGE_LAST` |
| `filter:length` | `installProfileGuards` | guardIntegerResult | subject {string\|seq\|map\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `DEBUG_CALL` `DICT_ARITY` `FMAP_DICT` `FMAP_LIST` `FMAP_NONSTRING_KEY` `FM_BATCH_DICT` `FM_BATCH_LIT` `FM_GROUPBY_DICT` `FM_GROUPBY_LIT` `FM_INDENT_DICT` `FM_INDENT_LIT` `FM_PPRINT_DICT` `FM_PPRINT_LIT` `FM_SELECTATTR_DICT` `FM_SELECTATTR_LIT` `FM_SLICE_DICT` `FM_SLICE_LIT` `FM_ZIP_DICT` `FM_ZIP_LIT` `ITEMS_CLS` `ITEMS_MAP` `ITEMS_NEST` `LEN_BOOL` `LEN_CLS` `LEN_INT` `LEN_LIST` `LEN_MAP` `LEN_NULL` `LEN_STR` `MAP_CONCAT` `MAP_LIST` `MAP_REVERSE_LIST` `MAP_STRING_LEN` `N10` `N11` `O9e` `RANGE_LIST` `RANGE_STEP` `SPLIT_LENGTH` `SPLIT_LIST` `TOJSON_MAP` `TOJSON_NEST` |
| `filter:lines` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:list` | `installProfileGuards`, `guardForeignMapping` | guardIntegerResult | subject {seq\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `CLS_LIST_ORDER` `FMAP_DICT` `FMAP_LIST` `FMAP_NONSTRING_KEY` `FM_BATCH_DICT` `FM_BATCH_LIT` `FM_CHAIN_DICT` `FM_CHAIN_LIT` `FM_GROUPBY_DICT` `FM_GROUPBY_LIT` `FM_LIST_DICT` `FM_LIST_LIT` `FM_REJECTATTR_DICT` `FM_REJECTATTR_LIT` `FM_REJECT_DICT` `FM_REJECT_LIT` `FM_REVERSE_DICT` `FM_REVERSE_LIT` `FM_SELECTATTR_DICT` `FM_SELECTATTR_LIT` `FM_SELECT_DICT` `FM_SELECT_LIT` `FM_SLICE_DICT` `FM_SLICE_LIT` `FM_UNIQUE_DICT` `FM_UNIQUE_LIT` `FM_ZIP_DICT` `FM_ZIP_LIT` `ITEMS_CLS` `ITEMS_MAP` `ITEMS_NEST` `MAP_LIST` `MAP_LIST_ORDER` `MAP_REVERSE_LIST` `MAP_REVERSE_ORDER` `MAP_REVERSE_ORDER_B` `RANGE_LIST` `RANGE_STEP` `SPLIT_LIST` |
| `filter:lower` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:map` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_MAP_DICT` `FM_MAP_LIT` |
| `filter:max` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:min` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:pprint` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_PPRINT_DICT` `FM_PPRINT_LIT` |
| `filter:regex_match` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `BS_REGEX` |
| `filter:reject` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_REJECT_DICT` `FM_REJECT_LIT` |
| `filter:rejectattr` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_REJECTATTR_DICT` `FM_REJECTATTR_LIT` |
| `filter:replace` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:reverse` | `installProfileGuards`, `guardForeignMapping` | guardIntegerResult | subject {seq\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `FM_REVERSE_DICT` `FM_REVERSE_LIT` `MAP_REVERSE_LIST` `MAP_REVERSE_ORDER` `MAP_REVERSE_ORDER_B` |
| `filter:round` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:safe` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:select` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_SELECT_DICT` `FM_SELECT_LIT` |
| `filter:selectattr` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_SELECTATTR_DICT` `FM_SELECTATTR_LIT` |
| `filter:slice` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_SLICE_DICT` `FM_SLICE_LIT` `N10` `N11` |
| `filter:sort` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `WT_SORT` |
| `filter:split` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `SPLIT_INDEX` `SPLIT_INDEX_BARE` `SPLIT_ITERABLE` `SPLIT_LENGTH` `SPLIT_LIST` |
| `filter:string` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_STRING_DICT` `FM_STRING_LIT` `MAP_STRING` `MAP_STRING_LEN` `MAP_STRING_ORDER` |
| `filter:sum` | `installProfileGuards` | guardIntegerResult | subject {seq\|iterable}; arity 0..0; no positional arguments; kwargs rejected | `PG_SUM` |
| `filter:title` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `WT_TITLE` |
| `filter:tojson` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | `TOJSON_MAP` `TOJSON_NEST` `TOJSON_ORDER_MAP` `TOJSON_ORDER_NEST` |
| `filter:trim` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:unique` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_UNIQUE_DICT` `FM_UNIQUE_LIT` |
| `filter:upper` | `installProfileGuards`, `withdrawnBuiltins` | guardIntegerResult | withdrawn: declines in every shape | — |
| `filter:zip` | `installProfileGuards`, `withdrawnBuiltins`, `guardForeignMapping` | guardIntegerResult | withdrawn: declines in every shape | `FM_ZIP_DICT` `FM_ZIP_LIT` |
| `test:is !=` | `installProfileGuards` | guardTestInput | no proven signature: declines in every shape | — |
| `test:is <` | `installProfileGuards` | guardTestInput | no proven signature: declines in every shape | — |
| `test:is <=` | `installProfileGuards` | guardTestInput | no proven signature: declines in every shape | — |
| `test:is ==` | `installProfileGuards` | guardTestInput | no proven signature: declines in every shape | — |
| `test:is >` | `installProfileGuards` | guardTestInput | no proven signature: declines in every shape | — |
| `test:is >=` | `installProfileGuards` | guardTestInput | no proven signature: declines in every shape | — |
| `test:is boolean` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is defined` | `installProfileGuards` | guardTestInput | subject {undefined\|none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is endingwith` | `installProfileGuards` | guardTestInput | subject {string}; arity 1..1; positional [{string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is eq` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is equalto` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is escaped` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is even` | `installProfileGuards` | guardTestInput | subject {number}; arity 0..0; no positional arguments; kwargs rejected | `N8` |
| `test:is false` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is filter` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | — |
| `test:is float` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is ge` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is greaterthan` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is gt` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is in` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | — |
| `test:is int` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is integer` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is iterable` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | `SPLIT_ITERABLE` |
| `test:is le` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is lessthan` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is lower` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | — |
| `test:is lt` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is mapping` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | `PG_IS_MAPPING` |
| `test:is ne` | `installProfileGuards` | guardTestInput | subject {bool\|number\|string}; arity 1..1; positional [{bool\|number\|string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is none` | `installProfileGuards` | guardTestInput | subject {undefined\|none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is number` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is odd` | `installProfileGuards` | guardTestInput | subject {number}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is safe` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is sameas` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | — |
| `test:is sequence` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is startingwith` | `installProfileGuards` | guardTestInput | subject {string}; arity 1..1; positional [{string}]; kwargs rejected; a non-integral numeric argument is refused | — |
| `test:is string` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is test` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | — |
| `test:is true` | `installProfileGuards` | guardTestInput | subject {none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is undefined` | `installProfileGuards` | guardTestInput | subject {undefined\|none\|bool\|number\|string\|seq\|map}; arity 0..0; no positional arguments; kwargs rejected | — |
| `test:is upper` | `installProfileGuards`, `withdrawnBuiltins` | guardTestInput | withdrawn: declines in every shape | — |
| `test:is divisibleby` | `installProfileGuards` | guardTestInput + the divisibleby guard | subject {number}; arity 1..1; positional [{number}]; kwargs rejected; a numeric argument is coerced rather than type-checked | `N12` `PG_DIVISIBLEBY` |
| `global:range` | `globalWithdrawals` | none — a global reaches neither wrapper | no proven signature: declines in every shape | `RANGE_LAST` `RANGE_LIST` `RANGE_STEP` |
| `global:debug` | `globalWithdrawals` | none — a global reaches neither wrapper | no proven signature: declines in every shape | `DEBUG_CALL` |
| `global:dict` | `globalWithdrawals` | none — a global reaches neither wrapper | no proven signature: declines in every shape | `DICT_ARITY` `FMAP_DICT` `FM_BATCH_DICT` `FM_CHAIN_DICT` `FM_FIRST_DICT` `FM_GROUPBY_DICT` `FM_INDENT_DICT` `FM_JOIN_DICT` `FM_LIST_DICT` `FM_MAP_DICT` `FM_PPRINT_DICT` `FM_REJECTATTR_DICT` `FM_REJECT_DICT` `FM_REVERSE_DICT` `FM_SELECTATTR_DICT` `FM_SELECT_DICT` `FM_SLICE_DICT` `FM_STRING_DICT` `FM_UNIQUE_DICT` `FM_ZIP_DICT` |
| `global:namespace` | `globalWithdrawals` | none — a global reaches neither wrapper | no proven signature: declines in every shape | `NAMESPACE_ATTR` |
| `filter:urlencode` | `withdrawNonBAMLBuiltins` | unknown-name stub | not part of BAML's feature set: raises an unknown-name error in every shape | `WB_URLENCODE` |
| `test:is containing` | `withdrawNonBAMLBuiltins` | unknown-name stub | not part of BAML's feature set: raises an unknown-name error in every shape | `WB_CONTAINING` |
| `global:cycler` | `withdrawNonBAMLBuiltins` | unknown-name stub | not part of BAML's feature set: raises an unknown-name error in every shape | `WB_CYCLER` |
| `global:joiner` | `withdrawNonBAMLBuiltins` | unknown-name stub | not part of BAML's feature set: raises an unknown-name error in every shape | `WB_JOINER` |
| `global:lipsum` | `withdrawNonBAMLBuiltins` | unknown-name stub | not part of BAML's feature set: raises an unknown-name error in every shape | `WB_LIPSUM` |
| `filter:truncate` | `withdrawnBuiltins` | none — the profile never registers it | withdrawn: declines in every shape | — |

## Entries

### `lengthGuard` — removed

- **What it is:** lengthGuard (length/count no-Len wrapper)
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** P
- **Fork capability that bears on it:** PATCHES.md #50 (filter/function type errors) — the fork's own `length` raises the error class stock raises instead of answering.
- **Witness rows:** `LEN_INT`, `LEN_BOOL`, `LEN_NULL`, `LEN_STR`, `LEN_LIST`, `LEN_MAP`, `LEN_CLS`, `CNT_INT`, `CNT_STR`, `CNT_LIST`, `CNT_MAP`, `CNT_CLS`, `CNT_BOOL`, `CNT_NULL`, `SPLIT_LENGTH`
- **Recorded stock envelope:** pass for LEN_STR, LEN_LIST, LEN_MAP, LEN_CLS, CNT_STR, CNT_LIST, CNT_MAP, CNT_CLS; evaluator-error for LEN_INT, CNT_INT (`invalid operation: cannot calculate length of value of type number`); LEN_BOOL, CNT_BOOL (`invalid operation: cannot calculate length of value of type bool`); SPLIT_LENGTH (`invalid operation: cannot calculate length of value of type iterator`); no-checks for LEN_NULL, CNT_NULL. LEN_NULL and CNT_NULL are no-checks for the same optional-swallow reason; SPLIT_LENGTH is the length-less ITERATOR case, which is why its message differs from the number/bool ones.
- **Recorded native envelope:** agreement — the same outcome stock recorded — for LEN_STR, LEN_LIST, LEN_MAP, LEN_CLS, CNT_STR, CNT_LIST, CNT_MAP, CNT_CLS; native-unsupported for LEN_INT, LEN_BOOL, LEN_NULL, CNT_INT, CNT_BOOL, CNT_NULL (attributed to checkCallParity/subject-kind); native-unsupported for SPLIT_LENGTH (attributed to operatorShapeIsProven). SPLIT_LENGTH is refused one gate earlier than its siblings: the operator gate declines the withdrawn `split` chain before the signature table is consulted.
- **Change this makes:** none (subsumed)
- **Now carried by:** checkCallParity + provenSignatures["length"/"count"].subject == kSized
- **Rollback condition:** TWO LAYERS stand behind the removal, and it is unsafe only when both fail together: a length-less value must REACH the builtin (checkCallParity stops refusing it, i.e. provenSignatures widens `length`/`count` beyond kSized, or kSized stops being exactly the set of kinds that answer Value.Len()) AND the engine's own `length` must stop RAISING for such a value. Either layer alone still yields a decline — the first leaves the engine's refusal, the second is unreachable while the signature table holds — so either on its own is an early warning rather than a wrong boolean. THREE INVARIANTS carry those two layers, and TestRemovedLengthGuardIsSubsumedAtTheFilterSeam asserts each directly, at the filter seam: (1) checkCallParity refuses every length-less subject for both `length` and `count`, and still admits every sized one; (2) kSized is exactly the has-a-Len set over every value the model produces, under both mapping projections; (3) the engine's own `length` raises — with stock's own error class — for a constructed kSized value that has no Len. Reinstate the guard if any of the three moves.

checkCallParity runs BEFORE the wrapped builtin (guardIntegerResult calls it first) and admits `length`/`count` only for kSized, which is exactly the set of kinds that answer Value.Len() over every value the model produces under both mapping projections — so the guard could never fire. Unlike the `last` candidate, its witness rows REACH the filter seam: LEN_INT, LEN_BOOL, CNT_INT and CNT_BOOL are attributed to checkCallParity's subject rule, not to an earlier gate, so the survivor named here is the live one. The residual case — a kSized value with no Len — is constructed in the negative and shown to RAISE in the engine, which is still a decline.

### `lastMappingGuard` — kept-unprovable

- **What it is:** `last` over a mapping guard
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** P
- **Fork capability that bears on it:** PATCHES.md #36/#43 (ordered mappings; `reverse` on a mapping) bear on the behaviour, and the fork's `last` now raises exactly where stock raises — but fork capability is not what is missing here: an observable witness is.
- **Witness rows:** `LAST_CLS_VALUE`, `LAST_CLS_KEY`, `LAST_MAP_KEY`, `LAST_LIST`, `FIRST_LIST`
- **Liveness proof (no row can reach it):** TestLastOverMappingGuardIsLiveInTheInstalledChain (the wrapper is installed and fires once checkCallParity's subject rule is lifted), TestLastOverMappingGuardPreEmptedByCheckCallParity (it is unreachable while that rule stands), TestLastOverMappingGuardRefusesDirectly
- **Recorded stock envelope:** pass for LAST_LIST, FIRST_LIST; evaluator-error for LAST_CLS_VALUE, LAST_CLS_KEY, LAST_MAP_KEY (`invalid operation: cannot get last item from value`)
- **Recorded native envelope:** agreement — the same outcome stock recorded — for LAST_LIST, FIRST_LIST; native-unsupported for LAST_CLS_VALUE, LAST_CLS_KEY, LAST_MAP_KEY (attributed to operatorShapeIsProven). Every attribution is to operatorShapeIsProven — NOT to the guard under discussion, which is precisely why no row can observe its absence and why it stays.
- **Change this makes:** none (unreachable in production; no row can observe its absence)
- **Rollback condition:** Revisit only when BOTH gates in front of this wrapper widen: operatorShapeIsProven must ADMIT `<mapping>|last` (today `|last` needs a sequence subject to infer an element kind) AND checkCallParity must admit a mapping subject for `last` (today provenSignatures["last"] is kSequence, which excludes kMap). Either gate alone still blocks every witness row, so neither on its own makes the removal testable. TestLastOverMappingGuardPreEmptedByCheckCallParity asserts both conditions and fails the moment either changes.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

THREE layers of redundancy, all measured. (1) Stock RAISES on `<mapping>|last` — LAST_CLS_VALUE, LAST_CLS_KEY and LAST_MAP_KEY all record `invalid operation: cannot get last item from value`. (2) checkCallParity refuses a kMap subject for `last` before this wrapper is delegated to, so it is UNREACHABLE in production. (3) Even with that lifted, the fork's own `last` raises the SAME message stock raises (TestLastOverMappingEngineAlreadyRaises) — the guard was written against the UPSTREAM port, which returned the mapping's final key instead, and that divergence is closed. It is kept anyway because the removal is not ROW-provable: a witness row is an expression, and reaching this seam from one needs BOTH operatorShapeIsProven and checkCallParity to admit a mapping subject for `last` — the operator gate refuses the expression outright and the signature table refuses the call, so no row can distinguish an environment carrying the wrapper from one without it. Liveness is therefore proved in-package, by lifting the signature-table shadow and asserting the wrapper's own marker — deleting the wrapper fails that test while leaving every CFFI row green.

### `withdrawNonBAMLBuiltins` — kept-inert

- **What it is:** withdrawNonBAMLBuiltins (urlencode, containing, cycler, joiner, lipsum)
- **Where:** internal/debaml/constraint_eval.go
- **Classification:** P
- **Fork capability that bears on it:** PATCHES.md #45 — the fork's default registry no longer registers any of the five.
- **Witness rows:** `WB_URLENCODE`, `WB_CONTAINING`, `WB_CYCLER`, `WB_JOINER`, `WB_LIPSUM`
- **Recorded stock envelope:** evaluator-error for WB_URLENCODE (`unknown filter: filter urlencode is unknown`); WB_CONTAINING (`unknown test: test containing is unknown`); WB_CYCLER (`unknown function: cycler is unknown`); WB_JOINER (`unknown function: joiner is unknown`); WB_LIPSUM (`unknown function: lipsum is unknown`)
- **Recorded native envelope:** native-unsupported for WB_URLENCODE, WB_CYCLER, WB_JOINER, WB_LIPSUM (attributed to operatorShapeIsProven); native-unsupported for WB_CONTAINING (attributed to engine/unknown-name)
- **Change this makes:** none (all rows agree)
- **Rollback condition:** Remove the stubs only together with their five rows. If the fork re-registers `containing`, WB_CONTAINING flips straight from agree-refusal to a native answer stock does not produce, because a test call is inside the predicate grammar and reaches evaluation on its own. The other four are refused by operatorShapeIsProven first, so each of those rows flips only when BOTH the operator gate admits its shape AND the fork re-registers the name. Either way the fail-closed test is what catches it.

Removal is PERMITTED by the evidence — every row is an agreement — and is declined anyway: the stubs cost no coverage, cannot widen anything (each only ever errors), and are a standing assertion of BAML's `default-features = false` feature set at the seam this package owns. Scope §1 states they need not be removed.

### `numericProfile` — kept-over-decline

- **What it is:** exceedsExactIntegerRange, numericParser/parseNumeric, isProvablySmallNumber, bracketRegionsBlanked, bracketIsListLiteral, listLiteralRegionIsSafe, subscriptRegionIsSafe, parsePow
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** P per operator family
- **Fork capability that bears on it:** PATCHES.md #10–#31 (checked i128 core, exact comparison, `%`/`//`/`**` conventions, slice bounds, AsInt consumers).
- **Witness rows:** `N1`, `N1b`, `N2`, `N3`, `N4`, `N5`, `N6`, `N7`, `BS_REGEX`
- **Recorded stock envelope:** pass for N1b, N2, N3, N4, N5, BS_REGEX; failed-check for N1, N6, N7. N1 is 2^53+1 against 2^53, which stock's exact integer core keeps apart; N6 is a fractional DIRECT subscript, which resolves to undefined rather than erroring; N7 is two integers manufactured past 2^53 by `|int`.
- **Recorded native envelope:** agreement — the same outcome stock recorded — for N1, N1b and N5; native-unsupported for N2, N3, N4, N6, N7, BS_REGEX (attributed to exceedsExactIntegerRange). N1 and N1b agree as of Slice 7.2c-2: their expression is the closed direct grammar `this OP <canonical i64>`, which EvaluateConstraint now decides with an exact int64 comparison (internal/debaml/constraint_direct_i64.go) instead of routing through this guard. Every other cited row is outside that grammar and still refuses, so the guard itself is unchanged.
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. A per-family widening must land with the family's own rows and may not lean on the fork ledger alone.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

Signed `%` and `//` are euclidean in stock and truncated/floored in Go, so only non-negative integer literal operands are proven; `**` is bounded only for a manifest literal base and exponent. BS_REGEX additionally records that a backslash inside a string literal makes the bracket-region scan refuse to say where the literal ends, so the whole expression is declined before the filter is reached. SLICE 7.2c-2: the guard was NOT relaxed and no clause of it was removed — a closed direct-comparison path was added AHEAD of it in EvaluateConstraint for `this OP <canonical i64>` over an integer `this`, which is total across the whole i64 range and therefore closes the post-claim unsupported hazard on the already-served `this > I` fingerprint. N1/N1b are the two witnesses that moved to agreement; the guard's own refusals (N2, N3, N4, N6, N7, BS_REGEX) are byte-for-byte what they were.

### `integerResultWrappers` — kept-over-decline

- **What it is:** guardIntegerResult, guardTestInput, containsInexactInteger, containsAsIntHazard, maxAbsInt, floatIntConversionMagnitude, asIntHazardError
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #10–#31 close the same numeric class at the engine, but the wrappers are applied to EVERY registered filter and test.
- **Witness rows:** `N7`, `N8`, `N9`, `N10`, `N11`
- **Recorded stock envelope:** pass for N9; failed-check for N7, N8; evaluator-error for N10 (`invalid operation: count cannot be 0`); N11 (`invalid operation: cannot convert number to usize`)
- **Recorded native envelope:** native-unsupported for N7, N8, N9 (attributed to exceedsExactIntegerRange); native-unsupported for N10, N11 (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. Deleting the wrappers wholesale is out of scope for 7.2a; a per-callable table-driven allowlist is a separate effort.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

N8 is the round-13 shape (a float at 2^63 read through AsInt); N9 manufactures an integer past 2^53 from a string subject. Neither is visible to a static scan, which is why the check is on the value at the point of production.

### `splitWithdrawal` — kept-over-decline

- **What it is:** `split` hard withdrawal
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** P after lifecycle rows
- **Fork capability that bears on it:** PATCHES.md #42/#50 (empty iterables; filter/function type errors).
- **Witness rows:** `SPLIT_LIST`, `SPLIT_ITERABLE`, `SPLIT_INDEX`, `SPLIT_INDEX_BARE`, `SPLIT_LENGTH`
- **Recorded stock envelope:** pass for SPLIT_LIST, SPLIT_ITERABLE, SPLIT_INDEX; evaluator-error for SPLIT_LENGTH (`invalid operation: cannot calculate length of value of type iterator`); source-rejected for SPLIT_INDEX_BARE. SPLIT_INDEX_BARE has no stock leg at either level: BAML refuses to COMPILE that spelling, which is the observation the row records.
- **Recorded native envelope:** native-unsupported for SPLIT_LIST, SPLIT_ITERABLE, SPLIT_INDEX, SPLIT_INDEX_BARE, SPLIT_LENGTH (attributed to operatorShapeIsProven). SPLIT_INDEX_BARE has no stock leg to compare against and is still required to decline.
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. The lifecycle rows show the withdrawal is still load-bearing.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

SPLIT_LENGTH is the row that keeps this guard: the fork's own Iterator CARRIES a length (value/value.go Len's `*Iterator` arm), so `this|split(" ")|length` would answer 2 where stock raises. The three rows on which the two engines WOULD match if the chain were admitted — SPLIT_LIST, SPLIT_ITERABLE and SPLIT_INDEX — do not offset that; in the ledger's own vocabulary none of the five is an agreement, because native declines every one of them.

### `itemsTojsonMappingGuards` — kept-over-decline

- **What it is:** `items` mapping guard, `tojson` containsMapping guard
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** P for ordered ConstraintValue maps
- **Fork capability that bears on it:** PATCHES.md #36/#37 (ordered mappings; map key spelling).
- **Witness rows:** `ITEMS_MAP`, `ITEMS_CLS`, `TOJSON_MAP`, `TOJSON_NEST`, `ITEMS_NEST`, `ITEMS_ORDER_MAP`, `ITEMS_ORDER_CLS`, `TOJSON_ORDER_MAP`, `TOJSON_ORDER_NEST`
- **Recorded stock envelope:** pass for ITEMS_MAP, ITEMS_CLS, TOJSON_MAP, TOJSON_NEST, ITEMS_NEST, ITEMS_ORDER_MAP, ITEMS_ORDER_CLS, TOJSON_ORDER_MAP, TOJSON_ORDER_NEST. TOJSON_MAP records 13 rendered bytes (`{"b":1,"a":2}`) and TOJSON_NEST 23; TOJSON_ORDER_MAP and TOJSON_ORDER_NEST read byte 2 and byte 11 of that rendering, so insertion order is observed at both levels rather than inferred.
- **Recorded native envelope:** native-unsupported for ITEMS_MAP, ITEMS_CLS, TOJSON_MAP, TOJSON_NEST, ITEMS_NEST, ITEMS_ORDER_MAP, ITEMS_ORDER_CLS, TOJSON_ORDER_MAP, TOJSON_ORDER_NEST (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

The recorded lengths pin stock's rendering as compact and INSERTION-ordered at both levels, which is the property the guard exists to protect. Native cannot observe it through minijinja-Go's unordered AsMap seam.

### `mappingDualRender` — kept-over-decline

- **What it is:** mappingOrdered/mappingNative dual render in renderConstraint; orderedMapping, isOrderedMapping, hasMapping
- **Where:** internal/debaml/constraint_eval.go, internal/debaml/constraint_value.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #32–#44 (generic value_cmp dispatch, containment, ordered mappings, slicing).
- **Witness rows:** `LEN_MAP`, `LEN_CLS`, `CNT_MAP`, `CNT_CLS`, `LAST_CLS_VALUE`, `LAST_CLS_KEY`, `LAST_MAP_KEY`, `ITEMS_MAP`, `ITEMS_CLS`, `ITEMS_NEST`, `O2`, `O2b`, `MAP_SUBSCRIPT`, `CLS_FIELD`, `MAP_LIST`, `MAP_REVERSE_LIST`, `MAP_STRING`, `MAP_STRING_LEN`, `MAP_CONCAT`, `MAP_EQUALITY`, `MAP_NESTED`, `CLS_NESTED_LIST`, `MAP_LIST_ORDER`, `CLS_LIST_ORDER`, `MAP_REVERSE_ORDER`, `MAP_REVERSE_ORDER_B`, `ITEMS_ORDER_MAP`, `ITEMS_ORDER_CLS`, `MAP_STRING_ORDER`, `PG_IS_MAPPING`
- **Recorded stock envelope:** pass for LEN_MAP, LEN_CLS, CNT_MAP, CNT_CLS, ITEMS_MAP, ITEMS_CLS, ITEMS_NEST, O2, MAP_SUBSCRIPT, CLS_FIELD, MAP_LIST, MAP_REVERSE_LIST, MAP_STRING, MAP_STRING_LEN, MAP_CONCAT, MAP_EQUALITY, MAP_NESTED, CLS_NESTED_LIST, MAP_LIST_ORDER, CLS_LIST_ORDER, MAP_REVERSE_ORDER_B, ITEMS_ORDER_MAP, ITEMS_ORDER_CLS, MAP_STRING_ORDER, PG_IS_MAPPING; failed-check for O2b, MAP_REVERSE_ORDER; evaluator-error for LAST_CLS_VALUE, LAST_CLS_KEY, LAST_MAP_KEY (`invalid operation: cannot get last item from value`). MAP_REVERSE_ORDER is false because stock does NOT reverse a mapping — a Rust quirk PATCHES.md #43 records — and MAP_REVERSE_ORDER_B is its true half.
- **Recorded native envelope:** agreement — the same outcome stock recorded — for LEN_MAP, LEN_CLS, CNT_MAP, CNT_CLS, MAP_SUBSCRIPT, CLS_FIELD, MAP_NESTED, CLS_NESTED_LIST, PG_IS_MAPPING; native-unsupported for LAST_CLS_VALUE, LAST_CLS_KEY, LAST_MAP_KEY, ITEMS_MAP, ITEMS_CLS, ITEMS_NEST, O2, O2b, MAP_STRING, MAP_STRING_LEN, MAP_CONCAT, MAP_EQUALITY, MAP_LIST_ORDER, CLS_LIST_ORDER, MAP_REVERSE_ORDER, MAP_REVERSE_ORDER_B, ITEMS_ORDER_MAP, ITEMS_ORDER_CLS, MAP_STRING_ORDER (attributed to operatorShapeIsProven); native-unsupported for MAP_LIST, MAP_REVERSE_LIST (attributed to checkCallParity/subject-kind). The agreements are the rows that only READ a mapping; every decline ENUMERATES or RENDERS one.
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. It stays until EVERY proposed operation has both an ordered-native row and a negative foreign-map row.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

The rows split cleanly: reading a mapping by key or field agrees, while anything that ENUMERATES or RENDERS it declines. That is the boundary the dual render draws, now measured per operation rather than asserted.

### `guardForeignMapping` — kept-over-decline

- **What it is:** guardForeignMapping over list, join, first, map, select, reject, selectattr, rejectattr, groupby, chain, zip, unique, batch, slice, reverse, pprint, string, indent
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A for non-string keys
- **Fork capability that bears on it:** PATCHES.md #36/#37; the named gap 'mappings are keyed by string, not by value' is explicitly still open.
- **Witness rows:** `FMAP_LIST`, `FMAP_DICT`, `FMAP_NONSTRING_KEY`, `FM_LIST_LIT`, `FM_LIST_DICT`, `FM_JOIN_LIT`, `FM_JOIN_DICT`, `FM_FIRST_LIT`, `FM_FIRST_DICT`, `FM_MAP_LIT`, `FM_MAP_DICT`, `FM_SELECT_LIT`, `FM_SELECT_DICT`, `FM_REJECT_LIT`, `FM_REJECT_DICT`, `FM_SELECTATTR_LIT`, `FM_SELECTATTR_DICT`, `FM_REJECTATTR_LIT`, `FM_REJECTATTR_DICT`, `FM_GROUPBY_LIT`, `FM_GROUPBY_DICT`, `FM_CHAIN_LIT`, `FM_CHAIN_DICT`, `FM_ZIP_LIT`, `FM_ZIP_DICT`, `FM_UNIQUE_LIT`, `FM_UNIQUE_DICT`, `FM_BATCH_LIT`, `FM_BATCH_DICT`, `FM_SLICE_LIT`, `FM_SLICE_DICT`, `FM_REVERSE_LIT`, `FM_REVERSE_DICT`, `FM_PPRINT_LIT`, `FM_PPRINT_DICT`, `FM_STRING_LIT`, `FM_STRING_DICT`, `FM_INDENT_LIT`, `FM_INDENT_DICT`
- **Recorded stock envelope:** pass for FMAP_LIST, FMAP_DICT, FMAP_NONSTRING_KEY, FM_LIST_LIT, FM_LIST_DICT, FM_JOIN_LIT, FM_JOIN_DICT, FM_FIRST_LIT, FM_FIRST_DICT, FM_MAP_LIT, FM_MAP_DICT, FM_SELECT_LIT, FM_SELECT_DICT, FM_REJECT_LIT, FM_REJECT_DICT, FM_SELECTATTR_LIT, FM_SELECTATTR_DICT, FM_REJECTATTR_LIT, FM_REJECTATTR_DICT, FM_CHAIN_LIT, FM_CHAIN_DICT, FM_ZIP_LIT, FM_ZIP_DICT, FM_UNIQUE_LIT, FM_UNIQUE_DICT, FM_BATCH_LIT, FM_BATCH_DICT, FM_SLICE_LIT, FM_SLICE_DICT, FM_PPRINT_LIT, FM_PPRINT_DICT, FM_STRING_LIT, FM_STRING_DICT, FM_INDENT_LIT, FM_INDENT_DICT; failed-check for FM_GROUPBY_LIT, FM_GROUPBY_DICT, FM_REVERSE_LIT, FM_REVERSE_DICT. The failed-check rows are `reverse` and `groupby` in both shapes; that is itself the order observation, since stock does not reverse a mapping.
- **Recorded native envelope:** native-unsupported for FMAP_LIST, FMAP_DICT, FMAP_NONSTRING_KEY, FM_LIST_LIT, FM_LIST_DICT, FM_JOIN_LIT, FM_JOIN_DICT, FM_FIRST_LIT, FM_FIRST_DICT, FM_MAP_LIT, FM_MAP_DICT, FM_SELECT_LIT, FM_SELECT_DICT, FM_REJECT_LIT, FM_REJECT_DICT, FM_SELECTATTR_LIT, FM_SELECTATTR_DICT, FM_REJECTATTR_LIT, FM_REJECTATTR_DICT, FM_GROUPBY_LIT, FM_GROUPBY_DICT, FM_CHAIN_LIT, FM_CHAIN_DICT, FM_ZIP_LIT, FM_ZIP_DICT, FM_UNIQUE_LIT, FM_UNIQUE_DICT, FM_BATCH_LIT, FM_BATCH_DICT, FM_SLICE_LIT, FM_SLICE_DICT, FM_REVERSE_LIT, FM_REVERSE_DICT, FM_PPRINT_LIT, FM_PPRINT_DICT, FM_STRING_LIT, FM_STRING_DICT, FM_INDENT_LIT, FM_INDENT_DICT (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. FMAP_NONSTRING_KEY is the MANDATORY negative: it must stay declined until the fork's non-string-key gap closes, and a string-key proof may not be used to remove this guard.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

A mapping manufactured inside the expression is identical under both projections, so the representation-agreement check cannot see it at all.

### `rangeWithdrawal` — kept-over-decline

- **What it is:** `range` global withdrawal
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** P for small bounded ranges only
- **Fork capability that bears on it:** PATCHES.md #26/#41 (range arguments; range error class).
- **Witness rows:** `RANGE_LIST`, `RANGE_LAST`, `RANGE_STEP`
- **Subprocess witness:** `TestGuardLedgerHugeRangeIsIsolated`
- **Recorded stock envelope:** pass for RANGE_LIST, RANGE_LAST, RANGE_STEP. The oversized-boundary row is not cited here — it cannot be driven in-process and is recorded from an isolated subprocess instead.
- **Recorded native envelope:** native-unsupported for RANGE_LIST, RANGE_LAST (attributed to operatorShapeIsProven); native-unsupported for RANGE_STEP (attributed to exceedsExactIntegerRange). RANGE_STEP is refused one gate earlier than its siblings, by the numeric sublanguage, because `-1` is an arithmetic byte outside any bracket region.
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. Admitting `range` is a bulk admission, not a row-by-row one.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

RANGE_LAST is the round-5 escape shape — an out-of-range integer reaching an exact comparison — and stock decides it, which is exactly why the withdrawal is a cost rather than a no-op.

### `globalWithdrawals` — kept-over-decline

- **What it is:** `dict`, `namespace`, `debug` global withdrawals
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #49 (argument contract for every filter, test and function).
- **Witness rows:** `FMAP_DICT`, `DICT_ARITY`, `NAMESPACE_ATTR`, `DEBUG_CALL`, `FM_LIST_DICT`, `FM_JOIN_DICT`, `FM_FIRST_DICT`, `FM_MAP_DICT`, `FM_SELECT_DICT`, `FM_REJECT_DICT`, `FM_SELECTATTR_DICT`, `FM_REJECTATTR_DICT`, `FM_GROUPBY_DICT`, `FM_CHAIN_DICT`, `FM_ZIP_DICT`, `FM_UNIQUE_DICT`, `FM_BATCH_DICT`, `FM_SLICE_DICT`, `FM_REVERSE_DICT`, `FM_PPRINT_DICT`, `FM_STRING_DICT`, `FM_INDENT_DICT`
- **Recorded stock envelope:** pass for FMAP_DICT, NAMESPACE_ATTR, DEBUG_CALL, FM_LIST_DICT, FM_JOIN_DICT, FM_FIRST_DICT, FM_MAP_DICT, FM_SELECT_DICT, FM_REJECT_DICT, FM_SELECTATTR_DICT, FM_REJECTATTR_DICT, FM_CHAIN_DICT, FM_ZIP_DICT, FM_UNIQUE_DICT, FM_BATCH_DICT, FM_SLICE_DICT, FM_PPRINT_DICT, FM_STRING_DICT, FM_INDENT_DICT; failed-check for FM_GROUPBY_DICT, FM_REVERSE_DICT; evaluator-error for DICT_ARITY (`invalid operation`). DICT_ARITY is the shape the withdrawal was introduced for: stock's functions::dict raises where the port answered.
- **Recorded native envelope:** native-unsupported for FMAP_DICT, DICT_ARITY, NAMESPACE_ATTR, DEBUG_CALL, FM_LIST_DICT, FM_JOIN_DICT, FM_FIRST_DICT, FM_MAP_DICT, FM_SELECT_DICT, FM_REJECT_DICT, FM_SELECTATTR_DICT, FM_REJECTATTR_DICT, FM_GROUPBY_DICT, FM_CHAIN_DICT, FM_ZIP_DICT, FM_UNIQUE_DICT, FM_BATCH_DICT, FM_SLICE_DICT, FM_REVERSE_DICT, FM_PPRINT_DICT, FM_STRING_DICT, FM_INDENT_DICT (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. No global is opened by another global's proof; each callable AND its returned object surface needs an allowlist.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

DICT_ARITY records the exact divergence the withdrawal was introduced for: the port answered TRUE where stock raises.

### `operatorShapeIsProven` — kept-over-decline

- **What it is:** operatorShapeIsProven and the predParser family (term, valueTerm, fieldTerm, subscriptTerm, indexBound, edgeElementTerm)
- **Where:** internal/debaml/constraint_operator.go
- **Classification:** P per grammar family
- **Fork capability that bears on it:** PATCHES.md #33–#40 (comparison structure, total ordering, containment, truthiness, constant-folded `and`, slicing).
- **Witness rows:** `O1`, `O1b`, `O2`, `O2b`, `O3`, `O4`, `O5`, `O6`, `O7`, `O8`, `O8b`, `O9`, `O9b`, `O9c`, `O9d`, `O9e`
- **Recorded stock envelope:** pass for O1, O2, O3, O4, O5, O6, O7, O8, O9b, O9d, O9e; failed-check for O1b, O2b, O8b, O9, O9c
- **Recorded native envelope:** agreement — the same outcome stock recorded — for O8, O9b, O9d, O9e; native-unsupported for O1, O1b, O2, O2b, O3, O4, O5, O6, O7, O8b, O9, O9c (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. The whole parser cannot be deleted as one change, and each family owes its own rows.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

O8/O8b are the nested-postfix pair: the shadowed `name` is an int inside and a string outside, so a gate resolving against the root would admit a mixed-kind comparison stock answers FALSE. O9e records that a slice result's length IS decided natively, which the first pass of this ledger predicted wrongly and the harness corrected.

### `displayString` — kept-over-decline

- **What it is:** displayString custom ObjectWithString dispatch
- **Where:** internal/debaml/constraint_eval.go
- **Classification:** P
- **Fork capability that bears on it:** PATCHES.md #36 and value/value.go:1016/:1040 — the fork dispatches ObjectWithString from Value.String and Value.Repr.
- **Witness rows:** `O3`, `MAP_STRING`, `MAP_STRING_LEN`, `MAP_CONCAT`, `MAP_STRING_ORDER`
- **Recorded stock envelope:** pass for O3, MAP_STRING, MAP_STRING_LEN, MAP_CONCAT, MAP_STRING_ORDER
- **Recorded native envelope:** native-unsupported for O3, MAP_STRING, MAP_STRING_LEN, MAP_CONCAT, MAP_STRING_ORDER (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. Its rows are native declines, so it has NO green witness and the removal rule does not permit deleting it.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

The 16-byte length pins stock's mapping rendering as `{"b": 1, "a": 2}` in insertion order, which is what orderedMapping.ObjectString reproduces — but every consumer of that rendering (`string`, `regex_match`, `~`) is withdrawn or refused, so the dispatch cannot be witnessed as an agreement.

### `withdrawnBuiltinsTable` — kept-over-decline

- **What it is:** withdrawnBuiltins (per-name default-decline list)
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #46–#48 close the Unicode case/whitespace/punctuation classes at the engine, which is capability evidence and not per-callable proof.
- **Witness rows:** `N10`, `N11`, `PY_UPPER`, `WT_TITLE`, `WT_SORT`, `BS_REGEX`
- **Recorded stock envelope:** pass for PY_UPPER, WT_TITLE, WT_SORT, BS_REGEX; evaluator-error for N10 (`invalid operation: count cannot be 0`); N11 (`invalid operation: cannot convert number to usize`)
- **Recorded native envelope:** native-unsupported for N10, N11, PY_UPPER, WT_TITLE, WT_SORT (attributed to operatorShapeIsProven); native-unsupported for BS_REGEX (attributed to exceedsExactIntegerRange)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. The only acceptable widening unit is a named callable plus a finite, representative CFFI matrix covering argument conversion and every observable return/error class.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

A green happy-path row does not prove an entire callable; the content space of a string filter is not enumerable from this side.

### `installProfileGuardsTable` — kept-over-decline

- **What it is:** installProfileGuards wrapper application over every registered filter and test
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #49 (argument contract) and #44 (VM integer conversion at every operand position).
- **Witness rows:** `MAP_LIST`, `MAP_REVERSE_LIST`, `PG_SUM`, `PG_ABS`, `PG_IS_MAPPING`, `PG_DIVISIBLEBY`
- **Recorded stock envelope:** pass for MAP_LIST, MAP_REVERSE_LIST, PG_SUM, PG_ABS, PG_IS_MAPPING, PG_DIVISIBLEBY
- **Recorded native envelope:** agreement — the same outcome stock recorded — for PG_SUM, PG_ABS, PG_IS_MAPPING, PG_DIVISIBLEBY; native-unsupported for MAP_LIST, MAP_REVERSE_LIST (attributed to checkCallParity/subject-kind)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. The table is an inventory record, not one removable guard.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

The control rows exist so the table's COST is separable from its correctness: the wrapped callables still decide everything they used to decide.

### `checkCallParitySignatures` — kept-over-decline

- **What it is:** checkCallParity, provenSignatures, kindOf, countDefaultingFilters, coercingNumericArg
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #49/#50 (argument contract; filter and function type errors).
- **Witness rows:** `LEN_INT`, `LEN_BOOL`, `LEN_NULL`, `CNT_INT`, `CNT_BOOL`, `CNT_NULL`, `LAST_CLS_VALUE`, `LAST_CLS_KEY`, `LAST_MAP_KEY`, `MAP_LIST`, `MAP_REVERSE_LIST`, `MAP_LIST_ORDER`, `CLS_LIST_ORDER`, `MAP_REVERSE_ORDER`, `MAP_REVERSE_ORDER_B`
- **Recorded stock envelope:** pass for MAP_LIST, MAP_REVERSE_LIST, MAP_LIST_ORDER, CLS_LIST_ORDER, MAP_REVERSE_ORDER_B; failed-check for MAP_REVERSE_ORDER; evaluator-error for LEN_INT, CNT_INT (`invalid operation: cannot calculate length of value of type number`); LEN_BOOL, CNT_BOOL (`invalid operation: cannot calculate length of value of type bool`); LAST_CLS_VALUE, LAST_CLS_KEY, LAST_MAP_KEY (`invalid operation: cannot get last item from value`); no-checks for LEN_NULL, CNT_NULL. LEN_NULL and CNT_NULL are no-checks because the OPTIONAL coercion swallows the failure and the node becomes null, so the two levels are indistinguishable there.
- **Recorded native envelope:** native-unsupported for LEN_INT, LEN_BOOL, LEN_NULL, CNT_INT, CNT_BOOL, CNT_NULL, MAP_LIST, MAP_REVERSE_LIST (attributed to checkCallParity/subject-kind); native-unsupported for LAST_CLS_VALUE, LAST_CLS_KEY, LAST_MAP_KEY, MAP_LIST_ORDER, CLS_LIST_ORDER, MAP_REVERSE_ORDER, MAP_REVERSE_ORDER_B (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. It is ALSO the guard the slice's SOLE removal (lengthGuard) falls through to, so narrowing it would reopen that removal: TestRemovedLengthGuardIsSubsumedAtTheFilterSeam fails if its subject rule stops refusing a length-less subject. It is separately ONE OF THE TWO gates in front of the retained `last`-over-a-mapping wrapper — operatorShapeIsProven is the other, and BOTH must widen before a witness row could reach that seam — which TestLastOverMappingGuardPreEmptedByCheckCallParity asserts at the installed seam.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

Its subject-kind arm is what refuses `<number>|length` and `<mapping>|last` today. That is why the removed length/count guard could never fire, and why the retained `last` wrapper is unreachable — two different conclusions from one mechanism, and both are asserted against it directly rather than through an expression that some earlier gate might refuse first.

### `divisibleByNonIntegral` — kept-over-decline

- **What it is:** the non-integral branch of the `divisibleby` guard
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** A
- **Fork capability that bears on it:** not closed — the fork's TestDivisibleBy performs the test only when both operands pass AsInt.
- **Witness rows:** `N12`
- **Recorded stock envelope:** pass for N12
- **Recorded native envelope:** native-unsupported for N12 (attributed to divisibleByNonIntegral)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. It stays until the fork carries stock's exact f64 branch and rows prove it.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

A wrong boolean at perfectly ordinary magnitudes, which no magnitude bound can see.

### `pycompatUnknownMethod` — kept-over-decline

- **What it is:** the deliberately UNINSTALLED SetUnknownMethodCallback (pycompat)
- **Where:** internal/debaml/constraint_eval.go
- **Classification:** A
- **Fork capability that bears on it:** PATCHES.md #8 (method calls) and the fork's ./pycompat package — the hook exists and is not installed.
- **Witness rows:** `PY_FORMAT`, `PY_UPPER`
- **Recorded stock envelope:** pass for PY_FORMAT, PY_UPPER
- **Recorded native envelope:** native-unsupported for PY_FORMAT, PY_UPPER (attributed to operatorShapeIsProven)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. Any future method needs a method-specific CFFI matrix and a permanent deny list; the callback is not enabled in 7.2a.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

Installing it exposes method dispatch including unbounded string and container semantics.

### `hasMedia` — kept-unwitnessable

- **What it is:** hasMedia
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** U
- **Fork capability that bears on it:** not applicable — BAML's two media conversions disagree and evaluate_predicate takes only one of them.
- **Witness rows:** none — see the notes for why no in-process row can be constructed
- **Recorded stock envelope:** no observable reference exists
- **Recorded native envelope:** native-unsupported (media values are refused before conversion)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. A witness needs a media value that can reach a constraint on the native path, which schema.Bundle.ValidateOutput makes impossible today.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

No row can be constructed: every media output shape is rejected before parsing ("media is not usable as an output type"), so there is no serving-parity witness to record.

### `divisibleByZero` — kept-unwitnessable

- **What it is:** the `divisibleby(0)` guard
- **Where:** internal/debaml/constraint_profile.go
- **Classification:** U
- **Fork capability that bears on it:** not applicable — stock takes the process down before any envelope can be read.
- **Witness rows:** none — see the notes for why no in-process row can be constructed
- **Subprocess witness:** `TestGuardLedgerDivisibleByZeroIsUnobservable`
- **Recorded stock envelope:** process-fatal: stock BAML v0.223 aborts or hangs its CFFI process
- **Recorded native envelope:** native-unsupported (attributed to divisibleByZero)
- **Change this makes:** coverage-only
- **Rollback condition:** n/a — nothing removed. If a future BAML makes the expression survivable, TestGuardLedgerDivisibleByZeroIsUnobservable fails and the U classification can be revisited on evidence.
- **Deferral record:** https://github.com/invakid404/baml-rest/issues/583#issuecomment-5189834323

Recorded from an ISOLATED SUBPROCESS under a deadline, and classified as process-fatal. No boolean is ever fabricated for it.
