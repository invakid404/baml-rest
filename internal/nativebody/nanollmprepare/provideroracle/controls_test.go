//go:build integration && nanollm_integration

package provideroracle

// TPBP-A mutation controls (scope §6.1 step 7). Each proves a POSITIVE assertion
// in anthropic_test.go is load-bearing by driving the SAME comparator primitive
// (testutil.Diff / NewSnapshot / topLevelKeys / strict DTO decode / sameOrder /
// sameKeySet) against a deliberate mutation and requiring it to be DETECTED. A
// control that failed to detect its mutation would mean the paired positive could
// pass on a broken request — so every control fails closed (t.Fatal when the
// mutation slips through). There is deliberately NO "semantic-equal = pass"
// escape hatch: an UNLISTED member reorder, an extra key, or a renamed field all
// fail even though the request stays semantically valid.

import (
	"bytes"
	"strings"
	"testing"

	nanollm "github.com/viktordanov/nanollm-ffi/go"

	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// controlRow is a small system+user row reused by the controls.
func controlRow() anthropicRow {
	return anthropicRow{
		name:        "control",
		serviceRoot: fenceServiceRoot,
		messages: []corpusMessage{
			{role: "system", text: "You are a concise assistant."},
			{role: "user", text: "Tell me about cats."},
		},
		opts:      bodyOptions{maxTokens: -1},
		bamlOrder: []string{"model", "max_tokens", "system", "messages"},
		nanoOrder: []string{"model", "messages", "system", "max_tokens"},
	}
}

// strictDecodeErr routes through the SHARED exact-one-value strict decode path
// (strictDecodeAnthropic: DisallowUnknownFields + trailing-JSON rejection) and
// returns its fixed, secret-free error (nil on success) WITHOUT failing the test —
// the controls assert on whether a mutation is rejected.
func strictDecodeErr(body []byte) error {
	_, err := strictDecodeAnthropic(body)
	return err
}

// TestControlWrongPath: a mutated endpoint path must be caught by the shared
// method/URL/header comparator.
func TestControlWrongPath(t *testing.T) {
	cap := runRow(t, controlRow())
	bad := headerOnly(cap.nanoSnap)
	bad.URL = strings.Replace(bad.URL, defaultMessagesPath, "/v1/complete", 1)
	if bad.URL == cap.nanoSnap.URL {
		t.Fatal("path mutation did not change the URL")
	}
	if diffs := testutil.Diff(headerOnly(cap.baml.snap), bad); len(diffs) == 0 {
		t.Fatal("a wrong endpoint path was NOT detected by the differential")
	}
}

// TestControlMissingAuth: dropping the required x-api-key must be caught (it is a
// semantic header, present on the BAML oracle and required on native).
func TestControlMissingAuth(t *testing.T) {
	cap := runRow(t, controlRow())
	bad := testutil.Snapshot{Method: cap.nanoSnap.Method, URL: cap.nanoSnap.URL, Headers: map[string]string{}}
	for k, v := range cap.nanoSnap.Headers {
		if k == "x-api-key" {
			continue
		}
		bad.Headers[k] = v
	}
	diffs := testutil.Diff(headerOnly(cap.baml.snap), bad)
	if len(diffs) == 0 {
		t.Fatal("a missing x-api-key was NOT detected by the differential")
	}
}

// TestControlDuplicateAuth: a duplicate auth header must be REJECTED at snapshot
// construction (fail closed), never silently merged.
func TestControlDuplicateAuth(t *testing.T) {
	cap := runRow(t, controlRow())
	dup := append(append([][2]string(nil), cap.prep.Headers...), [2]string{"x-api-key", "second-fake-key"})
	if _, err := testutil.NewSnapshot(cap.prep.Method, cap.prep.URL, dup, cap.prep.Body); err == nil {
		t.Fatal("a duplicate x-api-key was NOT rejected by the comparator")
	}
}

// TestControlAuthValueRedacted: a tampered x-api-key VALUE is detected AND never
// leaks into the diagnostics (redaction proof).
func TestControlAuthValueRedacted(t *testing.T) {
	cap := runRow(t, controlRow())
	bad := testutil.Snapshot{Method: cap.nanoSnap.Method, URL: cap.nanoSnap.URL, Headers: map[string]string{}}
	for k, v := range cap.nanoSnap.Headers {
		bad.Headers[k] = v
	}
	bad.Headers["x-api-key"] = "tampered-fake-key"
	diffs := testutil.Diff(headerOnly(cap.baml.snap), bad)
	if len(diffs) == 0 {
		t.Fatal("a tampered x-api-key value was NOT detected")
	}
	joined := strings.Join(diffs, "\n")
	tamperedLeaked := strings.Contains(joined, "tampered-fake-key")
	fenceLeaked := strings.Contains(joined, fenceAPIKey)
	if tamperedLeaked || fenceLeaked {
		// Report only WHICH secret leaked (booleans) — never re-print the diagnostic
		// content itself, or this redaction control would leak the value it protects.
		t.Fatalf("x-api-key value leaked into diagnostics (tampered_present=%v fence_present=%v)", tamperedLeaked, fenceLeaked)
	}
}

// TestControlWrongTarget: a native client whose configured target differs from
// the BAML model produces a body model the required-meaning anchor rejects
// (nanollm rewrites the top-level model to the configured target).
func TestControlWrongTarget(t *testing.T) {
	const wrongTarget = "wrong-oracle-target"
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       fenceAlias,
			Model:      "anthropic/" + wrongTarget,
			APIKey:     fenceAPIKey,
			BaseURL:    fenceServiceRoot + "/v1",
			MaxRetries: 0,
		}},
		UseProcessEnv: false,
	})
	if err != nil {
		// The engine error can echo the config (fake key/base) — report the stage only.
		t.Fatal("nanollm.New(anthropic) failed at stage=engine_new")
	}
	defer c.Close()

	r := controlRow()
	prep, _ := prepareNative(t, c, buildCanonicalRequest(t, r.messages, r.opts))
	got := decodeAnthropic(t, "nanollm", prep.Body)
	if got.Model == fenceModel {
		t.Fatalf("a wrong configured target did NOT change the body model (still %q) — the model anchor is not sensitive", fenceModel)
	}
	if got.Model != wrongTarget {
		t.Fatalf("expected native to rewrite the body model to the configured target %q, got a different value", wrongTarget)
	}
}

// TestControlRenamedBodyField: a renamed top-level field (max_tokens ->
// max_tokenz) is caught by the strict DTO decode (an unlisted field fails closed).
func TestControlRenamedBodyField(t *testing.T) {
	cap := runRow(t, controlRow())
	if err := strictDecodeErr(cap.prep.Body); err != nil {
		// A decode error can echo body bytes — report the stage only.
		t.Fatal("the unmutated native body failed strict decode (stage=precondition_decode)")
	}
	mutated := bytes.Replace(cap.prep.Body, []byte(`"max_tokens"`), []byte(`"max_tokenz"`), 1)
	if bytes.Equal(mutated, cap.prep.Body) {
		t.Fatal("the field-rename mutation did not change the body")
	}
	if err := strictDecodeErr(mutated); err == nil {
		t.Fatal("a renamed/unlisted top-level field was NOT rejected by the strict decode")
	}
}

// TestControlTrailingJSONRejected: a body that is a valid Anthropic request
// FOLLOWED BY a second top-level JSON value (trailing JSON) must be rejected by
// the shared strict decode path — a bare json.Decoder.Decode consumes only the
// first value and would silently accept the rest. Trailing WHITESPACE (not a
// second value) must still be tolerated.
func TestControlTrailingJSONRejected(t *testing.T) {
	cap := runRow(t, controlRow())

	// Precondition: the clean prepared body decodes.
	if err := strictDecodeErr(cap.prep.Body); err != nil {
		t.Fatal("the unmutated native body must decode cleanly (stage=precondition_decode)")
	}

	// A second top-level JSON object appended after the request must be rejected.
	trailingObj := append(append([]byte(nil), cap.prep.Body...), []byte(`{"x":1}`)...)
	if err := strictDecodeErr(trailingObj); err == nil {
		t.Fatal("a body with a trailing JSON object after the request was NOT rejected")
	}
	// A trailing bare scalar is likewise rejected.
	trailingScalar := append(append([]byte(nil), cap.prep.Body...), []byte(" 7")...)
	if err := strictDecodeErr(trailingScalar); err == nil {
		t.Fatal("a body with a trailing scalar after the request was NOT rejected")
	}

	// Trailing WHITESPACE only is NOT a second value and must be tolerated.
	whitespace := append(append([]byte(nil), cap.prep.Body...), []byte("  \n\t")...)
	if err := strictDecodeErr(whitespace); err != nil {
		t.Fatal("trailing whitespace must be tolerated by the strict decode (stage=whitespace)")
	}
}

// TestControlMalformedOptionValue: a corrupted stop_sequences value is caught by
// the required-meaning comparison (the decoded slice diverges from the expected).
func TestControlMalformedOptionValue(t *testing.T) {
	r := anthropicRow{
		name:        "control_stop",
		serviceRoot: fenceServiceRoot,
		messages:    []corpusMessage{{role: "user", text: "hi"}},
		opts:        bodyOptions{maxTokens: 100, stopSequences: []string{"STOP"}},
	}
	cap := runRow(t, r)
	got := decodeAnthropic(t, "nanollm", cap.prep.Body)
	if equalStrings(got.StopSequences, []string{"MANGLED"}) {
		t.Fatal("sanity: decoded stop_sequences unexpectedly equals the mangled control value")
	}
	if !equalStrings(got.StopSequences, r.opts.stopSequences) {
		t.Fatalf("sanity: decoded stop_sequences diverged from the input")
	}
	// The comparison is value-sensitive: a wrong expectation must NOT match.
	if equalStrings(got.StopSequences, append(r.opts.stopSequences, "EXTRA")) {
		t.Fatal("stop_sequences comparison is not length/value sensitive")
	}
}

// TestControlUnlistedOrderDivergence: an UNLISTED top-level member reorder (even
// though it stays semantically valid) must fail the pinned-order allowlist. This
// is the explicit "no semantic-equal escape hatch" control.
func TestControlUnlistedOrderDivergence(t *testing.T) {
	r := controlRow()
	cap := runRow(t, r)

	nanoKeys := topLevelKeys(t, "nanollm", cap.prep.Body)
	if !sameOrder(nanoKeys, r.nanoOrder) {
		t.Fatalf("precondition: native order %v != allowlist %v", nanoKeys, r.nanoOrder)
	}
	// Swap two adjacent keys => a valid but UNLISTED order.
	reordered := append([]string(nil), nanoKeys...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if sameOrder(reordered, nanoKeys) {
		t.Fatal("the reorder mutation did not change the key order")
	}
	// The pinned-order allowlist must REJECT the reordered (but still valid) body.
	if sameOrder(reordered, r.nanoOrder) {
		t.Fatal("an unlisted reorder was accepted by the allowlist")
	}
	// Same key SET, different order — proves the divergence gate is order-strict,
	// not merely set-based.
	if !sameKeySet(reordered, r.nanoOrder) {
		t.Fatal("the reorder should preserve the key set")
	}
}

// TestControlExtraKeySetDivergence: an added top-level key breaks BOTH the strict
// decode and the key-SET equality — an unlisted new field can never pass.
func TestControlExtraKeySetDivergence(t *testing.T) {
	r := controlRow()
	cap := runRow(t, r)

	// Inject an unlisted top-level key into the native body bytes.
	injected := bytes.Replace(cap.prep.Body, []byte(`{"model"`), []byte(`{"unlisted_field":1,"model"`), 1)
	if bytes.Equal(injected, cap.prep.Body) {
		t.Fatal("the extra-key mutation did not change the body")
	}
	if err := strictDecodeErr(injected); err == nil {
		t.Fatal("an injected unlisted top-level key was NOT rejected by the strict decode")
	}
	nanoKeys := topLevelKeys(t, "nanollm", injected)
	if sameKeySet(nanoKeys, r.nanoOrder) {
		t.Fatal("an injected unlisted key did NOT change the key set")
	}
}

// TestControlSystemBlockType: a system block with a NON-text `type` (same text,
// same top-level order) must be caught by the exact block-sequence comparison —
// the text-only compare that P1 removed would have accepted this provider-invalid
// block.
func TestControlSystemBlockType(t *testing.T) {
	r := controlRow()
	cap := runRow(t, r)
	want := r.wantSystemBlocks()

	// Precondition: the real native system blocks match the expected sequence.
	real := decodeAnthropic(t, "nanollm", cap.prep.Body)
	if !blocksEqual(real.System, want) {
		t.Fatal("precondition: native system blocks diverge from the expected sequence")
	}

	// Corrupt ONLY the system block's type (the marker prefix uniquely targets the
	// system array; message content blocks are emitted before it in this row).
	marker := []byte(`"system":[{"type":"text"`)
	if !bytes.Contains(cap.prep.Body, marker) {
		t.Fatal("precondition: could not locate the system text block to mutate")
	}
	mutated := bytes.Replace(cap.prep.Body, marker, []byte(`"system":[{"type":"txt"`), 1)

	// The mutated body stays STRUCTURALLY decodable (type is just a string)...
	if err := strictDecodeErr(mutated); err != nil {
		t.Fatal("the type mutation unexpectedly broke structural decodability")
	}
	// ...but the provider-invalid block type must fail the exact block comparison.
	got := decodeAnthropic(t, "nanollm", mutated)
	if got.System[0].Type == "text" {
		t.Fatal("the system block type mutation did not take effect")
	}
	if blocksEqual(got.System, want) {
		t.Fatal("a system block with a non-text type was NOT detected (text-only escape hatch)")
	}
}

// TestControlDeveloperRoleInMessagesRejected: a developer role surviving into
// provider `messages` (a mapping bug) must be detected by the forbidden-role gate.
func TestControlDeveloperRoleInMessagesRejected(t *testing.T) {
	// A synthetic provider body that wrongly left a developer role in messages.
	body := []byte(`{"model":"m","messages":[{"role":"developer","content":[{"type":"text","text":"d"}]}],"max_tokens":1}`)
	got := decodeAnthropic(t, "nanollm", body)
	if !hasForbiddenRole(got.Messages) {
		t.Fatal("a developer role surviving in provider messages was NOT detected")
	}
	// A clean user-only messages array must NOT trip the gate (control sanity).
	ok := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u"}]}],"max_tokens":1}`)
	if hasForbiddenRole(decodeAnthropic(t, "nanollm", ok).Messages) {
		t.Fatal("a clean user-only messages array wrongly tripped the forbidden-role gate")
	}
}

// hasForbiddenRole reports whether any message carries a system/developer role
// (which must never survive into provider `messages`).
func hasForbiddenRole(msgs []anthropicMessage) bool {
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			return true
		}
	}
	return false
}

// TestControlBamlOriginalUrlOneSided: if native ever emitted baml-original-url the
// divergence gate must catch it (the exemption is one-sided). This proves the
// gate is not a blanket two-sided pass.
func TestControlBamlOriginalUrlOneSided(t *testing.T) {
	cap := runRow(t, controlRow())
	// Native must not carry it in the real capture.
	if _, ok := cap.nanoSnap.Headers["baml-original-url"]; ok {
		t.Fatal("precondition: native unexpectedly already carries baml-original-url")
	}
	// A synthetic native snapshot that DOES carry it must diverge from the BAML
	// oracle (present-on-nanollm-only), because the testutil exemption is applied
	// to the oracle side only.
	bad := testutil.Snapshot{Method: cap.nanoSnap.Method, URL: cap.nanoSnap.URL, Headers: map[string]string{}}
	for k, v := range cap.nanoSnap.Headers {
		bad.Headers[k] = v
	}
	bad.Headers["baml-original-url"] = "https://anthropic-oracle.invalid"
	diffs := testutil.Diff(headerOnly(cap.baml.snap), bad)
	if len(diffs) == 0 {
		t.Fatal("native emitting baml-original-url was NOT detected (the exemption must be one-sided)")
	}
}
