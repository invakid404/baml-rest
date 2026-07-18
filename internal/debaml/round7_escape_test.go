package debaml

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Phase 7C round-7: the native streaming/final string scanner decodes BAML's exact
// jsonish escape set instead of deferring to BAML. Every expectation below was
// LIVE-CAPTURED from BAML (DynamicParseRaw) — see the 7C type-space differential's
// `escaped`/`escapedq` axes for the generative proof; these lock the behavior in
// pure Go (no CFFI needed for the regression).
//
// Decoded: \" \\ \n \t \r \b \f. Kept LITERAL (backslash + next byte scanned
// normally, matching BAML, NOT standard JSON): \/ , unknown letters like \z, a
// \u sequence, and a backslash that runs to EOF.

func twoStr() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(
		bamlutils.OrderedKV("a", &bamlutils.DynamicProperty{Type: "string"}),
		bamlutils.OrderedKV("b", &bamlutils.DynamicProperty{Type: "string"}),
	)}
}

func TestParse_StringEscapes(t *testing.T) {
	s := twoStr()

	// ---- FINAL: complete inputs decode and re-encode byte-exact. ----
	final := []struct{ raw, want string }{
		// standard decoded escapes.
		{`{"a":"x\ty","b":"z"}`, "{\"a\":\"x\\ty\",\"b\":\"z\"}"},
		{`{"a":"x\ny","b":"z"}`, "{\"a\":\"x\\ny\",\"b\":\"z\"}"},
		{`{"a":"x\\y","b":"z"}`, "{\"a\":\"x\\\\y\",\"b\":\"z\"}"},
		{`{"a":"x\r\b\fy","b":"z"}`, "{\"a\":\"x\\r\\b\\fy\",\"b\":\"z\"}"},
		// embedded quote (\") in a COMPLETE frame closes cleanly on both sides.
		{`{"a":"he \"hi\"","b":"z"}`, "{\"a\":\"he \\\"hi\\\"\",\"b\":\"z\"}"},
	}
	for _, c := range final {
		res, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: c.raw, OutputSchema: s})
		if err != nil {
			t.Errorf("FINAL %q: unexpected err %v", c.raw, err)
			continue
		}
		if string(res.JSON) != c.want {
			t.Errorf("FINAL %q:\n got %q\nwant %q", c.raw, string(res.JSON), c.want)
		}
	}

	// ---- STREAM partial: decoded content emitted as the string builds. ----
	stream := []struct{ raw, want string }{
		{`{"a":"x\ty`, "{\"a\":\"x\\ty\",\"b\":null}"},  // tab decoded, string still open
		{`{"a":"x\n`, "{\"a\":\"x\\n\",\"b\":null}"},    // newline decoded
		{`{"a":"x\\y`, "{\"a\":\"x\\\\y\",\"b\":null}"}, // escaped backslash -> one backslash
		{`{"a":"x\`, "{\"a\":\"x\\\\\",\"b\":null}"},    // dangling backslash -> literal
	}
	for _, c := range stream {
		j, err := ParseNativeStreamPartial(context.Background(), s, c.raw)
		if err != nil {
			t.Errorf("STREAM %q: unexpected err %v", c.raw, err)
			continue
		}
		if string(j) != c.want {
			t.Errorf("STREAM %q:\n got %q\nwant %q", c.raw, string(j), c.want)
		}
	}
}
