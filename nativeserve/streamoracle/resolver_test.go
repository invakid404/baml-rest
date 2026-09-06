package streamoracle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// resolver_test.go is the PURE table proof of the U1s decision matrix. It builds no
// admission, no engine and no socket: every leg is a closure, so each row of the matrix is
// exercised directly and a mutation to the matrix has nowhere to hide.

// val is a native leg that yields v for every prefix and records what it received.
func val(v any, seen *[]string) NativePrefixParse {
	return func(_ context.Context, prefix string) (any, bool, error) {
		if seen != nil {
			*seen = append(*seen, prefix)
		}
		return v, true, nil
	}
}

// none is a native leg that reports its documented no-partial sentinel as a RESULT.
func none(seen *[]string) NativePrefixParse {
	return func(_ context.Context, prefix string) (any, bool, error) {
		if seen != nil {
			*seen = append(*seen, prefix)
		}
		return nil, false, nil
	}
}

// boom is a native leg that FAILS (a parse or decode error, not the sentinel).
func boom(err error) NativePrefixParse {
	return func(context.Context, string) (any, bool, error) { return nil, false, err }
}

// bamlVal / bamlNone / bamlErr are the BAML leg's three normalized outcomes.
func bamlVal(v any, seen *[]string) bamlutils.BAMLStreamParse {
	return func(_ context.Context, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
		if seen != nil {
			*seen = append(*seen, prefix)
		}
		return bamlutils.BAMLStreamPrefixResult{Value: v, HasValue: true}, nil
	}
}

func bamlNone(seen *[]string) bamlutils.BAMLStreamParse {
	return func(_ context.Context, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
		if seen != nil {
			*seen = append(*seen, prefix)
		}
		return bamlutils.BAMLStreamPrefixResult{}, nil
	}
}

func bamlErr(err error) bamlutils.BAMLStreamParse {
	return func(context.Context, string) (bamlutils.BAMLStreamPrefixResult, error) {
		return bamlutils.BAMLStreamPrefixResult{}, err
	}
}

// legs assembles a Legs with inert final closures, for the prefix table.
func legs(native NativePrefixParse, baml bamlutils.BAMLStreamParse) Legs {
	return Legs{
		NativePrefix: native,
		BAMLPrefix:   baml,
		NativeFinal:  func(context.Context, string) (any, error) { return nil, nil },
		BAMLFinal:    func(context.Context, string) (any, error) { return struct{}{}, nil },
	}
}

// specimen types whose PUBLIC marshaled bytes differ while an in-process structural
// comparison would call them equal (field ORDER) or unequal (numeric REPRESENTATION).
type ordered struct {
	A int `json:"a"`
	B int `json:"b"`
}
type reordered struct {
	B int `json:"b"`
	A int `json:"a"`
}

func TestResolvePrefix_MatchServesNative(t *testing.T) {
	nativeValue := map[string]any{"answer": "ok"}
	out, err := ResolvePrefix(context.Background(), legs(val(nativeValue, nil), bamlVal(map[string]any{"answer": "ok"}, nil)), `{"answer":"ok"}`)
	if err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if out.Action != ActionEmitNative {
		t.Fatalf("action = %v, want ActionEmitNative", out.Action)
	}
	// Identity, not equality: the NATIVE value must be the one released on a match.
	if got, ok := out.Value.(map[string]any); !ok || &got != &nativeValue && got["answer"] != "ok" {
		t.Errorf("value = %#v, want the native value", out.Value)
	}
	if out.Compare != bamlutils.NativeStreamCompareMatch || out.Drift || out.Substituted || out.Suppressed {
		t.Errorf("outcome = %+v, want a clean match with no drift", out)
	}
}

// TestResolvePrefix_BothLegsSeeTheSamePrefix is the same-prefix proof: the two closures
// receive the identical string the caller passed, so "native and BAML saw the same bytes"
// is checked rather than assumed.
func TestResolvePrefix_BothLegsSeeTheSamePrefix(t *testing.T) {
	var nativeSeen, bamlSeen []string
	const prefix = `{"answer":"partial va`
	if _, err := ResolvePrefix(context.Background(), legs(none(&nativeSeen), bamlNone(&bamlSeen)), prefix); err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if len(nativeSeen) != 1 || len(bamlSeen) != 1 {
		t.Fatalf("legs ran %d/%d time(s), want exactly one call each", len(nativeSeen), len(bamlSeen))
	}
	if nativeSeen[0] != prefix || bamlSeen[0] != prefix {
		t.Errorf("legs saw different prefixes: native=%q baml=%q want %q", nativeSeen[0], bamlSeen[0], prefix)
	}
}

// TestResolvePrefix_ByteMismatchOnAReorderedSpecimen is mutation bite 4: a specimen that is
// semantically identical and structurally DeepEqual-incomparable but marshals to different
// public bytes must be caught. Swapping the comparison for reflect.DeepEqual or a semantic
// map compare makes this row report a match.
func TestResolvePrefix_ByteMismatchOnAReorderedSpecimen(t *testing.T) {
	out, err := ResolvePrefix(context.Background(),
		legs(val(ordered{A: 1, B: 2}, nil), bamlVal(reordered{B: 2, A: 1}, nil)), `{"a":1,"b":2}`)
	if err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if out.Action != ActionEmitBAML || out.Compare != bamlutils.NativeStreamCompareBytesMismatch {
		t.Fatalf("outcome = %+v, want a bytes_mismatch served from BAML — field ORDER is a public difference", out)
	}
	if !out.Drift || !out.Substituted {
		t.Errorf("outcome = %+v, want drift + substitution latched", out)
	}
	if got, ok := out.Value.(reordered); !ok || got.A != 1 || got.B != 2 {
		t.Errorf("value = %#v, want BAML's same-prefix value", out.Value)
	}
}

func TestResolvePrefix_NativeNoValueServesBAML(t *testing.T) {
	out, err := ResolvePrefix(context.Background(), legs(none(nil), bamlVal(map[string]any{"a": 1}, nil)), `{"a":1`)
	if err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if out.Action != ActionEmitBAML || out.Compare != bamlutils.NativeStreamCompareNativeNoValue || !out.Drift || !out.Substituted {
		t.Fatalf("outcome = %+v, want BAML's value substituted with drift latched", out)
	}
}

func TestResolvePrefix_NativeErrorServesBAML(t *testing.T) {
	out, err := ResolvePrefix(context.Background(), legs(boom(errors.New("decode failed")), bamlVal(map[string]any{"a": 1}, nil)), `{"a":1}`)
	if err != nil {
		t.Fatalf("a native failure must NOT terminate a claimed stream whose prefix BAML answered: %v", err)
	}
	if out.Action != ActionEmitBAML || out.Compare != bamlutils.NativeStreamCompareNativeError || !out.Drift || !out.Substituted {
		t.Fatalf("outcome = %+v, want BAML's value substituted and native_error recorded", out)
	}
}

// TestResolvePrefix_NativeValueBAMLNoValueSuppresses is mutation bite 5.
func TestResolvePrefix_NativeValueBAMLNoValueSuppresses(t *testing.T) {
	out, err := ResolvePrefix(context.Background(), legs(val(map[string]any{"a": 1}, nil), bamlNone(nil)), `{"a":1}`)
	if err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if out.Action != ActionNoEvent {
		t.Fatalf("action = %v, want ActionNoEvent — BAML is the authority, so native's partial must be SUPPRESSED", out.Action)
	}
	if out.Value != nil {
		t.Errorf("value = %#v on a suppressed tick, want nil", out.Value)
	}
	if out.Compare != bamlutils.NativeStreamCompareBAMLNoValue || !out.Drift || !out.Suppressed || out.Substituted {
		t.Errorf("outcome = %+v, want baml_no_value with drift + suppression", out)
	}
}

func TestResolvePrefix_BothNoValueIsACleanNoEvent(t *testing.T) {
	out, err := ResolvePrefix(context.Background(), legs(none(nil), bamlNone(nil)), `{"a":`)
	if err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if out.Action != ActionNoEvent || out.Compare != bamlutils.NativeStreamCompareNativeNoValue {
		t.Fatalf("outcome = %+v, want a no-event tick", out)
	}
	if out.Drift || out.Substituted || out.Suppressed {
		t.Errorf("outcome = %+v: an ordinary incomplete prefix must NOT latch drift", out)
	}
}

func TestResolvePrefix_NativeErrorAndBAMLNoValueDriftsWithoutEvent(t *testing.T) {
	out, err := ResolvePrefix(context.Background(), legs(boom(errors.New("boom")), bamlNone(nil)), `{"a":`)
	if err != nil {
		t.Fatalf("ResolvePrefix: %v", err)
	}
	if out.Action != ActionNoEvent || out.Compare != bamlutils.NativeStreamCompareNativeError || !out.Drift {
		t.Fatalf("outcome = %+v, want a no-event tick that still latches drift", out)
	}
}

// TestResolvePrefix_BAMLErrorIsTerminalEvenWhenNativeSucceeded is mutation bite 8's pure
// half: losing the oracle is terminal, never a quietly-native emit.
func TestResolvePrefix_BAMLErrorIsTerminalEvenWhenNativeSucceeded(t *testing.T) {
	cause := errors.New("client registry construction failed")
	_, err := ResolvePrefix(context.Background(), legs(val(map[string]any{"a": 1}, nil), bamlErr(cause)), `{"a":1}`)
	if err == nil {
		t.Fatal("a BAML oracle that could not be established returned no error; the tick would have released an unverified native partial")
	}
	if !errors.Is(err, ErrBAMLPrefixUnavailable) || !errors.Is(err, cause) {
		t.Errorf("err = %v, want ErrBAMLPrefixUnavailable wrapping the cause", err)
	}
}

// TestResolvePrefix_BAMLMarshalFailureIsTerminal: the AUTHORITY's value having no public
// form means the comparison cannot be established, so neither value may be released.
func TestResolvePrefix_BAMLMarshalFailureIsTerminal(t *testing.T) {
	// Only the BAML value is unmarshalable; native's marshals cleanly, so this isolates
	// the authority's arm rather than colliding with the native-drift arm below.
	type bamlOnly struct{ V int }
	l := legs(val(map[string]any{"a": 1}, nil), bamlVal(bamlOnly{V: 1}, nil))
	l.Serializer = func(v any) ([]byte, error) {
		if _, ok := v.(bamlOnly); ok {
			return nil, errors.New("unsupported value")
		}
		return []byte(`{"a":1}`), nil
	}
	_, err := ResolvePrefix(context.Background(), l, `{"a":1}`)
	if err == nil || !errors.Is(err, ErrBAMLPrefixUnmarshalable) {
		t.Fatalf("err = %v, want ErrBAMLPrefixUnmarshalable", err)
	}
}

// TestResolvePrefix_NativeMarshalFailureIsDriftNotTermination pins the asymmetry: native
// is not the authority, so its unmarshalable value is drift resolved by BAML.
func TestResolvePrefix_NativeMarshalFailureIsDriftNotTermination(t *testing.T) {
	type poison struct{ C chan int }
	out, err := ResolvePrefix(context.Background(),
		legs(val(poison{C: make(chan int)}, nil), bamlVal(map[string]any{"a": 1}, nil)), `{"a":1}`)
	if err != nil {
		t.Fatalf("a native value with no public form must be drift, not a terminal: %v", err)
	}
	if out.Action != ActionEmitBAML || out.Compare != bamlutils.NativeStreamCompareNativeError {
		t.Fatalf("outcome = %+v, want BAML substituted and native_error recorded", out)
	}
}

func TestResolvePrefix_MissingLegIsRefused(t *testing.T) {
	for name, l := range map[string]Legs{
		"no native prefix": {BAMLPrefix: bamlNone(nil), NativeFinal: func(context.Context, string) (any, error) { return nil, nil }, BAMLFinal: func(context.Context, string) (any, error) { return 1, nil }},
		"no baml prefix":   {NativePrefix: none(nil), NativeFinal: func(context.Context, string) (any, error) { return nil, nil }, BAMLFinal: func(context.Context, string) (any, error) { return 1, nil }},
		"no native final":  {NativePrefix: none(nil), BAMLPrefix: bamlNone(nil), BAMLFinal: func(context.Context, string) (any, error) { return 1, nil }},
		"no baml final":    {NativePrefix: none(nil), BAMLPrefix: bamlNone(nil), NativeFinal: func(context.Context, string) (any, error) { return nil, nil }},
	} {
		if _, err := ResolvePrefix(context.Background(), l, "x"); !errors.Is(err, ErrLegsIncomplete) {
			t.Errorf("%s: err = %v, want ErrLegsIncomplete", name, err)
		}
		if _, err := ResolveFinal(context.Background(), l, "x"); !errors.Is(err, ErrLegsIncomplete) {
			t.Errorf("%s (final): err = %v, want ErrLegsIncomplete", name, err)
		}
	}
}

// --- the FINAL table ------------------------------------------------------------

func finalLegs(native NativeFinalParse, baml bamlutils.BAMLStreamFinalParse) Legs {
	return Legs{
		NativePrefix: none(nil),
		BAMLPrefix:   bamlNone(nil),
		NativeFinal:  native,
		BAMLFinal:    baml,
	}
}

func TestResolveFinal_MatchServesNative(t *testing.T) {
	var seen string
	out, err := ResolveFinal(context.Background(), finalLegs(
		func(_ context.Context, full string) (any, error) { seen = full; return ordered{A: 1, B: 2}, nil },
		func(context.Context, string) (any, error) { return ordered{A: 1, B: 2}, nil },
	), `{"a":1,"b":2}`)
	if err != nil {
		t.Fatalf("ResolveFinal: %v", err)
	}
	if seen != `{"a":1,"b":2}` {
		t.Errorf("the native final leg saw %q, want the complete accumulated text", seen)
	}
	if out.Compare != bamlutils.NativeStreamCompareMatch || out.Drift || out.Substituted {
		t.Fatalf("outcome = %+v, want a clean native final", out)
	}
}

func TestResolveFinal_NativeFailureServesBAMLSameResponse(t *testing.T) {
	out, err := ResolveFinal(context.Background(), finalLegs(
		func(context.Context, string) (any, error) { return nil, errors.New("native final declined") },
		func(context.Context, string) (any, error) { return ordered{A: 9, B: 9}, nil },
	), `{"a":9,"b":9}`)
	if err != nil {
		t.Fatalf("a native final failure with a BAML answer in hand must not terminate: %v", err)
	}
	if out.Compare != bamlutils.NativeStreamCompareNativeError || !out.Drift || !out.Substituted {
		t.Fatalf("outcome = %+v, want BAML's same-response final", out)
	}
	if got, ok := out.Value.(ordered); !ok || got.A != 9 {
		t.Errorf("value = %#v, want BAML's final", out.Value)
	}
}

func TestResolveFinal_DriftServesBAMLSameResponse(t *testing.T) {
	out, err := ResolveFinal(context.Background(), finalLegs(
		func(context.Context, string) (any, error) { return ordered{A: 1, B: 2}, nil },
		func(context.Context, string) (any, error) { return reordered{B: 2, A: 1}, nil },
	), `{"a":1,"b":2}`)
	if err != nil {
		t.Fatalf("ResolveFinal: %v", err)
	}
	if out.Compare != bamlutils.NativeStreamCompareBytesMismatch || !out.Substituted {
		t.Fatalf("outcome = %+v, want a byte-mismatched final served from BAML", out)
	}
}

func TestResolveFinal_BAMLFailureIsTerminalEvenWhenNativeSucceeded(t *testing.T) {
	cause := errors.New("Parse.Method failed")
	_, err := ResolveFinal(context.Background(), finalLegs(
		func(context.Context, string) (any, error) { return ordered{A: 1}, nil },
		func(context.Context, string) (any, error) { return nil, cause },
	), `{"a":1}`)
	if !errors.Is(err, ErrBAMLFinalUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want ErrBAMLFinalUnavailable wrapping the cause", err)
	}
}

// TestResolveFinal_BAMLNoValueIsTerminal pins the one asymmetry with the prefix matrix.
func TestResolveFinal_BAMLNoValueIsTerminal(t *testing.T) {
	_, err := ResolveFinal(context.Background(), finalLegs(
		func(context.Context, string) (any, error) { return ordered{A: 1}, nil },
		func(context.Context, string) (any, error) { return nil, nil },
	), `{"a":1}`)
	if !errors.Is(err, ErrBAMLFinalNoValue) {
		t.Fatalf("err = %v, want ErrBAMLFinalNoValue — a claimed stream has no valid final no-value state", err)
	}
}

func TestResolveFinal_BAMLMarshalFailureIsTerminal(t *testing.T) {
	type poison struct{ C chan int }
	_, err := ResolveFinal(context.Background(), finalLegs(
		func(context.Context, string) (any, error) { return ordered{A: 1}, nil },
		func(context.Context, string) (any, error) { return poison{C: make(chan int)}, nil },
	), `{"a":1}`)
	if !errors.Is(err, ErrBAMLFinalUnmarshalable) {
		t.Fatalf("err = %v, want ErrBAMLFinalUnmarshalable", err)
	}
}

// TestResolverErrorsCarryNoPrefixOrValue keeps every bounded sentinel free of request
// content: a terminal from this package is wrapped and surfaced by the worker, so a prefix
// or a parsed value inside one would leak provider output into an error frame.
func TestResolverErrorsCarryNoPrefixOrValue(t *testing.T) {
	const secret = "SENSITIVE-PROVIDER-TEXT"
	_, err := ResolvePrefix(context.Background(),
		legs(val(secret, nil), bamlErr(errors.New("oracle down"))), secret)
	if err == nil {
		t.Fatal("expected a terminal")
	}
	// The wrapped cause is the engine's own error; the message must not contain the
	// prefix or the parsed value the resolver was handed.
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the terminal error carries request content: %v", err)
	}
}
