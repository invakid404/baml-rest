//go:build integration

package guardledger

// The rows, grouped by the ledger guard they are evidence for.
//
// Every StockCheck / StockAssert below is a RECORDING of stock BAML v0.223.0,
// produced by the recording mode (GUARD_LEDGER_RECORD=1) and re-asserted live on
// every run by TestGuardLedgerDifferential. Nothing here is a prediction.
//
// A row that native REFUSES pins NativeGuard: the guard the refusal is
// attributed to, read out of the error text. That pin is what a guard removal
// stands on — a guard is removable only when its rows already name a DIFFERENT,
// retained guard here, so removing it cannot change any envelope.

// guardRows is the whole witness corpus: the named §1 rows below, plus the
// per-filter foreign-mapping matrix in corpus_foreignmap_test.go. The order is
// deterministic because the rendered fixture is a pure function of it.
var guardRows = append(namedGuardRows, foreignMappingRows...)

var namedGuardRows = []guardRow{
	// -----------------------------------------------------------------------
	// N1–N7 — the numeric/operator/subscript family (exceedsExactIntegerRange,
	// numericParser, isProvablySmallNumber, bracket bounds, parsePow).
	// -----------------------------------------------------------------------
	// N1 / N1b were this guard's two DIRECT-COMPARISON witnesses, and Slice 7.2c-2
	// moved both from a native refusal to an AGREEMENT. Their expression is the
	// closed direct grammar `this OP <canonical i64>`, which is now decided by an
	// exact int64 comparison inside EvaluateConstraint (constraint_direct_i64.go)
	// rather than refused by the numeric whitelist — the totality repair the 7.2c
	// scope requires before a direct-int schema may be admitted.
	//
	// They are RETAINED as witnesses rather than deleted, and that is the point:
	// they are now the rows that record what the exact path decides, beside N2/N3/N4
	// (signed `%`, signed `//`, a value-model `**` base) and N6/N7/BS_REGEX, which
	// are outside the direct grammar and STILL refuse under the unchanged guard. A
	// reader comparing the two halves can see that the guard was narrowed by a
	// grammar rather than loosened by a magnitude.
	//
	// NativeGuard and Note are empty because the row no longer refuses; the harness
	// requires both to be set exactly when it does.
	{
		ID: "N1", Guards: []string{"numericProfile"}, Group: "bigint",
		// 2^53+1 against 2^53: two DISTINCT integers that are one float64. Stock's
		// exact integer core keeps them apart and answers false; native's exact
		// int64 comparison now reaches the same answer, where a float64 comparator
		// would say true.
		Expr:       "this == 9007199254740992",
		StockCheck: envFailedCheck, StockAssert: envAssertError,
	},
	{
		ID: "N1b", Guards: []string{"numericProfile"}, Group: "bigint",
		// The same value compared against ITSELF, so the pair is symmetric: the exact
		// path has to answer true here and false at N1, which no single-sided fix
		// could satisfy.
		Expr:       "this == 9007199254740993",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "N2", Guards: []string{"numericProfile"}, Group: "intneg7",
		Expr:       "this % 2 == 1",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "signed `%` is euclidean in stock and truncated in Go; the numeric sublanguage admits only non-negative integer literal operands, and `this` is not one.",
	},
	{
		ID: "N3", Guards: []string{"numericProfile"}, Group: "intneg7",
		Expr:       "this // 2 == -4",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "signed `//` is euclidean in stock and floored in Go; refused on the same rule as N2.",
	},
	{
		ID: "N4", Guards: []string{"numericProfile"}, Group: "int2",
		Expr:       "this ** 63 > 0",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "`**` over a non-literal base: the profile bounds a promoted power only for a manifest literal, so a value-model base is refused.",
	},
	{
		ID: "N5", Guards: []string{"numericProfile"}, Group: "list123",
		Expr:       "this[1] == 2",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		// A FRACTIONAL DIRECT SUBSCRIPT, which stock does not reject: `this[1.5]`
		// resolves to undefined and the comparison is simply false. (A fractional
		// SLICE bound is a different case, and stock errors on that one — see N11.)
		ID: "N6", Guards: []string{"numericProfile"}, Group: "list123",
		Expr:       "this[1.5] == 2",
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "a fractional index is refused structurally, before evaluation, because the bracket rule admits only integer literals; stock decides it false.",
	},
	{
		ID: "N7", Guards: []string{"numericProfile", "integerResultWrappers"}, Group: "int1",
		Expr:       `"9007199254740993"|int == "9007199254740992"|int`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "two integers MANUFACTURED past 2^53 inside the expression; stock's i128 `int` keeps them apart.",
	},

	// -----------------------------------------------------------------------
	// N8–N12 — guardIntegerResult / guardTestInput entry points.
	// -----------------------------------------------------------------------
	{
		ID: "N8", Guards: []string{"integerResultWrappers"}, Group: "fbig",
		Expr:       "this is even",
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "a FLOAT at 2^63 read through AsInt: Go's conversion is implementation-defined there and Rust's saturates, so the value-model magnitude bound refuses before the test runs.",
	},
	{
		ID: "N9", Guards: []string{"integerResultWrappers"}, Group: "strnum",
		Expr:       "this|int == 9007199254740993",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "an integer manufactured from a string subject; refused by the source-literal magnitude bound.",
	},
	{
		ID: "N10", Guards: []string{"integerResultWrappers", "withdrawnBuiltinsTable"}, Group: "list123",
		Expr:       "this|slice(0)|length == 1",
		StockCheck: envEvaluatorError,
		StockInner: "invalid operation: count cannot be 0", StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "",
	},
	{
		ID: "N11", Guards: []string{"integerResultWrappers", "withdrawnBuiltinsTable"}, Group: "list123",
		Expr:       "this|slice(1.5)|length == 1",
		StockCheck: envEvaluatorError,
		StockInner: "invalid operation: cannot convert number to usize", StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "N12", Guards: []string{"divisibleByNonIntegral"}, Group: "f15",
		Expr:       "this is divisibleby(0.5)",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "divisibleByNonIntegral",
		Note:        "stock has an f64 branch (1.5 % 0.5 == 0) that minijinja-Go's integral-only test lacks; the guard refuses rather than reimplement it.",
	},

	// -----------------------------------------------------------------------
	// lengthGuard — length/count, with a control row per kind.
	// -----------------------------------------------------------------------
	{
		ID: "LEN_INT", Guards: []string{"lengthGuard", "checkCallParitySignatures"}, Group: "int1",
		Expr:        "this|length == 0",
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot calculate length of value of type number",
		StockAssert: envEvaluatorError,
		NativeGuard: "checkCallParity/subject-kind",
	},
	{
		ID: "LEN_BOOL", Guards: []string{"lengthGuard", "checkCallParitySignatures"}, Group: "boolt",
		Expr:        "this|length == 0",
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot calculate length of value of type bool",
		StockAssert: envEvaluatorError,
		NativeGuard: "checkCallParity/subject-kind",
	},
	{
		ID: "LEN_NULL", Guards: []string{"lengthGuard", "checkCallParitySignatures"}, Group: "nullint",
		Expr:          "this|length == 0",
		StockCheck:    envNoChecks,
		AssertOmitted: "the predicate errors and the OPTIONAL coercion swallows it; the value becomes null and no check is emitted at either level.",
		NativeGuard:   "checkCallParity/subject-kind",
	},
	{
		ID: "LEN_STR", Guards: []string{"lengthGuard"}, Group: "strab",
		Expr:       "this|length == 3",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "LEN_LIST", Guards: []string{"lengthGuard"}, Group: "list123",
		Expr:       "this|length == 3",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "LEN_MAP", Guards: []string{"lengthGuard", "mappingDualRender"}, Group: "mapba",
		Expr:       "this|length == 2",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "LEN_CLS", Guards: []string{"lengthGuard", "mappingDualRender"}, Group: "probe",
		Expr:       "this|length == 2",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CNT_INT", Guards: []string{"lengthGuard", "checkCallParitySignatures"}, Group: "int1",
		Expr:        "this|count == 0",
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot calculate length of value of type number",
		StockAssert: envEvaluatorError,
		NativeGuard: "checkCallParity/subject-kind",
	},
	{
		ID: "CNT_STR", Guards: []string{"lengthGuard"}, Group: "strab",
		Expr:       "this|count == 3",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CNT_LIST", Guards: []string{"lengthGuard"}, Group: "list123",
		Expr:       "this|count == 3",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CNT_MAP", Guards: []string{"lengthGuard", "mappingDualRender"}, Group: "mapba",
		Expr:       "this|count == 2",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CNT_CLS", Guards: []string{"lengthGuard", "mappingDualRender"}, Group: "probe",
		Expr:       "this|count == 2",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CNT_BOOL", Guards: []string{"lengthGuard", "checkCallParitySignatures"}, Group: "boolt",
		Expr:        "this|count == 0",
		StockCheck:  envEvaluatorError,
		StockAssert: envEvaluatorError,
		StockInner:  "invalid operation: cannot calculate length of value of type bool",
		NativeGuard: "checkCallParity/subject-kind",
	},
	{
		ID: "CNT_NULL", Guards: []string{"lengthGuard", "checkCallParitySignatures"}, Group: "nullint",
		Expr:          "this|count == 0",
		StockCheck:    envNoChecks,
		AssertOmitted: "the predicate errors and the OPTIONAL coercion swallows it; the value becomes null and no check is emitted at either level, so the two levels are indistinguishable (see LEN_NULL).",
		NativeGuard:   "checkCallParity/subject-kind",
	},

	// -----------------------------------------------------------------------
	// lastMappingGuard — the stock meaning of `last` over a mapping, made
	// explicit from both sides (VALUE vs KEY vs error), plus a list control.
	// -----------------------------------------------------------------------
	{
		ID: "LAST_CLS_VALUE", Guards: []string{"lastMappingGuard", "checkCallParitySignatures", "mappingDualRender"}, Group: "probe",
		Expr:        `this|last == "x"`,
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot get last item from value",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "LAST_CLS_KEY", Guards: []string{"lastMappingGuard", "checkCallParitySignatures", "mappingDualRender"}, Group: "probe",
		Expr:        `this|last == "a"`,
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot get last item from value",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "LAST_MAP_KEY", Guards: []string{"lastMappingGuard", "checkCallParitySignatures", "mappingDualRender"}, Group: "mapba",
		Expr:        `this|last == "a"`,
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot get last item from value",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "LAST_LIST", Guards: []string{"lastMappingGuard"}, Group: "list123",
		Expr:       "this|last == 3",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "FIRST_LIST", Guards: []string{"lastMappingGuard"}, Group: "list123",
		Expr:       "this|first == 1",
		StockCheck: envPass, StockAssert: envPass,
	},

	// -----------------------------------------------------------------------
	// items / tojson mapping guards and containsMapping.
	// -----------------------------------------------------------------------
	{
		ID: "ITEMS_MAP", Guards: []string{"itemsTojsonMappingGuards", "mappingDualRender"}, Group: "mapba",
		Expr:       "this|items|list|length == 2",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`items` over a mapping is decided by stock and refused natively; the ordered/native projections cannot both be observed through minijinja-Go's unordered AsMap seam.",
	},
	{
		ID: "ITEMS_CLS", Guards: []string{"itemsTojsonMappingGuards", "mappingDualRender"}, Group: "probe",
		Expr:       "this|items|list|length == 2",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "same as ITEMS_MAP over a CLASS value, whose entry order is schema order rather than input order.",
	},
	{
		// The rendered document is observed through its LENGTH, not through a
		// quoted literal: BAML's attribute grammar rejects an escaped quote inside
		// a predicate outright, so the escaped-literal spelling cannot compile at
		// all. 13 is `{"b":1,"a":2}` — compact, and in INSERTION order.
		ID: "TOJSON_MAP", Guards: []string{"itemsTojsonMappingGuards"}, Group: "mapba",
		Expr:       "this|tojson|length == 13",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock renders the mapping to JSON in insertion order and decides a length; native refuses because `tojson` is withdrawn and the operator gate declines the chain first.",
	},
	{
		// 23 is `{"outer":{"b":1,"a":2}}`: the nested mapping keeps insertion order
		// at both levels.
		ID: "TOJSON_NEST", Guards: []string{"itemsTojsonMappingGuards"}, Group: "nestmap",
		Expr:       "this|tojson|length == 23",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a nested mapping rendered through `tojson`; stock decides a length, native refuses.",
	},
	{
		ID: "ITEMS_NEST", Guards: []string{"itemsTojsonMappingGuards", "mappingDualRender"}, Group: "nestmap",
		Expr:       "this|items|list|length == 1",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the outer mapping of a nested map/map value; stock decides, native refuses.",
	},

	// -----------------------------------------------------------------------
	// split hard withdrawal — the full lifecycle scope §1 asks for.
	// -----------------------------------------------------------------------
	{
		ID: "SPLIT_LIST", Guards: []string{"splitWithdrawal"}, Group: "strab",
		Expr:       `this|split(" ")|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "materialising the split result agrees; native still refuses because `split` is withdrawn and the operator gate declines the chain first.",
	},
	{
		ID: "SPLIT_ITERABLE", Guards: []string{"splitWithdrawal"}, Group: "strab",
		Expr:       `this|split(" ") is iterable`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock's split result IS iterable; the row exists so the lazy-iterator claim is observed rather than asserted.",
	},
	{
		// PARENTHESISED on purpose: BAML's expression grammar rejects a subscript
		// applied directly to a filter result, so the unparenthesised spelling
		// cannot be compiled at all — a fact about the attribute language rather
		// than about either engine.
		ID: "SPLIT_INDEX", Guards: []string{"splitWithdrawal"}, Group: "strab",
		Expr:       `(this|split(" "))[0] == "a"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock INDEXES its lazy split result and decides; native refuses because `split` is withdrawn and the operator gate declines the chain first.",
	},
	{
		// THE AUTHORITATIVE SPELLING scope §1 names, kept even though it can never
		// be driven: BAML parses a constraint attribute with minijinja's own
		// expression parser, and that parser refuses a subscript applied directly
		// to a filter result — the whole project fails to build with
		// `Error parsing jinja template: syntax error: unexpected input after
		// expression`. That refusal IS the observation, and it is proved
		// executably by TestGuardLedgerRejectedSourceSpellings rather than
		// asserted; SPLIT_INDEX above carries the accepted spelling.
		ID: "SPLIT_INDEX_BARE", Guards: []string{"splitWithdrawal"}, Group: "strab",
		Expr:                `this|split(" ")[0] == "a"`,
		AcceptedAlternative: `(this|split(" "))[0] == "a"`,
		StockCheck:          envSourceRejected,
		AssertOmitted:       "there is no generated method at either level: BAML refuses to compile the spelling, so the project carrying it does not build.",
		NativeGuard:         "operatorShapeIsProven",
	},
	{
		ID: "SPLIT_LENGTH", Guards: []string{"splitWithdrawal", "lengthGuard"}, Group: "strab",
		Expr:        `this|split(" ")|length == 2`,
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation: cannot calculate length of value of type iterator",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},

	// -----------------------------------------------------------------------
	// guardForeignMapping — an expression-created mapping, including the
	// MANDATORY non-string-key negative that must stay declined.
	// -----------------------------------------------------------------------
	{
		ID: "FMAP_LIST", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a mapping literal built INSIDE the expression: identical under both projections, so the representation-agreement check cannot see it and the operator gate refuses the literal outright.",
	},
	{
		ID: "FMAP_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the same shape via the `dict` global, which is withdrawn on its own terms as well.",
	},
	{
		ID: "FMAP_NONSTRING_KEY", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{1: "a"}|list|length == 1`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "MANDATORY NEGATIVE. The fork keys mappings by string, not by Value (PATCHES.md, named gap), so a non-string-key mapping stays declined however the string-keyed rows land.",
	},

	// -----------------------------------------------------------------------
	// range withdrawal.
	// -----------------------------------------------------------------------
	{
		ID: "RANGE_LIST", Guards: []string{"rangeWithdrawal"}, Group: "int1",
		Expr:       "range(3)|list|length == 3",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a small bounded range decided by stock; refused natively because `range` is withdrawn and no global callable parses in the predicate grammar.",
	},
	{
		ID: "RANGE_LAST", Guards: []string{"rangeWithdrawal"}, Group: "int1",
		Expr:       "range(3)|last == 2",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the round-5 escape shape (`range(...)|last` carrying an integer into an exact comparison); stock decides it, native refuses.",
	},
	{
		ID: "RANGE_STEP", Guards: []string{"rangeWithdrawal"}, Group: "int1",
		Expr:       "range(3, 0, -1)|list|length == 3",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "a negative-step range. It is refused one gate EARLIER than the other range rows: the `-1` argument is an arithmetic byte outside any bracket region, so the numeric sublanguage refuses the whole expression before the operator gate ever sees the global.",
	},

	// -----------------------------------------------------------------------
	// dict / namespace / debug global withdrawals.
	// -----------------------------------------------------------------------
	{
		ID: "DICT_ARITY", Guards: []string{"globalWithdrawals"}, Group: "int1",
		Expr:        "dict(1)|length == 0",
		StockCheck:  envEvaluatorError,
		StockInner:  "invalid operation",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "NAMESPACE_ATTR", Guards: []string{"globalWithdrawals"}, Group: "int1",
		Expr:       "namespace(x=1).x == 1",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock decides a namespace attribute; native refuses every global callable.",
	},
	{
		ID: "DEBUG_CALL", Guards: []string{"globalWithdrawals"}, Group: "int1",
		Expr:       "debug(this)|length > 0",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`debug`'s exact stock return is observed here rather than guessed; native refuses it as a global callable.",
	},

	// -----------------------------------------------------------------------
	// withdrawNonBAMLBuiltins — the five names BAML's feature set does not have.
	// -----------------------------------------------------------------------
	{
		ID: "WB_URLENCODE", Guards: []string{"withdrawNonBAMLBuiltins"}, Group: "strab",
		Expr:        `this|urlencode == "ab"`,
		StockCheck:  envEvaluatorError,
		StockInner:  "unknown filter: filter urlencode is unknown",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "WB_CONTAINING", Guards: []string{"withdrawNonBAMLBuiltins"}, Group: "strab",
		Expr:        `this is containing("a")`,
		StockCheck:  envEvaluatorError,
		StockInner:  "unknown test: test containing is unknown",
		StockAssert: envEvaluatorError,
		NativeGuard: "engine/unknown-name",
	},
	{
		ID: "WB_CYCLER", Guards: []string{"withdrawNonBAMLBuiltins"}, Group: "int1",
		Expr:        "cycler(1, 2) == 1",
		StockCheck:  envEvaluatorError,
		StockInner:  "unknown function: cycler is unknown",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "WB_JOINER", Guards: []string{"withdrawNonBAMLBuiltins"}, Group: "int1",
		Expr:        "joiner() == 1",
		StockCheck:  envEvaluatorError,
		StockInner:  "unknown function: joiner is unknown",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},
	{
		ID: "WB_LIPSUM", Guards: []string{"withdrawNonBAMLBuiltins"}, Group: "int1",
		Expr:        "lipsum() == 1",
		StockCheck:  envEvaluatorError,
		StockInner:  "unknown function: lipsum is unknown",
		StockAssert: envEvaluatorError,
		NativeGuard: "operatorShapeIsProven",
	},

	// -----------------------------------------------------------------------
	// O1–O9 — the operator gate's excluded grammar families.
	// -----------------------------------------------------------------------
	{
		ID: "O1", Guards: []string{"operatorShapeIsProven"}, Group: "int1",
		Expr:       `1 in "1"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock's containment STRINGIFIES a non-string needle for a string haystack; minijinja-Go's string arm takes only a string, so the two disagree and `in` is refused.",
	},
	{
		ID: "O1b", Guards: []string{"operatorShapeIsProven"}, Group: "int1",
		Expr:       `1 not in "1"`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the inverse of O1, so the family is pinned from both sides.",
	},
	{
		ID: "O2", Guards: []string{"operatorShapeIsProven", "mappingDualRender"}, Group: "mapba",
		Expr:       `"b" in this`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "membership over an ordered mapping: minijinja-Go's Contains type-switches on the concrete payload and has no object arm, so the ordered projection cannot answer it.",
	},
	{
		ID: "O2b", Guards: []string{"operatorShapeIsProven", "mappingDualRender"}, Group: "mapba",
		Expr:       `"z" in this`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the false representative of O2.",
	},
	{
		ID: "O3", Guards: []string{"operatorShapeIsProven", "displayString"}, Group: "int1",
		Expr:       `"a" ~ 1 == "a1"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`~` stringifies its operands, and number rendering is not proven identical across the two engines.",
	},
	{
		ID: "O4", Guards: []string{"operatorShapeIsProven"}, Group: "int1",
		Expr:       `("" or "fallback") == "fallback"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`or` selects on truthiness, which was never shown to agree over none/empty string/empty container.",
	},
	{
		ID: "O5", Guards: []string{"operatorShapeIsProven"}, Group: "int1",
		Expr:       "not false",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`not` is the same truthiness question as O4.",
	},
	{
		ID: "O6", Guards: []string{"operatorShapeIsProven"}, Group: "int1",
		Expr:       "(1 if true else 2) == 1",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the ternary selects on the same unproven truthiness.",
	},
	{
		ID: "O7", Guards: []string{"operatorShapeIsProven"}, Group: "int1",
		Expr:       "true == 1",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a SAME-VALUE mixed-kind comparison: stock's bool arm converts, and a mixed-kind comparison is refused rather than trusted per-value.",
	},
	{
		ID: "O8", Guards: []string{"operatorShapeIsProven"}, Group: "nest",
		Expr:       "this.rows[0].name == 7",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "O8b", Guards: []string{"operatorShapeIsProven"}, Group: "nest",
		Expr:       `this.a.name == "x"`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the SHADOW chain: `name` is an int inside and a string outside, so a gate that resolved against the root would admit a mixed-kind comparison. It is refused.",
	},
	{
		ID: "O9", Guards: []string{"operatorShapeIsProven"}, Group: "list123",
		Expr:       "this[9] == 1",
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "an index past the end is UNDEFINED in both engines, and undefined is not comparable, so the gate refuses rather than claim the element kind.",
	},
	{
		ID: "O9b", Guards: []string{"operatorShapeIsProven"}, Group: "list123",
		Expr:       "this[-1] == 3",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "O9c", Guards: []string{"operatorShapeIsProven"}, Group: "strhello",
		Expr:       `this[9] == "x"`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the string arm of O9.",
	},
	{
		ID: "O9d", Guards: []string{"operatorShapeIsProven"}, Group: "strhello",
		Expr:       `this[1] == "e"`,
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "O9e", Guards: []string{"operatorShapeIsProven"}, Group: "list123",
		Expr:       "this[1:]|length == 2",
		StockCheck: envPass, StockAssert: envPass,
	},

	// -----------------------------------------------------------------------
	// The mapping dual-render (mappingOrdered / mappingNative), one row per
	// operation scope §1 names, over a map AND a class.
	// -----------------------------------------------------------------------
	{
		ID: "MAP_SUBSCRIPT", Guards: []string{"mappingDualRender"}, Group: "mapba",
		Expr:       `this["b"] == 1`,
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CLS_FIELD", Guards: []string{"mappingDualRender"}, Group: "probe",
		Expr:       "this.b == 1",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "MAP_LIST", Guards: []string{"mappingDualRender", "checkCallParitySignatures", "installProfileGuardsTable"}, Group: "mapba",
		Expr:       "this|list|length == 2",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "checkCallParity/subject-kind",
		Note:        "`list` over a mapping enumerates its KEYS; the signature table admits only a sequence subject.",
	},
	{
		ID: "MAP_REVERSE_LIST", Guards: []string{"mappingDualRender", "checkCallParitySignatures", "installProfileGuardsTable"}, Group: "mapba",
		Expr:       "this|reverse|list|length == 2",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "checkCallParity/subject-kind",
		Note:        "`reverse` over a mapping is faithful to a Rust quirk (PATCHES.md #43); the signature table admits only a sequence subject.",
	},
	{
		// Same constraint as TOJSON_MAP: the rendered bytes are observed through
		// `first` rather than through an escaped literal the BAML attribute
		// grammar will not accept.
		ID: "MAP_STRING", Guards: []string{"mappingDualRender", "displayString"}, Group: "mapba",
		Expr:       `this|string|first == "{"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the FIRST rendered byte of an ordered mapping; stock decides it, native refuses because `string` is withdrawn.",
	},
	{
		// 16 is `{"b": 1, "a": 2}` — minijinja's own mapping rendering, in
		// insertion order, which is what [orderedMapping.ObjectString] reproduces.
		ID: "MAP_STRING_LEN", Guards: []string{"mappingDualRender", "displayString"}, Group: "mapba",
		Expr:       "this|string|length == 16",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the RENDERED byte count of an ordered mapping; stock decides it, native refuses because `string` is withdrawn.",
	},
	{
		ID: "MAP_CONCAT", Guards: []string{"mappingDualRender", "displayString"}, Group: "mapba",
		Expr:       `(this ~ "")|length == 16`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`~` over a mapping reaches the same rendering as MAP_STRING_LEN through an OPERATOR, where no wrapper can run — which is why the operator gate, not a filter guard, is what refuses it.",
	},
	{
		ID: "MAP_EQUALITY", Guards: []string{"mappingDualRender"}, Group: "mapba",
		Expr:       "this == this",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a container comparison; the gate admits only same-kind scalar operands.",
	},
	{
		ID: "MAP_NESTED", Guards: []string{"mappingDualRender"}, Group: "nestmap",
		Expr:       `this["outer"]["b"] == 1`,
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "CLS_NESTED_LIST", Guards: []string{"mappingDualRender"}, Group: "nest",
		Expr:       `this.a.tags[0] == "x"`,
		StockCheck: envPass, StockAssert: envPass,
	},

	// ORDER-OBSERVING rows. Every group whose value is a mapping declares its
	// entries b,a — deliberately NOT alphabetical — so an observation that reports
	// `a` first is reporting a SORTED enumeration rather than BAML's insertion
	// order. A length assertion cannot tell the two apart, which is why these
	// exist alongside the length rows rather than instead of them.
	{
		ID: "MAP_LIST_ORDER", Guards: []string{"mappingDualRender", "checkCallParitySignatures"}, Group: "mapba",
		Expr:       `this|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock enumerates the mapping's keys in INSERTION order, so the first is `b`; native refuses one gate earlier: `|first` over the `list` result needs a proven element kind, and a mapping has none.",
	},
	{
		ID: "CLS_LIST_ORDER", Guards: []string{"mappingDualRender", "checkCallParitySignatures"}, Group: "probe",
		Expr:       `this|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the same over a CLASS value, whose order is schema order rather than input order.",
	},
	{
		// `reverse` over a MAPPING is a no-op in stock, faithfully reproducing a
		// Rust quirk (PATCHES.md #43: a map enumerates with a double-ended
		// iterator and `Value::reverse` re-boxes it without reversing). So the
		// first key is STILL `b`, not `a` — the pair of rows below records that
		// from both sides rather than assuming reversal happened.
		ID: "MAP_REVERSE_ORDER", Guards: []string{"mappingDualRender", "checkCallParitySignatures"}, Group: "mapba",
		Expr:       `this|reverse|list|first == "a"`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "stock does NOT reverse a mapping, so `a` is not first; native refuses one gate earlier, at the operator gate, regardless.",
	},
	{
		ID: "MAP_REVERSE_ORDER_B", Guards: []string{"mappingDualRender", "checkCallParitySignatures"}, Group: "mapba",
		Expr:       `this|reverse|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the true half of MAP_REVERSE_ORDER: the enumeration is unchanged, so `b` is still first.",
	},
	{
		ID: "ITEMS_ORDER_MAP", Guards: []string{"itemsTojsonMappingGuards", "mappingDualRender"}, Group: "mapba",
		Expr:       `this|items|first|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the FIRST key `items` yields, which is `b` under insertion order and `a` under a sorted one.",
	},
	{
		ID: "ITEMS_ORDER_CLS", Guards: []string{"itemsTojsonMappingGuards", "mappingDualRender"}, Group: "probe",
		Expr:       `this|items|first|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the same over a class value.",
	},
	{
		ID: "TOJSON_ORDER_MAP", Guards: []string{"itemsTojsonMappingGuards"}, Group: "mapba",
		Expr:       `(this|tojson)[2] == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "byte 2 of the rendered JSON is the first KEY: `b` in insertion order, `a` if the keys were sorted.",
	},
	{
		ID: "TOJSON_ORDER_NEST", Guards: []string{"itemsTojsonMappingGuards"}, Group: "nestmap",
		Expr:       `(this|tojson)[11] == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the same observation one level DOWN: in `{\"outer\":{\"b\":1,\"a\":2}}` byte 11 is the first key of the INNER mapping, so nested order is observed rather than assumed from the outer one.",
	},
	{
		ID: "MAP_STRING_ORDER", Guards: []string{"mappingDualRender", "displayString"}, Group: "mapba",
		Expr:       `(this|string)[2] == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "the same for the DISPLAY rendering, which is the one [orderedMapping.ObjectString] reproduces.",
	},

	// -----------------------------------------------------------------------
	// pycompat / unknown-method — kept A, witnessed so the cost is measured.
	// -----------------------------------------------------------------------
	{
		ID: "PY_FORMAT", Guards: []string{"pycompatUnknownMethod"}, Group: "int1",
		Expr:       `"{:,}".format(1234567) == "1,234,567"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "BAML installs minijinja-contrib's pycompat callback; the native environment deliberately does not, so every method call is refused.",
	},
	{
		ID: "PY_UPPER", Guards: []string{"pycompatUnknownMethod", "withdrawnBuiltinsTable"}, Group: "strab",
		Expr:       `this.upper() == "A B"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a pycompat STRING method, whose content-sensitive Unicode behaviour is the reason the callback stays uninstalled.",
	},

	// -----------------------------------------------------------------------
	// The two broad default-decline tables — witnessed, not removed.
	// -----------------------------------------------------------------------
	{
		ID: "WT_TITLE", Guards: []string{"withdrawnBuiltinsTable"}, Group: "strab",
		Expr:       `this|title == "A B"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "a content-sensitive string transform: Rust's punctuation classes are not Go's, which is why the whole table stays default-decline.",
	},
	{
		ID: "WT_SORT", Guards: []string{"withdrawnBuiltinsTable"}, Group: "list123",
		Expr:       "this|sort|first == 1",
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "an ordering transform over elements; withdrawn on the same terms.",
	},
	{
		ID: "PG_SUM", Guards: []string{"installProfileGuardsTable"}, Group: "list123",
		Expr:       "this|sum == 6",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "PG_ABS", Guards: []string{"installProfileGuardsTable"}, Group: "intneg7",
		Expr:       "this|abs == 7",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "PG_IS_MAPPING", Guards: []string{"installProfileGuardsTable", "mappingDualRender"}, Group: "mapba",
		Expr:       "this is mapping",
		StockCheck: envPass, StockAssert: envPass,
	},
	{
		ID: "PG_DIVISIBLEBY", Guards: []string{"installProfileGuardsTable"}, Group: "int2",
		Expr:       "this is divisibleby(2)",
		StockCheck: envPass, StockAssert: envPass,
	},

	// -----------------------------------------------------------------------
	// SOURCE BYTES. BAML's attribute lexer DOUBLES backslashes, so the row's
	// Expr and the expression stock evaluates are different byte strings. The
	// native leg is fed the RETAINED bytes; the differential asserts stock
	// reported exactly those, which is what makes the distinction observable
	// rather than assumed.
	// -----------------------------------------------------------------------
	{
		ID: "BS_REGEX", Guards: []string{"numericProfile", "withdrawnBuiltinsTable"}, Group: "strhello",
		Expr:       `this|regex_match("\d") == false`,
		Retained:   `this|regex_match("\\d") == false`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "exceedsExactIntegerRange",
		Note:        "refused by the NUMERIC gate rather than by the withdrawal: a backslash inside a string literal makes the bracket-region scan refuse to say where the literal ends, so the whole expression is declined before `regex_match` is reached. The row is kept for the SOURCE-BYTE comparison it pins.",
	},
}
