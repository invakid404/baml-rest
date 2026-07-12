//go:build integration

package staticoracle

// Static stock-BAML differential raw-body oracle for the native canonical OpenAI
// chat-completions body builder (de-BAML Phase 4a). It is the static counterpart
// of internal/nativebody's dynamic oracle and lives HERE, in the staticoracle
// package, because it must link the STOCK upstream BAML v0.223.0 generated client
// (not the patched dynclient runtime) — the same reason the static prompt oracle
// lives here. Keeping both BAML runtimes out of one test binary is deliberate.
//
// For every claimed-corpus row it renders the prompt natively (RenderStatic),
// normalizes the client from the descriptor's passive ClientConfig
// (nativebody.NormalizeStaticClient — the model comes from the descriptor, proving
// the P4a descriptor threading works end to end), builds the native canonical
// body, and compares it BYTE-FOR-BYTE against the body BAML v0.223.0 actually
// builds via Request.<Function>() (read directly from req.Body().Text(), no send).
//
// Raw bytes are primary and neither side is normalized before comparison — key
// order, the content-array layout, escaping, and (once proven) any flattened
// request_body order are exactly what must match. This binary also runs the
// existing TestBAMLVersionPinned and TestSourceMapDriftGuard guards, so a green
// result is pinned to stock BAML v0.223.0 rendering the checked-in source.
//
// Run:
//
//	CGO_ENABLED=1 go test -tags integration ./internal/nativeprompt/staticoracle

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	"github.com/invakid404/baml-rest/internal/nativeschema"
)

// staticBodyAlias is the SEPARATE nanollm alias threaded through the body
// builder. It deliberately differs from the fixture target model
// ("fake-static-oracle-model") so the model-separation assertion is meaningful.
const staticBodyAlias = "nanollm://openai/fake-static-oracle-model"

// fixtureTargetModel is the exact model literal declared in the fixture
// clients.baml; it must appear verbatim in every BAML body's top-level model.
const fixtureTargetModel = "fake-static-oracle-model"

// TestStaticBodyOracleParity proves, for every claimed-corpus row, that the
// native canonical body equals BAML v0.223.0's actual request body byte-for-byte.
func TestStaticBodyOracleParity(t *testing.T) {
	descriptors := buildDescriptors(t)

	for _, c := range oracleCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fn, ok := descriptors[c.fn]
			if !ok {
				t.Fatalf("no built descriptor for %q", c.fn)
			}

			// The descriptor must carry the resolved literal model (proves the P4a
			// ClientConfig extraction + threading populated it).
			if fn.ClientConfig.Model.Provenance != promptdescriptor.ModelProvenanceLiteral ||
				fn.ClientConfig.Model.Value != fixtureTargetModel {
				t.Fatalf("descriptor ClientConfig model = %+v, want literal %q", fn.ClientConfig.Model, fixtureTargetModel)
			}

			// Native leg: render + normalize client + build canonical body.
			native := buildNativeBody(t, fn, c.args)

			// BAML leg: build the provider request (no send) and read its body.
			req, err := c.build()
			if err != nil {
				t.Fatalf("BAML Request.%s: %v", c.fn, err)
			}
			text, err := bodyText(req)
			if err != nil {
				t.Fatalf("BAML body: %v", err)
			}
			if text == "" {
				t.Fatal("BAML body is empty — request build failed before producing a body")
			}

			if string(native.Bytes()) != text {
				t.Errorf("native canonical body diverged from BAML v0.223 body\n--- native ---\n%s\n--- BAML ---\n%s",
					native.Bytes(), text)
			}

			// Model separation: the JSON model is the target; the alias never leaks.
			if native.TargetModel() != fixtureTargetModel {
				t.Errorf("target model = %q, want %q", native.TargetModel(), fixtureTargetModel)
			}
			if strings.Contains(string(native.Bytes()), staticBodyAlias) {
				t.Errorf("nanollm alias leaked into JSON body: %s", native.Bytes())
			}
		})
	}
}

// TestStaticBodyOracleMutation is the negative control: a single-byte mutation of
// the native body must break byte equality with BAML's body.
func TestStaticBodyOracleMutation(t *testing.T) {
	descriptors := buildDescriptors(t)
	fn, ok := descriptors["StaticRoleChat"]
	if !ok {
		t.Fatalf("no built descriptor for StaticRoleChat")
	}
	args := map[string]any{"topic": "weather", "count": int64(3)}
	native := buildNativeBody(t, fn, args)

	req, err := bamlclient.Request.StaticRoleChat("weather", 3)
	if err != nil {
		t.Fatalf("BAML Request.StaticRoleChat: %v", err)
	}
	text, err := bodyText(req)
	if err != nil {
		t.Fatalf("BAML body: %v", err)
	}
	if string(native.Bytes()) != text {
		t.Fatalf("precondition: native must equal BAML before mutation\n--- native ---\n%s\n--- BAML ---\n%s", native.Bytes(), text)
	}

	mutated := native.Bytes()
	mutated[len(mutated)/2] ^= 0x20
	if string(mutated) == text {
		t.Fatal("a one-byte mutation was NOT detected — the oracle is not byte-sensitive")
	}
}

// TestStaticClientConfigFencedFixture proves the P4a descriptor extractor
// (nativeschema.BuildClientConfigs) retains the ordered typed request_body,
// last-wins duplicate placement, nested object/list values, and the transport-vs
// body-affecting split — from the SAME source-map-fenced fixture the stock BAML
// runtime loads (TestSourceMapDriftGuard / TestBAMLVersionPinned run in this
// binary). RichBodyClient is referenced by no function, so BAML never builds a
// body from it; it is a fenced extraction + native-decline fixture only.
func TestStaticClientConfigFencedFixture(t *testing.T) {
	files := readFixtureFiles(t)
	cfgs := nativeschema.BuildClientConfigs(files)
	cfg, ok := cfgs["RichBodyClient"]
	if !ok {
		t.Fatalf("no ClientConfig for RichBodyClient")
	}
	if !cfg.Present {
		t.Fatalf("RichBodyClient config must be Present")
	}

	// Literal model.
	if cfg.Model.Provenance != promptdescriptor.ModelProvenanceLiteral || cfg.Model.Value != "rich-body-model" {
		t.Errorf("model = %+v, want literal rich-body-model", cfg.Model)
	}
	// Transport-only options in source order.
	if got := clientOptionKeys(cfg.TransportOptions); !eqStrings(got, []string{"api_key", "base_url"}) {
		t.Errorf("transport options = %v, want [api_key base_url]", got)
	}
	// Body-affecting (non-request_body) options.
	if got := clientOptionKeys(cfg.BodyAffectingOptions); !eqStrings(got, []string{"temperature"}) {
		t.Errorf("body-affecting options = %v, want [temperature]", got)
	}
	// request_body order with last-wins on the duplicate `seed` (moves to its last
	// position, value 7).
	wantOrder := []string{"top_p", "metadata", "stop", "seed", "stream_hint"}
	if got := requestBodyKeys(cfg.RequestBody); !eqStrings(got, wantOrder) {
		t.Fatalf("request_body order = %v, want %v", got, wantOrder)
	}
	byKey := map[string]promptdescriptor.OptionValue{}
	for _, e := range cfg.RequestBody {
		byKey[e.Key] = e.Value
	}
	if v := byKey["seed"]; v.Kind != promptdescriptor.OptionNumber || v.Number != "7" {
		t.Errorf("seed = %+v, want number 7 (last-wins)", v)
	}
	if v := byKey["stream_hint"]; v.Kind != promptdescriptor.OptionBool || !v.Bool {
		t.Errorf("stream_hint = %+v, want bool true", v)
	}
	// nested object preserves order.
	meta := byKey["metadata"]
	if meta.Kind != promptdescriptor.OptionObject || !eqStrings(requestBodyKeys(meta.Object), []string{"tag", "rank"}) {
		t.Errorf("metadata = %+v, want ordered object [tag rank]", meta)
	}
	// list value.
	stop := byKey["stop"]
	if stop.Kind != promptdescriptor.OptionList || len(stop.List) != 2 || stop.List[0].String != "a" || stop.List[1].String != "b" {
		t.Errorf("stop = %+v, want list [a b]", stop)
	}

	// Native admission: a nonempty request_body declines FeatureRequestBody before
	// serialization (4a admits only an absent request_body; an explicit empty
	// request_body {} also declines — see TestStaticEmptyRequestBodyFencedFixture).
	intent, err := nativebody.NormalizeStaticClient(cfg, "alias", false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	var d *nativebody.Decline
	err = nativebody.SupportsOpenAIChat(&nativeprompt.RenderedPrompt{
		Kind:     nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{{Role: "user", Parts: []nativeprompt.Part{{Text: strPtr("hi")}}}},
	}, intent)
	if !errors.As(err, &d) || d.Feature != nativebody.FeatureRequestBody {
		t.Fatalf("RichBodyClient must decline FeatureRequestBody, got %v", err)
	}
}

// TestStaticEmptyRequestBodyFencedFixture proves, from the source-map-fenced
// fixture (stock BAML v0.223 loads EmptyBodyClient), that an EXPLICIT empty
// `request_body {}` block is recorded as RequestBodyPresent==true with zero
// entries — NOT equivalent to an absent block — and that the native builder
// DECLINES it (FeatureRequestBody). This establishes the empty-vs-absent boundary
// on the stock-BAML-loaded path: BAML v0.223 emits "request_body":{} for the
// present-but-empty block (verified against the oracle in review), so a native
// body that omits it would diverge; native declines instead.
func TestStaticEmptyRequestBodyFencedFixture(t *testing.T) {
	files := readFixtureFiles(t)
	cfgs := nativeschema.BuildClientConfigs(files)

	empty, ok := cfgs["EmptyBodyClient"]
	if !ok {
		t.Fatalf("no ClientConfig for EmptyBodyClient")
	}
	if !empty.RequestBodyPresent {
		t.Errorf("EmptyBodyClient: explicit request_body {} must set RequestBodyPresent=true")
	}
	if len(empty.RequestBody) != 0 {
		t.Errorf("EmptyBodyClient: request_body {} must have zero entries, got %v", empty.RequestBody)
	}

	// Absent-block sibling for contrast: StaticOracleClient declares no
	// request_body, so it is NOT present and is admitted (its byte-exact parity is
	// proven by TestStaticBodyOracleParity).
	admitted, ok := cfgs["StaticOracleClient"]
	if !ok {
		t.Fatalf("no ClientConfig for StaticOracleClient")
	}
	if admitted.RequestBodyPresent {
		t.Errorf("StaticOracleClient declares no request_body; RequestBodyPresent must be false")
	}

	// Native: the explicit empty block declines FeatureRequestBody before
	// serialization.
	intent, err := nativebody.NormalizeStaticClient(empty, "alias", false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	var d *nativebody.Decline
	err = nativebody.SupportsOpenAIChat(&nativeprompt.RenderedPrompt{
		Kind:     nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{{Role: "user", Parts: []nativeprompt.Part{{Text: strPtr("hi")}}}},
	}, intent)
	if !errors.As(err, &d) || d.Feature != nativebody.FeatureRequestBody {
		t.Fatalf("explicit empty request_body must decline FeatureRequestBody, got %v", err)
	}
}

// TestStaticEscapedModelFencedFixture proves, from the source-map-fenced fixture
// (stock BAML v0.223 loads EscapedModelClient), that a REGULAR string model
// literal bearing an escape sequence is retained verbatim by the descriptor
// (RawString==false, escape bytes preserved) and DECLINES via FeatureModelEscape.
// BAML decodes such a literal (`"gpt\t4"` -> a real tab, emitted as "model":"gpt\t4",
// verified against the oracle during review) while native retains the raw
// "gpt\\t4" bytes, so admitting it would serialize a different model than BAML.
func TestStaticEscapedModelFencedFixture(t *testing.T) {
	files := readFixtureFiles(t)
	cfgs := nativeschema.BuildClientConfigs(files)

	esc, ok := cfgs["EscapedModelClient"]
	if !ok {
		t.Fatalf("no ClientConfig for EscapedModelClient")
	}
	if esc.Model.Provenance != promptdescriptor.ModelProvenanceLiteral {
		t.Errorf("EscapedModelClient model provenance = %q, want literal", esc.Model.Provenance)
	}
	if esc.Model.RawString {
		t.Errorf("EscapedModelClient model is a REGULAR literal; RawString must be false")
	}
	if !strings.Contains(esc.Model.Value, `\`) {
		t.Errorf("descriptor must retain the raw escape bytes, got %q", esc.Model.Value)
	}

	intent, err := nativebody.NormalizeStaticClient(esc, "alias", false)
	if !errors.Is(err, nativebody.ErrUnsupported) {
		t.Fatalf("escaped regular model literal must decline, got intent=%+v err=%v", intent, err)
	}
	var d *nativebody.Decline
	if !errors.As(err, &d) || d.Feature != nativebody.FeatureModelEscape {
		t.Fatalf("escaped regular model literal must decline FeatureModelEscape, got %v", err)
	}
}

func strPtr(s string) *string { return &s }

func clientOptionKeys(opts []promptdescriptor.ClientOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Key)
	}
	return out
}

func requestBodyKeys(entries []promptdescriptor.RequestBodyEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}

func eqStrings(a, b []string) bool {
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

// buildNativeBody runs the native leg: SupportsStatic must accept, then
// RenderStatic + NormalizeStaticClient + BuildOpenAIChat must produce a body.
func buildNativeBody(t *testing.T, fn promptdescriptor.Function, args map[string]any) *nativebody.CanonicalBody {
	t.Helper()
	if err := nativeprompt.SupportsStatic(fn, args); err != nil {
		t.Fatalf("SupportsStatic declined a claimed case: %v", err)
	}
	rendered, err := nativeprompt.RenderStatic(fn, args)
	if err != nil {
		t.Fatalf("RenderStatic failed after SupportsStatic accepted: %v", err)
	}
	intent, err := nativebody.NormalizeStaticClient(fn.ClientConfig, staticBodyAlias, false /* non-stream */)
	if err != nil {
		t.Fatalf("NormalizeStaticClient declined the fixture client: %v", err)
	}
	native, err := nativebody.BuildOpenAIChat(rendered, intent)
	if err != nil {
		t.Fatalf("BuildOpenAIChat declined a claimed case: %v", err)
	}
	return native
}

// TestStaticBodyOracleStreamControl is the streaming decline CONTROL for the
// stock-BAML path: the generated StreamRequest.<Fn>() body appends
// "stream":true,"stream_options":{...} after messages, while the native builder
// must DECLINE a streaming attempt (FeatureStreaming) — the stream request never
// reaches serialization.
func TestStaticBodyOracleStreamControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	fn, ok := descriptors["StaticCompletion"]
	if !ok {
		t.Fatalf("no built descriptor for StaticCompletion")
	}

	// BAML leg: the stock stream request body carries stream:true.
	req, err := bamlclient.StreamRequest.StaticCompletion("cats and dogs")
	if err != nil {
		t.Fatalf("BAML StreamRequest.StaticCompletion: %v", err)
	}
	text, err := bodyText(req)
	if err != nil {
		t.Fatalf("BAML stream body: %v", err)
	}
	// The v0.223 stream body carries BOTH stream:true AND the stream_options
	// object; a regression that drops either must fail this control.
	if !strings.Contains(text, `"stream":true`) {
		t.Fatalf("BAML stream body missing \"stream\":true, got %s", text)
	}
	if !strings.Contains(text, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("BAML stream body missing \"stream_options\":{\"include_usage\":true}, got %s", text)
	}

	// Native leg: a streaming attempt must DECLINE FeatureStreaming, not serialize.
	rendered, err := nativeprompt.RenderStatic(fn, map[string]any{"topic": "cats and dogs"})
	if err != nil {
		t.Fatalf("RenderStatic: %v", err)
	}
	intent, err := nativebody.NormalizeStaticClient(fn.ClientConfig, staticBodyAlias, true /* stream */)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	body, err := nativebody.BuildOpenAIChat(rendered, intent)
	if body != nil || err == nil {
		t.Fatalf("streaming attempt must not serialize; got body=%v err=%v", body, err)
	}
	if !errors.Is(err, nativebody.ErrUnsupported) {
		t.Fatalf("stream decline must unwrap to ErrUnsupported: %v", err)
	}
	var d *nativebody.Decline
	if !errors.As(err, &d) || d.Feature != nativebody.FeatureStreaming {
		t.Fatalf("streaming attempt must decline FeatureStreaming, got %v", err)
	}
}
