package bamlfuzz

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeParser is a scripted Parser for the differential unit tests. It
// returns a fixed JSON / error regardless of the request, so each test
// can pin one (BAML, native) outcome pair and assert how DiffParsers
// reconciles them.
type fakeParser struct {
	name string
	json json.RawMessage
	err  error
}

func (p fakeParser) Name() string { return p.name }

func (p fakeParser) Parse(context.Context, ParseRequest) (ParseResult, error) {
	if p.err != nil {
		return ParseResult{}, p.err
	}
	return ParseResult{JSON: p.json}, nil
}

// twoFieldSchema is a tiny Root{a:string, b:string} schema used by the
// order-mismatch case; declaration order is a, then b.
func twoFieldSchema() FuzzSchema {
	return FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "a", Type: FuzzType{Kind: KindString}},
				{Name: "b", Type: FuzzType{Kind: KindString}},
			},
		}},
		RootClass: "Root",
	}
}

func TestDiffParsersNoopNativeSkips(t *testing.T) {
	baml := fakeParser{name: "baml", json: json.RawMessage(`{"name":"Ada"}`)}
	res := DiffParsers(context.Background(), baml, NoopParser{}, ParseRequest{Raw: "x"}, nil)
	if !res.SkippedNative {
		t.Fatalf("expected SkippedNative=true with NoopParser, got %+v", res)
	}
	if len(res.Failures) != 0 {
		t.Fatalf("expected no failures on native skip, got %v", res.Failures)
	}
	if res.BAML.JSON == nil {
		t.Fatalf("expected BAML outcome captured even when native skipped")
	}
}

func TestDiffParsersBothSucceedMatch(t *testing.T) {
	js := json.RawMessage(`{"name":"Ada","age":36}`)
	// Key order differs but PreserveSchemaOrder is off → semantic match.
	baml := fakeParser{name: "baml", json: js}
	native := fakeParser{name: "native", json: json.RawMessage(`{"age":36,"name":"Ada"}`)}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) != 0 {
		t.Fatalf("expected match, got failures %v (diff %v)", res.Failures, res.SemanticDiff)
	}
	if res.SkippedNative {
		t.Fatalf("native should not be skipped when it returns a real result")
	}
}

func TestDiffParsersBothErrorPass(t *testing.T) {
	baml := fakeParser{name: "baml", err: errors.New("baml: truncated input")}
	native := fakeParser{name: "native", err: errors.New("native: unexpected eof")}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) != 0 {
		t.Fatalf("both-error should pass parity, got %v", res.Failures)
	}
	if res.BAML.Error == "" || res.Native.Error == "" {
		t.Fatalf("expected both error strings captured, got %+v", res)
	}
}

func TestDiffParsersBamlSuccessNativeError(t *testing.T) {
	baml := fakeParser{name: "baml", json: json.RawMessage(`{"name":"Ada"}`)}
	native := fakeParser{name: "native", err: errors.New("native: rejected")}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) == 0 {
		t.Fatalf("expected parity failure when BAML succeeds and native errors")
	}
}

func TestDiffParsersBamlErrorNativeSuccess(t *testing.T) {
	baml := fakeParser{name: "baml", err: errors.New("baml: rejected")}
	native := fakeParser{name: "native", json: json.RawMessage(`{"name":"Ada"}`)}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) == 0 {
		t.Fatalf("expected parity failure when BAML errors and native succeeds")
	}
}

func TestDiffParsersJSONMismatch(t *testing.T) {
	baml := fakeParser{name: "baml", json: json.RawMessage(`{"name":"Ada","age":36}`)}
	native := fakeParser{name: "native", json: json.RawMessage(`{"name":"Ada","age":37}`)}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) == 0 {
		t.Fatalf("expected semantic-mismatch failure")
	}
	if len(res.SemanticDiff) == 0 {
		t.Fatalf("expected a SemanticDiff entry for the differing field")
	}
}

// A leaked null key on the native side must NOT be tolerated by the
// strict comparator (unlike SemanticDiff for the call oracles).
func TestDiffParsersStrictRejectsExtraNullKey(t *testing.T) {
	baml := fakeParser{name: "baml", json: json.RawMessage(`{"name":"Ada"}`)}
	native := fakeParser{name: "native", json: json.RawMessage(`{"name":"Ada","extra":null}`)}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) == 0 || len(res.SemanticDiff) == 0 {
		t.Fatalf("strict comparator must flag an extra null key, got %+v", res)
	}
}

func TestDiffParsersOrderMismatch(t *testing.T) {
	schema := twoFieldSchema()
	// Same key set, both valid, but native flips declaration order.
	baml := fakeParser{name: "baml", json: json.RawMessage(`{"a":"x","b":"y"}`)}
	native := fakeParser{name: "native", json: json.RawMessage(`{"b":"y","a":"x"}`)}
	req := ParseRequest{Raw: "x", Schema: schema, PreserveSchemaOrder: true}
	res := DiffParsers(context.Background(), baml, native, req, nil)
	if len(res.OrderDiff) == 0 {
		t.Fatalf("expected an OrderDiff entry for the flipped key order, got %+v", res)
	}
	hasOrderFailure := false
	for _, f := range res.Failures {
		if f == "baml ≠ native (order)" {
			hasOrderFailure = true
		}
	}
	if !hasOrderFailure {
		t.Fatalf("expected an order failure reason, got %v", res.Failures)
	}
}

// With PreserveSchemaOrder off, the same flipped order is a pass: only
// semantic equality is gated.
func TestDiffParsersOrderIgnoredWhenPreserveOff(t *testing.T) {
	schema := twoFieldSchema()
	baml := fakeParser{name: "baml", json: json.RawMessage(`{"a":"x","b":"y"}`)}
	native := fakeParser{name: "native", json: json.RawMessage(`{"b":"y","a":"x"}`)}
	req := ParseRequest{Raw: "x", Schema: schema, PreserveSchemaOrder: false}
	res := DiffParsers(context.Background(), baml, native, req, nil)
	if len(res.Failures) != 0 {
		t.Fatalf("preserve-off flipped order should pass, got %v", res.Failures)
	}
}

func TestDiffParsersBamlUnavailableIsHarnessFailure(t *testing.T) {
	baml := fakeParser{name: "baml", err: ErrParserUnavailable}
	res := DiffParsers(context.Background(), baml, NoopParser{}, ParseRequest{Raw: "x"}, nil)
	if res.SkippedNative {
		t.Fatalf("BAML-unavailable must not be a native skip")
	}
	if len(res.Failures) == 0 {
		t.Fatalf("BAML ErrParserUnavailable must be a harness failure")
	}
}

func TestDiffParsersBothSucceedEmptyJSONFails(t *testing.T) {
	baml := fakeParser{name: "baml", json: json.RawMessage(``)}
	native := fakeParser{name: "native", json: json.RawMessage(`{"name":"Ada"}`)}
	res := DiffParsers(context.Background(), baml, native, ParseRequest{Raw: "x"}, nil)
	if len(res.Failures) == 0 {
		t.Fatalf("empty BAML JSON on a reported success must fail")
	}
}

func TestDiffParserPrefixesGrowthAndDiff(t *testing.T) {
	// Both parsers echo the same final JSON regardless of prefix, so the
	// per-prefix diffs all pass; the monotonicity assertion is the focus.
	js := json.RawMessage(`{"name":"Ada"}`)
	baml := fakeParser{name: "baml", json: js}
	native := fakeParser{name: "native", json: js}
	good := []string{"{", `{"name"`, `{"name":"Ada"}`}
	results := DiffParserPrefixes(context.Background(), baml, native, ParseRequest{}, good, nil)
	if len(results) != len(good) {
		t.Fatalf("expected %d results, got %d", len(good), len(results))
	}
	for i, r := range results {
		if len(r.Failures) != 0 {
			t.Fatalf("prefix %d unexpectedly failed: %v", i, r.Failures)
		}
	}

	// A non-growing sequence must flag the offending step.
	bad := []string{`{"name"`, "{"}
	badRes := DiffParserPrefixes(context.Background(), baml, native, ParseRequest{}, bad, nil)
	if len(badRes) != 2 || len(badRes[1].Failures) == 0 {
		t.Fatalf("expected monotonicity failure on shrinking prefix, got %+v", badRes)
	}
}

func TestRegisterNativeParserRoundTrip(t *testing.T) {
	if _, ok := RegisteredNativeParser().(NoopParser); !ok {
		t.Fatalf("default registered parser should be NoopParser, got %T", RegisteredNativeParser())
	}
	fake := fakeParser{name: "fake"}
	restore := RegisterNativeParser(fake)
	if got := RegisteredNativeParser(); got.Name() != "fake" {
		t.Fatalf("expected fake parser registered, got %q", got.Name())
	}
	restore()
	if _, ok := RegisteredNativeParser().(NoopParser); !ok {
		t.Fatalf("restore should reinstate NoopParser, got %T", RegisteredNativeParser())
	}
	// Idempotent restore must not corrupt the registry.
	restore()
	if _, ok := RegisteredNativeParser().(NoopParser); !ok {
		t.Fatalf("second restore must be a no-op, got %T", RegisteredNativeParser())
	}
	// nil registration falls back to NoopParser.
	restore2 := RegisterNativeParser(nil)
	defer restore2()
	if _, ok := RegisteredNativeParser().(NoopParser); !ok {
		t.Fatalf("nil registration should install NoopParser, got %T", RegisteredNativeParser())
	}
}
