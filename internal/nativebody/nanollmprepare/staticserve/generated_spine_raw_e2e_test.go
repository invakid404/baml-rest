//go:build integration && nanollm_integration

package staticserve

// The /call-with-raw default-serve DIFFERENTIAL through the REAL generated adapter. For
// every corpus row the SAME provider response is served twice through the fixture's
// generated `StaticRecursiveAliasJSON` on StreamModeCallWithRaw:
//
//	flag OFF -> stock BAML v0.223 end to end (generated Request + Parse over CFFI)
//	flag ON  -> installNativeStaticCall -> the standardspineoracle composite -> the spine
//	            CallWithOracle lane (live plan compare, one native socket, the raw-aware
//	            reference oracle) -> the generated decoder
//
// and the COMPLETE public outcome must be identical: the final value's bytes, the raw and
// reasoning channels, or — for a failing row — the error text and its details.raw.
//
// Equality alone cannot tell native serving from a lane that always handed the request
// back to BAML, so the OWNERSHIP facts are asserted separately on the flag-on leg: the
// serve seam ran once, the spine claimed exactly one socket, the provider saw exactly one
// request, and the outcome winner is the row's expected engine (native on every row whose
// response native reads identically; the reference parse where native declines or fails).

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	fwadapter "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated/adapter"
)

// rawOutcome is ONE leg's complete public outcome, reduced to what a /call-with-raw client
// receives (the worker marshals the final carrier; raw/reasoning ride beside it; an error
// carries its text and details.raw).
type rawOutcome struct {
	data, raw, reasoning string
	err, errRaw          string
	winner               string
}

// openAIEnvelope renders an OpenAI chat completion whose message is exactly msg.
func openAIEnvelope(msg map[string]any) []byte {
	env, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	return env
}

func assistantMsg(content any) map[string]any {
	return map[string]any{"role": "assistant", "content": content}
}

func withReasoning(msg map[string]any, reasoning any) map[string]any {
	msg["reasoning_content"] = reasoning
	return msg
}

// driveCallWithRaw drives the U1 method on the given adapter and drains every frame into
// the leg's public outcome.
func driveCallWithRaw(t *testing.T, a bamlutils.Adapter) rawOutcome {
	t.Helper()
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("StaticRecursiveAliasJSON install: %v", err)
	}
	var out rawOutcome
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			out.data = jsonOf(t, r.Final())
			out.raw, out.reasoning = r.Raw(), r.Reasoning()
		case bamlutils.StreamResultKindError:
			if e := r.Error(); e != nil {
				out.err = e.Error()
			}
			out.errRaw = r.Raw()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
				out.winner = md.WinnerEngine
			}
		}
		r.Release()
	}
	return out
}

// callWithRawRow is one corpus row: the provider's exact 2xx (or non-2xx) response, the
// request's reasoning preference, and the engine expected to own the flag-on envelope.
type callWithRawRow struct {
	name             string
	status           int
	body             []byte
	includeReasoning bool
	// wantWinner is the flag-on outcome winner: "native" when native's reading of the
	// response is identical facet-for-facet, "native_baml_parse" when the claimed lane
	// served the reference envelope (native declined or failed), "" for an error row.
	wantWinner string
	// wantError marks a row whose flag-off outcome is a failure.
	wantError bool
}

func callWithRawCorpus() []callWithRawRow {
	ok := func(name string, content any, wantWinner string) callWithRawRow {
		return callWithRawRow{name: name, status: http.StatusOK, body: openAIEnvelope(assistantMsg(content)), wantWinner: wantWinner}
	}
	rows := []callWithRawRow{}
	// The served exact-JSON manifest, so raw (pre-coercion text) and data (BAML coercion +
	// canonical serialization) visibly differ on the coercing rows (float->int, null->[],
	// key sort, duplicate-key overwrite).
	for _, r := range aliasManifestRows() {
		rows = append(rows, ok("manifest/"+r.name, r.content, "native"))
	}
	rows = append(rows,
		ok("whitespace_and_newlines", "\n  {\"k\" :\t[1, 2]}\n\n", "native"),
		ok("escapes_unicode_html", `{"k":"café ☕ \"q\" \\ back \t tab <b>&amp;</b> `+" "+` end","n":-9223372036854775808}`, "native"),
		ok("int64_limits", `[9223372036854775807,-9223372036854775808,0,-1]`, "native"),
		ok("markdown_fenced", "Here you go:\n```json\n{\"a\":[1,\"x\",true]}\n```\n", "native"),
		// Multipart content: raw is the concatenated TEXT parts; non-text parts are skipped.
		ok("multipart_text", []any{
			map[string]any{"type": "text", "text": `{"a":`},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/x.png"}},
			map[string]any{"type": "text", "text": `[1,2]}`},
		}, "native"),
		// A response native's static parser declines: the claimed lane serves the reference.
		ok("plain_text_decline", "the weather is sunny", "native_baml_parse"),
		ok("empty_content", "", "native_baml_parse"),
		ok("null_content", nil, "native_baml_parse"),
		// A lone UTF-16 surrogate: nanollm's translation fails, BAML's extractor decodes
		// it to U+FFFD — the reference envelope recovers the SAME response.
		callWithRawRow{
			name: "lone_surrogate_native_translate_failure", status: http.StatusOK,
			body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"{\"a\":\"\ud800\"}"}}]}`),
			wantWinner: "native_baml_parse",
		},
		// Reasoning channel variants (opted in): present with control characters, absent,
		// non-string (ignored by BAML's extractor), and present but NOT opted in.
		callWithRawRow{
			name: "reasoning_present", status: http.StatusOK, includeReasoning: true,
			body:       openAIEnvelope(withReasoning(assistantMsg(`{"a":1}`), "step 1\nstep \"2\"\tdone ☕")),
			wantWinner: "native",
		},
		callWithRawRow{
			name: "reasoning_opted_in_absent", status: http.StatusOK, includeReasoning: true,
			body: openAIEnvelope(assistantMsg(`{"a":1}`)), wantWinner: "native",
		},
		callWithRawRow{
			name: "reasoning_non_string", status: http.StatusOK, includeReasoning: true,
			body:       openAIEnvelope(withReasoning(assistantMsg(`{"a":1}`), 42)),
			wantWinner: "native",
		},
		callWithRawRow{
			name: "reasoning_present_not_opted_in", status: http.StatusOK, includeReasoning: false,
			body:       openAIEnvelope(withReasoning(assistantMsg(`{"a":1}`), "hidden")),
			wantWinner: "native",
		},
		// Error rows: the flag-on failure must be BAML's own failure, byte for byte.
		callWithRawRow{
			name: "refusal", status: http.StatusOK,
			body:      openAIEnvelope(map[string]any{"role": "assistant", "content": nil, "refusal": "I can't"}),
			wantError: true,
		},
		callWithRawRow{name: "malformed_json_body", status: http.StatusOK, body: []byte(`{"choices":[`), wantError: true},
		callWithRawRow{name: "missing_content", status: http.StatusOK, body: []byte(`{"choices":[{"message":{"role":"assistant"}}]}`), wantError: true},
		callWithRawRow{name: "provider_429", status: http.StatusTooManyRequests, body: []byte(`{"error":{"message":"rate limited"}}`), wantError: true},
	)
	return rows
}

// runCallWithRawLeg serves the row once on a fresh fixture server and returns the leg's
// outcome plus the ownership facts the flag-on assertions need.
func runCallWithRawLeg(t *testing.T, row callWithRawRow, flagOn bool) (out rawOutcome, serveCalls int, providerHits int64, snapClaims, snapSockets int64) {
	t.Helper()
	serveFn, exec, calls := fixtureBamlSrcSpineServe(t)
	server := newFixtureServer(t, row.status, row.body)
	defer server.close()
	a := buildFixtureAdapterWithServe(t, serveFn, flagOn)
	ba := a.(*fwadapter.BamlAdapter)
	ba.SetStreamMode(bamlutils.StreamModeCallWithRaw)
	ba.SetIncludeReasoning(row.includeReasoning)
	out = driveCallWithRaw(t, a)
	snap := exec.Metrics().Snapshot()
	return out, *calls, server.count.Load(), int64(snap.Claims), int64(snap.Sockets)
}

// TestSpineComposite_CallWithRawMatchesStockBAML is the headline differential.
func TestSpineComposite_CallWithRawMatchesStockBAML(t *testing.T) {
	for _, row := range callWithRawCorpus() {
		t.Run(row.name, func(t *testing.T) {
			stock, stockCalls, stockHits, _, _ := runCallWithRawLeg(t, row, false)
			native, calls, hits, claims, sockets := runCallWithRawLeg(t, row, true)

			// The stock leg really is stock BAML: no serve seam, one BAML send.
			if stockCalls != 0 || stockHits != 1 {
				t.Fatalf("stock leg: serve seam ran %d times, provider saw %d requests; want 0 and 1", stockCalls, stockHits)
			}
			if (stock.err != "") != row.wantError {
				t.Fatalf("stock leg outcome = %+v, want error=%v — the corpus row no longer exercises what it names", stock, row.wantError)
			}

			// OWNERSHIP (flag on), asserted separately from the bytes: the native lane CLAIMED
			// the one socket — a pre-socket decline back to BAML would show claims=0.
			if calls != 1 {
				t.Fatalf("flag-on serve seam ran %d times, want exactly 1", calls)
			}
			if claims != 1 || sockets != 1 {
				t.Fatalf("flag-on spine claims=%d sockets=%d, want 1 and 1 — the raw request must be claimed natively, not declined to BAML", claims, sockets)
			}
			if hits != 1 {
				t.Fatalf("flag-on provider saw %d requests, want exactly 1 (no BAML resend)", hits)
			}
			if !row.wantError && native.winner != row.wantWinner {
				t.Errorf("flag-on winner = %q, want %q", native.winner, row.wantWinner)
			}

			// BYTES: the complete public outcome is stock BAML's.
			if native.data != stock.data || native.raw != stock.raw || native.reasoning != stock.reasoning {
				t.Errorf("served envelope differs from stock BAML:\n  stock:  data=%s raw=%q reasoning=%q\n  native: data=%s raw=%q reasoning=%q",
					stock.data, stock.raw, stock.reasoning, native.data, native.raw, native.reasoning)
			}
			if native.err != stock.err || native.errRaw != stock.errRaw {
				t.Errorf("failure differs from stock BAML:\n  stock:  err=%q raw=%q\n  native: err=%q raw=%q", stock.err, stock.errRaw, native.err, native.errRaw)
			}
		})
	}
}

// TestSpineComposite_CallWithRawCorpusIsNotVacuous guards the differential's own corpus: it
// must contain rows native WINS (so the lane is proven to serve, not merely to hand back),
// rows where raw and data visibly differ (so raw cannot be reconstructed from data), and a
// row whose reasoning channel is non-empty.
func TestSpineComposite_CallWithRawCorpusIsNotVacuous(t *testing.T) {
	var nativeRows, coercing, reasoningRows int
	for _, row := range callWithRawCorpus() {
		if row.wantWinner == "native" {
			nativeRows++
		}
		if row.includeReasoning && row.name == "reasoning_present" {
			reasoningRows++
		}
	}
	for _, r := range aliasManifestRows() {
		if r.content != r.want {
			coercing++
		}
	}
	if nativeRows < 10 || coercing < 3 || reasoningRows != 1 {
		t.Fatalf("corpus native-win rows=%d coercing rows=%d reasoning rows=%d; the differential would prove too little", nativeRows, coercing, reasoningRows)
	}
}
