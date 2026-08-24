package codegenspine

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

// manifestJSON is the frozen, machine-readable M0 contract manifest. It is the
// single source of truth for the enumerable freeze data (retained endpoints,
// error codes, descriptor versions, capability/decline taxonomy, and the
// grounding fixture set). The docs under docs/codegen-spine/ are the prose ADR;
// this file is what a test — and, in M1+, the build — can check.
//
//go:embed manifest.json
var manifestJSON []byte

// Manifest is the top-level frozen contract. Field names and enum strings are
// the contract; internal Go layout may change. Every value here is validated
// against the live tree by manifest_test.go, so this struct must stay a faithful
// mirror of manifest.json.
type Manifest struct {
	// ManifestVersion is monotonic. M0 freezes it at 1; a breaking reshape of
	// this manifest bumps it (never a Go-map-order or additive change).
	ManifestVersion int `json:"manifest_version"`
	// Note is a human breadcrumb; not load-bearing.
	Note string `json:"note"`

	DescriptorVersions  DescriptorVersions `json:"descriptor_versions"`
	DynamicEndpointName string             `json:"dynamic_endpoint_name"`
	DynamicMethodName   string             `json:"dynamic_method_name"`

	RetainedEndpoints []Endpoint      `json:"retained_endpoints"`
	ErrorTaxonomy     ErrorTaxonomy   `json:"error_taxonomy"`
	OptionsEnvelope   OptionsEnvelope `json:"options_envelope"`

	Capabilities []Capability `json:"capabilities"`
	Declines     Declines     `json:"declines"`

	Fixtures                   []Fixture `json:"fixtures"`
	RequiredCapabilityCoverage []string  `json:"required_capability_coverage"`

	// GuardedPaths are the pin/tar collision prefixes an M0/P slice must not
	// touch. The source-guard (guard_test.go) enforces byte-freeze; this list
	// is the human-readable copy carried alongside the machine baseline.
	GuardedPaths []string `json:"guarded_paths"`
}

// DescriptorVersions freezes the existing passive-descriptor versions the native
// lane composes, plus the planned initial ProjectDescriptor version (D1). The
// two existing values are grounded against schemadescriptor.Version /
// promptdescriptor.Version at test time.
type DescriptorVersions struct {
	Schemadescriptor         int `json:"schemadescriptor"`
	Promptdescriptor         int `json:"promptdescriptor"`
	ProjectDescriptorPlanned int `json:"project_descriptor_planned"`
}

// Endpoint is one retained public HTTP surface. StreamMode is the bamlutils
// StreamMode constant name for the four call/stream families, or "" for /parse
// (which is dispatched through ParseMethod, not a StreamMode). NeedsRaw /
// NeedsPartials mirror the bamlutils.StreamMode predicates and are grounded
// against them.
type Endpoint struct {
	Path            string   `json:"path"`
	StreamMode      string   `json:"stream_mode"`
	NeedsRaw        bool     `json:"needs_raw"`
	NeedsPartials   bool     `json:"needs_partials"`
	SuccessEnvelope string   `json:"success_envelope"`
	Servers         []string `json:"servers"`
	SourceRef       string   `json:"source_ref"`
}

// ErrorTaxonomy freezes the public error envelope and code set. Codes is the
// canonical declaration order and is grounded, order-sensitive, against
// internal/apierror.AllCodes(). WorkerFacingCodes is grounded against
// Code.IsWorkerFacing(). EnvelopeFields is grounded against the JSON tags of
// internal/apierror.Response.
type ErrorTaxonomy struct {
	EnvelopeType              string   `json:"envelope_type"`
	EnvelopeFields            []string `json:"envelope_fields"`
	Codes                     []string `json:"codes"`
	WorkerFacingCodes         []string `json:"worker_facing_codes"`
	ProviderErrorDetailFields []string `json:"provider_error_detail_fields"`
	SourceRef                 string   `json:"source_ref"`
}

// OptionsEnvelope freezes the per-call options object. WireKey is the JSON key
// (__baml_options__); Fields mirrors the JSON tags of bamlutils.BamlOptions and
// is grounded against it.
type OptionsEnvelope struct {
	WireKey   string        `json:"wire_key"`
	GoType    string        `json:"go_type"`
	Fields    []OptionField `json:"fields"`
	SourceRef string        `json:"source_ref"`
}

// OptionField pairs a JSON tag with its Go field name.
type OptionField struct {
	JSON string `json:"json"`
	Go   string `json:"go"`
}

// Capability is one stable native feature code. Kind buckets it; ProvenStatus
// records whether the native lane admits it today (proven), declines it
// (declined), or serves it only behind the transition oracle (transitional).
// A descriptor may exist for a method whose capability is not "proven" — native
// codegen must never infer support from mere presence (see Declines.Principle).
type Capability struct {
	Code         string `json:"code"`
	Kind         string `json:"kind"`
	ProvenStatus string `json:"proven_status"`
	SourceRef    string `json:"source_ref"`
}

// Declines points at the live decline taxonomy rather than duplicating it. The
// full Stage/Reason enums live in nativeserve/admission/decline.go; this freezes
// the umbrella sentinel, the source of record, the never-infer-from-presence
// principle, and a curated set of representative reason codes.
type Declines struct {
	UmbrellaSentinel      string   `json:"umbrella_sentinel"`
	ReasonSource          string   `json:"reason_source"`
	Principle             string   `json:"principle"`
	RepresentativeReasons []string `json:"representative_reasons"`
}

// Fixture is one representative .baml corpus file the manifest is validated
// against. MustParse asserts bamlparser.ParseBytes succeeds; each Capabilities
// entry asserts a genuine feature signal appears in the real source, grounding
// the claim that the fixture set represents the required capability categories.
type Fixture struct {
	ID           string              `json:"id"`
	Path         string              `json:"path"`
	MustParse    bool                `json:"must_parse"`
	Capabilities []FixtureCapability `json:"capabilities"`
}

// FixtureCapability binds a capability code to a regexp signal that must match at
// least Min times in the fixture's raw source.
type FixtureCapability struct {
	Code   string `json:"code"`
	Signal string `json:"signal"`
	Min    int    `json:"min"`
}

// Load parses the embedded manifest. It is the only runtime surface M0 exposes.
//
// Unknown keys are rejected (DisallowUnknownFields): a renamed or misspelled key
// in manifest.json must fail at load rather than leave the matching Go field at
// its zero value, which would let fields no test asserts (declines.*, every
// source_ref, note) drift silently — defeating a frozen, machine-checked manifest.
func Load() (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(manifestJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}
