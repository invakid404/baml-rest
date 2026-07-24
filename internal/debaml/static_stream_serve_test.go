package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// De-BAML Phase 3b — the static-stream admission lockstep + parse contract. Phase B
// ENABLES the exact five-arm JSON alias: it is now a proven static-STREAM family
// (admission admits, both parse entrypoints serve), while every other bundle (nil, a
// non-alias final family, the wider JsonValue) still declines. The byte/event-exact
// proof lives in the gated differentials (staticoracle); this pins the pure-Go gates.

func TestStaticStream_JSONAliasAdmitted(t *testing.T) {
	ctx := context.Background()
	b := jsonAliasBundle(t)

	// The JSON alias is BOTH the proven FINAL family and (Phase B) the proven STREAM family.
	if !IsProvenRecursiveAliasStaticFamily(b) {
		t.Fatal("JSON alias must be the proven FINAL alias family (Phase-3a pin unchanged)")
	}
	if !IsProvenRecursiveAliasStaticStreamFamily(b) {
		t.Fatal("Phase B: JSON alias must be a proven static-STREAM family")
	}
	if err := SupportsNativeStaticStreamBundle(b); err != nil {
		t.Fatalf("Phase B: SupportsNativeStaticStreamBundle(JSON) must admit, got: %v", err)
	}

	// The partial entrypoint SERVES the proven cadence (spot pins from the differential).
	partials := []struct{ prefix, want string }{
		{`1`, `1`}, {`tru`, `"tru"`}, {`true`, `true`}, {`nul`, `"nul"`}, {`null`, `[]`},
		{`1.5`, `[]`}, {`[1`, `[1]`}, {`{"a":1,"`, `{"a":"1,\""}`}, {`{"a":1,"b":"two"}`, `{"a":1,"b":"two"}`},
	}
	for _, p := range partials {
		res, err := ParseStaticStreamPartial(ctx, b, p.prefix)
		if err != nil {
			t.Fatalf("ParseStaticStreamPartial(%q) must emit, got err: %v", p.prefix, err)
		}
		if string(res.JSON) != p.want {
			t.Fatalf("ParseStaticStreamPartial(%q) = %s, want %s", p.prefix, res.JSON, p.want)
		}
	}

	// The final entrypoint reuses the proven Phase-3a final coercer.
	if res, err := ParseStaticStreamFinal(ctx, b, `{"z":1,"a":2}`); err != nil {
		t.Fatalf("ParseStaticStreamFinal must serve, got err: %v", err)
	} else if string(res.JSON) != `{"a":2,"z":1}` {
		t.Fatalf("ParseStaticStreamFinal = %s, want sorted-public {\"a\":2,\"z\":1}", res.JSON)
	}
}

// TestStaticStream_NilAndNonAliasDecline pins the defensive gates: a nil bundle and a
// non-alias (final-admitted) bundle both decline the static-stream family and support
// gate, so nothing outside the exact JSON-alias set can claim (stream admission is NOT
// inherited from final).
func TestStaticStream_NilAndNonAliasDecline(t *testing.T) {
	ctx := context.Background()

	if IsProvenRecursiveAliasStaticStreamFamily(nil) {
		t.Fatal("nil bundle must not be a proven static-stream family")
	}
	if err := SupportsNativeStaticStreamBundle(nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil bundle must decline SupportsNativeStaticStreamBundle with the unsupported sentinel; got %v", err)
	}
	if _, err := ParseStaticStreamPartial(ctx, nil, `1`); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil bundle must decline ParseStaticStreamPartial with the unsupported sentinel; got %v", err)
	}
	if _, err := ParseStaticStreamFinal(ctx, nil, `1`); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil bundle must decline ParseStaticStreamFinal with the unsupported sentinel; got %v", err)
	}

	// A non-alias final-admitted bundle (StaticAnswer{answer,confidence}) is NOT a
	// static-stream family either — stream admission is not inherited from final.
	nonAlias := lowerOrFatal(t, staticAnswerDescriptor())
	if IsProvenRecursiveAliasStaticStreamFamily(nonAlias) {
		t.Fatal("non-alias final bundle must NOT be a proven static-stream family")
	}
	if err := SupportsNativeStaticStreamBundle(nonAlias); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("non-alias final bundle must decline SupportsNativeStaticStreamBundle with the unsupported sentinel (stream admission is not inherited from final); got %v", err)
	}
}
