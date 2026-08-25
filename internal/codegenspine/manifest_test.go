package codegenspine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// repoRoot returns the repository root, computed from this test file's location
// (<root>/internal/codegenspine/manifest_test.go). Using runtime.Caller keeps
// the test independent of the working directory `go test` chooses.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func loadManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := Load()
	if err != nil {
		t.Fatalf("Load manifest: %v", err)
	}
	return m
}

// jsonTagBase returns the wire name of a struct field: the json tag name portion
// (before any ",omitempty"/",omitzero"), or the Go field name when an exported
// field carries no tag — because encoding/json still serializes such a field
// under its Go name, so the conformance check must see it. Unexported fields and
// json:"-" fields return "" (not on the wire). Without the untagged-exported
// fallback, adding an untagged exported field to apierror.Response or
// bamlutils.BamlOptions would change the public wire shape while both freeze
// tests kept passing — the exact drift they exist to catch.
func jsonTagBase(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if tag == "" {
		return f.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		if tag[:i] == "" {
			return f.Name
		}
		return tag[:i]
	}
	return tag
}

func TestManifestLoadsAndVersion(t *testing.T) {
	m := loadManifest(t)
	if m.ManifestVersion < 1 {
		t.Fatalf("manifest_version must be >= 1, got %d", m.ManifestVersion)
	}
}

// TestDescriptorVersionsMatchLiveCode grounds the frozen descriptor versions
// against the live constants. If either package bumps its version, this fails
// until the manifest (and the ADR) are reconciled — the fail-closed fence the
// native lane depends on.
func TestDescriptorVersionsMatchLiveCode(t *testing.T) {
	m := loadManifest(t)
	if got, want := m.DescriptorVersions.Schemadescriptor, schemadescriptor.Version; got != want {
		t.Errorf("descriptor_versions.schemadescriptor = %d, live schemadescriptor.Version = %d", got, want)
	}
	if got, want := m.DescriptorVersions.Promptdescriptor, promptdescriptor.Version; got != want {
		t.Errorf("descriptor_versions.promptdescriptor = %d, live promptdescriptor.Version = %d", got, want)
	}
	if m.DescriptorVersions.ProjectDescriptorPlanned < 1 {
		t.Errorf("project_descriptor_planned must be >= 1, got %d", m.DescriptorVersions.ProjectDescriptorPlanned)
	}
}

// TestDynamicNamesMatchLiveCode grounds the _dynamic endpoint segment and the
// internal dynamic method name against bamlutils.
func TestDynamicNamesMatchLiveCode(t *testing.T) {
	m := loadManifest(t)
	if m.DynamicEndpointName != bamlutils.DynamicEndpointName {
		t.Errorf("dynamic_endpoint_name = %q, live bamlutils.DynamicEndpointName = %q", m.DynamicEndpointName, bamlutils.DynamicEndpointName)
	}
	if m.DynamicMethodName != bamlutils.DynamicMethodName {
		t.Errorf("dynamic_method_name = %q, live bamlutils.DynamicMethodName = %q", m.DynamicMethodName, bamlutils.DynamicMethodName)
	}
}

// TestErrorCodesMatchApierror grounds the frozen error-code list, order-sensitive,
// against the live canonical enum, plus the worker-facing subset.
func TestErrorCodesMatchApierror(t *testing.T) {
	m := loadManifest(t)

	var liveCodes []string
	var liveWorkerFacing []string
	for _, c := range apierror.AllCodes() {
		liveCodes = append(liveCodes, string(c))
		if c.IsWorkerFacing() {
			liveWorkerFacing = append(liveWorkerFacing, string(c))
		}
	}
	if !reflect.DeepEqual(m.ErrorTaxonomy.Codes, liveCodes) {
		t.Errorf("error_taxonomy.codes mismatch\n manifest: %v\n live:     %v", m.ErrorTaxonomy.Codes, liveCodes)
	}
	if !reflect.DeepEqual(m.ErrorTaxonomy.WorkerFacingCodes, liveWorkerFacing) {
		t.Errorf("error_taxonomy.worker_facing_codes mismatch\n manifest: %v\n live:     %v", m.ErrorTaxonomy.WorkerFacingCodes, liveWorkerFacing)
	}
}

// TestErrorEnvelopeFieldsMatchStruct grounds the frozen envelope field names
// against the JSON tags of the live apierror.Response struct, in declaration
// order.
func TestErrorEnvelopeFieldsMatchStruct(t *testing.T) {
	m := loadManifest(t)
	rt := reflect.TypeOf(apierror.Response{})
	var live []string
	for i := 0; i < rt.NumField(); i++ {
		if base := jsonTagBase(rt.Field(i)); base != "" {
			live = append(live, base)
		}
	}
	if !reflect.DeepEqual(m.ErrorTaxonomy.EnvelopeFields, live) {
		t.Errorf("error_taxonomy.envelope_fields mismatch\n manifest: %v\n live:     %v", m.ErrorTaxonomy.EnvelopeFields, live)
	}
}

// TestOptionsEnvelopeMatchesBamlOptions grounds the frozen __baml_options__
// field set against the live bamlutils.BamlOptions struct (json tag + Go name),
// in declaration order.
func TestOptionsEnvelopeMatchesBamlOptions(t *testing.T) {
	m := loadManifest(t)
	if m.OptionsEnvelope.WireKey != "__baml_options__" {
		t.Errorf("options_envelope.wire_key = %q, want %q", m.OptionsEnvelope.WireKey, "__baml_options__")
	}
	rt := reflect.TypeOf(bamlutils.BamlOptions{})
	var live []OptionField
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		base := jsonTagBase(f)
		if base == "" {
			continue
		}
		live = append(live, OptionField{JSON: base, Go: f.Name})
	}
	if !reflect.DeepEqual(m.OptionsEnvelope.Fields, live) {
		t.Errorf("options_envelope.fields mismatch\n manifest: %+v\n live:     %+v", m.OptionsEnvelope.Fields, live)
	}
}

// streamModeByName maps the manifest's StreamMode constant names to the live
// bamlutils values so the endpoint semantics can be grounded.
var streamModeByName = map[string]bamlutils.StreamMode{
	"StreamModeCall":          bamlutils.StreamModeCall,
	"StreamModeStream":        bamlutils.StreamModeStream,
	"StreamModeCallWithRaw":   bamlutils.StreamModeCallWithRaw,
	"StreamModeStreamWithRaw": bamlutils.StreamModeStreamWithRaw,
}

// TestRetainedEndpoints checks the five families are present exactly once and,
// for the four call/stream families, that needs_raw/needs_partials match the
// live bamlutils.StreamMode predicates. /parse carries an empty stream_mode.
func TestRetainedEndpoints(t *testing.T) {
	m := loadManifest(t)

	wantPaths := map[string]bool{
		"/call": false, "/call-with-raw": false, "/stream": false,
		"/stream-with-raw": false, "/parse": false,
	}
	for _, e := range m.RetainedEndpoints {
		seen, known := wantPaths[e.Path]
		if !known {
			t.Errorf("unexpected retained endpoint %q", e.Path)
			continue
		}
		if seen {
			t.Errorf("duplicate retained endpoint %q", e.Path)
		}
		wantPaths[e.Path] = true

		if e.Path == "/parse" {
			if e.StreamMode != "" {
				t.Errorf("/parse stream_mode should be empty, got %q", e.StreamMode)
			}
			continue
		}
		mode, ok := streamModeByName[e.StreamMode]
		if !ok {
			t.Errorf("endpoint %q: unknown stream_mode %q", e.Path, e.StreamMode)
			continue
		}
		if mode.NeedsRaw() != e.NeedsRaw {
			t.Errorf("endpoint %q: needs_raw = %v, live %s.NeedsRaw() = %v", e.Path, e.NeedsRaw, e.StreamMode, mode.NeedsRaw())
		}
		if mode.NeedsPartials() != e.NeedsPartials {
			t.Errorf("endpoint %q: needs_partials = %v, live %s.NeedsPartials() = %v", e.Path, e.NeedsPartials, e.StreamMode, mode.NeedsPartials())
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Errorf("retained endpoint %q missing from manifest", p)
		}
	}
}

// TestFixturesParseAndExhibitCapabilities is the grounding heart of the manifest:
// every declared fixture is a real .baml file that parses via the production
// bamlparser, and each declared capability signal genuinely appears in the raw
// source. This keeps the freeze anchored to code, not to intentions.
func TestFixturesParseAndExhibitCapabilities(t *testing.T) {
	m := loadManifest(t)
	root := repoRoot(t)

	if len(m.Fixtures) == 0 {
		t.Fatal("manifest declares no fixtures")
	}
	for _, fx := range m.Fixtures {
		fx := fx
		t.Run(fx.ID, func(t *testing.T) {
			abs := filepath.Join(root, filepath.FromSlash(fx.Path))
			data, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("read fixture %s: %v", fx.Path, err)
			}
			if fx.MustParse {
				if _, err := bamlparser.ParseBytes(fx.Path, data); err != nil {
					t.Fatalf("fixture %s must parse via bamlparser, got: %v", fx.Path, err)
				}
			}
			if len(fx.Capabilities) == 0 {
				t.Errorf("fixture %s declares no capability signals", fx.ID)
			}
			for _, c := range fx.Capabilities {
				re, err := regexp.Compile(c.Signal)
				if err != nil {
					t.Errorf("fixture %s capability %s: bad signal regexp %q: %v", fx.ID, c.Code, c.Signal, err)
					continue
				}
				n := len(re.FindAllIndex(data, -1))
				if n < c.Min {
					t.Errorf("fixture %s capability %s: signal %q matched %d times, need >= %d", fx.ID, c.Code, c.Signal, n, c.Min)
				}
			}
		})
	}
}

// TestCapabilityCoverageAndConsistency proves the fixture set represents every
// required capability category (dynamic/static, final/stream, strategies, media,
// checks, TypeBuilder), and that every code referenced anywhere is a defined
// capability with a unique code.
func TestCapabilityCoverageAndConsistency(t *testing.T) {
	m := loadManifest(t)

	defined := map[string]bool{}
	for _, c := range m.Capabilities {
		if c.Code == "" {
			t.Error("capability with empty code")
			continue
		}
		if defined[c.Code] {
			t.Errorf("duplicate capability code %q", c.Code)
		}
		defined[c.Code] = true
	}

	exercised := map[string]bool{}
	ids := map[string]bool{}
	for _, fx := range m.Fixtures {
		if ids[fx.ID] {
			t.Errorf("duplicate fixture id %q", fx.ID)
		}
		ids[fx.ID] = true
		for _, c := range fx.Capabilities {
			if !defined[c.Code] {
				t.Errorf("fixture %s references undefined capability code %q", fx.ID, c.Code)
			}
			exercised[c.Code] = true
		}
	}

	for _, req := range m.RequiredCapabilityCoverage {
		if !defined[req] {
			t.Errorf("required_capability_coverage %q is not a defined capability", req)
		}
		if !exercised[req] {
			t.Errorf("required_capability_coverage %q is not exercised by any fixture", req)
		}
	}
}

// TestDynamicTypesCapabilityGroundedInLiveInput grounds the transitional
// `dynamic_types` capability against an ACTUAL runtime TypeBuilder.DynamicTypes
// input, not the schema-side `@@dynamic` marker (which is the separate declined
// `schema_dynamic_class` capability). `DynamicTypes` is a request-time construct,
// not `.baml` syntax, so it cannot be grounded by a fixture signal; instead we
// prove a real `__baml_options__`-shaped payload deserializes into a populated
// bamlutils.TypeBuilder.DynamicTypes.
func TestDynamicTypesCapabilityGroundedInLiveInput(t *testing.T) {
	m := loadManifest(t)

	// The two dynamic capabilities must be distinct and correctly classified.
	byCode := map[string]Capability{}
	for _, c := range m.Capabilities {
		byCode[c.Code] = c
	}
	dt, ok := byCode["dynamic_types"]
	if !ok {
		t.Fatal("manifest missing capability dynamic_types")
	}
	if dt.ProvenStatus != "transitional" {
		t.Errorf("dynamic_types proven_status = %q, want transitional", dt.ProvenStatus)
	}
	sd, ok := byCode["schema_dynamic_class"]
	if !ok {
		t.Fatal("manifest missing capability schema_dynamic_class")
	}
	if sd.ProvenStatus != "declined" {
		t.Errorf("schema_dynamic_class proven_status = %q, want declined (the @@dynamic schema marker is fail-closed declined)", sd.ProvenStatus)
	}

	// A real per-call options payload carrying a TypeBuilder.DynamicTypes overlay
	// must deserialize into the live bamlutils types with DynamicTypes populated.
	const payload = `{"type_builder":{"dynamic_types":{"classes":{"Person":{"properties":{"name":{"type":"string"}}}},"preserve_order":true}}}`
	var opts bamlutils.BamlOptions
	if err := json.Unmarshal([]byte(payload), &opts); err != nil {
		t.Fatalf("unmarshal TypeBuilder.DynamicTypes input: %v", err)
	}
	if opts.TypeBuilder == nil {
		t.Fatal("type_builder did not deserialize")
	}
	if opts.TypeBuilder.DynamicTypes == nil {
		t.Fatal("dynamic_types did not deserialize into bamlutils.TypeBuilder.DynamicTypes")
	}
}

// TestCodegenAdmissionDeclineCatalogue grounds the M1 native codegen classifier's
// decline vocabulary against the manifest: the exact set of codes
// internal/nativespine can emit must equal manifest declines.codegen_admission_declines,
// and every one of those codes must itself be catalogued (a capability code, or a
// serving representative reason). This is the "declines use codes from the
// manifest" contract (scope §4), machine-checked against the live classifier.
func TestCodegenAdmissionDeclineCatalogue(t *testing.T) {
	m := loadManifest(t)

	manifestSet := map[string]bool{}
	for _, c := range m.Declines.CodegenAdmissionDeclines {
		manifestSet[c] = true
	}
	liveSet := map[string]bool{}
	for _, c := range nativespine.DeclineCodes() {
		if liveSet[string(c)] {
			t.Errorf("duplicate code %q in nativespine.DeclineCodes()", c)
		}
		liveSet[string(c)] = true
	}
	if !reflect.DeepEqual(manifestSet, liveSet) {
		t.Errorf("codegen_admission_declines mismatch\n manifest: %v\n live nativespine.DeclineCodes(): %v",
			m.Declines.CodegenAdmissionDeclines, nativespine.DeclineCodes())
	}
	if len(m.Declines.CodegenAdmissionDeclines) == 0 {
		t.Fatal("codegen_admission_declines is empty")
	}

	// Non-empty, unique codes. Codes that reuse a serving concept (media_*,
	// checks, asserts, schema_dynamic_class, strategy_*, provider_not_openai,
	// model_not_literal) must be spelled exactly as the capability / representative
	// reason they mirror; the remainder (unsupported_*_shape) are codegen-native
	// and catalogued here.
	capSet := map[string]bool{}
	for _, c := range m.Capabilities {
		capSet[c.Code] = true
	}
	reasonSet := map[string]bool{}
	for _, r := range m.Declines.RepresentativeReasons {
		reasonSet[r] = true
	}
	codegenNative := map[string]bool{
		"unsupported_output_shape": true,
		"unsupported_input_shape":  true,
		"prompt_dependency":        true,
		"name_collision":           true,
	}
	seen := map[string]bool{}
	for _, c := range m.Declines.CodegenAdmissionDeclines {
		if c == "" {
			t.Error("empty codegen decline code")
			continue
		}
		if seen[c] {
			t.Errorf("duplicate codegen decline code %q", c)
		}
		seen[c] = true
		if !capSet[c] && !reasonSet[c] && !codegenNative[c] {
			t.Errorf("codegen decline code %q is neither a capability, a representative reason, nor a known codegen-native code", c)
		}
	}
}

// TestCapabilityProvenStatusVocabulary keeps proven_status a closed vocabulary
// so a later milestone can machine-branch on it.
func TestCapabilityProvenStatusVocabulary(t *testing.T) {
	m := loadManifest(t)
	allowed := map[string]bool{"proven": true, "declined": true, "transitional": true}
	for _, c := range m.Capabilities {
		if !allowed[c.ProvenStatus] {
			t.Errorf("capability %q has proven_status %q outside {proven,declined,transitional}", c.Code, c.ProvenStatus)
		}
	}
}

// TestGuardedPathsFrozen keeps the guarded-path list in the manifest aligned
// with the three collision prefixes the source-guard enforces.
func TestGuardedPathsFrozen(t *testing.T) {
	m := loadManifest(t)
	want := []string{"internal/debaml", "nativeserve", "internal/nativebody/nanollmprepare"}
	if !reflect.DeepEqual(m.GuardedPaths, want) {
		t.Errorf("guarded_paths = %v, want %v", m.GuardedPaths, want)
	}
	// Each guarded prefix must exist on disk.
	root := repoRoot(t)
	for _, p := range m.GuardedPaths {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil || !fi.IsDir() {
			t.Errorf("guarded path %q is not a directory in the tree (err=%v)", p, err)
		}
	}
}
