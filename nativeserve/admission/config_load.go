package admission

import (
	"fmt"
	"os"
	"strings"
)

// De-BAML serving cutover S1 — the CONFIG-LOAD / control-plane inventory path.
//
// # What the scope asks for, and what this is
//
// S1 owes "a config-load/control-plane inventory mapping opaque configuration IDs to
// operator-visible configuration records". The record TYPE and its publication live
// in cohort.go and metrics.go; this file is the missing half — the path by which a
// real deployment declares which of the predeclared opaque slots it uses, and what
// each one stands for.
//
// A deployment sets ConfigInventoryEnv to a bounded spec; a native-capable worker
// decodes it at config load, and the declared records are published as
// baml_rest_debaml_config_inventory_info so operators can join each opaque ID to
// their own approved-configuration document, offline. Nothing about the real
// configuration — no client name, model, URL, prompt, header or secret — is in the
// spec, because none of those can be spelled in any of its five fields.
//
// # DECLARING IS NOT ENROLLING, and this path cannot enroll
//
// This is the load-bearing property. The spec carries records only; the ENROLLMENT
// manifest (productionEnrollments) is compile-time and unreachable from configuration.
// A deployment can declare every slot it likes and every one of them still declines,
// because the policy that permits a claim has no entry for any of them. So this is not
// a second rollout switch and not a way around BAML_REST_USE_DEBAML — it is a
// dashboard/label input whose worst case is an operator seeing a row they did not mean
// to declare. TestConfigLoadCanDeclareButNeverEnroll drives a fully-populated spec
// across every surface and requires every request to still decline.
//
// # Fail loudly, not quietly
//
// A malformed spec returns an error, which the metrics constructor propagates and
// workerboot turns into a boot failure. It does NOT degrade to an empty inventory: a
// worker that silently published nothing because someone fat-fingered a separator
// would leave operators reading a dashboard that looks default-deny for the wrong
// reason.

// ConfigInventoryEnv is the environment variable a deployment declares its
// configuration inventory in. Absent or empty means "declare nothing", which is what
// every current deployment does and what the shipped default is.
const ConfigInventoryEnv = "BAML_REST_DEBAML_CONFIG_INVENTORY"

// configRecordFields is the number of colon-separated fields in one spec record.
const configRecordFields = 5

// LoadDeclaredInventory decodes a configuration-inventory spec into validated,
// operator-visible records.
//
// The grammar is deliberately tiny, positional and unquoted, so there is nowhere for
// free text to hide. Records are separated by `,` or newline; fields within a record
// by `:`; surfaces within the surface field by `|`:
//
//	cfg001:openai_unary_a:dynamic_call|dynamic_stream:openai:DEBAML-1234
//	<opaque ID>:<cohort>:<surfaces>:<provider class>:<approval ref>
//
// Every field is decoded by the SAME validator the compile-time manifest uses, so a
// config-loaded record is exactly as bounded as a hand-written one: the ID must be in
// the predeclared vocabulary, the cohort must be a non-reserved bounded bucket, the
// surfaces must be declared surfaces, the provider must be a declared CLASS, and the
// approval reference must match its tiny grammar. Whitespace around records and
// fields is trimmed; nothing else is repaired.
//
// An empty spec yields an empty inventory and no error.
func LoadDeclaredInventory(spec string) (*ConfigInventory, error) {
	records, err := parseInventorySpec(spec)
	if err != nil {
		return nil, err
	}
	return newConfigInventory(records)
}

// parseInventorySpec decodes the spec into unvalidated-but-well-formed records;
// newConfigInventory then applies every field validator and the collision/cap rules.
func parseInventorySpec(spec string) ([]ConfigRecord, error) {
	entries := strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == '\n' })
	out := make([]ConfigRecord, 0, len(entries))
	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, ":")
		if len(fields) != configRecordFields {
			return nil, fmt.Errorf("nativeserve/admission: inventory spec record %d has %d fields, want %d (<id>:<cohort>:<surfaces>:<provider>:<approval>)",
				i, len(fields), configRecordFields)
		}
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}
		surfaces, err := parseSurfaceList(fields[2])
		if err != nil {
			return nil, fmt.Errorf("nativeserve/admission: inventory spec record %d: %w", i, err)
		}
		out = append(out, ConfigRecord{
			Fingerprint: ConfigFingerprint(fields[0]),
			Cohort:      CohortID(fields[1]),
			Surfaces:    surfaces,
			Provider:    ConfigProviderClass(fields[3]),
			Approval:    ApprovalRef(fields[4]),
		})
	}
	return out, nil
}

// parseSurfaceList decodes `dynamic_call|static_call` into the closed enum. An
// unknown surface is an error, never a silently-dropped element: a record that
// declared three surfaces and loaded two would misrepresent what an operator
// approved.
func parseSurfaceList(field string) ([]Surface, error) {
	parts := strings.Split(field, "|")
	out := make([]Surface, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty surface in surface list")
		}
		found := false
		for _, s := range AllSurfaces() {
			if s.Label() == p {
				out = append(out, s)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown surface %q", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("surface list is empty")
	}
	return out, nil
}

// loadProductionInventory reads the deployment's declared inventory from the
// environment at config load. It is the ONLY source of production inventory records:
// productionInventoryRecords() is deliberately empty, because which class a
// deployment declares — and under which approval reference — is operator input, not
// something this repository may guess.
func loadProductionInventory() (*ConfigInventory, error) {
	spec := strings.TrimSpace(os.Getenv(ConfigInventoryEnv))
	compiled := productionInventoryRecords()
	if spec == "" {
		return newConfigInventory(compiled)
	}
	loaded, err := parseInventorySpec(spec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigInventoryEnv, err)
	}
	return newConfigInventory(append(append([]ConfigRecord(nil), compiled...), loaded...))
}
