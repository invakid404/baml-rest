//go:build integration

package guardledger

// The FOREIGN-MAPPING matrix: one row per filter [guardForeignMapping] wraps,
// over a mapping the value model did NOT build.
//
// Scope §1 asks for two shapes per candidate filter — a `{ "b": 1, "a": 2 }`
// literal and a `dict(b=1, a=2)` global — because those are the two ways an
// expression can manufacture a mapping, and neither is visible to the
// representation-agreement check: a foreign mapping is identical under both
// projections, so rendering twice cannot disagree about it.
//
// Where the filter's result can carry the enumeration, the expression observes
// ORDER rather than only a count: the literal declares `b` before `a`, so a
// sorted enumeration and an insertion-ordered one give different answers. Where
// it cannot (a partitioning or rendering filter), the row observes the shape the
// filter does produce and says so.
//
// Every row here is a KEPT guard's witness. The mandatory NEGATIVE that must stay
// declined whatever these record — a non-string-key mapping — is
// FMAP_NONSTRING_KEY in corpus_test.go.

var foreignMappingRows = []guardRow{
	{
		ID: "FM_LIST_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`list` over a literal mapping: materialises the mapping's keys; the FIRST one is `b` in insertion order and `a` in a sorted one.",
	},
	{
		ID: "FM_LIST_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`list` over a `dict(...)` global mapping: materialises the mapping's keys; the FIRST one is `b` in insertion order and `a` in a sorted one.",
	},
	{
		ID: "FM_JOIN_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|join(",") == "b,a"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`join` over a literal mapping: renders the keys in order, so the whole string distinguishes the two enumerations.",
	},
	{
		ID: "FM_JOIN_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|join(",") == "b,a"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`join` over a `dict(...)` global mapping: renders the keys in order, so the whole string distinguishes the two enumerations.",
	},
	{
		ID: "FM_FIRST_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`first` over a literal mapping: the first key reached.",
	},
	{
		ID: "FM_FIRST_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`first` over a `dict(...)` global mapping: the first key reached.",
	},
	{
		ID: "FM_MAP_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|map("upper")|first == "B"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`map` over a literal mapping: maps over the keys; the first result carries the order.",
	},
	{
		ID: "FM_MAP_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|map("upper")|first == "B"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`map` over a `dict(...)` global mapping: maps over the keys; the first result carries the order.",
	},
	{
		ID: "FM_SELECT_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|select("string")|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`select` over a literal mapping: selects over the keys, preserving order.",
	},
	{
		ID: "FM_SELECT_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|select("string")|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`select` over a `dict(...)` global mapping: selects over the keys, preserving order.",
	},
	{
		ID: "FM_REJECT_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|reject("number")|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`reject` over a literal mapping: rejects over the keys, preserving order.",
	},
	{
		ID: "FM_REJECT_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|reject("number")|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`reject` over a `dict(...)` global mapping: rejects over the keys, preserving order.",
	},
	{
		ID: "FM_SELECTATTR_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|selectattr("q")|list|length == 0`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`selectattr` over a literal mapping: attribute selection over the keys.",
	},
	{
		ID: "FM_SELECTATTR_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|selectattr("q")|list|length == 0`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`selectattr` over a `dict(...)` global mapping: attribute selection over the keys.",
	},
	{
		ID: "FM_REJECTATTR_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|rejectattr("q")|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`rejectattr` over a literal mapping: attribute rejection, preserving order.",
	},
	{
		ID: "FM_REJECTATTR_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|rejectattr("q")|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`rejectattr` over a `dict(...)` global mapping: attribute rejection, preserving order.",
	},
	{
		ID: "FM_GROUPBY_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|groupby("q")|list|length == 0`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`groupby` over a literal mapping: grouping over the keys yields a non-empty result, so the zero-length assertion is false — recorded as stock decided it.",
	},
	{
		ID: "FM_GROUPBY_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|groupby("q")|list|length == 0`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`groupby` over a `dict(...)` global mapping: grouping over the keys yields a non-empty result, so the zero-length assertion is false — recorded as stock decided it.",
	},
	{
		ID: "FM_CHAIN_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|chain([])|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`chain` over a literal mapping: chaining preserves the leading enumeration.",
	},
	{
		ID: "FM_CHAIN_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|chain([])|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`chain` over a `dict(...)` global mapping: chaining preserves the leading enumeration.",
	},
	{
		ID: "FM_ZIP_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|zip([1, 2])|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`zip` over a literal mapping: pairs the keys positionally, so its shape depends on the enumeration.",
	},
	{
		ID: "FM_ZIP_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|zip([1, 2])|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`zip` over a `dict(...)` global mapping: pairs the keys positionally, so its shape depends on the enumeration.",
	},
	{
		ID: "FM_UNIQUE_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|unique|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`unique` over a literal mapping: de-duplication preserves first-seen order.",
	},
	{
		ID: "FM_UNIQUE_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|unique|list|first == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`unique` over a `dict(...)` global mapping: de-duplication preserves first-seen order.",
	},
	{
		ID: "FM_BATCH_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|batch(2)|list|length == 1`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`batch` over a literal mapping: batching partitions the enumeration.",
	},
	{
		ID: "FM_BATCH_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|batch(2)|list|length == 1`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`batch` over a `dict(...)` global mapping: batching partitions the enumeration.",
	},
	{
		ID: "FM_SLICE_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|slice(2)|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`slice` over a literal mapping: slicing partitions the enumeration.",
	},
	{
		ID: "FM_SLICE_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|slice(2)|list|length == 2`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`slice` over a `dict(...)` global mapping: slicing partitions the enumeration.",
	},
	{
		ID: "FM_REVERSE_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|reverse|list|first == "a"`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`reverse` over a literal mapping: reversal is a NO-OP over a mapping in stock (PATCHES.md #43 records the Rust quirk), so `a` is not first and this row is false — which is itself the order observation.",
	},
	{
		ID: "FM_REVERSE_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|reverse|list|first == "a"`,
		StockCheck: envFailedCheck, StockAssert: envAssertError,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`reverse` over a `dict(...)` global mapping: reversal is a NO-OP over a mapping in stock (PATCHES.md #43 records the Rust quirk), so `a` is not first and this row is false — which is itself the order observation.",
	},
	{
		ID: "FM_PPRINT_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|pprint|length > 0`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`pprint` over a literal mapping: pretty-printing renders the mapping.",
	},
	{
		ID: "FM_PPRINT_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|pprint|length > 0`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`pprint` over a `dict(...)` global mapping: pretty-printing renders the mapping.",
	},
	{
		ID: "FM_STRING_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `({"b": 1, "a": 2}|string)[2] == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`string` over a literal mapping: the display rendering carries the order in its bytes.",
	},
	{
		ID: "FM_STRING_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `(dict(b=1, a=2)|string)[2] == "b"`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`string` over a `dict(...)` global mapping: the display rendering carries the order in its bytes.",
	},
	{
		ID: "FM_INDENT_LIT", Guards: []string{"guardForeignMapping"}, Group: "int1",
		Expr:       `{"b": 1, "a": 2}|indent(2)|length > 0`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`indent` over a literal mapping: indentation renders the mapping.",
	},
	{
		ID: "FM_INDENT_DICT", Guards: []string{"guardForeignMapping", "globalWithdrawals"}, Group: "int1",
		Expr:       `dict(b=1, a=2)|indent(2)|length > 0`,
		StockCheck: envPass, StockAssert: envPass,
		NativeGuard: "operatorShapeIsProven",
		Note:        "`indent` over a `dict(...)` global mapping: indentation renders the mapping.",
	},
}
