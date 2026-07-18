package main

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// De-BAML Phase 7C — public SSE / NDJSON wire goldens for a native-owned dynamic
// stream. The differential (nanollmprepare/dynamic) proves the native parser
// reproduces BAML's flattened partial/final JSON byte-for-byte; this test proves
// the REAL public stream writer serializes that native-owned StreamResult sequence
// into the SAME public SSE / NDJSON bytes for BOTH modes, WITHOUT changing the
// wire format. The Data payloads below are the exact flattened partial/final JSON
// the 7C differential emits for the person_growing_object fixture (the content-free
// open object `{` -> {"name":null,"age":null} gap closure included).

// nativeStreamGoldenResults builds the native-owned StreamResult sequence a
// dynamic person stream produces: planned metadata, three partials (the
// content-free open object, a name-present partial, and the completed object),
// then the final. raw carries the accumulated assistant text (stream-with-raw).
func nativeStreamGoldenResults() []*workerplugin.StreamResult {
	return []*workerplugin.StreamResult{
		// Raw on a partial is the per-frame DELTA (consumeStream accumulates them);
		// the final carries the FULL raw. The deltas accumulate to the full text.
		{Kind: workerplugin.StreamResultKindMetadata, Data: []byte(`{"phase":"planned","path":"buildrequest"}`)},
		{Kind: workerplugin.StreamResultKindStream, Data: []byte(`{"name":null,"age":null}`), Raw: `{`},
		{Kind: workerplugin.StreamResultKindStream, Data: []byte(`{"name":"Ada","age":null}`), Raw: `"name":"Ada"`},
		{Kind: workerplugin.StreamResultKindStream, Data: []byte(`{"name":"Ada","age":36}`), Raw: `,"age":36}`},
		{Kind: workerplugin.StreamResultKindFinal, Data: []byte(`{"name":"Ada","age":36}`), Raw: `{"name":"Ada","age":36}`},
	}
}

func resultChan(results []*workerplugin.StreamResult) <-chan *workerplugin.StreamResult {
	ch := make(chan *workerplugin.StreamResult, len(results))
	for _, r := range results {
		ch <- r
	}
	close(ch)
	return ch
}

// renderSSE / renderNDJSON drive the REAL publishers into a buffer.
func renderSSE(results []*workerplugin.StreamResult, mode bamlutils.StreamMode) string {
	var buf bytes.Buffer
	pub := &SSEStreamWriterPublisher{w: bufio.NewWriter(&buf), cancel: func() {}, needsRaw: mode.NeedsRaw()}
	consumeStream(resultChan(results), pub, mode, true, nil)
	_ = pub.w.Flush()
	return buf.String()
}

func renderNDJSON(results []*workerplugin.StreamResult, mode bamlutils.StreamMode) string {
	var buf bytes.Buffer
	pub := &NDJSONStreamWriterPublisher{w: bufio.NewWriter(&buf), cancel: func() {}}
	consumeStream(resultChan(results), pub, mode, true, nil)
	_ = pub.w.Flush()
	return buf.String()
}

func TestStream7C_PublicWireGolden_SSE_Stream(t *testing.T) {
	got := renderSSE(nativeStreamGoldenResults(), bamlutils.StreamModeStream)
	want := "event: metadata\n" +
		"data: {\"phase\":\"planned\",\"path\":\"buildrequest\"}\n\n" +
		"data: {\"name\":null,\"age\":null}\n\n" +
		"data: {\"name\":\"Ada\",\"age\":null}\n\n" +
		"data: {\"name\":\"Ada\",\"age\":36}\n\n" +
		"event: final\n" +
		"data: {\"name\":\"Ada\",\"age\":36}\n\n"
	if got != want {
		t.Errorf("SSE stream wire golden mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestStream7C_PublicWireGolden_SSE_StreamWithRaw(t *testing.T) {
	got := renderSSE(nativeStreamGoldenResults(), bamlutils.StreamModeStreamWithRaw)
	want := "event: metadata\n" +
		"data: {\"phase\":\"planned\",\"path\":\"buildrequest\"}\n\n" +
		"data: {\"data\":{\"name\":null,\"age\":null},\"raw\":\"{\"}\n\n" +
		"data: {\"data\":{\"name\":\"Ada\",\"age\":null},\"raw\":\"{\\\"name\\\":\\\"Ada\\\"\"}\n\n" +
		"data: {\"data\":{\"name\":\"Ada\",\"age\":36},\"raw\":\"{\\\"name\\\":\\\"Ada\\\",\\\"age\\\":36}\"}\n\n" +
		"event: final\n" +
		"data: {\"data\":{\"name\":\"Ada\",\"age\":36},\"raw\":\"{\\\"name\\\":\\\"Ada\\\",\\\"age\\\":36}\"}\n\n"
	if got != want {
		t.Errorf("SSE stream-with-raw wire golden mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestStream7C_PublicWireGolden_NDJSON_Stream(t *testing.T) {
	got := renderNDJSON(nativeStreamGoldenResults(), bamlutils.StreamModeStream)
	want := "{\"type\":\"metadata\",\"data\":{\"phase\":\"planned\",\"path\":\"buildrequest\"}}\n" +
		"{\"type\":\"data\",\"data\":{\"name\":null,\"age\":null}}\n" +
		"{\"type\":\"data\",\"data\":{\"name\":\"Ada\",\"age\":null}}\n" +
		"{\"type\":\"data\",\"data\":{\"name\":\"Ada\",\"age\":36}}\n" +
		"{\"type\":\"final\",\"data\":{\"name\":\"Ada\",\"age\":36}}\n"
	if got != want {
		t.Errorf("NDJSON stream wire golden mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestStream7C_PublicWireGolden_NDJSON_StreamWithRaw(t *testing.T) {
	got := renderNDJSON(nativeStreamGoldenResults(), bamlutils.StreamModeStreamWithRaw)
	want := "{\"type\":\"metadata\",\"data\":{\"phase\":\"planned\",\"path\":\"buildrequest\"}}\n" +
		"{\"type\":\"data\",\"data\":{\"name\":null,\"age\":null},\"raw\":\"{\"}\n" +
		"{\"type\":\"data\",\"data\":{\"name\":\"Ada\",\"age\":null},\"raw\":\"{\\\"name\\\":\\\"Ada\\\"\"}\n" +
		"{\"type\":\"data\",\"data\":{\"name\":\"Ada\",\"age\":36},\"raw\":\"{\\\"name\\\":\\\"Ada\\\",\\\"age\\\":36}\"}\n" +
		"{\"type\":\"final\",\"data\":{\"name\":\"Ada\",\"age\":36},\"raw\":\"{\\\"name\\\":\\\"Ada\\\",\\\"age\\\":36}\"}\n"
	if got != want {
		t.Errorf("NDJSON stream-with-raw wire golden mismatch:\n got %q\nwant %q", got, want)
	}
}
