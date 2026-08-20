package worker

// De-BAML serving cutover S3a — the PUBLIC-REQUEST-PATH proof for the
// TRUSTED-CONFIGURATION SEAL.
//
// The seal is the whole provenance boundary: a configuration identity exists only
// for a client the DEPLOYMENT configured, and never for one the caller described.
// Unit-testing trustedclients.Set.Seal proves the rule; it does not prove the rule
// is applied where a real request goes.
//
// So this drives the PUBLIC `/call/Baml_Rest_Dynamic` request BODY through the same
// pipeline the served route runs it through — the exact decode
// cmd/serve's handler performs (sonic into bamlutils.DynamicInput), the same
// DynamicInput.Validate, the same ToWorkerInput, and the real Handler.CallStream —
// and then reads the registry the ADAPTER was handed, which is the same pointer the
// native serve seam later resolves identity from.
//
// The two arms are the ones an enrollment depends on: a request that merely NAMES an
// approved class comes out SEALED with the deployment's own configuration installed,
// and a request that DEFINES the same configuration itself comes out UNSEALED and
// completely untouched.

import (
	"context"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
)

const (
	routeSealFingerprint = "cfg001"
	routeSealClient      = "ApprovedRouteClient"
	routeSealModel       = "gpt-4o-mini"
	routeSealBaseURL     = "https://approved.invalid/v1"
	routeSealAPIKey      = "sk-route-seal"
)

// routeSealDeclaration is the deployment's approved-configuration declaration.
func routeSealDeclaration(t *testing.T) *trustedclients.Set {
	t.Helper()
	set, err := trustedclients.Parse(`{"trusted_clients":[{
		"name":"` + routeSealClient + `","fingerprint":"` + routeSealFingerprint + `","provider":"openai",
		"options":{"model":"` + routeSealModel + `","base_url":"` + routeSealBaseURL + `","api_key":"` + routeSealAPIKey + `"}
	}]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	return set
}

// publicCallBody builds the PUBLIC `/call` request body. namedOnly picks the shape:
// a request that merely NAMES the approved class, or one that DEFINES the very same
// configuration itself.
func publicCallBody(t *testing.T, namedOnly bool) []byte {
	t.Helper()
	primary := routeSealClient
	text := "hello"
	client := &bamlutils.ClientProperty{Name: routeSealClient}
	if !namedOnly {
		client = &bamlutils.ClientProperty{
			Name:     routeSealClient,
			Provider: "openai",
			Options: map[string]any{
				"model":    routeSealModel,
				"base_url": routeSealBaseURL,
				"api_key":  routeSealAPIKey,
			},
		}
	}
	body, err := sonic.Marshal(bamlutils.DynamicInput{
		Messages:       []bamlutils.DynamicMessage{{Role: "user", TextContent: &text}},
		ClientRegistry: &bamlutils.ClientRegistry{Primary: &primary, Clients: []*bamlutils.ClientProperty{client}},
		OutputSchema: &bamlutils.DynamicOutputSchema{
			Properties: bamlutils.MustOrderedMap(
				bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			),
		},
	})
	if err != nil {
		t.Fatalf("marshal the public /call body: %v", err)
	}
	return body
}

// routeSealedRegistry runs one public `/call` body through the REAL request path and
// returns the registry the adapter was handed.
//
// The decode/validate/convert steps below are literally what cmd/serve's dynamic call
// handler does before it hands bytes to the pool, so this is the public route's own
// pipeline rather than a reconstruction of it.
func routeSealedRegistry(t *testing.T, declared *trustedclients.Set, namedOnly bool) *bamlutils.ClientRegistry {
	t.Helper()

	var captured *fakeAdapter
	rt := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{
		bamlutils.DynamicMethodName: {
			MakeInput: func() any { return &map[string]any{} },
			Impl: func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
				captured = adapter.(*fakeAdapter)
				ch := make(chan bamlutils.StreamResult)
				close(ch)
				return ch, nil
			},
		},
	}}
	h := newTestHandler(t, Config{Runtime: rt, TrustedClients: declared})

	// The public HTTP body, through the public decoder.
	var input bamlutils.DynamicInput
	if err := sonic.Unmarshal(publicCallBody(t, namedOnly), &input); err != nil {
		t.Fatalf("decode the public /call body: %v", err)
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("DynamicInput.Validate: %v", err)
	}
	workerInput, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("DynamicInput.ToWorkerInput: %v", err)
	}

	out, err := h.CallStream(context.Background(), bamlutils.DynamicMethodName, workerInput, bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	for range out {
	}
	if captured == nil {
		t.Fatal("the method was never invoked; the request did not reach the adapter")
	}
	reg := captured.OriginalClientRegistry()
	if reg == nil || len(reg.Clients) != 1 {
		t.Fatalf("the adapter received %v, want one client", reg)
	}
	return reg
}

// TestPublicCallBodyNamingAnApprovedClassIsSealed is the positive half: a request
// that NAMES an approved class arrives at the adapter carrying the deployment's own
// provider and options, and SEALED — which is the only state from which the native
// admission seam will resolve an identity.
func TestPublicCallBodyNamingAnApprovedClassIsSealed(t *testing.T) {
	reg := routeSealedRegistry(t, routeSealDeclaration(t), true)
	cp := reg.Clients[0]

	fingerprint, digest, sealed := cp.TrustedConfigSeal()
	if !sealed {
		t.Fatal("a request naming an approved class reached the adapter UNSEALED; no request could ever obtain an identity")
	}
	if fingerprint != routeSealFingerprint {
		t.Errorf("seal carries fingerprint %q, want %q", fingerprint, routeSealFingerprint)
	}
	want, err := bamlutils.TrustedConfigDigest(cp, "openai")
	if err != nil {
		t.Fatalf("TrustedConfigDigest: %v", err)
	}
	if digest != want {
		t.Errorf("the seal's digest does not describe the client the adapter received")
	}

	// The DEPLOYMENT's configuration was installed — the request carried none of it.
	if cp.Provider != "openai" {
		t.Errorf("provider = %q, want the declared openai", cp.Provider)
	}
	for k, want := range map[string]string{"model": routeSealModel, "base_url": routeSealBaseURL, "api_key": routeSealAPIKey} {
		if got, _ := cp.Options[k].(string); got != want {
			t.Errorf("option %q = %q, want the declared %q", k, got, want)
		}
	}
}

// TestPublicCallBodyDefiningTheConfigurationIsNeverSealed is the P1 half at the
// public boundary: the SAME deployment, the SAME approved class, the SAME values —
// but the request supplied them, so the client arrives UNSEALED and UNTOUCHED, and
// can never obtain an identity.
func TestPublicCallBodyDefiningTheConfigurationIsNeverSealed(t *testing.T) {
	reg := routeSealedRegistry(t, routeSealDeclaration(t), false)
	cp := reg.Clients[0]

	if _, _, sealed := cp.TrustedConfigSeal(); sealed {
		t.Error("a caller-defined configuration reached the adapter SEALED; matching bytes are not provenance, and an enrolled cohort would be claimable by unapproved traffic")
	}
	// Untouched: the sealing pass must not rewrite a request it refused to seal.
	if got, _ := cp.Options["base_url"].(string); got != routeSealBaseURL {
		t.Errorf("base_url = %q, want the caller's %q — a refused seal must change nothing", got, routeSealBaseURL)
	}
	if len(cp.Options) != 3 {
		t.Errorf("the refused client carries %d options, want the caller's 3", len(cp.Options))
	}
}

// TestPublicCallBodyIsUnchangedWithoutADeclaration pins the shipped default at the
// public boundary: with nothing declared the sealing pass is inert — the adapter
// receives exactly what the caller sent, and nothing is sealed.
func TestPublicCallBodyIsUnchangedWithoutADeclaration(t *testing.T) {
	empty, err := trustedclients.Parse("")
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	for _, namedOnly := range []bool{true, false} {
		reg := routeSealedRegistry(t, empty, namedOnly)
		cp := reg.Clients[0]
		if _, _, sealed := cp.TrustedConfigSeal(); sealed {
			t.Errorf("namedOnly=%v: an undeclared deployment sealed a client", namedOnly)
		}
		wantOptions := 3
		if namedOnly {
			wantOptions = 0
		}
		if len(cp.Options) != wantOptions {
			t.Errorf("namedOnly=%v: the adapter received %d options, want the caller's %d — an undeclared deployment must change nothing",
				namedOnly, len(cp.Options), wantOptions)
		}
	}
}

// TestSealCannotArriveOnTheWire is the structural guard at the request boundary: the
// seal has no wire representation, so a request that spells the field names cannot
// smuggle one past the decoder into the adapter's registry.
func TestSealCannotArriveOnTheWire(t *testing.T) {
	var captured *fakeAdapter
	rt := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{
		"x": {
			MakeInput: func() any { return &map[string]any{} },
			Impl: func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
				captured = adapter.(*fakeAdapter)
				ch := make(chan bamlutils.StreamResult)
				close(ch)
				return ch, nil
			},
		},
	}}
	// No declaration at all, and a body that tries every spelling of the seal.
	h := newTestHandler(t, Config{Runtime: rt})
	body := []byte(`{"__baml_options__":{"client_registry":{"primary":"C","clients":[{"name":"C","provider":"openai",` +
		`"seal":{"fingerprint":"cfg001","digest":"x"},"fingerprint":"cfg001","trustedConfig":"cfg001",` +
		`"options":{"model":"m","base_url":"https://x.invalid/v1","api_key":"k"}}]}}}`)
	out, err := h.CallStream(context.Background(), "x", body, bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	for range out {
	}
	if captured == nil {
		t.Fatal("the method was never invoked")
	}
	if _, _, sealed := captured.OriginalClientRegistry().Clients[0].TrustedConfigSeal(); sealed {
		t.Error("a request forged a trusted-configuration seal through the wire")
	}
}
