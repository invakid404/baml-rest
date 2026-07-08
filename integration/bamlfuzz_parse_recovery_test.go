//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/debaml"
)

// parseRecoveryCorpusDir is the in-tree JSONish recovery corpus. The
// bamlfuzz package lives under adapters/common; package integration reaches
// it via a repo-root-relative path, matching the dynamic/static corpora.
const parseRecoveryCorpusDir = "../adapters/common/codegen/testdata/bamlfuzz/parse_recovery"

// parseRecoveryArtifactDir is where parse-diff failure envelopes land on
// failure or with BAMLFUZZ_KEEP_ARTIFACTS=1. Gitignored like the other
// bamlfuzz oracle artifact dirs.
const parseRecoveryArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/parse_recovery/_artifacts"

// bamlDynamicParser adapts dynclient's dynamic parse to the bamlfuzz.Parser
// interface so the differential harness can drive BAML as the oracle. It
// covers BOTH final parse and parse-stream: a Stream=true request drives
// BAML's parse-stream (partial) path over the accumulated prefix, normalized
// into the SAME flattened shape a final parse produces. This exposes the
// direct parse-stream oracle the streaming differential needs — BAML remains
// the reference the native candidate must reproduce.
type bamlDynamicParser struct {
	dyn *dynclient.Client
}

// Name identifies the BAML oracle leg in diff output and envelopes.
func (p bamlDynamicParser) Name() string { return "baml_dynamic" }

// Parse drives dynclient.DynamicParse for a final parse, or — when
// req.Stream is true — BAML's parse-stream (partial) path over the same
// raw text. The returned JSON is the flattened, absent-optional-injected,
// order-normalized payload the dynamic endpoints expose (identical shape
// for final and stream), exactly what a native parser must reproduce. BAML
// may reject an early streaming prefix; that surfaces as a real parse error
// here and the comparator records error parity.
func (p bamlDynamicParser) Parse(ctx context.Context, req bamlfuzz.ParseRequest) (bamlfuzz.ParseResult, error) {
	lowered, err := bamlfuzz.LowerToDynamicSchema(req.Schema)
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	preserve := req.PreserveSchemaOrder
	resp, err := p.dyn.DynamicParse(ctx, dynclient.ParseRequest{
		Raw:                 req.Raw,
		OutputSchema:        &lowered,
		PreserveSchemaOrder: &preserve,
		Stream:              req.Stream,
	})
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	return bamlfuzz.ParseResult{JSON: append(json.RawMessage(nil), resp.Data...)}, nil
}

// nativeDeBAMLParser adapts the bounded native de-BAML parser
// (internal/debaml.Parse) to the bamlfuzz.Parser interface so the
// differential harness can diff the native candidate against the BAML
// oracle. It lowers the FuzzSchema with the SAME LowerToDynamicSchema
// helper the BAML leg uses, calls the native parser, maps
// ErrDeBAMLParseUnsupported to ErrParserUnavailable (so the comparator
// records a fallback skip rather than spurious drift), and runs the
// identical absent-optional-injection + order/sort normalization
// dynclient's parse-only path applies after a (already-flattened) parse —
// so a CLAIMED native result is directly comparable to BAML's. A claimed
// native parse ERROR is surfaced unchanged so the comparator checks
// error parity.
type nativeDeBAMLParser struct{}

// Name identifies the native candidate leg in diff output and envelopes.
func (nativeDeBAMLParser) Name() string { return "native_debaml" }

// Parse drives internal/debaml.Parse for both a final parse and (M4b) a
// STREAMING parse, normalizing its flattened output exactly like dynclient's
// parse-only path. A Stream=true request now routes to the native streaming path,
// which CLAIMS the smallest useful partial surface (jsonish recovery + class
// null-filling, no stream annotations / StreamState) and DECLINES everything else
// with ErrDeBAMLParseUnsupported — mapped to ErrParserUnavailable below, so an
// unclaimed prefix falls back (SkippedNative) and BAML parse-stream stands alone.
// A claimed prefix is held to BAML byte-exact by the per-prefix differential.
func (nativeDeBAMLParser) Parse(ctx context.Context, req bamlfuzz.ParseRequest) (bamlfuzz.ParseResult, error) {
	lowered, err := bamlfuzz.LowerToDynamicSchema(req.Schema)
	if err != nil {
		// A schema bamlfuzz cannot lower onto the dynamic path is out of the
		// native parser's scope; decline so the comparator falls back rather
		// than flag spurious drift (the BAML leg lowers via the same helper,
		// so it declines/errors symmetrically).
		return bamlfuzz.ParseResult{}, bamlfuzz.ErrParserUnavailable
	}
	res, err := debaml.Parse(ctx, bamlutils.DeBAMLParseRequest{
		Raw:          req.Raw,
		OutputSchema: &lowered,
		Stream:       req.Stream,
	})
	if err != nil {
		if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			// Native declined -> comparator skips the native leg (fallback).
			return bamlfuzz.ParseResult{}, bamlfuzz.ErrParserUnavailable
		}
		// Claimed native parse failure -> surfaced so the comparator checks
		// success/error parity against BAML.
		return bamlfuzz.ParseResult{}, err
	}

	// Post-process the already-flattened native JSON exactly like
	// dynclient.DynamicParse does after FlattenDynamicOutput (which is a
	// no-op here — the native parser never emits a DynamicProperties
	// envelope): inject absent optionals, then order by schema (preserve)
	// or sort alphabetically.
	out := append(json.RawMessage(nil), res.JSON...)
	out, err = bamlutils.InjectAbsentOptionals(out, &lowered)
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	if req.PreserveSchemaOrder {
		out, err = bamlutils.ReorderDynamicOutputBySchema(out, &lowered)
	} else {
		out, err = bamlutils.SortDynamicOutput(out)
	}
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	return bamlfuzz.ParseResult{JSON: out}, nil
}

// perCallOracleTimeout is the bounded budget a SINGLE live BAML oracle call
// gets. The final-parse and dynamic parse_diff legs already wrap their one
// live call in context.WithTimeout(..., 30s); the streaming leg fans one live
// call out per accumulated prefix, so it applies this SAME per-call budget to
// each call (via perCallTimeoutParser) rather than sharing one loop-wide
// deadline across all of them.
const perCallOracleTimeout = 30 * time.Second

// perCallTimeoutParser wraps a bamlfuzz.Parser so every Parse call derives its
// own bounded deadline from the incoming context instead of drawing down a
// single shared budget. DiffParserPrefixes drives the BAML oracle once per
// accumulated prefix (up to 7 per streaming fixture) through one ctx; without
// a per-call bound those live calls would all share one deadline and a slow
// oracle call under CI load could starve the later prefixes. This mirrors the
// per-call WithTimeout the single-call final-parse leg already uses. Only the
// live BAML leg is wrapped; the native candidate declines every stream prefix
// without a live call, so its disposition (fallback) is unaffected.
type perCallTimeoutParser struct {
	inner   bamlfuzz.Parser
	timeout time.Duration
}

// Name passes through the wrapped parser's identity so diff output and
// failure envelopes are unchanged.
func (p perCallTimeoutParser) Name() string { return p.inner.Name() }

// Parse bounds a single delegated call to p.timeout, derived from ctx so an
// outer cancellation still wins, then forwards to the wrapped parser.
func (p perCallTimeoutParser) Parse(ctx context.Context, req bamlfuzz.ParseRequest) (bamlfuzz.ParseResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	return p.inner.Parse(ctx, req)
}

// parseRecoveryNativeClaim pins the native-parser cut-line per corpus case
// (by Name): true means the native parser is expected to CLAIM the final
// parse — produce JSON, or a claimed parse error that matches BAML's error
// — and false means it FALLS BACK to BAML (ErrDeBAMLParseUnsupported ->
// SkippedNative). Strict / markdown-fenced / prose-extracted JSON is
// claimed; the conservative M2a fixing subset (trailing commas, unquoted
// keys, single quotes, mixed jsonish, and the nested/literal/prose variants)
// is now also claimed. Cases the native parser DECLINES — repairs outside
// the fixing subset (comments), and "couldn't find/complete a candidate"
// (an unterminated/truncated structure, which BAML recovers but M2a defers)
// — stay fallback. Cases absent from the map (the streaming-only ones)
// carry no final leg and are not asserted.
//
// MAP PARITY-DECLINE: internal/debaml.Parse now declines EVERY map-containing
// schema (checkNoMap) because native coerceMap emits entries in parsed input-key
// order, which cannot be proven equal to BAML's observable map-key order under
// preserve-order. So any map-containing case FALLS BACK regardless of its pinned
// value here — runParseRecoveryCase forces the expected disposition to fallback
// via parseRecoverySchemaContainsMap. The map/enum/class map entries below keep
// their pre-decline (claimed) pins as documentation of the coerceMap behavior a
// future PR re-enables behind a proven map-order fixture; the override — not a
// mass pin flip — is what keeps this differential green today.
var parseRecoveryNativeClaim = map[string]bool{
	"markdown_fence_object":    true,
	"prose_before_after_json":  true,
	"quoted_brace_prose":       true,
	"fenced_backticks_in_json": true,
	"strict_list_optional":     true,
	"strict_literal_enum":      true,
	// Truncated mid-value: an opening brace that never closes. Native finds
	// no cleanly-claimable candidate and DECLINES (BAML closes it at EOF and
	// recovers a partial value, which M2a defers) — so it falls back, not a
	// claimed error.
	"truncated_final_error": false,
	// M2a fixing-parser subset: now native-claimed and diff-green vs BAML.
	"trailing_commas": true,
	"unquoted_keys":   true,
	"single_quotes":   true,
	"mixed_jsonish":   true,
	// M2a differential-guarded additions.
	"unquoted_keys_literals":        true,
	"single_quotes_nested":          true,
	"prose_jsonish_unquoted_single": true,
	// Nested trailing commas with QUOTED values — parity-safe, claimed.
	"nested_trailing_commas_quoted": true,
	// Nested trailing commas around UNQUOTED NUMBER values: BAML greedily
	// consumes the comma and errors on coercion; native declines (fallback)
	// rather than claim a cleanly-parsed object it can't match.
	"trailing_commas_nested_object_array": false,
	// Leading / repeated / stray commas — also part of the claimed subset
	// (BAML's object/array states ignore stray commas while waiting for
	// content), so native claims them too.
	"leading_comma_object":   true,
	"repeated_commas_object": true,
	"array_stray_commas":     true,
	// Deferred repair (comments) — pinned fallback until claimed.
	"comments_fallback": false,
	// M2b native MAPS: clean maps are claimed and diff-green vs BAML —
	// object input, exact key match (string / enum / string-literal /
	// string-literal-union), in-scope values, emitted in INPUT key order.
	"strict_map_string_int":        true,
	"map_enum_keys":                true,
	"map_string_class_values":      true,
	"map_literal_union_keys_exact": true,
	// M2b decline set: every map BAML would return as partial/scored — a
	// skipped bad key/value (MapKeyParseError / MapValueParseError) or an
	// unterminated/incomplete map (M2a defers). Native declines (fallback)
	// rather than claim a clean result where BAML carries flags or skips
	// entries. (map_fuzzy_enum_key flipped to claimed in Mcoerce-a.)
	"map_bad_enum_key": false,
	// Mcoerce-c native MAPS flip: an OBJECT map value can't coerce to int
	// (error_unexpected_type), a PROVEN MapValueParseError, so BAML skips just
	// that entry and native now reproduces the partial map {"a":1} — CLAIMED.
	"map_bad_value_type":     true,
	"map_partial_incomplete": false,
	// Mcoerce-a native match_string parity: enum / string-literal / class
	// field-key / map-key fuzzy matching (case / accent+ligature fold /
	// punctuation strip / case-insensitive / substring) is reproduced
	// byte-exact, so fuzzy matches BAML accepts are now CLAIMED. A non-union
	// substring TIE is a CLAIMED error (StrMatchOneFromMany). Multi-success
	// unions stay deferred to M3.
	"map_fuzzy_enum_key":                     true,
	"enum_fuzzy_case_punct_accent":           true,
	"literal_fuzzy_string_non_union":         true,
	"class_fuzzy_field_key":                  true,
	"map_fuzzy_literal_key":                  true,
	"match_string_ambiguous_substring_error": true,
	// Mcoerce-a F1 / M3a: a NULLABLE flat class union scores its null arm
	// (DefaultButHadValue=110) for non-null input. M3a computes the arm score, so
	// native claims the non-null arm when it scores < 110 and claims NULL when it
	// scores > 110 (the 115-extra-key Book arm loses to null). Both are now claimed.
	"nullable_class_union_clean_claimed":   true,
	"nullable_class_union_extra_keys_null": true,
	// M2c native SCORE-FREE SIMPLE UNIONS: claimed when native can PROVE BAML
	// also resolves to exactly one clean zero-score winner.
	//
	// Claimed: homogeneous exact-literal unions (string arms proven pairwise
	// match-disjoint; bool/int by value equality), a flat disjoint-key class
	// union (input == one variant's full field set), and the nullable
	// multi-union null fast path.
	"literal_union_string_exact": true,
	"literal_union_bool_exact":   true,
	"literal_union_int_exact":    true,
	"class_union_single_shape":   true,
	"nullable_multi_union_null":  true,
	// M3b SCALAR-LEAF unions: checkSupportedUnionShape now admits any union whose
	// arms are all fully-modeled non-composite leaves (primitive int/float/bool/
	// string, literal, enum, + hoisted null), and coerceUnionSafeMulti scores
	// each arm through the same per-kind coercer BAML uses, applying the early
	// first-score-0 rule + pick_best. Bare-primitive, mixed-scalar, non-disjoint
	// string-literal, and (post-flatten) nested-scalar unions are now CLAIMED —
	// their scored winner is reproduced exactly; a value-equal int/float, an
	// order-reversal, and an enum arm are all pinned.
	"string_int_union_numeric_string": true,
	"int_float_union_number":          true,
	"literal_union_fuzzy_string":      true,
	"nested_union":                    true,
	"int_string_union_reversal":       true,
	"float_int_union_reversal":        true,
	"bool_string_union_string_wins":   true,
	"enum_string_union_exact":         true,
	// M3c CLAIMED — CLASS + MIXED literal/enum/class union scoring. The union gate
	// now admits class arms (required flat-leaf fields, single-field allowed,
	// overlapping keys allowed) and mixes of scalar/literal/enum/class, and
	// coerceUnionSafeMulti resolves them TWO-PHASE like BAML: a phase-1 try_cast pass
	// (tryCastClass ports Class::try_cast — a STRICT exact-key object cast) and, only
	// when NO arm try_casts, a phase-2 lenient coerce + array_helper::pick_best (with
	// the class / scalar-vs-composite special ordering). These four resolve
	// byte-identical to live BAML:
	//   - overlapping-key classes where one arm's full field set try_casts (39);
	//   - single-field class arms with a scalar input inferred-object-absorbed (40);
	//   - a literal|class mix where the class try_casts first (42);
	//   - an enum|class mix scored in phase 2 (43).
	"class_union_overlapping_keys":            true,
	"single_field_class_union_implied_key":    true,
	"literal_class_union_object_to_primitive": true,
	"enum_class_union_object_to_string":       true,
	// M3c added coverage: the pick_best classSingleImplied devalue (a single-string
	// implied-key class ties an ordinary class on score and loses the tie under the
	// union target), and a mixed literal|class where the class arm provably errors so
	// the literal wins via ObjectToPrimitive — both CLAIMED green vs live BAML.
	"class_union_single_string_implied_devalue":            true,
	"literal_class_union_object_to_primitive_literal_wins": true,
	// M3d guard: a class union with a DEFAULTABLE-field arm (a list field defaulting
	// to [], an all-default class) stays fallback — its default-fill / all-default
	// devalue scoring inside a union is not modeled until M3d, so native declines at
	// the gate (the arm has a non-flat-leaf field).
	"class_union_all_default_stays_fallback": false,
	// M3d CLAIMED — unions with LIST / MAP arms + array-to-singular. The union gate
	// now admits a list arm (scored by coerceList + pick_best list ordering) and a
	// string-keyed map arm (tryCastMap phase 1 + coerceMap phase 2), and the
	// primitive int/float/bool coercers array-to-singular on array input. These
	// resolve byte-identical to live BAML:
	//   - string | list<string> with a scalar → the string arm try_casts first (44);
	//   - int | list<int> with a partial array → the list arm ([1,2] via
	//     ArrayItemParseError) beats the int arm's array-to-singular (Int 1 devalued
	//     by FirstMatch against the composite list) (100);
	//   - map<string,int> | string with a partial-value map → the map arm ({"a":1}
	//     via MapValueParseError) beats the stringified array/object (45).
	"union_list_singleton_ambiguity": true,
	"union_map_value_partial":        true,
	// M3d CLAIMED: array input to a scalar union resolves via array-to-singular —
	// string | int with [1,2] → the int arm scores 1 (UnionMatch+FirstMatch, since a
	// union arm's array-to-singular target IS the union) and beats the string arm's
	// JsonToString "[1, 2]" (score 2).
	"scalar_union_array_input_claimed": true,
	// Still fallback: a scalar union where no arm proves a winner (all arms error).
	"scalar_union_no_match_fallback": false,
	// A multi-arm union as a LIST ELEMENT declines: BAML threads the previous
	// element's arm as ctx.union_variant_hint (coerce_array.rs) but native has no
	// hint, so per-element arm selection can diverge. Array union hints are M3d.
	"list_scalar_union_stays_fallback": false,
	// Mcoerce-b native LENIENT PRIMITIVE + LITERAL numeric/bool/null coercion:
	// numeric-string parsing (trim + trailing-comma trim, i64 / u64-wrap / f64 /
	// fraction / extracted-number regex), float→int rounding (half-away,
	// saturating cast), string→bool (casefold + match_string), and non-null→null
	// defaulting — plus int/bool literals by primitive-coerce-then-compare. These
	// resolve byte-identical to BAML, so the in-scope conversions are CLAIMED.
	"primitive_int_json_float_round":          true,
	"primitive_int_numeric_string_trim_comma": true,
	"primitive_int_u64_wrap":                  true,
	"primitive_int_fraction_string":           true,
	"primitive_int_extracted_currency":        true,
	"primitive_float_numeric_string":          true,
	"primitive_float_fraction_string":         true,
	"primitive_float_percent_not_ratio":       true,
	"primitive_float_extracted_sentence":      true,
	"primitive_bool_casefold":                 true,
	"primitive_bool_match_string_substring":   true,
	"primitive_null_non_null_default":         true,
	"literal_int_float_round_match":           true,
	"literal_int_numeric_string_match":        true,
	"literal_bool_string_match":               true,
	// M2c union revisit: exactly one lenient success claims.
	"union_literal_int_string_one_lenient_success": true,
	"class_union_lenient_leaf_one_success":         true,
	// Nullable clean-only rule: a CLEAN non-null arm (direct string→int parse)
	// beats the scored null arm, so it claims.
	"nullable_optional_int_clean_string_claim": true,
	// Mcoerce-b FALLBACK set: a literal VALUE mismatch after a successful
	// primitive coercion (native declines BAML's error/default choice).
	"literal_int_numeric_string_mismatch": false,
	"literal_bool_string_mismatch":        false,
	// M3a CLAIMED (score model + pick_best): the two-success safe-family unions
	// and the score-bearing nullable arms that were pinned fallback under the
	// pre-M3 clean-only rule now resolve via the scored selection (winner < 110,
	// or two successes picked by pick_best) — all live-captured green vs BAML.
	"class_union_strict_plus_lenient_two_successes_scored": true,
	"nullable_optional_int_float_round_claims":             true,
	"nullable_optional_bool_string_claims":                 true,
	// CR-B1 FALLBACK set: float spellings Go's ParseFloat accepts but Rust's
	// str::parse::<f64>() rejects (hex floats, digit-group underscores) must NOT
	// be claimed — parseF64Rust rejects them so native declines exactly where
	// BAML declines. Non-finite results (NaN / ±Inf) have no valid JSON number
	// form, so the float paths decline; the int path also declines non-finite
	// (a parity-safe under-claim: BAML saturates inf->i64::MAX / nan->0, but
	// native falls back rather than claim against the dynamic bridge).
	"float_hex_stays_fallback":                false,
	"float_signed_hex_stays_fallback":         false,
	"float_underscore_stays_fallback":         false,
	"float_hex_fraction_stays_fallback":       false,
	"int_hex_stays_fallback":                  false,
	"int_underscore_stays_fallback":           false,
	"int_hex_fraction_stays_fallback":         false,
	"literal_int_hex_spelling_stays_fallback": false,
	"float_nan_stays_fallback":                false,
	"float_inf_stays_fallback":                false,
	"float_nan_fraction_stays_fallback":       false,
	"int_inf_stays_fallback":                  false,
	"int_nan_stays_fallback":                  false,
	// Mcoerce-c native LISTS (coerceList / coerce_array.rs): non-array
	// SingleToArray wrapping, PARTIAL array skips of PROVEN-parse-error items,
	// and empty-list-on-singleton-failure resolve byte-identical to BAML, so the
	// deterministic collection claims are CLAIMED. A child native merely DECLINED
	// (could be a DEFERRED Mcoerce-d success — JsonToString, etc.) is NOT skipped;
	// native declines the whole list. The union revisit counts a list arm as a
	// lenient success and, for nullable lists, keeps the clean-only rule: a
	// SingleToArray/partial-skip/flagged-child list arm declines against the
	// scored null arm.
	"list_singleton_int_success":               true,
	"list_singleton_bad_int_empty":             true,
	"list_array_partial_bad_int":               true,
	"list_array_lenient_elements_kept":         true,
	"list_class_non_object_partial":            true,
	"nullable_optional_list_clean_array_claim": true,
	// M3a CLAIMED: a nullable list arm scored by SingleToArray (score 1) beats the
	// null arm (110).
	"nullable_optional_list_singleton_claims": true,
	// M3d CLAIMED (union WITH a list arm): int | list<int> with [1,"bad",2] — the
	// int arm array-to-singulars to Int 1 (a proven bad-numeric-string item is
	// excluded) but carries FirstMatch, so pick_best's scalar-vs-composite rule
	// devalues it behind the list arm ([1,2] via ArrayItemParseError(1)).
	"union_list_partial_claimed": true,
	// Mcoerce-c native MAPS (coerceMap / coerce_map.rs): object→map ObjectToMap
	// flagging, VALUE-then-KEY coercion, and PARTIAL entry skips of PROVEN map
	// VALUE parse errors (MapValueParseError) resolve byte-identical to BAML, so
	// the deterministic partial-map claims are CLAIMED — accepted entries in INPUT
	// key order under their ORIGINAL key strings.
	"map_value_partial_bad_int": true,
	"map_value_lenient_kept":    true,
	// Mcoerce-c MAP fallback set. KEY misses are NOT native skips: the dynamic
	// bridge keeps non-matching enum / string-literal / literal-union keys
	// leniently (live-captured FULL maps, not partial), so a key miss is a
	// DEFERRED Mcoerce-d keep and native declines the WHOLE map. Also fallback: a
	// duplicate original key (unproven insert order). Map-key non-member probes
	// stay fallback (M3d).
	"map_literal_key_partial_bad_key":   false,
	"map_bad_key_original_order":        false,
	"map_enum_key_nonmember_live_probe": false,
	"map_duplicate_key_stays_fallback":  false,
	// M3a CLAIMED: a nullable map arm carrying ObjectToMap (score 1) plus its clean
	// value scores beats the null arm (110) — the map's inherent score is now
	// computed (own + value scores).
	"nullable_optional_map_object_claims": true,
	// (map_string_string_non_string_value flipped to CLAIMED in Mcoerce-d PR 1.)

	// Mcoerce-d PR 1 — STRINGIFICATION + LITERAL EXTRACTION. Leaf coercers now
	// port BAML's coerce_string (JsonToString), match_string ObjectToString
	// (enum / string-literal), and coerce_literal's single-key-object
	// ObjectToPrimitive prelude. A NON-null non-string into a string/enum/literal
	// target, and a single-key-object into a literal, resolve byte-identical to
	// BAML, so the deterministic non-union cases are CLAIMED — including the
	// leaf-level collection flips (a stringified list element / map value is KEPT,
	// and a direct string←null child is a PROVEN skip). A JSON null into a
	// standalone string target still DECLINES (error_unexpected_null; native
	// cannot score error-vs-default). No class-structural / union-broadening /
	// pick_best work here (PR 2 / PR 3 / M3).
	//
	// Flipped from the Mcoerce-b/c fallback set (were *_stays_fallback):
	"literal_int_single_key_object_claimed":      true,
	"primitive_string_non_string_json_to_string": true,
	"list_string_non_string_kept":                true,
	"map_string_string_non_string_value_kept":    true,
	// New leaf-level coverage:
	"primitive_string_object_json_to_string":               true,
	"primitive_string_array_json_to_string":                true,
	"primitive_string_null_stays_fallback":                 false,
	"enum_object_to_string_one_match":                      true,
	"literal_string_number_object_to_string_one_match":     true,
	"literal_bool_single_key_object_claimed":               true,
	"literal_string_single_key_object_claimed":             true,
	"list_string_bool_object_array_values_kept":            true,
	"map_string_string_bool_object_array_values_kept":      true,
	"list_enum_object_to_string_value_kept":                true,
	"map_string_enum_object_to_string_value_kept":          true,
	"list_literal_int_single_key_object_kept":              true,
	"map_string_literal_bool_single_key_object_value_kept": true,
	"list_string_null_skipped":                             true,
	// Number-display parity (over-claim guard): a NON-integer number spelling is
	// canonicalized by BAML's serde_json f64 Display (5e0 -> "5.0"), which native's
	// raw-token render cannot prove byte-identical, so native marks it UNCERTAIN and
	// DECLINES — standalone and (whole-collection, not a partial skip) in a list/map.
	// Integer stringification still claims (see the CLAIMED entries above).
	"primitive_string_number_noninteger_stays_fallback": false,
	"list_string_noninteger_number_stays_fallback":      false,

	// Mcoerce-d PR 2 — STRUCTURAL CLASS DEFAULTS. coerceClass now ports BAML's
	// coerce_class.rs (non-array subset): single-field OBJECT implied-key and
	// SCALAR/null inferred-object absorption, missing-optional null fill, and
	// TypeIR::default_value required-field fills (list→[], map→{}, null→null;
	// DefaultFromNoValue) plus the present-map-non-object default {}
	// (DefaultButHadUnparseableValue). These resolve byte-identical to BAML, so the
	// deterministic non-union class cases are CLAIMED — including the collection
	// flips (a single-field-class scalar element / class-with-defaults element is
	// KEPT in a list/map). ARRAY input to a class still DECLINES (M3
	// array-to-singular). No union-family broadening (PR 3).
	"single_field_class_scalar_inferred":                true,
	"single_field_class_object_implied_key":             true,
	"class_missing_optional_null":                       true,
	"class_required_list_default_from_no_value":         true,
	"class_required_map_default_from_no_value":          true,
	"class_required_null_default_from_no_value":         true,
	"class_map_field_default_but_had_unparseable_value": true,
	"list_single_field_class_scalar_kept":               true,
	"map_string_single_field_class_scalar_value_kept":   true,
	"list_class_required_default_value_kept":            true,
	"map_string_class_required_default_value_kept":      true,

	// Mcoerce-d PR 3 — UNION REVISIT (one-success safe-family claims, unchanged).
	"literal_union_object_to_primitive_one_success_claimed": true,
	"literal_union_object_to_string_one_success_claimed":    true,
	"class_union_stringification_one_success_claimed":       true,

	// M3 slice a — SCORE MODEL + safe-family pick_best. The safe-family union
	// coercers now compute the types.rs inherent score for every arm, apply BAML's
	// early first-score-0 winner rule, and otherwise run a faithful
	// array_helper::pick_best over the successes plus (when nullable) the null arm
	// (DefaultButHadValue, score 110). No gate broadening — only the existing
	// literal/class safe families are scored, so 37-46/100 and the map-key
	// non-member probes STAY fallback (M3b/c/d). These were pinned fallback under
	// the pre-M3 clean-only rule and now CLAIM the scored winner (all live-captured
	// green vs BAML):
	//   - two-success safe-family unions resolved by pick_best (scored winner);
	//   - score-bearing nullable arms scoring < 110 (claim the arm);
	//   - a nullable class arm scoring > 110 (claim null);
	//   - the score boundary cases (109 < 110 -> arm, 110 tie -> lower index arm,
	//     111 > 110 -> null);
	//   - a two-substring literal union (pick_best picks the lower-index arm);
	//   - a class union where a LOSING arm has a PROVABLE required-field parse
	//     error (BAML errors just that arm; native excludes it and claims the
	//     winner rather than declining the whole union) — including int/bool
	//     LITERAL field value mismatches.
	"class_union_stringification_two_successes_scored": true,
	"nullable_optional_string_json_to_string_claims":   true,
	"nullable_optional_class_default_claims":           true,
	"class_union_extra_keys_109_below_null":            true,
	"class_union_extra_keys_110_tie_null":              true,
	"class_union_extra_keys_111_above_null":            true,
	"literal_union_two_substring_pick_first":           true,
	"class_union_provable_losing_arm_claims":           true,
	"class_union_literal_int_field_losing_arm":         true,
	"class_union_literal_bool_field_losing_arm":        true,

	// M3d — ARRAY-TO-SINGULAR + LIST/MAP-VARIANT unions (this slice's new corpus).
	// The primitive int/float/bool coercers array-to-singular on array input
	// (coerce_array_to_singular over the items + pick_best + FirstMatch), the
	// multi-field all-required-flat-leaf class coerces array input the same way, and
	// the union gate admits list + string-keyed map arms. These resolve
	// byte-identical to live BAML:
	"primitive_int_array_first_best":      true,
	"primitive_int_array_partial_bad":     true,
	"class_array_to_singular_length_one":  true,
	"class_array_to_singular_multi_item":  true,
	"union_list_scalar_string_wins":       true,
	"union_list_list_empty_error_devalue": true,
	"union_map_clean_try_cast":            true,
	// Over-claim GUARD: C{a:int} | map with an extra key resolves to the MAP (the
	// class try_cast rejects the extra key; the map try_cast keeps it), which
	// tryCastMap + the try_cast_union non-zero pick_best reproduce.
	"union_class_map_extra_key_prefers_map": true,
	// Over-claim GUARDs (cold review): a LIST / MAP-value / UNION-value arm's phase-1
	// try_cast REJECTS a numeric-string element that its LENIENT coerce would accept,
	// so BAML's try_cast_union picks the string-typed arm — tryCastArray +
	// tryCastUnionArm (recursive tryCastArm dispatch) reproduce it, not a lenient
	// early-return of the numeric arm.
	"union_list_int_string_try_cast": true,
	"union_map_list_value_try_cast":  true,
	"union_map_union_value_try_cast": true,
	// FALLBACK: an EMPTY array to a scalar is BAML's error_unexpected_empty_array;
	// native models it as a provenError (wraps the fallback sentinel), so native
	// DECLINES the whole parse rather than claim the error.
	"primitive_int_array_empty_stays_fallback": false,
}

// parseRecoveryStreamNativeClaim pins the per-prefix native disposition for
// streaming recovery cases: caseName -> prefixName -> claimed. A prefix set to
// true is expected to be CLAIMED by internal/debaml.Parse(Stream=true) and held
// to BAML parse-stream byte-exact by the per-prefix differential; a prefix absent
// from the map defaults to false (native FALLS BACK to BAML).
//
// M4b flips the live-captured success prefixes whose observable behavior is
// jsonish recovery + class null-filling WITHOUT stream annotations or displayed
// stream state: an open/repaired root object AFTER at least one field value is
// available (BAML's Pending null fills the missing fields), truncated /
// markdown-fence-recovered incomplete STRING values (strings are not done-required
// so a partial string is kept as its value — no AnyOf leak), and trailing-comma
// list/string prefixes BAML accepts. The prefixes that stay FALLBACK (absent) are
// the over-claim guards: a bare open brace / dangling key / dangling colon and
// bare prose / just-opened fence (NO field value yet — the empty-object and
// allow_as_string→class recovery paths are not claimed), which M4b deliberately
// over-declines. Completed done-required scalars are claimed only where the
// enclosing structure proves them done (the closed full_object); an INCOMPLETE
// done-required scalar and any semantic child deletion stay fallback (M4c).
var parseRecoveryStreamNativeClaim = map[string]map[string]bool{
	// 20_streaming_growing_object (Root{name:string, age:int}). The bare `{`,
	// key-only, and dangling-colon prefixes have no field value yet → fallback;
	// every prefix from the first present field value on is claimed (missing fields
	// null-filled, the closed int kept).
	"streaming_growing_object": {
		"partial_string": true,
		"full_string":    true,
		"second_key":     true,
		"full_object":    true,
	},
	// 21_streaming_markdown_fence (Root{name:string, age:int}). Bare prose and the
	// just-opened fence have no JSON content → fallback; the object emerging inside
	// the (still-open, then closed) fence is claimed.
	"streaming_markdown_fence": {
		"object_partial": true,
		"object_full":    true,
		"fence_close":    true,
	},
	// 22_streaming_truncated_string (Root{name:string}). Every prefix carries a
	// present (possibly empty / truncated / incomplete) STRING value, which is not
	// done-required, so all four are claimed as the exact string value.
	"streaming_truncated_string": {
		"open":          true,
		"mid_string":    true,
		"closed_string": true,
		"closed_object": true,
	},
	// 23_streaming_trailing_comma (Root{name:string, tags:list<string>}). Both
	// fields are present with completed string / list-of-completed-string values,
	// and the dangling trailing comma is dropped, so all four are claimed.
	"streaming_trailing_comma": {
		"list_open":      true,
		"trailing_comma": true,
		"list_closed":    true,
		"object_closed":  true,
	},
	// anyof_string_no_leak (Root{note:string}): a truncated string value BAML
	// represents internally as an AnyOf must surface as the STRING value natively,
	// never an AnyOf[...] rendering — claimed from the first present value on.
	"anyof_string_no_leak": {
		"partial": true,
		"closed":  true,
	},
	// list_multi_arm_union_stream (Root{items:list<int|bool>}): a direct
	// list<multi-arm-union> element stays NATIVE-SKIPPED in stream mode — BAML's
	// cross-element union_variant_hint (coerce_array.rs) was intentionally deferred
	// after M3, so native declines at the gate (checkSupported) on every prefix and
	// BAML parse-stream stays authoritative. No prefix is claimed (absent → false).

	// --- M4c: annotation-free semantic streaming (required-done child deletion,
	// class field null-replacement, map entry drop, Pending fillers). These flip
	// prefixes M4b DECLINED (an incomplete done-required child that BAML DELETES) to
	// CLAIMED: native now reproduces the deletion/null-replacement byte-exact. The
	// annotation-DEPENDENT behavior (@stream.done / @@stream.done / @stream.not_null /
	// @stream.with_state) stays fallback and has NO fixture — BAML's dynamic
	// TypeBuilder cannot attach those annotations, so neither the BAML oracle nor the
	// native bridge can carry them (nothing to differential-test).
	//
	// M4d adds NO streaming corpus fixture: its part (A) — @stream.with_state output
	// wrappers — is UNREPRESENTABLE on a dynamic schema for exactly the reason above
	// (schema.FromDynamicOutputSchema lowers every class with a zero StreamingBehavior),
	// so with_state STAYS BAML fallback and there is nothing to capture here. M4d's part
	// (B) — the native-first runtime parseStreamFn seam — wires internal/debaml.Parse
	// (Stream=true, the SAME parser this differential holds to BAML byte-exact) into the
	// generated streaming path; it is exercised end-to-end in
	// dynamic_debaml_stream_seam_test.go (native-first CLAIM + silent BAML fallback),
	// not by new corpus prefixes.

	// 173_streaming_numbers_list_int (Root{nums:list<int>}). int is done-required, so
	// a still-streaming trailing element is DROPPED and completed (comma-terminated)
	// elements are kept; the list itself is not done-required.
	"streaming_numbers_list_int": {
		"open_list":     true,
		"one_partial":   true,
		"one_done":      true,
		"two_partial":   true,
		"three_partial": true,
		"list_closed":   true,
		"object_closed": true,
	},
	// 174_streaming_class_incomplete_scalar_field (Root{name:string, age:int}). An
	// incomplete done-required int class field is NULL-REPLACED; the completed string
	// sibling is kept; the closed int is kept.
	"streaming_class_incomplete_scalar_field": {
		"name_done_age_partial":     true,
		"name_done_age_more_digits": true,
		"age_closed":                true,
	},
	// 175_streaming_list_class_partial_field (Root{items:list<Inner{a:int}>}). A
	// partial trailing class element is KEPT (class not done-required) with its
	// incomplete int field null-replaced; completed elements remain.
	"streaming_list_class_partial_field": {
		"first_done_second_partial": true,
		"second_closed":             true,
		"list_closed":               true,
	},
	// 176_streaming_map_int_partial (Root{m:map<string,int>}). An entry whose
	// incomplete done-required int value is dropped WHOLE (key+value); completed
	// entries stay in input key order.
	"streaming_map_int_partial": {
		"one_done_two_partial": true,
		"two_closed":           true,
		"map_closed":           true,
	},
	// 177_streaming_missing_field_fillers (Root{a:int, tags:list<string>,
	// scores:map<string,int>, note:string?}). Missing fields are filled with BAML's
	// TypeIR::default_value (LIVE-CAPTURED: required list→[], required map→{},
	// optional→null), NOT null. key_only (`{"a"`) has no field value yet → native
	// over-declines (the ≥1-present-field guard, BAML still succeeds). a_partial
	// (`{"a":1`) has an incomplete int → deleted → a:null with the default fillers;
	// a_closed (`{"a":1}`) has the completed int → a:1. Both claimed.
	"streaming_missing_field_fillers": {
		"key_only":  false,
		"a_partial": true,
		"a_closed":  true,
	},
}

// streamPrefixNativeClaim returns the expected native disposition for one
// streaming prefix: true = native is expected to CLAIM it (and be diffed
// against BAML), false = native FALLS BACK to BAML (the M4a default for every
// prefix).
func streamPrefixNativeClaim(caseName, prefixName string) bool {
	if m, ok := parseRecoveryStreamNativeClaim[caseName]; ok {
		return m[prefixName]
	}
	return false
}

// parseRecoverySchemaContainsMap reports whether the schema reaches a map type
// anywhere — the effective root, any reachable class field, or a nested list /
// optional / union / map / class-ref edge. It backs the MAP PARITY-DECLINE
// override: internal/debaml.Parse declines every map-containing schema
// (checkNoMap), so such a case must fall back regardless of its pinned final /
// streaming disposition. Class-ref cycles terminate via the visited set.
func parseRecoverySchemaContainsMap(s bamlfuzz.FuzzSchema) bool {
	classes := make(map[string]bamlfuzz.FuzzClass, len(s.Classes))
	for _, c := range s.Classes {
		classes[c.Name] = c
	}
	seen := make(map[string]bool, len(s.Classes))
	var walkType func(t bamlfuzz.FuzzType) bool
	var walkClass func(name string) bool
	walkClass = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		c, ok := classes[name]
		if !ok {
			return false
		}
		for i := range c.Properties {
			if walkType(c.Properties[i].Type) {
				return true
			}
		}
		return false
	}
	walkType = func(t bamlfuzz.FuzzType) bool {
		switch t.Kind {
		case bamlfuzz.KindMap:
			return true
		case bamlfuzz.KindOptional, bamlfuzz.KindList:
			return t.Inner != nil && walkType(*t.Inner)
		case bamlfuzz.KindUnion:
			for i := range t.Variants {
				if walkType(t.Variants[i]) {
					return true
				}
			}
			return false
		case bamlfuzz.KindClassRef:
			return walkClass(t.Ref)
		}
		return false
	}
	return walkType(s.EffectiveRoot())
}

// parseRecoveryStats tallies how many final-parse and streaming-prefix legs
// the native parser claimed vs fell back on, logged as a summary so a shift
// in the native cut-line is visible even when every case still passes. The
// harness runs the per-case subtests sequentially (no t.Parallel), so a plain
// pointer-shared counter needs no locking.
type parseRecoveryStats struct {
	claimed        int
	fallback       int
	streamClaimed  int
	streamFallback int
}

// TestBamlfuzzParseRecovery characterizes BAML's JSONish final-parse
// recovery behavior against the checked-in corpus and, when a native
// parser is registered, diffs native against BAML. BAML is the oracle:
// each case's `want` is BAML's observed outcome, so the gating leg asserts
// the live BAML parse still matches the recorded outcome (a drift detector
// for BAML behavior changes), and the differential leg holds any future
// native parser to BAML's exact behavior.
//
// The native de-BAML parser is registered as the differential candidate, so
// both the final-parse and streaming legs are live differentials. Streaming
// cases additionally drive the direct BAML parse-stream oracle
// (DynamicParse with Stream=true) per accumulated prefix and gate BAML
// against the live-captured prefix `want`; the native candidate CLAIMS the M4b
// partial surface (per parseRecoveryStreamNativeClaim) and is held to BAML
// byte-exact there, and FALLS BACK (SkippedNative) on every other prefix.
//
// Run with BAMLFUZZ_PARSE_CAPTURE=1 to log BAML's observed final-parse AND
// per-prefix parse-stream outcomes instead of gating — used to (re)capture
// the corpus `want` values when adding cases or after an intentional BAML
// behavior change. Streaming captures carry full prefix identity (case +
// prefix name + a `stream` marker) so a stream want can never be reused as a
// final-parse want.
func TestBamlfuzzParseRecovery(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	dynclientCallGate(t)

	corpus, err := bamlfuzz.LoadParseRecoveryCorpus(parseRecoveryCorpusDir)
	if err != nil {
		t.Fatalf("load parse recovery corpus from %s: %v", parseRecoveryCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("parse recovery corpus at %s is empty", parseRecoveryCorpusDir)
	}

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	baml := bamlDynamicParser{dyn: dyn}
	// Register the bounded M1 native parser as the differential candidate so
	// DiffParsers diffs it against BAML; restore the prior (no-op) parser
	// when the test ends.
	restore := bamlfuzz.RegisterNativeParser(nativeDeBAMLParser{})
	defer restore()
	native := bamlfuzz.RegisteredNativeParser()
	capture := os.Getenv("BAMLFUZZ_PARSE_CAPTURE") == "1"

	stats := &parseRecoveryStats{}
	for i, c := range corpus {
		i := i
		c := c
		t.Run(c.Name, func(t *testing.T) {
			runParseRecoveryCase(t, baml, native, c, i, capture, stats)
		})
	}
	t.Logf("native de-BAML final-parse dispositions: %d claimed, %d fell back to BAML", stats.claimed, stats.fallback)
	t.Logf("native de-BAML parse-stream dispositions: %d claimed, %d fell back to BAML (M4b claims the basic-partial surface)", stats.streamClaimed, stats.streamFallback)
}

// runParseRecoveryCase drives the final and/or streaming legs of one
// recovery case.
func runParseRecoveryCase(t *testing.T, baml, native bamlfuzz.Parser, c bamlfuzz.ParseRecoveryCase, idx int, capture bool, stats *parseRecoveryStats) {
	t.Helper()

	if c.HasFinal() {
		t.Run("final", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req := bamlfuzz.ParseRequest{
				Schema:              c.Schema,
				Raw:                 c.Raw,
				Stream:              false,
				PreserveSchemaOrder: c.PreserveSchemaOrder,
			}

			bamlRes, bamlErr := baml.Parse(ctx, req)
			if capture {
				logObservedOutcome(t, "final", c.Raw, bamlRes, bamlErr)
			} else {
				characterizeFinal(t, c, idx, bamlRes, bamlErr)
			}

			// Native-vs-BAML differential. choices carries the case's
			// union-arm metadata so the schema-order check can descend into
			// the exercised arm (it fails closed on a union path otherwise);
			// it is nil for the union-free majority of cases.
			res := bamlfuzz.DiffParsers(ctx, baml, native, req, c.UnionChoices)
			if !res.SkippedNative && len(res.Failures) > 0 {
				dumpParseDiffAndFail(t, parseRecoveryArtifactDir, parseDiffEnvelope(c, idx, -1, "", c.Raw, false, res), strings.Join(res.Failures, "; "))
			}

			// Record and assert the native parser's disposition (claimed vs
			// fallback). SkippedNative means it returned ErrParserUnavailable
			// (mapped from ErrDeBAMLParseUnsupported) and the BAML leg stands
			// alone; otherwise it claimed the parse and the diff above held it
			// to BAML. The expected disposition pins the M1 cut-line per case.
			claimed := !res.SkippedNative
			if claimed {
				stats.claimed++
			} else {
				stats.fallback++
			}
			// Every final-parse fixture MUST pin an expected disposition, so a
			// new corpus case can't silently skip the cut-line assertion.
			want, ok := parseRecoveryNativeClaim[c.Name]
			if !ok {
				t.Fatalf("final-parse case %q has no pinned native disposition; add it to parseRecoveryNativeClaim (true=claimed, false=fallback)", c.Name)
			}
			// MAP PARITY-DECLINE: a map-containing schema always falls back
			// (checkNoMap), overriding the pinned pre-decline claim. See
			// parseRecoverySchemaContainsMap and the parseRecoveryNativeClaim doc.
			if parseRecoverySchemaContainsMap(c.Schema) {
				want = false
			}
			if claimed {
				t.Logf("native CLAIMED final parse for %q", c.Name)
			} else {
				t.Logf("native FELL BACK to BAML for %q", c.Name)
			}
			if claimed != want {
				t.Errorf("native disposition for %q: got claimed=%v, want claimed=%v (M1 cut-line drift)", c.Name, claimed, want)
			}
		})
	}

	if c.HasPrefixes() {
		t.Run("streaming", func(t *testing.T) {
			// Exercise the corpus-format invariant: accumulated prefixes
			// must grow. This holds independent of any BAML plumbing.
			raws := c.PrefixRaws()
			for i := 1; i < len(raws); i++ {
				if !strings.HasPrefix(raws[i], raws[i-1]) {
					t.Errorf("prefix[%d] %q does not extend prefix[%d] %q",
						i, c.Prefixes[i].Name, i-1, c.Prefixes[i-1].Name)
				}
			}

			// Per-prefix native-vs-BAML parse-stream differential. BAML is the
			// oracle: DiffParserPrefixes forces Stream=true and diffs each
			// accumulated prefix on its own (early accept/reject is part of the
			// spec, so no prefix is privileged). BAML being unavailable on a
			// prefix is a harness failure, surfaced through res.Failures with
			// SkippedNative=false; the native candidate declining is a skip.
			// Each accumulated prefix issues its OWN live BAML parse-stream
			// call. Give every call the same bounded headroom the single-call
			// final-parse leg gets (perCallOracleTimeout) instead of sharing
			// one deadline across up to len(raws) sequential calls, so a slow
			// oracle call under CI load can't starve the later prefixes.
			// perCallTimeoutParser applies the per-call bound inside
			// DiffParserPrefixes' loop; an outer budget scaled to the prefix
			// count still caps the whole streaming leg as a backstop.
			ctx, cancel := context.WithTimeout(context.Background(), perCallOracleTimeout*time.Duration(len(raws)))
			defer cancel()
			gatedBAML := perCallTimeoutParser{inner: baml, timeout: perCallOracleTimeout}
			req := bamlfuzz.ParseRequest{
				Schema:              c.Schema,
				PreserveSchemaOrder: c.PreserveSchemaOrder,
			}
			results := bamlfuzz.DiffParserPrefixes(ctx, gatedBAML, native, req, raws, c.UnionChoices)

			for i := range results {
				i := i
				p := c.Prefixes[i]
				res := results[i]
				t.Run(p.Name, func(t *testing.T) {
					// Capture mode logs BAML's observed per-prefix outcome
					// (with full prefix identity) instead of gating, so a
					// developer running BAMLFUZZ_PARSE_CAPTURE=1 records the
					// corpus `want`; otherwise gate BAML against it.
					if capture {
						logObservedStreamOutcome(t, c.Name, p, res.BAML)
					} else {
						characterizeStreamPrefix(t, c, p, res.BAML)
					}

					// Native-vs-BAML per-prefix differential. For an M4b-claimed
					// prefix native returns real JSON and this gate holds it to
					// BAML byte-exact; for a fallback prefix SkippedNative is the
					// expected shape and this gate never fires. It also fires on a
					// BAML harness failure (SkippedNative=false with failures).
					if !res.SkippedNative && len(res.Failures) > 0 {
						dumpParseDiffAndFail(t, parseRecoveryArtifactDir,
							parseDiffEnvelope(c, idx, i, p.Name, p.Raw, true, res),
							strings.Join(res.Failures, "; "))
					}

					// Per-prefix native disposition (claimed vs fallback),
					// pinned so a later slice can flip individual prefixes.
					claimed := !res.SkippedNative
					if claimed {
						stats.streamClaimed++
					} else {
						stats.streamFallback++
					}
					want := streamPrefixNativeClaim(c.Name, p.Name)
					// MAP PARITY-DECLINE: parseStream shares the checkSupported
					// gate, so a map-containing schema falls back on every prefix,
					// overriding the pinned pre-decline claim.
					if parseRecoverySchemaContainsMap(c.Schema) {
						want = false
					}
					if claimed {
						t.Logf("native CLAIMED stream prefix %q/%q", c.Name, p.Name)
					} else {
						t.Logf("native FELL BACK to BAML for stream prefix %q/%q", c.Name, p.Name)
					}
					if claimed != want {
						t.Errorf("native stream disposition for %q/%q: got claimed=%v, want claimed=%v (M4b cut-line drift)", c.Name, p.Name, claimed, want)
					}
				})
			}
		})
	}
}

// characterizeStreamPrefix gates the live BAML parse-stream outcome for one
// accumulated prefix against the recorded `want`: status (success/error)
// parity, plus strict JSON + key-order equality on a successful partial parse.
// `want` is BAML's own captured parse-stream output (recorded via
// BAMLFUZZ_PARSE_CAPTURE=1), so strict equality is correct — a divergence
// means BAML's parse-stream behavior drifted from the checked-in
// characterization. It reads the BAML outcome DiffParserPrefixes already
// produced (res.BAML) rather than re-calling BAML.
func characterizeStreamPrefix(t *testing.T, c bamlfuzz.ParseRecoveryCase, p bamlfuzz.ParseRecoveryPrefix, outcome bamlfuzz.ParseOutcome) {
	t.Helper()
	observed := bamlfuzz.ParseStatusSuccess
	if outcome.Error != "" {
		observed = bamlfuzz.ParseStatusError
	}
	if observed != p.Want.Status {
		t.Errorf("stream prefix %q/%q raw=%q: BAML status %q ≠ want %q (BAML json=%s err=%s)",
			c.Name, p.Name, p.Raw, observed, p.Want.Status, string(outcome.JSON), outcome.Error)
		return
	}
	if !p.Want.IsSuccess() {
		return // both errored — parity holds, no payload to compare.
	}
	diff, err := bamlfuzz.SemanticDiffStrict("want_vs_baml_stream", p.Want.JSON, outcome.JSON)
	if err != nil {
		t.Errorf("characterize stream %q/%q: semantic diff: %v", c.Name, p.Name, err)
		return
	}
	if len(diff) > 0 {
		t.Errorf("stream prefix %q/%q raw=%q: want ≠ BAML (semantic): %+v", c.Name, p.Name, p.Raw, diff)
	}
	if c.PreserveSchemaOrder {
		od, oerr := bamlfuzz.SchemaOrderDiffWithChoices("want_vs_baml_stream", c.Schema, p.Want.JSON, outcome.JSON, c.UnionChoices)
		if oerr != nil {
			t.Errorf("stream prefix %q/%q: schema order: %v", c.Name, p.Name, oerr)
		} else if len(od) > 0 {
			t.Errorf("stream prefix %q/%q raw=%q: want ≠ BAML (order): %+v", c.Name, p.Name, p.Raw, od)
		}
	}
}

// logObservedStreamOutcome prints BAML's observed parse-stream outcome for one
// accumulated prefix in a stable, greppable form so a developer running
// BAMLFUZZ_PARSE_CAPTURE=1 can copy the values into the corpus prefix `want`.
// It carries the full prefix identity (case + prefix name + exact raw) and a
// `stream` marker so a stream want can never be confused with — or reused
// from — a final-parse want. Not a gate.
func logObservedStreamOutcome(t *testing.T, caseName string, p bamlfuzz.ParseRecoveryPrefix, outcome bamlfuzz.ParseOutcome) {
	t.Helper()
	if outcome.Error != "" {
		t.Logf("CAPTURE stream case=%q prefix=%q raw=%q -> status=error err=%s", caseName, p.Name, p.Raw, outcome.Error)
		return
	}
	t.Logf("CAPTURE stream case=%q prefix=%q raw=%q -> status=success json=%s", caseName, p.Name, p.Raw, string(outcome.JSON))
}

// characterizeFinal gates the live BAML final parse against the recorded
// `want`: status (success/error) parity, plus strict JSON + key-order
// equality on a successful parse. `want` is BAML's own captured output, so
// strict equality is correct — a divergence means BAML's parse behavior
// drifted from the checked-in characterization.
func characterizeFinal(t *testing.T, c bamlfuzz.ParseRecoveryCase, idx int, res bamlfuzz.ParseResult, parseErr error) {
	t.Helper()
	observed := bamlfuzz.ParseStatusSuccess
	if parseErr != nil {
		observed = bamlfuzz.ParseStatusError
	}
	if observed != c.Want.Status {
		outcome := bamlfuzz.ParseOutcome{Parser: "baml_dynamic"}
		if parseErr != nil {
			outcome.Error = parseErr.Error()
		} else {
			outcome.JSON = res.JSON
		}
		env := &bamlfuzz.ParseDiffFailureEnvelope{
			CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
			PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
			UnionChoices: c.UnionChoices,
			PrefixIndex:  -1, Raw: c.Raw,
			ExpectedStatus: c.Want.Status, Expected: c.Want.JSON,
			BAML:     outcome,
			Failures: []string{fmt.Sprintf("status parity: want %q, BAML produced %q", c.Want.Status, observed)},
		}
		dumpParseDiffAndFail(t, parseRecoveryArtifactDir, env, fmt.Sprintf("BAML status %q ≠ want %q", observed, c.Want.Status))
		return
	}
	if !c.Want.IsSuccess() {
		return // both errored — parity holds, no payload to compare.
	}
	diff, err := bamlfuzz.SemanticDiffStrict("want_vs_baml", c.Want.JSON, res.JSON)
	if err != nil {
		t.Errorf("characterize %q: semantic diff: %v", c.Name, err)
		return
	}
	var failures []string
	if len(diff) > 0 {
		failures = append(failures, "want ≠ BAML (semantic)")
	}
	var orderDiff []bamlfuzz.SchemaOrderDiffEntry
	if c.PreserveSchemaOrder {
		od, oerr := bamlfuzz.SchemaOrderDiffWithChoices("want_vs_baml", c.Schema, c.Want.JSON, res.JSON, c.UnionChoices)
		if oerr != nil {
			failures = append(failures, fmt.Sprintf("schema order: %v", oerr))
		} else if len(od) > 0 {
			orderDiff = od
			failures = append(failures, "want ≠ BAML (order)")
		}
	}
	if len(failures) == 0 {
		return
	}
	env := &bamlfuzz.ParseDiffFailureEnvelope{
		CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
		PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
		UnionChoices: c.UnionChoices,
		PrefixIndex:  -1, Raw: c.Raw,
		ExpectedStatus: c.Want.Status, Expected: c.Want.JSON,
		BAML:         bamlfuzz.ParseOutcome{Parser: "baml_dynamic", JSON: res.JSON},
		SemanticDiff: diff,
		OrderDiff:    orderDiff,
		Failures:     failures,
	}
	dumpParseDiffAndFail(t, parseRecoveryArtifactDir, env, strings.Join(failures, "; "))
}

// runDynamicParseDiffLeg runs the issue-523 direct final-parse leg on an
// OracleCase: it parses the exact mock content through BAML directly,
// confirms the result matches the walker's Expected (using the same
// lenient #3690-tolerant comparators the call legs use), and diffs it
// against any registered native parser. With the default NoopParser the
// native differential is a vacuous skip, so today this leg only adds the
// BAML-direct-parse-vs-Expected anchor on top of the existing call/REST
// legs. The moment a native parser registers, the same corpus + rapid
// cases become live native-vs-BAML differentials.
func runDynamicParseDiffLeg(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, idx int) {
	t.Helper()
	t.Run("parse_diff", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		baml := bamlDynamicParser{dyn: dyn}
		native := bamlfuzz.RegisteredNativeParser()
		req := bamlfuzz.ParseRequest{
			Schema:              c.Schema,
			Raw:                 string(c.MockLLMContent),
			Stream:              false,
			PreserveSchemaOrder: c.PreserveSchemaOrder,
		}

		bamlRes, bamlErr := baml.Parse(ctx, req)
		if bamlErr != nil {
			env := dynParseDiffEnvelope(c, idx,
				bamlfuzz.ParseOutcome{Parser: baml.Name(), Error: bamlErr.Error()},
				nil, nil, []string{fmt.Sprintf("BAML direct parse errored: %v", bamlErr)})
			dumpParseDiffAndFail(t, dynamicOracleArtifactDir, env, fmt.Sprintf("BAML direct parse errored: %v", bamlErr))
			return
		}

		var (
			failures []string
			semDiff  []bamlfuzz.SemanticDiffEntry
			ordDiff  []bamlfuzz.SchemaOrderDiffEntry
		)
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_baml_parse", c.Expected, bamlRes.JSON); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_baml_parse diff: %v", err))
		} else if len(diff) > 0 {
			semDiff = diff
			failures = append(failures, "expected ≠ baml_parse")
		}
		if c.PreserveSchemaOrder {
			od, oerr := bamlfuzz.SchemaOrderDiffWithChoices("expected_vs_baml_parse", c.Schema, c.Expected, bamlRes.JSON, c.Metadata.UnionChoices)
			switch {
			case errors.Is(oerr, bamlfuzz.ErrSchemaOrderUnsupported):
				failures = append(failures, fmt.Sprintf("schema order unsupported: %v", oerr))
			case oerr != nil:
				failures = append(failures, fmt.Sprintf("schema order: %v", oerr))
			case len(od) > 0:
				ordDiff = od
				failures = append(failures, "expected ≠ baml_parse (order)")
			}
		}
		if len(failures) > 0 {
			env := dynParseDiffEnvelope(c, idx,
				bamlfuzz.ParseOutcome{Parser: baml.Name(), JSON: bamlRes.JSON}, semDiff, ordDiff, failures)
			dumpParseDiffAndFail(t, dynamicOracleArtifactDir, env, strings.Join(failures, "; "))
			return
		}

		// Native-vs-BAML differential (NoopParser → SkippedNative today).
		res := bamlfuzz.DiffParsers(ctx, baml, native, req, c.Metadata.UnionChoices)
		if !res.SkippedNative && len(res.Failures) > 0 {
			env := &bamlfuzz.ParseDiffFailureEnvelope{
				CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
				PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
				PrefixIndex: -1, Raw: string(c.MockLLMContent),
				ExpectedStatus: bamlfuzz.ParseStatusSuccess, Expected: c.Expected,
				SkippedNative: res.SkippedNative, BAML: res.BAML, Native: res.NativeOutcome(),
				SemanticDiff: res.SemanticDiff, OrderDiff: res.OrderDiff, Failures: res.Failures,
			}
			dumpParseDiffAndFail(t, dynamicOracleArtifactDir, env, strings.Join(res.Failures, "; "))
		}
	})
}

// dynParseDiffEnvelope builds a ParseDiffFailureEnvelope for the dynamic
// oracle's direct-parse leg, where BAML is compared against the walker's
// Expected rather than a native parser.
func dynParseDiffEnvelope(c bamlfuzz.OracleCase, idx int, baml bamlfuzz.ParseOutcome, semDiff []bamlfuzz.SemanticDiffEntry, ordDiff []bamlfuzz.SchemaOrderDiffEntry, failures []string) *bamlfuzz.ParseDiffFailureEnvelope {
	return &bamlfuzz.ParseDiffFailureEnvelope{
		CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
		PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
		PrefixIndex: -1, Raw: string(c.MockLLMContent),
		ExpectedStatus: bamlfuzz.ParseStatusSuccess, Expected: c.Expected,
		BAML:         baml,
		SemanticDiff: semDiff,
		OrderDiff:    ordDiff,
		Failures:     failures,
	}
}

// logObservedOutcome prints BAML's observed parse outcome in a stable,
// greppable form so a developer running BAMLFUZZ_PARSE_CAPTURE=1 can copy
// the values into the corpus `want`. Not a gate.
func logObservedOutcome(t *testing.T, leg, raw string, res bamlfuzz.ParseResult, parseErr error) {
	t.Helper()
	if parseErr != nil {
		t.Logf("CAPTURE %s raw=%q -> status=error err=%v", leg, raw, parseErr)
		return
	}
	t.Logf("CAPTURE %s raw=%q -> status=success json=%s", leg, raw, string(res.JSON))
}

// parseDiffEnvelope builds a ParseDiffFailureEnvelope from a
// ParseDiffResult for the native-vs-BAML differential leg.
func parseDiffEnvelope(c bamlfuzz.ParseRecoveryCase, idx, prefixIdx int, prefixName, raw string, stream bool, res bamlfuzz.ParseDiffResult) *bamlfuzz.ParseDiffFailureEnvelope {
	return &bamlfuzz.ParseDiffFailureEnvelope{
		CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
		PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
		UnionChoices: c.UnionChoices,
		Stream:       stream, PrefixIndex: prefixIdx, PrefixName: prefixName, Raw: raw,
		SkippedNative: res.SkippedNative,
		BAML:          res.BAML,
		Native:        res.NativeOutcome(),
		SemanticDiff:  res.SemanticDiff,
		OrderDiff:     res.OrderDiff,
		Failures:      res.Failures,
	}
}

// dumpParseDiffAndFail writes the envelope to the given artifact dir and
// fails the test with a message pointing at the replay path.
func dumpParseDiffAndFail(t *testing.T, dir string, env *bamlfuzz.ParseDiffFailureEnvelope, msg string) {
	t.Helper()
	path, err := bamlfuzz.WriteParseDiffReplayArtifact(dir, env)
	if err != nil {
		t.Errorf("write parse-diff artifact: %v", err)
		t.Errorf("%s", msg)
		return
	}
	t.Errorf("%s\nreplay: %s", msg, path)
}
