//go:build integration && nanollm_integration

package provideroracle

// TPBP-A Anthropic provider-correctness differential (scope §6.2).
//
// For every corpus row this proves native emits a PROVIDER-CORRECT Anthropic
// Messages request and records the BAML divergence as a checked-in allowlist
// entry:
//
//   - exact method (POST), exact endpoint (service-root/v1/messages after S2's
//     /v1 adapter), and the required semantic header SET (x-api-key,
//     anthropic-version, content-type, plus any safe custom header) compared
//     through the shared redacted testutil comparator;
//   - both bodies decoded into the Anthropic contract DTO and compared for
//     required meaning: the fence model, max_tokens, top-level system text, the
//     ordered user/assistant turns, and mapped temperature/top_p/top_k/
//     stop_sequences — with NO system/developer role surviving into messages;
//   - native's OWN header order pinned independently (BAML's is unclaimed);
//   - the expected top-level key-order divergence + the one-sided
//     `baml-original-url` header asserted against the per-row allowlist. Each
//     side's exact order is pinned, so an unlisted reorder/new key FAILS
//     (controls_test.go drives the negatives).

import (
	"fmt"
	"strings"
	"testing"

	nanollm "github.com/viktordanov/nanollm-ffi/go"

	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// anthropicRow is one corpus case. The SAME structured inputs (serviceRoot,
// messages, opts, customHeaders) drive both legs — BAML through a generated
// in-memory `.baml` source, native through the canonical OpenAI builder — so the
// two requests cannot silently drift apart. bamlOrder/nanoOrder are the checked-in
// expected top-level member orders (the expected-divergence allowlist).
type anthropicRow struct {
	name          string
	serviceRoot   string // "" => provider default (api.anthropic.com)
	messages      []corpusMessage
	opts          bodyOptions
	customHeaders map[string]string

	bamlOrder []string
	nanoOrder []string
}

// anthropicCorpus is the §6.2 Anthropic corpus.
func anthropicCorpus() []anthropicRow {
	sys := corpusMessage{role: "system", text: "You are a concise assistant."}
	return []anthropicRow{
		{
			// default service root; system + user; default max_tokens; no options.
			name:        "default_root_system_user_default_max_tokens",
			serviceRoot: "",
			messages: []corpusMessage{
				sys,
				{role: "user", text: "Tell me about cats."},
			},
			opts:      bodyOptions{maxTokens: -1},
			bamlOrder: []string{"model", "max_tokens", "system", "messages"},
			nanoOrder: []string{"model", "messages", "system", "max_tokens"},
		},
		{
			// custom service root; system + ordered user/assistant text.
			name:        "custom_root_system_user_assistant",
			serviceRoot: fenceServiceRoot,
			messages: []corpusMessage{
				sys,
				{role: "user", text: "Name one fact about the moon."},
				{role: "assistant", text: "It has no atmosphere to speak of."},
			},
			opts:      bodyOptions{maxTokens: -1},
			bamlOrder: []string{"model", "max_tokens", "system", "messages"},
			nanoOrder: []string{"model", "messages", "system", "max_tokens"},
		},
		{
			// explicit max_tokens (moves the member into the input body).
			name:        "explicit_max_tokens",
			serviceRoot: fenceServiceRoot,
			messages: []corpusMessage{
				sys,
				{role: "user", text: "Summarize photosynthesis."},
			},
			opts:      bodyOptions{maxTokens: 100},
			bamlOrder: []string{"model", "max_tokens", "system", "messages"},
			nanoOrder: []string{"model", "messages", "max_tokens", "system"},
		},
		{
			// temperature / top_p / top_k / stop->stop_sequences + Unicode & escaping.
			name:        "sampling_options_unicode_escaping",
			serviceRoot: fenceServiceRoot,
			messages: []corpusMessage{
				sys,
				{role: "user", text: `Escape café ☕ "q" and a backslash \b then 🙂.`},
			},
			opts: bodyOptions{
				maxTokens:     100,
				temperature:   f64(0.7),
				topP:          f64(0.9),
				topK:          iptr(5),
				stopSequences: []string{"STOP", "END"},
			},
			bamlOrder: []string{"model", "temperature", "top_p", "top_k", "max_tokens", "stop_sequences", "system", "messages"},
			nanoOrder: []string{"max_tokens", "messages", "model", "stop_sequences", "temperature", "top_k", "top_p", "system"},
		},
		{
			// one safe custom header; user only (no system in the body).
			name:          "custom_header_user_only",
			serviceRoot:   fenceServiceRoot,
			customHeaders: map[string]string{fenceCustomHdrKey: fenceCustomHdrVal},
			messages: []corpusMessage{
				{role: "user", text: "Ping."},
			},
			opts:      bodyOptions{maxTokens: -1},
			bamlOrder: []string{"model", "max_tokens", "messages"},
			nanoOrder: []string{"model", "messages", "max_tokens"},
		},
	}
}

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

// wantMaxTokens resolves the expected max_tokens for a row (both builders default
// an absent value to 4096).
func (r anthropicRow) wantMaxTokens() int {
	if r.opts.maxTokens >= 0 {
		return r.opts.maxTokens
	}
	return 4096
}

// wantSystemBlocks is the EXACT provider-native system block sequence a row's
// system turns must produce: one {type:"text", text} block each, in order (the
// provider request lifts them to top-level `system`). Nil when the row has no
// system turn.
func (r anthropicRow) wantSystemBlocks() []anthropicBlock {
	var texts []string
	for _, m := range r.messages {
		if m.role == "system" {
			texts = append(texts, m.text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return textBlockSeq(texts...)
}

// wantProviderMessages is the row's non-system turns, in order — what must remain
// in the provider `messages` array (system/developer never survive there).
func (r anthropicRow) wantProviderMessages() []corpusMessage {
	var out []corpusMessage
	for _, m := range r.messages {
		if m.role == "system" || m.role == "developer" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// --- BAML in-memory source generation (keeps the two legs coupled). ---

// bamlAnthropicSource renders the tiny in-memory `.baml` project for a row: one
// anthropic client at the fence values with the row's mapped options, and one
// zero-arg function whose prompt is the row's ordered role turns.
func bamlAnthropicSource(r anthropicRow) (src, fn string) {
	fn = "OracleFn"
	var opt strings.Builder
	fmt.Fprintf(&opt, "    model %q\n", fenceModel)
	fmt.Fprintf(&opt, "    api_key %q\n", fenceAPIKey)
	if r.serviceRoot != "" {
		fmt.Fprintf(&opt, "    base_url %q\n", r.serviceRoot)
	}
	// Body-affecting options in the EXACT order that fixes BAML's top-level member
	// order (bamlOrder). Only emit those a row sets.
	if r.opts.temperature != nil {
		fmt.Fprintf(&opt, "    temperature %v\n", *r.opts.temperature)
	}
	if r.opts.topP != nil {
		fmt.Fprintf(&opt, "    top_p %v\n", *r.opts.topP)
	}
	if r.opts.topK != nil {
		fmt.Fprintf(&opt, "    top_k %d\n", *r.opts.topK)
	}
	if r.opts.maxTokens >= 0 {
		fmt.Fprintf(&opt, "    max_tokens %d\n", r.opts.maxTokens)
	}
	if len(r.opts.stopSequences) > 0 {
		quoted := make([]string, len(r.opts.stopSequences))
		for i, s := range r.opts.stopSequences {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&opt, "    stop_sequences [%s]\n", strings.Join(quoted, ", "))
	}
	if len(r.customHeaders) > 0 {
		opt.WriteString("    headers {\n")
		for k, v := range r.customHeaders {
			fmt.Fprintf(&opt, "      %q %q\n", k, v)
		}
		opt.WriteString("    }\n")
	}

	var prompt strings.Builder
	for _, m := range r.messages {
		fmt.Fprintf(&prompt, "    {{ _.role(%q) }}\n    %s\n", m.role, bamlPromptLine(m.text))
	}

	src = fmt.Sprintf(`
client<llm> OracleAnthropic {
  provider anthropic
  options {
%s  }
}

function %s() -> string {
  client OracleAnthropic
  prompt #"
%s  "#
}
`, opt.String(), fn, prompt.String())
	return src, fn
}

// bamlPromptLine returns literal message text safe to embed verbatim in a BAML
// raw-string (#"..."#) prompt block: raw strings are verbatim, so a backslash
// stays a backslash and matches the native Go literal — but a stray newline would
// break the single-line-per-turn shape, so it is rejected loudly (the corpus keeps
// each turn single-line).
func bamlPromptLine(text string) string {
	if strings.ContainsAny(text, "\n\r") {
		panic("corpus turn text must be single-line (no embedded newline)")
	}
	return text
}

// --- Row runner. ---

type rowCapture struct {
	baml     bamlCapture
	prep     *nanollm.PreparedRequest
	nanoSnap testutil.Snapshot
}

func runRow(t *testing.T, r anthropicRow) rowCapture {
	t.Helper()
	src, fn := bamlAnthropicSource(r)
	baml := captureBAMLAnthropic(t, src, fn, nil)

	client := newAnthropicClient(t, r.serviceRoot, r.customHeaders)
	req := buildCanonicalRequest(t, r.messages, r.opts)
	prep, nanoSnap := prepareNative(t, client, req)
	return rowCapture{baml: baml, prep: prep, nanoSnap: nanoSnap}
}

// TestAnthropicProviderCorrectness is the core differential over the corpus.
func TestAnthropicProviderCorrectness(t *testing.T) {
	for _, r := range anthropicCorpus() {
		r := r
		t.Run(r.name, func(t *testing.T) {
			cap := runRow(t, r)
			wantURL := messagesURL(r.serviceRoot)

			// (1) Method + endpoint exact on BOTH legs (not merely baml==nano).
			for _, leg := range []struct {
				name string
				snap testutil.Snapshot
			}{{"baml", cap.baml.snap}, {"nanollm", cap.nanoSnap}} {
				if leg.snap.Method != "POST" {
					t.Errorf("%s method = %q, want POST", leg.name, leg.snap.Method)
				}
				if leg.snap.URL != wantURL {
					// Never print the actual URL (it can carry query creds): report its
					// length and the fixed expected endpoint only.
					t.Errorf("%s endpoint mismatch: got_len=%d want=%q", leg.name, len(leg.snap.URL), wantURL)
				}
			}

			// (2) method + URL + required semantic header SET, via the shared redacted
			// comparator, with the body EXCLUDED (its member order is the expected
			// divergence, handled in step 4). baml-original-url is exempted on the
			// oracle side by testutil; any other header mismatch fails here.
			if diffs := testutil.Diff(headerOnly(cap.baml.snap), headerOnly(cap.nanoSnap)); len(diffs) != 0 {
				t.Errorf("method/URL/semantic-header differential mismatch:\n%s", strings.Join(diffs, "\n"))
			}

			// (3) Required provider MEANING on each leg (absolute anchors), then
			// cross-leg equality.
			bamlC := decodeAnthropic(t, "baml", cap.baml.body)
			nanoC := decodeAnthropic(t, "nanollm", cap.prep.Body)
			assertRequiredContract(t, "baml", bamlC, r)
			assertRequiredContract(t, "nanollm", nanoC, r)
			assertContractsEquivalent(t, bamlC, nanoC)

			// (4) Auth/version/content-type + native header order + the expected
			// key-order & baml-original-url divergence allowlist.
			assertAuthVersionContentType(t, cap.baml.snap.Headers, cap.nanoSnap.Headers)
			assertNativeHeaderOrder(t, cap.prep, r)
			assertExpectedDivergence(t, r, cap)
		})
	}
}

// TestAnthropicDeveloperRoleDisposition covers the required `developer`-source
// turn (§6.2 "absence of system/developer roles from provider messages"). BAML
// and native DISPOSE of a developer turn DIFFERENTLY — a documented, provider-
// valid divergence (a #583 fact, not a native defect):
//
//   - NATIVE (provider-correct): lifts the developer text into top-level `system`
//     alongside the system turn (OpenAI's developer-as-system semantics); provider
//     `messages` keep only the user/assistant turns.
//   - BAML: keeps top-level `system` = the system turn only and FOLDS the developer
//     text into the following user message's content as a leading text block.
//
// Both are valid Anthropic Messages requests; native's developer->system mapping
// is the provider-correct one. This test pins native's exact disposition, records
// BAML's divergent disposition, and asserts the divergence is real — so a future
// change that breaks native's mapping (or silently converges) is caught. It is NOT
// a cross-leg-equivalent row, so it runs outside the generic corpus loop.
func TestAnthropicDeveloperRoleDisposition(t *testing.T) {
	r := anthropicRow{
		name:        "developer_role",
		serviceRoot: fenceServiceRoot,
		messages: []corpusMessage{
			{role: "system", text: "Sys turn."},
			{role: "developer", text: "Dev turn."},
			{role: "user", text: "User turn."},
		},
		opts: bodyOptions{maxTokens: -1},
	}
	cap := runRow(t, r)
	wantURL := messagesURL(r.serviceRoot)

	// Transport still matches across legs: method, endpoint, required header set.
	for _, leg := range []struct {
		name string
		snap testutil.Snapshot
	}{{"baml", cap.baml.snap}, {"nanollm", cap.nanoSnap}} {
		if leg.snap.Method != "POST" {
			t.Errorf("%s method = %q, want POST", leg.name, leg.snap.Method)
		}
		if leg.snap.URL != wantURL {
			t.Errorf("%s endpoint mismatch: got_len=%d want=%q", leg.name, len(leg.snap.URL), wantURL)
		}
	}
	if diffs := testutil.Diff(headerOnly(cap.baml.snap), headerOnly(cap.nanoSnap)); len(diffs) != 0 {
		t.Errorf("method/URL/semantic-header differential mismatch:\n%s", strings.Join(diffs, "\n"))
	}
	assertAuthVersionContentType(t, cap.baml.snap.Headers, cap.nanoSnap.Headers)

	bamlC := decodeAnthropic(t, "baml", cap.baml.body)
	nanoC := decodeAnthropic(t, "nanollm", cap.prep.Body)

	// NATIVE provider-correct disposition: developer lifted into top-level system.
	wantNativeSystem := textBlockSeq("Sys turn.", "Dev turn.")
	if !blocksEqual(nanoC.System, wantNativeSystem) {
		t.Errorf("native: developer turn not lifted into top-level system (system_blocks=%d, want 2)", len(nanoC.System))
	}
	assertNoForbiddenRoles(t, "nanollm", nanoC.Messages)
	if len(nanoC.Messages) != 1 || nanoC.Messages[0].Role != "user" {
		t.Errorf("native: messages must be exactly [user], got %d turns", len(nanoC.Messages))
	} else if !blocksEqual(nanoC.Messages[0].Content, textBlockSeq("User turn.")) {
		t.Errorf("native: user content mismatch (blocks=%d)", len(nanoC.Messages[0].Content))
	}

	// BAML disposition (DOCUMENTED divergence): system = [Sys] only; developer text
	// folded into the user message content as a leading block.
	if !blocksEqual(bamlC.System, textBlockSeq("Sys turn.")) {
		t.Errorf("baml: expected system=[Sys] only (developer folded into user), got system_blocks=%d", len(bamlC.System))
	}
	assertNoForbiddenRoles(t, "baml", bamlC.Messages)
	if len(bamlC.Messages) != 1 || bamlC.Messages[0].Role != "user" {
		t.Errorf("baml: messages must be exactly [user], got %d turns", len(bamlC.Messages))
	} else if !blocksEqual(bamlC.Messages[0].Content, textBlockSeq("Dev turn.", "User turn.")) {
		t.Errorf("baml: expected developer folded into user content [Dev, User], got content_blocks=%d", len(bamlC.Messages[0].Content))
	}

	// The disposition genuinely DIVERGES: native's top-level system carries the
	// developer text, BAML's does not. Assert the divergence is real so a silent
	// convergence (or a broken native mapping) is caught.
	if blocksEqual(nanoC.System, bamlC.System) {
		t.Error("expected a documented BAML/native developer-disposition divergence in top-level system")
	}
}

// assertNoForbiddenRoles requires no system/developer role to survive into
// provider `messages` (Anthropic has neither; both must be lifted/mapped out).
func assertNoForbiddenRoles(t *testing.T, leg string, msgs []anthropicMessage) {
	t.Helper()
	for i, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			t.Errorf("%s: message[%d] retained forbidden role %q", leg, i, m.Role)
		}
	}
}

// headerOnly returns a body-less copy of a snapshot so testutil.Diff compares
// only method/URL/semantic headers (both bodies nil => equal); the body's member
// order is the intended divergence, proven separately.
func headerOnly(s testutil.Snapshot) testutil.Snapshot {
	return testutil.Snapshot{Method: s.Method, URL: s.URL, Headers: s.Headers, Body: nil}
}

// assertRequiredContract pins the provider-required meaning of one decoded leg
// against the row's expectations — the absolute anchors that fail even a bug that
// corrupts BOTH legs identically. Text values are compared but reported by LENGTH
// only (never the raw content).
func assertRequiredContract(t *testing.T, leg string, c anthropicContract, r anthropicRow) {
	t.Helper()
	if c.Model != fenceModel {
		t.Errorf("%s: model = %q, want %q", leg, c.Model, fenceModel)
	}
	if c.MaxTokens == nil {
		t.Errorf("%s: max_tokens absent; Anthropic requires it", leg)
	} else if *c.MaxTokens != r.wantMaxTokens() {
		t.Errorf("%s: max_tokens = %d, want %d", leg, *c.MaxTokens, r.wantMaxTokens())
	}

	// System lifted to top-level `system` (never left in messages): assert the
	// EXACT block sequence — count, per-block type, and text — not just the
	// concatenated text (a text-only compare would accept a missing/invalid block
	// `type`, a provider-invalid system block).
	wantSysBlocks := r.wantSystemBlocks()
	if len(c.System) != len(wantSysBlocks) {
		t.Errorf("%s: system block count = %d, want %d", leg, len(c.System), len(wantSysBlocks))
	} else {
		for i, blk := range c.System {
			if blk.Type != "text" {
				t.Errorf("%s: system block[%d] type = %q, want text", leg, i, blk.Type)
			}
			if blk.Text != wantSysBlocks[i].Text {
				t.Errorf("%s: system block[%d] text mismatch (got_len=%d want_len=%d)", leg, i, len(blk.Text), len(wantSysBlocks[i].Text))
			}
		}
	}

	// Provider messages: exact ordered non-system turns, each a single text block.
	wantMsgs := r.wantProviderMessages()
	if len(c.Messages) != len(wantMsgs) {
		t.Fatalf("%s: messages count = %d, want %d", leg, len(c.Messages), len(wantMsgs))
	}
	for i, m := range c.Messages {
		if m.Role == "system" || m.Role == "developer" {
			t.Errorf("%s: message[%d] retained forbidden role %q", leg, i, m.Role)
		}
		if m.Role != wantMsgs[i].role {
			t.Errorf("%s: message[%d] role = %q, want %q", leg, i, m.Role, wantMsgs[i].role)
		}
		if len(m.Content) != 1 {
			t.Fatalf("%s: message[%d] content blocks = %d, want 1", leg, i, len(m.Content))
		}
		if m.Content[0].Type != "text" {
			t.Errorf("%s: message[%d] block type = %q, want text", leg, i, m.Content[0].Type)
		}
		if m.Content[0].Text != wantMsgs[i].text {
			t.Errorf("%s: message[%d] text mismatch (got_len=%d want_len=%d)", leg, i, len(m.Content[0].Text), len(wantMsgs[i].text))
		}
	}

	// Mapped sampling options.
	assertPtrFloat(t, leg, "temperature", c.Temperature, r.opts.temperature)
	assertPtrFloat(t, leg, "top_p", c.TopP, r.opts.topP)
	assertPtrInt(t, leg, "top_k", c.TopK, r.opts.topK)
	if !equalStrings(c.StopSequences, r.opts.stopSequences) {
		t.Errorf("%s: stop_sequences count = %d, want %d", leg, len(c.StopSequences), len(r.opts.stopSequences))
	}
}

// assertContractsEquivalent proves the two legs carry the SAME provider meaning —
// a diagnostic cross-check layered on the absolute anchors (never an escape hatch:
// the divergence allowlist still pins each side's byte-order independently).
func assertContractsEquivalent(t *testing.T, a, b anthropicContract) {
	t.Helper()
	if a.Model != b.Model {
		t.Errorf("cross-leg model mismatch: baml=%q nanollm=%q", a.Model, b.Model)
	}
	if !equalPtrInt(a.MaxTokens, b.MaxTokens) {
		t.Error("cross-leg max_tokens mismatch")
	}
	// Exact system block sequence (type+text+order), not concatenated text.
	if !blocksEqual(a.System, b.System) {
		t.Errorf("cross-leg system block mismatch (baml_blocks=%d nanollm_blocks=%d)", len(a.System), len(b.System))
	}
	if len(a.Messages) != len(b.Messages) {
		t.Fatalf("cross-leg message count mismatch: baml=%d nanollm=%d", len(a.Messages), len(b.Messages))
	}
	for i := range a.Messages {
		if a.Messages[i].Role != b.Messages[i].Role {
			t.Errorf("cross-leg message[%d] role mismatch: baml=%q nanollm=%q", i, a.Messages[i].Role, b.Messages[i].Role)
		}
		// Exact content block sequence (type+text+order), not concatenated text.
		if !blocksEqual(a.Messages[i].Content, b.Messages[i].Content) {
			t.Errorf("cross-leg message[%d] content block mismatch (baml_blocks=%d nanollm_blocks=%d)", i, len(a.Messages[i].Content), len(b.Messages[i].Content))
		}
	}
	if !equalPtrFloat(a.Temperature, b.Temperature) {
		t.Error("cross-leg temperature mismatch")
	}
	if !equalPtrFloat(a.TopP, b.TopP) {
		t.Error("cross-leg top_p mismatch")
	}
	if !equalPtrInt(a.TopK, b.TopK) {
		t.Error("cross-leg top_k mismatch")
	}
	if !equalStrings(a.StopSequences, b.StopSequences) {
		t.Error("cross-leg stop_sequences mismatch")
	}
}

// assertAuthVersionContentType pins the required Anthropic auth/version/content
// headers on BOTH legs: exactly the fake x-api-key (redacted), a non-empty
// anthropic-version, JSON content-type, and NO OpenAI-style bearer authorization.
func assertAuthVersionContentType(t *testing.T, bamlHeaders, nanoHeaders map[string]string) {
	t.Helper()
	for _, leg := range []struct {
		name string
		h    map[string]string
	}{{"baml", bamlHeaders}, {"nanollm", nanoHeaders}} {
		if v, ok := leg.h["x-api-key"]; !ok || v != fenceAPIKey {
			t.Errorf("%s: x-api-key missing or mismatched (present=%v, matched=%v)", leg.name, ok, v == fenceAPIKey)
		}
		if v, ok := leg.h["anthropic-version"]; !ok || v != anthropicVersion {
			t.Errorf("%s: anthropic-version = %q (present=%v), want %q", leg.name, v, ok, anthropicVersion)
		}
		if v, ok := leg.h["content-type"]; !ok || !strings.HasPrefix(v, "application/json") {
			t.Errorf("%s: content-type = %q (present=%v), want application/json", leg.name, v, ok)
		}
		if _, ok := testutil.Authorization(leg.h); ok {
			t.Errorf("%s: unexpected OpenAI-style authorization header on an Anthropic request", leg.name)
		}
	}
}

// assertNativeHeaderOrder pins native's OWN emitted header order — Content-Type,
// x-api-key, anthropic-version, then any custom headers — and is NEVER compared to
// BAML (whose header order/casing is explicitly unclaimed).
func assertNativeHeaderOrder(t *testing.T, prep *nanollm.PreparedRequest, r anthropicRow) {
	t.Helper()
	wantPrefix := []string{"Content-Type", "x-api-key", "anthropic-version"}
	if len(prep.Headers) != len(wantPrefix)+len(r.customHeaders) {
		t.Fatalf("native emitted %d headers, want %d", len(prep.Headers), len(wantPrefix)+len(r.customHeaders))
	}
	for i, want := range wantPrefix {
		if prep.Headers[i][0] != want {
			t.Errorf("native header[%d] name = %q, want %q", i, prep.Headers[i][0], want)
		}
	}
	// Custom headers follow the fixed prefix (order among them is unclaimed).
	for name, val := range r.customHeaders {
		found := false
		for _, p := range prep.Headers[len(wantPrefix):] {
			if p[0] == name {
				found = true
				if p[1] != val {
					t.Errorf("native custom header %q value mismatch (len got=%d want=%d)", name, len(p[1]), len(val))
				}
			}
		}
		if !found {
			t.Errorf("native missing custom header %q", name)
		}
	}
}

// assertExpectedDivergence is the checked-in expected-divergence allowlist: it
// pins each side's EXACT top-level member order, proves the two orders differ but
// carry the same key SET, and proves `baml-original-url` is the ONE, one-sided
// BAML-only header (native emits none such). No semantic-equality escape hatch:
// an unlisted reorder/new key fails the pinned-order assertion.
func assertExpectedDivergence(t *testing.T, r anthropicRow, cap rowCapture) {
	t.Helper()

	bamlKeys := topLevelKeys(t, "baml", cap.baml.body)
	nanoKeys := topLevelKeys(t, "nanollm", cap.prep.Body)

	if !sameOrder(bamlKeys, r.bamlOrder) {
		t.Errorf("BAML top-level order = %v, want allowlisted %v", bamlKeys, r.bamlOrder)
	}
	if !sameOrder(nanoKeys, r.nanoOrder) {
		t.Errorf("native top-level order = %v, want allowlisted %v", nanoKeys, r.nanoOrder)
	}
	if !sameKeySet(bamlKeys, nanoKeys) {
		t.Errorf("top-level key SETS differ (not a pure ordering divergence): baml=%v nanollm=%v", bamlKeys, nanoKeys)
	}
	if sameOrder(bamlKeys, nanoKeys) {
		t.Errorf("expected a member-order divergence but BAML and native emitted identical order %v", bamlKeys)
	}

	// One-sided baml-original-url: BAML carries exactly it (beyond the shared
	// semantic set); native carries no such transport header.
	_, bamlTransport := testutil.SplitSemantic(cap.baml.snap.Headers)
	if _, ok := bamlTransport["baml-original-url"]; !ok {
		t.Error("expected BAML to carry its internal baml-original-url transport header")
	}
	if len(bamlTransport) != 1 {
		t.Errorf("BAML carries %d transport-only headers, want exactly baml-original-url", len(bamlTransport))
	}
	if _, ok := cap.nanoSnap.Headers["baml-original-url"]; ok {
		t.Error("native must NOT emit baml-original-url")
	}
}

// --- small typed comparison helpers (redacted diagnostics). ---

func assertPtrFloat(t *testing.T, leg, field string, got, want *float64) {
	t.Helper()
	if !equalPtrFloat(got, want) {
		t.Errorf("%s: %s presence/value mismatch (got_set=%v want_set=%v)", leg, field, got != nil, want != nil)
	}
}

func assertPtrInt(t *testing.T, leg, field string, got, want *int) {
	t.Helper()
	if !equalPtrInt(got, want) {
		t.Errorf("%s: %s presence/value mismatch (got_set=%v want_set=%v)", leg, field, got != nil, want != nil)
	}
}

func equalPtrFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalPtrInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
