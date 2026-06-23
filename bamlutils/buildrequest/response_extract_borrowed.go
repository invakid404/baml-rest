// Package buildrequest — response_extract_borrowed.go is the aliasing twin of
// response_extract.go's ExtractResponseContent / ExtractResponseContentBytes.
//
// It exists for the fasthttp BORROWED unary lane (#487 Stage 3), where the
// response body aliases pooled transport storage that the orchestrator
// releases per-attempt. cloudwego/gjson.GetBytes always copies matched
// Raw/Str out of its input, so the byte extractor can never alias; but
// gjson.Get over a *string* returns json[s:e] / json[1:i] substrings that
// ALIAS the source for unescaped scalars. ExtractResponseContentBorrowed
// leans on that: it validates and extracts via gjson.Get over an unsafe
// string view of the borrowed body, so unescaped scalar / single-block
// content flows out as a zero-copy alias of the borrowed buffer.
//
// Two data-mandated copies remain and are intentionally NOT optimized away:
// escaped strings still unescape-allocate inside gjson, and genuine
// multi-block responses (2+ text segments) still concatenate through
// strings.Builder. Only the single-segment case aliases — that is what the
// aliasJoiner below selects.
package buildrequest

import (
	"bytes"
	"fmt"
	"strings"
	"unsafe"

	"github.com/cloudwego/gjson"
)

// parseableJoiner accumulates the parseable text segments emitted by the
// per-provider extractors. It abstracts the one decision that differs between
// the owned and borrowed extraction paths: whether a result built from a
// single content block may alias its source.
//
//   - copyingJoiner (string/byte entry points) always concatenates through a
//     strings.Builder, so the returned parseable owns its bytes regardless of
//     segment count — preserving the owned-output contract of
//     ExtractResponseContent / ExtractResponseContentBytes.
//   - aliasJoiner (borrowed entry point) returns a lone segment verbatim
//     (zero copy — it aliases the borrowed body) and only falls back to a
//     strings.Builder when two or more segments must be concatenated.
//
// A joiner never changes the extracted VALUE, only whether a single-segment
// result aliases the source. Joiners are single-use and not safe for
// concurrent use; dispatchResponseContent constructs a fresh one per call.
type parseableJoiner interface {
	// WriteString appends one parseable text segment. The provider extractors
	// call it exactly where they previously called strings.Builder.WriteString.
	WriteString(s string)
	// String returns the accumulated parseable text.
	String() string
}

// copyingJoiner is the owned-output joiner: a thin strings.Builder wrapper that
// always copies, reproducing the pre-Stage-3 behavior of the string and byte
// extractors byte-for-byte.
type copyingJoiner struct {
	sb strings.Builder
}

func (j *copyingJoiner) WriteString(s string) { j.sb.WriteString(s) }
func (j *copyingJoiner) String() string       { return j.sb.String() }

// aliasJoiner is the borrowed/aliasing joiner with the single-segment
// fast-path. The first segment is held verbatim; if a second arrives, the
// builder is back-filled with the held first segment and accumulation
// continues there. Thus exactly one segment returns the original (aliasing)
// string with zero copy, while zero or 2+ segments behave like a plain
// strings.Builder (empty or data-mandated concatenation).
type aliasJoiner struct {
	first string
	count int
	sb    strings.Builder
}

func (j *aliasJoiner) WriteString(s string) {
	j.count++
	switch j.count {
	case 1:
		// Hold the single segment verbatim — this is the aliasing view we
		// return unchanged when no further segment arrives.
		j.first = s
	case 2:
		// A second segment makes this a genuine multi-part response: spill the
		// held first segment into the builder, then append this one. From here
		// the result is a fresh owned concatenation (a data-mandated copy).
		j.sb.WriteString(j.first)
		j.first = ""
		j.sb.WriteString(s)
	default:
		j.sb.WriteString(s)
	}
}

func (j *aliasJoiner) String() string {
	if j.count == 1 {
		return j.first
	}
	return j.sb.String()
}

// ExtractResponseContentBorrowed is the aliasing twin of
// ExtractResponseContentBytes for the fasthttp BORROWED unary lane
// (llmhttp.Response.BorrowedBytes()). Unlike the byte extractor — which runs
// gjson.GetBytes and therefore always copies matched content out — this path
// validates with gjson.Valid and extracts with gjson.Get over an unsafe string
// VIEW of body (bytesToStringView), so unescaped scalar /
// single content-block text is returned as a zero-copy alias of body.
//
// Returns results identical to ExtractResponseContent / ExtractResponseContentBytes
// for the same JSON (see TestExtractResponseContentBorrowedMatchesString); the
// only difference is allocation: single-segment unescaped content aliases body
// instead of being copied.
//
// # Lifetime contract (use-after-free critical)
//
// body MUST be the live borrowed buffer and MUST stay valid AND unmodified for
// the duration of this call. The returned parseable/raw MAY alias body (the
// single-segment / scalar cases above); reasoning is always assembled through
// a strings.Builder and is therefore always owned. The CALLER must not retain
// any aliasing return past the borrow's Release: the orchestrator consumes
// parseable in-attempt via parseFinal (which copies it into the protobuf parse
// arguments before the borrow is released) and clones every value that escapes
// the attempt (raw/reasoning under NeedsRaw, parse-failure raw,
// extraction-failure body) before Release. strings.Clone / string([]byte) on
// an aliasing view yields owned bytes, so cloning-before-Release is sufficient.
func ExtractResponseContentBorrowed(provider string, body []byte, includeReasoning bool) (parseable, raw, reasoning string, err error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", "", "", fmt.Errorf("provider %s: empty response body", provider)
	}

	// View the borrowed bytes as a string WITHOUT copying. gjson.Valid and
	// gjson.Get only read the string for the duration of this call; the
	// substrings gjson hands back (and that the extractors may return) alias
	// this view, hence the lifetime contract above.
	view := bytesToStringView(body)

	if !gjson.Valid(view) {
		return "", "", "", fmt.Errorf("provider %s: response body is not valid JSON", provider)
	}

	return dispatchResponseContent(provider, func(path string) gjson.Result {
		return gjson.Get(view, path)
	}, includeReasoning, &aliasJoiner{})
}

// bytesToStringView returns a string that shares b's backing array without
// copying. It mirrors llmhttp's bytesToBodyString: the bamlutils module is
// standalone and deliberately does not import the root module's
// internal/unsafeutil, so the unsafe conversion is inlined here. b must stay
// valid and unmodified for the lifetime of any string that aliases the
// returned view (see ExtractResponseContentBorrowed's lifetime contract).
// Returning "" for an empty slice keeps the nil-data case safe.
func bytesToStringView(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}
