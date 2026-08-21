package admission

// De-BAML serving cutover S3a — the IDENTITY BITING MATRIX.
//
// The property under test is not "the resolver returns a fingerprint". It is that
// the fingerprint follows the DEPLOYMENT-SEALED configuration and nothing else:
// not the process, not the caller, and not a configuration the caller described
// perfectly. So the matrix runs every arm TWICE — once with the deployment's seal
// present and once with the identical registry supplied by the caller — and the
// caller-supplied side must resolve NOTHING even on the arm that otherwise
// succeeds.
//
// The three mutation bites at the end drive the SAME matrix through the three
// implementations this slice exists to rule out: a missing seal, a worker-wide
// identity, and a resolver that trusts caller-supplied configuration.

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
)

// approvedFingerprint is the opaque slot the deployment assigned to its approved
// configuration class. It is a declared-but-unassigned production slot; the
// assignment lives in the deployment's declaration, never in shipped source.
const approvedFingerprint ConfigFingerprint = "cfg001"

const approvedClientName = "ApprovedClient"

func identityStr(s string) *string { return &s }

// approvedDeclaration is the deployment's approved-configuration declaration: the
// real provider and option values it owns for the class above.
func approvedDeclaration(t *testing.T) *trustedclients.Set {
	t.Helper()
	set, err := trustedclients.Parse(`{"trusted_clients":[{
		"name":"` + approvedClientName + `",
		"fingerprint":"` + string(approvedFingerprint) + `",
		"provider":"openai",
		"options":{"model":"gpt-4o-mini","base_url":"https://approved.example/v1","api_key":"sk-approved-value"}
	}]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	if set.Len() != 1 {
		t.Fatalf("declaration holds %d classes, want 1", set.Len())
	}
	return set
}

// namedOnlyRegistry is what a request that merely NAMES the approved class looks
// like on the wire: a primary and a client name, and nothing else. This is the only
// shape the deployment will seal.
func namedOnlyRegistry() *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: identityStr(approvedClientName),
		Clients: []*bamlutils.ClientProperty{{Name: approvedClientName}},
	}
}

// callerSuppliedRegistry is the ATTACK SHAPE: a request that describes the approved
// configuration itself, byte for byte, instead of naming it. Everything about it
// matches what the deployment would have installed.
func callerSuppliedRegistry() *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: identityStr(approvedClientName),
		Clients: []*bamlutils.ClientProperty{{
			Name:     approvedClientName,
			Provider: "openai",
			Options: map[string]any{
				"model":    "gpt-4o-mini",
				"base_url": "https://approved.example/v1",
				"api_key":  "sk-approved-value",
			},
		}},
	}
}

// sealedRegistry is the request above after the worker's config-load pass ran: the
// deployment installed its own provider and options and sealed the client.
func sealedRegistry(t *testing.T) *bamlutils.ClientRegistry {
	t.Helper()
	reg := namedOnlyRegistry()
	approvedDeclaration(t).Seal(reg)
	if _, _, sealed := reg.Clients[0].TrustedConfigSeal(); !sealed {
		t.Fatal("the declaration did not seal a request that merely named the approved class")
	}
	return reg
}

// selectionOver is the full set of per-request facts for an otherwise-approved
// request over reg.
func selectionOver(reg *bamlutils.ClientRegistry) ConfigSelection {
	return ConfigSelection{
		Registry:          reg,
		ResolvedProvider:  "openai",
		SelectedLeaf:      approvedClientName,
		SingleLeaf:        true,
		HasBAMLPlanOracle: true,
	}
}

// identityCase is one arm of the matrix: a mutation of the approved selection and
// whether the approved identity may survive it when the deployment DID seal.
type identityCase struct {
	name     string
	mutate   func(*ConfigSelection)
	identity bool
}

// identityMatrix is the whole biting matrix. Exactly ONE arm may resolve, and only
// on the sealed side.
func identityMatrix() []identityCase {
	return []identityCase{
		{name: "the approved configuration", mutate: func(*ConfigSelection) {}, identity: true},

		// --- alternate / aliased / overridden configurations -----------------------
		{name: "an alternate OpenAI configuration (different base_url)", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].Options["base_url"] = "https://alternate.example/v1"
		}},
		{name: "an alternate OpenAI configuration (different model)", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].Options["model"] = "gpt-4o"
		}},
		{name: "an aliased registry entry (same options, different client name)", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].Name = approvedClientName + "Alias"
			s.Registry.Primary = identityStr(approvedClientName + "Alias")
			s.SelectedLeaf = approvedClientName + "Alias"
		}},
		{name: "an alias sitting ALONGSIDE the approved client, selected by primary", mutate: func(s *ConfigSelection) {
			alias := *s.Registry.Clients[0]
			alias.Name = approvedClientName + "Alias"
			alias.ClearTrustedConfigSeal()
			s.Registry.Clients = append(s.Registry.Clients, &alias)
			s.Registry.Primary = identityStr(alias.Name)
			s.SelectedLeaf = alias.Name
		}},
		{name: "a client override steering to an unsealed leaf", mutate: func(s *ConfigSelection) {
			other := &bamlutils.ClientProperty{
				Name:     "OtherClient",
				Provider: "openai",
				Options: map[string]any{
					"model":    "gpt-4o-mini",
					"base_url": "https://approved.example/v1",
					"api_key":  "sk-approved-value",
				},
			}
			s.Registry.Clients = append(s.Registry.Clients, other)
			s.Registry.Primary = identityStr("OtherClient")
			s.SelectedLeaf = "OtherClient"
		}},
		{name: "a changed selected leaf (BAML named a leaf the registry does not resolve)", mutate: func(s *ConfigSelection) {
			s.SelectedLeaf = "SomeOtherLeaf"
		}},
		{name: "no named selected leaf at all", mutate: func(s *ConfigSelection) { s.SelectedLeaf = "" }},

		// --- orchestration-shape ambiguity ----------------------------------------
		{name: "more than one resolved leaf", mutate: func(s *ConfigSelection) { s.SingleLeaf = false }},
		{name: "a fallback chain", mutate: func(s *ConfigSelection) { s.HasFallbackChain = true }},
		{name: "round robin", mutate: func(s *ConfigSelection) { s.HasRoundRobin = true }},
		{name: "a request retry override", mutate: func(s *ConfigSelection) { s.HasRequestRetryOverride = true }},
		{name: "a client retry policy on the selected client", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].RetryPolicy = identityStr("Exponential")
		}},
		{name: "the legacy native-first probe route (no BAML plan oracle)", mutate: func(s *ConfigSelection) {
			s.HasBAMLPlanOracle = false
		}},

		// --- registry ambiguity ----------------------------------------------------
		{name: "no registry at all", mutate: func(s *ConfigSelection) { s.Registry = nil }},
		{name: "two clients and no primary", mutate: func(s *ConfigSelection) {
			second := *s.Registry.Clients[0]
			second.Name = "SecondClient"
			s.Registry.Clients = append(s.Registry.Clients, &second)
			s.Registry.Primary = nil
		}},
		{name: "a primary naming no client in the registry", mutate: func(s *ConfigSelection) {
			s.Registry.Primary = identityStr("MissingClient")
		}},
		{name: "a duplicate client name (an ambiguous registry)", mutate: func(s *ConfigSelection) {
			dup := *s.Registry.Clients[0]
			s.Registry.Clients = append(s.Registry.Clients, &dup)
		}},

		// --- provider ambiguity ----------------------------------------------------
		{name: "an absent resolved leaf provider", mutate: func(s *ConfigSelection) { s.ResolvedProvider = "" }},
		{name: "a resolved provider disagreeing with the selected client", mutate: func(s *ConfigSelection) {
			s.ResolvedProvider = "anthropic"
		}},
		{name: "a provider outside the declared classes", mutate: func(s *ConfigSelection) {
			s.ResolvedProvider = "acme-llm"
			s.Registry.Clients[0].Provider = "acme-llm"
		}},

		// --- post-seal mutation ------------------------------------------------------
		{name: "an option mutated after the seal was applied", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].Options["base_url"] = "https://elsewhere.example/v1"
		}},
		{name: "an option ADDED after the seal was applied", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].Options["temperature"] = "0.2"
		}},
		{name: "an option removed after the seal was applied", mutate: func(s *ConfigSelection) {
			delete(s.Registry.Clients[0].Options, "base_url")
		}},
		{name: "a non-literal option value", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].Options["headers"] = map[string]any{"x": "y"}
		}},
		{name: "the seal removed", mutate: func(s *ConfigSelection) {
			s.Registry.Clients[0].ClearTrustedConfigSeal()
		}},
	}
}

// identityResolveFn is the resolver under test, so the matrix can be driven through
// the real implementation and through the mutants this slice rules out.
type identityResolveFn func(ConfigSelection) ConfigIdentity

// matrixTB is the minimal reporter the matrix runner needs, so a mutation bite can
// observe whether the matrix would have failed instead of failing the real test.
type matrixTB interface {
	Errorf(string, ...any)
}

type recordingMatrixTB struct{ failed bool }

func (r *recordingMatrixTB) Errorf(string, ...any) { r.failed = true }

// runIdentityMatrix is THE predicate. It drives every arm over BOTH provenances —
// the deployment's seal and the caller's own bytes — so the two properties this
// slice must hold are checked by one function: the sealed configuration resolves,
// and nothing else does, including its perfect caller-supplied twin.
func runIdentityMatrix(tb matrixTB, t *testing.T, resolve identityResolveFn) {
	for _, c := range identityMatrix() {
		for _, provenance := range []struct {
			name   string
			build  func() *bamlutils.ClientRegistry
			sealed bool
		}{
			{name: "sealed by the deployment", build: func() *bamlutils.ClientRegistry { return sealedRegistry(t) }, sealed: true},
			{name: "supplied by the caller", build: callerSuppliedRegistry},
		} {
			sel := selectionOver(provenance.build())
			c.mutate(&sel)
			got := resolve(sel)
			want := ConfigIdentity{}
			if c.identity && provenance.sealed {
				want = ConfigIdentity{Fingerprint: approvedFingerprint, Provider: ConfigProviderOpenAI}
			}
			if got != want {
				tb.Errorf("%s / %s: resolved %+v, want %+v", c.name, provenance.name, got, want)
			}
		}
	}
}

// TestConfigIdentityBitingMatrix is the headline proof.
func TestConfigIdentityBitingMatrix(t *testing.T) {
	runIdentityMatrix(t, t, ResolveConfigIdentity)
}

// TestCallerSuppliedConfigurationIsNeverAnIdentity states the P1 property on its
// own, in the plainest possible terms, because it is the one an enrollment depends
// on: a request that describes the approved configuration perfectly — same client
// name, same provider, same model, same base_url, same credential, chosen as
// primary — gets NO identity, while the same request naming the class and letting
// the deployment configure it does.
func TestCallerSuppliedConfigurationIsNeverAnIdentity(t *testing.T) {
	sealed := ResolveConfigIdentity(selectionOver(sealedRegistry(t)))
	if sealed.Fingerprint != approvedFingerprint {
		t.Fatalf("the deployment-sealed configuration resolved %+v; the comparison below would be vacuous", sealed)
	}
	supplied := ResolveConfigIdentity(selectionOver(callerSuppliedRegistry()))
	if supplied != (ConfigIdentity{}) {
		t.Errorf("a caller-supplied configuration resolved %+v; identity must come from the deployment, not from matching bytes", supplied)
	}
}

// TestSealingRefusesAnyCallerContribution pins the sealing rule at its own layer:
// a request may NAME an approved class, and nothing more. Every arm here supplies
// one extra thing, and each must leave the client unsealed AND untouched.
func TestSealingRefusesAnyCallerContribution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *bamlutils.ClientProperty
	}{
		{"an option", &bamlutils.ClientProperty{Name: approvedClientName, Options: map[string]any{"model": "gpt-4o-mini"}}},
		{"the approved options verbatim", callerSuppliedRegistry().Clients[0]},
		{"a provider", &bamlutils.ClientProperty{Name: approvedClientName, Provider: "openai"}},
		{"a present-empty provider", &bamlutils.ClientProperty{Name: approvedClientName, Provider: "", ProviderSet: true}},
		{"a retry policy", &bamlutils.ClientProperty{Name: approvedClientName, RetryPolicy: identityStr("Exponential")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tc.client.Options)
			reg := &bamlutils.ClientRegistry{
				Primary: identityStr(approvedClientName),
				Clients: []*bamlutils.ClientProperty{tc.client},
			}
			approvedDeclaration(t).Seal(reg)
			if _, _, sealed := tc.client.TrustedConfigSeal(); sealed {
				t.Errorf("a request supplying %s was sealed; naming is allowed, defining is not", tc.name)
			}
			if len(tc.client.Options) != before {
				t.Errorf("the sealing pass mutated a client it refused to seal (%d options, was %d)", len(tc.client.Options), before)
			}
			if got := ResolveConfigIdentity(selectionOver(reg)); got != (ConfigIdentity{}) {
				t.Errorf("resolved %+v for a request supplying %s", got, tc.name)
			}
		})
	}
}

// TestSealingIsANoOpForAnUndeclaredDeployment pins the shipped default: with
// nothing declared, the sealing pass changes no request at all and nothing resolves.
func TestSealingIsANoOpForAnUndeclaredDeployment(t *testing.T) {
	empty, err := trustedclients.Parse("")
	if err != nil {
		t.Fatalf("trustedclients.Parse(empty): %v", err)
	}
	reg := namedOnlyRegistry()
	empty.Seal(reg)
	if len(reg.Clients[0].Options) != 0 || reg.Clients[0].Provider != "" {
		t.Errorf("an empty declaration mutated the request: %+v", reg.Clients[0])
	}
	if _, _, sealed := reg.Clients[0].TrustedConfigSeal(); sealed {
		t.Error("an empty declaration sealed a client")
	}
	if got := ResolveConfigIdentity(selectionOver(reg)); got != (ConfigIdentity{}) {
		t.Errorf("an undeclared deployment resolved %+v", got)
	}
}

// TestMissingSealBitesTheMatrix is the first required mutation: with the deployment
// declaring nothing, NOTHING may resolve — including the approved class.
func TestMissingSealBitesTheMatrix(t *testing.T) {
	rec := &recordingMatrixTB{}
	runIdentityMatrix(rec, t, func(sel ConfigSelection) ConfigIdentity {
		if sel.Registry != nil {
			for _, cp := range sel.Registry.Clients {
				cp.ClearTrustedConfigSeal()
			}
		}
		return ResolveConfigIdentity(sel)
	})
	if !rec.failed {
		t.Error("the identity matrix still passes with every seal removed; it does not prove the seal is load-bearing")
	}
}

// TestWorkerWideIdentityBitesTheMatrix is the second required mutation, and the one
// the original wiring gap was: an implementation that stamps the fingerprint on
// every request the worker hosts must make the matrix fail.
func TestWorkerWideIdentityBitesTheMatrix(t *testing.T) {
	workerWide := func(ConfigSelection) ConfigIdentity {
		return ConfigIdentity{Fingerprint: approvedFingerprint, Provider: ConfigProviderOpenAI}
	}
	rec := &recordingMatrixTB{}
	runIdentityMatrix(rec, t, workerWide)
	if !rec.failed {
		t.Error("the identity matrix still passes for a worker-wide identity; it does not prove identity follows the effective selected configuration")
	}
}

// TestTrustingCallerSuppliedConfigurationBitesTheMatrix is the third required
// mutation, and the specific defect this revision fixes: a resolver that accepts a
// configuration because its BYTES match the approved one, rather than because the
// deployment configured it, must make the matrix fail.
func TestTrustingCallerSuppliedConfigurationBitesTheMatrix(t *testing.T) {
	sealedDigest, err := bamlutils.TrustedConfigDigest(sealedRegistry(t).Clients[0], "openai")
	if err != nil {
		t.Fatalf("TrustedConfigDigest: %v", err)
	}
	// The mutant: identical to the real predicate except that it re-seals whatever
	// it is given when the bytes match, i.e. it treats value-equality as provenance.
	byteMatching := func(sel ConfigSelection) ConfigIdentity {
		if sel.Registry != nil {
			for _, cp := range sel.Registry.Clients {
				if d, derr := bamlutils.TrustedConfigDigest(cp, sel.ResolvedProvider); derr == nil && d == sealedDigest {
					cp.SealTrustedConfig(string(approvedFingerprint), d)
				}
			}
		}
		return ResolveConfigIdentity(sel)
	}
	rec := &recordingMatrixTB{}
	runIdentityMatrix(rec, t, byteMatching)
	if !rec.failed {
		t.Error("the identity matrix still passes for a resolver that trusts matching caller-supplied bytes; it does not prove the provenance boundary")
	}
}

// TestSealIsWireUnreachable is the structural half of the provenance boundary: a
// seal has no wire representation, so a request cannot forge one and a marshalled
// registry cannot carry one out.
func TestSealIsWireUnreachable(t *testing.T) {
	reg := sealedRegistry(t)
	encoded, err := reg.Clients[0].MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	for _, leak := range []string{"seal", "fingerprint", string(approvedFingerprint)} {
		if strings.Contains(string(encoded), leak) {
			t.Errorf("a marshalled sealed client carries %q: %s", leak, encoded)
		}
	}
	// Every hostile shape a request could try, including the exact field names.
	for _, body := range []string{
		`{"name":"` + approvedClientName + `","seal":{"fingerprint":"cfg001","digest":"x"}}`,
		`{"name":"` + approvedClientName + `","fingerprint":"cfg001"}`,
		`{"name":"` + approvedClientName + `","trustedConfig":"cfg001"}`,
		string(encoded),
	} {
		var cp bamlutils.ClientProperty
		// An unknown key is either ignored or rejected; either way a seal must not
		// appear. Decode errors are fine — they are the stronger outcome.
		if err := cp.UnmarshalJSON([]byte(body)); err != nil {
			continue
		}
		if _, _, sealed := cp.TrustedConfigSeal(); sealed {
			t.Errorf("decoding %s produced a sealed client", body)
		}
	}
}

// TestTrustedClientsDeclarationRejectsMalformedInput pins the strict decoder. Every
// case here would, if repaired instead of rejected, mean the running declaration is
// not the reviewed one.
func TestTrustedClientsDeclarationRejectsMalformedInput(t *testing.T) {
	const ok = `{"name":"A","fingerprint":"cfg001","provider":"openai","options":{"model":"m"}}`
	for _, tc := range []struct{ name, spec string }{
		{"not JSON", `{`},
		{"unknown envelope key", `{"clients":[]}`},
		{"unknown client key", `{"trusted_clients":[{"name":"A","fingerprint":"cfg001","provider":"openai","options":{"model":"m"},"extra":1}]}`},
		{"trailing content", `{"trusted_clients":[]} {}`},
		{"empty name", `{"trusted_clients":[{"name":"","fingerprint":"cfg001","provider":"openai","options":{"model":"m"}}]}`},
		{"whitespace-padded name", `{"trusted_clients":[{"name":" A ","fingerprint":"cfg001","provider":"openai","options":{"model":"m"}}]}`},
		{"non-opaque fingerprint", `{"trusted_clients":[{"name":"A","fingerprint":"gpt-4o-acme","provider":"openai","options":{"model":"m"}}]}`},
		{"absent fingerprint", `{"trusted_clients":[{"name":"A","provider":"openai","options":{"model":"m"}}]}`},
		{"empty provider", `{"trusted_clients":[{"name":"A","fingerprint":"cfg001","provider":"","options":{"model":"m"}}]}`},
		{"no options", `{"trusted_clients":[{"name":"A","fingerprint":"cfg001","provider":"openai","options":{}}]}`},
		{"empty option value", `{"trusted_clients":[{"name":"A","fingerprint":"cfg001","provider":"openai","options":{"model":""}}]}`},
		{"one name declared twice", `{"trusted_clients":[` + ok + `,{"name":"A","fingerprint":"cfg002","provider":"openai","options":{"model":"m"}}]}`},
		{"one fingerprint on two clients", `{"trusted_clients":[` + ok + `,{"name":"B","fingerprint":"cfg001","provider":"openai","options":{"model":"m"}}]}`},
		// The shapes a More()-based end check ACCEPTED: Decoder.More reports whether
		// another ELEMENT follows and answers false on a closing delimiter, so a
		// declaration with a stray `}` or `]` decoded as a valid prefix. A declaration
		// the operator did not finish writing must fail closed.
		{"a stray closing brace", `{"trusted_clients":[` + ok + `]}}`},
		{"a stray closing bracket", `{"trusted_clients":[` + ok + `]}]`},
		{"a second value after the declaration", `{"trusted_clients":[` + ok + `]} {"trusted_clients":[]}`},
		{"a bare token after the declaration", `{"trusted_clients":[` + ok + `]} 7`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := trustedclients.Parse(tc.spec); err == nil {
				t.Errorf("declaration %q decoded; a malformed or ambiguous declaration must fail loudly", tc.spec)
			}
		})
	}
	// Split, so a future Parse error is REPORTED rather than reached through on a
	// value the failing call did not promise to return.
	empty, err := trustedclients.Parse("")
	if err != nil {
		t.Fatalf("the empty declaration must decode without error: %v", err)
	}
	if empty.Len() != 0 {
		t.Errorf("the empty declaration yielded %d classes, want 0", empty.Len())
	}
}

// TestAnUndeclaredFingerprintIsRefused pins the vocabulary boundary: a deployment
// chooses WHICH declared bucket it assigned, it does not get to invent one.
func TestAnUndeclaredFingerprintIsRefused(t *testing.T) {
	set, err := trustedclients.Parse(`{"trusted_clients":[{
		"name":"` + approvedClientName + `","fingerprint":"cfg999999","provider":"openai",
		"options":{"model":"m","base_url":"https://x.example/v1","api_key":"k"}}]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	reg := namedOnlyRegistry()
	set.Seal(reg)
	if _, _, sealed := reg.Clients[0].TrustedConfigSeal(); !sealed {
		t.Fatal("the declaration did not seal; the refusal below would be vacuous")
	}
	if got := ResolveConfigIdentity(selectionOver(reg)); got != (ConfigIdentity{}) {
		t.Errorf("a fingerprint outside this build's declared vocabulary resolved %+v", got)
	}
}

// TestSealedIdentityStillDeclinesUnderTheEmptyPolicy is the S3a serving guarantee in
// the gate's own terms: a request that DOES resolve a sealed identity still
// declines, because sealing is not enrolling and the shipped policy enrolls nothing.
func TestSealedIdentityStillDeclinesUnderTheEmptyPolicy(t *testing.T) {
	id := ResolveConfigIdentity(selectionOver(sealedRegistry(t)))
	if id.Fingerprint != approvedFingerprint {
		t.Fatalf("the sealed configuration resolved %+v; the rest of this test would be vacuous", id)
	}
	in := CohortInput{Fingerprint: id.Fingerprint, Provider: id.Provider}
	for _, s := range AllSurfaces() {
		cohort, d := admitCohort(s, in)
		if d == nil {
			t.Fatalf("%s: a sealed identity was ADMITTED against the shipped policy", s.Label())
		}
		if d.Stage != StageCohort || d.Reason != ReasonCohortNotEnrolled {
			t.Errorf("%s: declined with (%s,%s), want the bounded cohort refusal", s.Label(), d.Stage, d.Reason)
		}
		if cohort != CohortUnrecognized {
			t.Errorf("%s: a sealed-but-uninventoried identity resolved cohort %q, want %q", s.Label(), cohort, CohortUnrecognized)
		}
	}
}

// TestIdentityIsBoundToItsInventoryRecord pins the record binding: an identity is
// its record's cohort only for the provider CLASS and the SURFACES that record
// declares. The opaque bucket alone is not enough, which is what stops an approved
// class being claimed on a surface it was never approved for.
func TestIdentityIsBoundToItsInventoryRecord(t *testing.T) {
	g, err := buildCohortGate("s3a-identity-declared-not-enrolled", []ConfigRecord{{
		Fingerprint: approvedFingerprint,
		Cohort:      "fe_v1_candidate",
		Surfaces:    []Surface{SurfaceDynamicCall},
		Provider:    ConfigProviderOpenAI,
		Approval:    "DEBAML-673",
	}}, nil)
	if err != nil {
		t.Fatalf("buildCohortGate: %v", err)
	}
	matching := CohortInput{Fingerprint: approvedFingerprint, Provider: ConfigProviderOpenAI, gate: g}
	if got := g.Resolve(SurfaceDynamicCall, matching); got != "fe_v1_candidate" {
		t.Fatalf("the record's own surface resolved %q; the mutations below would be vacuous", got)
	}
	for _, tc := range []struct {
		name    string
		surface Surface
		in      CohortInput
	}{
		{"a surface the record does not declare", SurfaceDynamicStream, matching},
		{"a static surface the record does not declare", SurfaceStaticCall, matching},
		{"a provider class the record does not declare", SurfaceDynamicCall,
			CohortInput{Fingerprint: approvedFingerprint, Provider: ConfigProviderAnthropic, gate: g}},
		{"no provider class at all", SurfaceDynamicCall,
			CohortInput{Fingerprint: approvedFingerprint, gate: g}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Resolve(tc.surface, tc.in); got != CohortUnrecognized {
				t.Errorf("resolved %q, want %q — an identity is only its cohort where its record says so", got, CohortUnrecognized)
			}
		})
	}
	// And even on its own surface, with its own class, it is NOT enrolled.
	if g.Policy().Enrolled(SurfaceDynamicCall, "fe_v1_candidate") {
		t.Error("an inventory record enrolled its cohort; declaring must never enroll")
	}
}

// TestNoIdentityDetailEverCarriesConfiguration pins the privacy contract at the one
// boundary a resolved identity can reach: the cohort decline.
func TestNoIdentityDetailEverCarriesConfiguration(t *testing.T) {
	reg := sealedRegistry(t)
	digest, err := bamlutils.TrustedConfigDigest(reg.Clients[0], "openai")
	if err != nil {
		t.Fatalf("TrustedConfigDigest: %v", err)
	}
	id := ResolveConfigIdentity(selectionOver(reg))
	_, d := admitCohort(SurfaceDynamicCall, CohortInput{Fingerprint: id.Fingerprint, Provider: id.Provider})
	if d == nil {
		t.Fatal("the shipped policy admitted a sealed identity")
	}
	for _, secret := range []string{
		digest, approvedClientName, "gpt-4o-mini", "https://approved.example/v1", "sk-approved-value",
	} {
		if strings.Contains(d.Detail, secret) {
			t.Errorf("the cohort decline detail carries %q", secret)
		}
	}
	if !strings.Contains(d.Detail, "not in the inventory") {
		t.Errorf("the decline detail lost its structural shape: %q", d.Detail)
	}
}

// TestTrustedConfigDigestIgnoresTheCredentialValue pins the one deliberate exclusion
// from the canonical encoding. A rotated credential is the SAME configuration class,
// so a deployment's own seal survives a key rotation; dropping the option entirely
// is a DIFFERENT class.
func TestTrustedConfigDigestIgnoresTheCredentialValue(t *testing.T) {
	base, err := bamlutils.TrustedConfigDigest(callerSuppliedRegistry().Clients[0], "openai")
	if err != nil {
		t.Fatalf("TrustedConfigDigest: %v", err)
	}
	rotated := callerSuppliedRegistry().Clients[0]
	rotated.Options["api_key"] = "sk-rotated-value"
	got, err := bamlutils.TrustedConfigDigest(rotated, "openai")
	if err != nil {
		t.Fatalf("TrustedConfigDigest(rotated): %v", err)
	}
	if got != base {
		t.Error("rotating the api_key changed the digest; a deployment's own seal would break on every key rotation")
	}
	dropped := callerSuppliedRegistry().Clients[0]
	delete(dropped.Options, "api_key")
	if got, err := bamlutils.TrustedConfigDigest(dropped, "openai"); err == nil && got == base {
		t.Error("dropping the api_key option left the digest unchanged; option PRESENCE must be part of the configuration class")
	}
}

// TestTrustedConfigDigestSeparatesFieldsThatShareBytes proves the length-prefixed
// encoding: moving bytes across a field boundary must not produce the same digest.
func TestTrustedConfigDigestSeparatesFieldsThatShareBytes(t *testing.T) {
	build := func(name, model string) string {
		cp := &bamlutils.ClientProperty{
			Name:     name,
			Provider: "openai",
			Options:  map[string]any{"model": model, "base_url": "https://x.example/v1", "api_key": "k"},
		}
		d, err := bamlutils.TrustedConfigDigest(cp, "openai")
		if err != nil {
			t.Fatalf("TrustedConfigDigest(%q,%q): %v", name, model, err)
		}
		return d
	}
	if build("ab", "c") == build("a", "bc") {
		t.Error("two configurations that differ only in where a field boundary falls digest identically; the canonical encoding is not injection-resistant")
	}
}

// TestTrustedConfigDigestIsStableAcrossMapIterationOrder pins determinism: Go map
// iteration order is randomized, so an encoding that walked Options directly would
// produce a different digest per call and a seal would match only sometimes.
func TestTrustedConfigDigestIsStableAcrossMapIterationOrder(t *testing.T) {
	want, err := bamlutils.TrustedConfigDigest(callerSuppliedRegistry().Clients[0], "openai")
	if err != nil {
		t.Fatalf("TrustedConfigDigest: %v", err)
	}
	for i := 0; i < 64; i++ {
		got, err := bamlutils.TrustedConfigDigest(callerSuppliedRegistry().Clients[0], "openai")
		if err != nil {
			t.Fatalf("TrustedConfigDigest: %v", err)
		}
		if got != want {
			t.Fatalf("the digest is not stable across calls: %q vs %q", got, want)
		}
	}
}
