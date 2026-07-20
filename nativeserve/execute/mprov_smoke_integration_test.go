//go:build nanollm_integration

package execute

// S2 multi-provider go-mocklm smokes (§10.3): the minimal registry -> AdmitClaim
// -> exactly one ExactExecutor RoundTrip -> provider-native go-mocklm response ->
// TranslateResponse -> assistant extraction -> native SAP -> expected structured
// JSON, one request per provider. It proves OUR admission+execute plumbing wires a
// trusted provider end-to-end — NOT nanollm-vs-BAML.
//
// anthropic/cerebras run over the clear-text loopback mock; bedrock runs over the
// mock's TLS listener with a custom test dial that connects the UNTOUCHED prepared
// AWS host to loopback (no URL/Host/header/signature/body rewrite). Every key/
// credential is fake and every transport is loopback-fenced. Cohere has NO smoke
// (deferred; embeddings-only in v0.4.3 — it auto-declines pre-socket, covered by
// admission's gated tripwire).

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

const (
	// smokeAlias is the SEPARATE internal nanollm alias — distinct from every target
	// model and client name so admission's alias gate is satisfied.
	smokeAlias = "__s2_mprov_smoke_alias__"
	// smokeContent is the exact assistant text the mock returns; the native SAP
	// cleanly claims it against personSchema6b.
	smokeContent = `{"name":"Ada","age":36}`
)

// smokeInput builds a fully-formed admitted unary `_dynamic` call for a trusted
// provider registry.
func smokeInput(reg *bamlutils.ClientRegistry, provider string) admission.Input {
	return admission.Input{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		Method:              bamlutils.DynamicMethodName,
		Mode:                admission.ModeCall,
		SingleLeaf:          true,
		ResolvedProvider:    provider,
		Registry:            reg,
		Alias:               smokeAlias,
		Messages: []bamlutils.DynamicMessage{
			{Role: "system", TextContent: sp("You are a precise extractor.")},
			{Role: "user", TextContent: sp("Return the person as JSON.")},
		},
		OutputSchema: personSchema6b(),
	}
}

// runSmoke drives registry -> AdmitClaim -> RunAttempt over exec against the mock,
// asserting exactly one structured request. scenarioID must already be registered.
func runSmoke(t *testing.T, reg *bamlutils.ClientRegistry, provider, scenarioID string, exec *llmhttp.ExactExecutor, m *mockServer) {
	t.Helper()
	admitter := admission.NewAdmitter(nil, exec)
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	claim, err := admitter.AdmitClaim(ctx, smokeInput(reg, provider))
	if err != nil {
		t.Fatalf("%s: AdmitClaim: %v", provider, err)
	}
	defer claim.Close()

	spy := &parseSpy{fn: debaml.Parse}
	res, aerr := RunAttempt(ctx, AttemptConfig{
		Client:       claim.Client(),
		Prepared:     claim.Prepared,
		Executor:     exec,
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if aerr != nil {
		t.Fatalf("%s: RunAttempt: %v", provider, aerr)
	}
	if res.Outcome != OutcomeStructured {
		t.Fatalf("%s: outcome = %s, want structured (provider body: %s)", provider, res.Outcome, bodyDigest(res.ProviderBody))
	}
	if !res.SAPInvoked || spy.calls != 1 {
		t.Errorf("%s: SAP invoked=%v calls=%d, want once", provider, res.SAPInvoked, spy.calls)
	}
	if !jsonSemEqual(t, res.Structured, []byte(smokeContent)) {
		t.Errorf("%s: structured %s != want", provider, bodyDigest(res.Structured))
	}
	if got := m.scenarioRequestCount(scenarioID); got != 1 {
		t.Errorf("%s: scenario request count = %d, want exactly 1", provider, got)
	}
}

// TestMprovSmokeAnthropic: the registry service-root base is /v1-adapted by the
// mapper so nanollm's anthropic route hits the mock's /v1/messages, returns a
// native Anthropic message, and the pipeline reaches native structured output.
func TestMprovSmokeAnthropic(t *testing.T) {
	m := startMock(t)
	const target = "claude-3-haiku-mock"
	const id = "s2-smoke-anthropic"
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "anthropic",
		Model:    target,
		Output:   &exactOutput{Text: smokeContent, OutputTokens: 7},
	})
	reg := &bamlutils.ClientRegistry{
		Primary: sp("A"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "A",
			Provider: "anthropic",
			Options: map[string]any{
				"model":    target,
				"api_key":  "sk-ant-fake",
				"base_url": m.baseURL, // service root; the mapper appends /v1
			},
		}},
	}
	runSmoke(t, reg, "anthropic", id, exactLoopbackExecutor(), m)

	// Harden the Anthropic path: beyond the shared structured-output plumbing, assert
	// the ONE request that crossed the wire is a provider-correct Anthropic Messages
	// call. go-mocklm is a functional responder (a header MAP, no exact-byte order),
	// so exact plan parity lives in the no-send provideroracle oracle; here we pin the
	// captured request-contract facts the smoke can prove.
	assertAnthropicSmokeContract(t, m, id, target)
}

// assertAnthropicSmokeContract pins the captured Anthropic request contract: POST
// /v1/messages, the fake x-api-key + a present anthropic-version + JSON
// content-type, a native Anthropic body carrying the target model and a required
// max_tokens, the system turn lifted to top-level `system`, and NO system/developer
// role surviving into provider `messages`. Secrets are reported by presence/match
// only (never the raw key), and the body is digested, never dumped raw.
func assertAnthropicSmokeContract(t *testing.T, m *mockServer, id, target string) {
	t.Helper()

	// Method/path/headers from the global request log (canonicalized header map).
	rec := m.recordedFor("anthropic", "/v1/messages")
	if rec.Method != http.MethodPost {
		t.Errorf("captured Anthropic method = %q, want POST", rec.Method)
	}
	if v, ok := headerValue(rec.Headers, "x-api-key"); !ok || v != "sk-ant-fake" {
		// Never print the api-key value; report presence + match only.
		t.Errorf("x-api-key missing or mismatched (present=%v, matched=%v)", ok, v == "sk-ant-fake")
	}
	if v, ok := headerValue(rec.Headers, "anthropic-version"); !ok || v == "" {
		t.Errorf("anthropic-version = %q (present=%v), want non-empty", v, ok)
	}
	if v, ok := headerValue(rec.Headers, "content-type"); !ok || !strings.HasPrefix(v, "application/json") {
		t.Errorf("content-type = %q (present=%v), want application/json", v, ok)
	}

	// Byte-exact captured body: a native Anthropic Messages request.
	cap := m.lastRequest(id)
	if cap.Path != "/v1/messages" {
		t.Errorf("captured path = %q, want /v1/messages", cap.Path)
	}
	var areq anthropicRequestBody
	if err := json.Unmarshal(cap.Body, &areq); err != nil {
		// Never echo the body or a content-derived digest — report stage + length only.
		t.Fatalf("captured Anthropic body is not JSON (stage=json_unmarshal, len=%d)", len(cap.Body))
	}
	if areq.Model != target {
		t.Errorf("captured model = %q, want %q", areq.Model, target)
	}
	if areq.MaxTokens < 1 {
		t.Errorf("captured max_tokens = %d, want >= 1 (Anthropic requires it)", areq.MaxTokens)
	}
	if len(areq.System) == 0 || string(areq.System) == "null" {
		t.Errorf("captured top-level system missing; the system turn must be lifted out of messages")
	}
	roles := make([]string, len(areq.Messages))
	for i, msg := range areq.Messages {
		roles[i] = msg.Role
	}
	assertMessageRoles(t, roles, "system", "developer")
}

// TestMprovSmokeCerebras: cerebras is OpenAI-wire-compatible, so nanollm's
// cerebras route hits the mock's strict OpenAI /v1/chat/completions route (there is
// no first-class cerebras route in go-mocklm v0.4.0 — documented compatibility
// route). base_url is the nanollm API base (mock + /v1).
func TestMprovSmokeCerebras(t *testing.T) {
	m := startMock(t)
	const target = "llama-3.1-8b-mock"
	const id = "s2-smoke-cerebras"
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "openai", // cerebras uses the OpenAI-compatible route
		Model:    target,
		Output:   &exactOutput{Text: smokeContent, OutputTokens: 7},
	})
	reg := &bamlutils.ClientRegistry{
		Primary: sp("C"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "C",
			Provider: "cerebras",
			Options: map[string]any{
				"model":    target,
				"api_key":  "sk-cb-fake",
				"base_url": m.baseURL + "/v1",
			},
		}},
	}
	runSmoke(t, reg, "cerebras", id, exactLoopbackExecutor(), m)
}

// TestMprovSmokeBedrock: nanollm signs a SigV4 plan for the fixed
// bedrock-runtime.<region>.amazonaws.com host; a custom test dial connects that
// UNTOUCHED host to the mock's loopback TLS port (no URL/Host/header/signature/body
// rewrite), the mock's /model/{id}/converse route returns Converse JSON, and
// nanollm translates it back to OpenAI for the native SAP.
func TestMprovSmokeBedrock(t *testing.T) {
	m, tlsPort := startMockTLS(t)
	const target = "anthropic.claude-v2"
	const id = "s2-smoke-bedrock"
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "bedrock",
		Model:    target,
		Output:   &exactOutput{Text: smokeContent, OutputTokens: 7},
	})
	reg := &bamlutils.ClientRegistry{
		Primary: sp("B"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "B",
			Provider: "aws-bedrock",
			Options: map[string]any{
				"model_id":          target,
				"region":            "us-east-1",
				"access_key_id":     "AKIAFAKEFAKEFAKE",
				"secret_access_key": "fakefakefakefakefakefakefakefakefakefake",
			},
		}},
	}
	runSmoke(t, reg, "aws-bedrock", id, bedrockTLSExecutor(t, tlsPort), m)
}

// bedrockTLSExecutor builds an ExactExecutor whose dial connects the SIGNED Bedrock
// runtime host to the mock's loopback TLS port WITHOUT touching the request's URL,
// Host, headers, SigV4 signature, or body. Before redirecting it ASSERTS the dial
// target is exactly the us-east-1 fixture host, so a wrong signed endpoint cannot
// pass the smoke by being silently redirected to loopback (CR-B). It is loopback-
// fenced (it only ever dials loopback after the assertion) and accepts the mock's
// self-signed cert (InsecureSkipVerify — a loopback test only). Keep-alives/
// compression are disabled to match the exact lane's transparency.
func bedrockTLSExecutor(t *testing.T, tlsPort int) *llmhttp.ExactExecutor {
	t.Helper()
	return llmhttp.NewExactExecutor(&http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// The prepared request must target the SigV4-signed us-east-1 Bedrock
			// runtime host. Reject any other dial target (t.Errorf is safe from the
			// dial goroutine; t.Fatalf is not) and abort the dial rather than masking a
			// wrong endpoint behind the loopback redirect. addr is a bounded host:port,
			// not a secret.
			const wantHost = "bedrock-runtime.us-east-1.amazonaws.com:443"
			if addr != wantHost {
				t.Errorf("bedrock dial target = %q, want the signed host %q (a wrong signed endpoint must not be redirected to loopback)", addr, wantHost)
				return nil, fmt.Errorf("unexpected bedrock dial target %q", addr)
			}
			var d net.Dialer
			// redirect the dial (only) to the loopback TLS port. The request itself is
			// transmitted byte-for-byte, so the SigV4 signature stays valid.
			return d.DialContext(ctx, network, net.JoinHostPort(loopbackHost, strconv.Itoa(tlsPort)))
		},
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
		DisableCompression: true,
		DisableKeepAlives:  true,
	})
}

// startMockTLS launches an isolated go-mocklm subprocess with BOTH the clear-text
// lane (for admin/health/scenario management) and the TLS lane (for provider
// requests) enabled, and returns the running server plus the loopback TLS port.
// It mirrors startMock's spawn/health/cleanup plumbing, adding the TLS listener.
func startMockTLS(t *testing.T) (*mockServer, int) {
	t.Helper()

	bin := mocklmBinary(t)
	httpPort := freeLoopbackPort(t)
	tlsPort := freeLoopbackPort(t)

	cfgPath := filepath.Join(t.TempDir(), "mocklm.toml")
	cfg := fmt.Sprintf("[server]\nhost = %q\nport = %d\n", loopbackHost, httpPort)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing mock config: %v", err)
	}

	logs := &lockedBuffer{}
	cmd := exec.Command(bin)
	// go-mocklm reads only these variables; a scoped env keeps ambient values out.
	// The TLS lane generates an in-memory self-signed cert (accepted via the dial's
	// InsecureSkipVerify).
	cmd.Env = []string{
		"CONFIG_PATH=" + cfgPath,
		"MOCKLM_HTTP_ENABLED=1",
		"MOCKLM_TLS_ENABLED=1",
		"MOCKLM_TLS_PORT=" + strconv.Itoa(tlsPort),
		"MOCKLM_VALIDATE_RESPONSES=1",
	}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting go-mocklm (TLS, %s): %v", bin, err)
	}

	m := &mockServer{
		t:       t,
		cmd:     cmd,
		baseURL: fmt.Sprintf("http://%s:%d", loopbackHost, httpPort),
		port:    httpPort,
		admin:   &http.Client{Transport: loopbackTransport(), Timeout: adminTimeout},
		logs:    logs,
		done:    make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(m.done)
	}()
	t.Cleanup(m.stop)

	m.waitHealthy()
	return m, tlsPort
}
